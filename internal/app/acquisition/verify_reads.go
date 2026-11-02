package acquisition

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Content verification — the digests that bind a payload to its manifest,
// prove a duplicate redundant, and check a placement — reads whole files, and
// on a NAS every one of those reads crosses the storage network. Field report
// 2026-11-01: Curator's verification reads held nuc3's 2.5 GbE link to the
// QNAP at line rate for hours (three proofs hashing the same 2.9 GB library
// file at once, plus a 19.6 GB placement check), and each time a write joined
// that load the NAS stopped answering NFS for half a minute to nine minutes.
// Hard mounts froze everything that touched the storage, Curator and Runner
// alike, six times in one day.
//
// Two things keep verification a background citizen:
//
//   - one shared read budget for every verification read in the process,
//     so their sum leaves headroom for placement writes, Runner and playback;
//   - concurrent digests of the same unchanged file share one read.
//
// The budget is MONARR_VERIFY_READ_MBPS megabytes per second (default 100,
// about a third of a 2.5 GbE link; 0 = unlimited). It paces verification
// only. Placement copies run at full speed: they are the import itself.
//
// Reads made while holding a title's import target, or on behalf of an
// operator's request, are foreground: they hold up other work (an import
// lock, an HTTP response) and draw from their own budget. While any
// foreground read is active, background reads — redundancy proofs and
// verification outside the lock — slow to a tenth of the budget instead of
// competing, so the lock holder is never starved by work that is not
// waiting on anything.

const defaultVerifyReadMBps = 100

// verifyReadBurst bounds a single paced read, so a large buffer cannot take
// the whole budget in one call and the limiter's burst stays meaningful.
const verifyReadBurst = 4 << 20

var verifyReads = newVerifyBudget(os.Getenv("MONARR_VERIFY_READ_MBPS"))

// verifyBudget is the process's verification read budget: a foreground
// limiter for reads something is waiting on, a background limiter for the
// rest, and a much smaller yield limiter background reads also draw from
// while foreground reads are in progress (one seen within foregroundWindow).
// A nil budget is unlimited.
type verifyBudget struct {
	foreground, background, yield *rate.Limiter
	lastForeground                atomic.Int64 // unix nanoseconds
}

// foregroundWindow is how recently a foreground read must have happened for
// background reads to yield. Time-based, so a reader abandoned before EOF
// can never leave background reads slowed.
const foregroundWindow = time.Second

func newVerifyBudget(raw string) *verifyBudget {
	mbps := verifyReadMBps(raw)
	if mbps == 0 {
		return nil
	}
	limit := rate.Limit(float64(mbps) * 1e6)
	return &verifyBudget{
		foreground: rate.NewLimiter(limit, verifyReadBurst),
		background: rate.NewLimiter(limit, verifyReadBurst),
		yield:      rate.NewLimiter(limit/10, verifyReadBurst),
	}
}

func verifyReadMBps(raw string) int {
	v := strings.TrimSpace(raw)
	if v == "" {
		return defaultVerifyReadMBps
	}
	parsed, err := strconv.Atoi(v)
	if err != nil || parsed < 0 {
		// A typo must not silently become "unlimited"; keep the default, and
		// say so.
		slog.Warn("ignoring invalid MONARR_VERIFY_READ_MBPS; using the default",
			"value", raw, "defaultMBps", defaultVerifyReadMBps)
		return defaultVerifyReadMBps
	}
	return parsed
}

type foregroundVerifyKey struct{}

// withForegroundVerification marks reads made for an operator's request as
// foreground (something is waiting on them).
func withForegroundVerification(ctx context.Context) context.Context {
	return context.WithValue(ctx, foregroundVerifyKey{}, true)
}

func foregroundVerification(ctx context.Context) bool {
	if _, held := heldTarget(ctx); held {
		return true
	}
	fg, _ := ctx.Value(foregroundVerifyKey{}).(bool)
	return fg
}

// wait charges n bytes read for ctx against the budget.
func (b *verifyBudget) wait(ctx context.Context, foreground bool, n int) error {
	var err error
	if foreground {
		b.lastForeground.Store(time.Now().UnixNano())
		err = b.foreground.WaitN(ctx, n)
	} else {
		err = b.background.WaitN(ctx, n)
		if err == nil && time.Since(time.Unix(0, b.lastForeground.Load())) < foregroundWindow {
			err = b.yield.WaitN(ctx, n)
		}
	}
	if err != nil && ctx.Err() == nil {
		// rate.Limiter refuses a wait that would overrun the context's
		// deadline without wrapping context.DeadlineExceeded. Report it as
		// the deadline it is, so callers (and joinDigest) recognize it.
		return fmt.Errorf("%w: %v", context.DeadlineExceeded, err)
	}
	if err != nil {
		return ctx.Err()
	}
	return nil
}

// verifyReader paces reads against the shared verification budget and stops
// at cancellation. Every content digest reads through one of these.
type verifyReader struct {
	ctx        context.Context
	r          io.Reader
	budget     *verifyBudget
	foreground bool
}

func newVerifyReader(ctx context.Context, r io.Reader) *verifyReader {
	return &verifyReader{ctx: ctx, r: r, budget: verifyReads, foreground: foregroundVerification(ctx)}
}

func (v *verifyReader) Read(p []byte) (int, error) {
	if err := v.ctx.Err(); err != nil {
		return 0, err
	}
	if v.budget == nil {
		return v.r.Read(p)
	}
	if len(p) > verifyReadBurst {
		p = p[:verifyReadBurst]
	}
	n, err := v.r.Read(p)
	if n > 0 {
		if waitErr := v.budget.wait(v.ctx, v.foreground, n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

// sharedDigests lets concurrent digests of one file generation share a read.
// It is not a cache: an entry lives only while its read is in flight, and a
// caller joins only when the file's generation before the read matches the
// one the read started from. Each caller still checks, after the shared read
// returns, that the file it names is that same generation.
var sharedDigests = struct {
	mu       sync.Mutex
	inflight map[string]*sharedDigest
}{inflight: map[string]*sharedDigest{}}

type sharedDigest struct {
	done       chan struct{}
	digest     string
	size       int64
	generation string
	err        error
	// leaderEnded: the read failed because the leader's own context ended
	// (or it panicked), which says nothing about the file.
	leaderEnded bool
}

// joinDigest runs read once per key among concurrent callers. The leader's
// context governs the read; a waiter whose own context ends stops waiting.
func joinDigest(ctx context.Context, key string, read func() (string, int64, string, error)) (string, int64, string, error) {
	sharedDigests.mu.Lock()
	if call, ok := sharedDigests.inflight[key]; ok {
		sharedDigests.mu.Unlock()
		select {
		case <-call.done:
			// The leader's cancellation, deadline or panic is not this
			// caller's: read again.
			if call.err != nil && (call.leaderEnded || errors.Is(call.err, context.Canceled) || errors.Is(call.err, context.DeadlineExceeded)) && ctx.Err() == nil {
				return read()
			}
			return call.digest, call.size, call.generation, call.err
		case <-ctx.Done():
			return "", 0, "", ctx.Err()
		}
	}
	call := &sharedDigest{done: make(chan struct{})}
	sharedDigests.inflight[key] = call
	sharedDigests.mu.Unlock()

	// Release waiters however the read ends, a panic included: a stranded
	// entry would hold every later digest of this file until its context
	// ended, possibly while holding an import target.
	finished := false
	defer func() {
		if !finished {
			call.err, call.leaderEnded = fmt.Errorf("shared digest read panicked"), true
		}
		sharedDigests.mu.Lock()
		delete(sharedDigests.inflight, key)
		sharedDigests.mu.Unlock()
		close(call.done)
	}()
	call.digest, call.size, call.generation, call.err = read()
	call.leaderEnded = call.err != nil && ctx.Err() != nil
	finished = true
	return call.digest, call.size, call.generation, call.err
}
