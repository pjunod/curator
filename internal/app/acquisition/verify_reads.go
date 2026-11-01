package acquisition

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

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

const defaultVerifyReadMBps = 100

// verifyReadBurst bounds a single paced read, so a large buffer cannot take
// the whole budget in one call and the limiter's burst stays meaningful.
const verifyReadBurst = 4 << 20

var verifyReadLimiter = newVerifyReadLimiter(os.Getenv("MONARR_VERIFY_READ_MBPS"))

func newVerifyReadLimiter(raw string) *rate.Limiter {
	mbps := defaultVerifyReadMBps
	if v := strings.TrimSpace(raw); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil || parsed < 0 {
			// A typo must not silently become "unlimited"; keep the default.
			parsed = defaultVerifyReadMBps
		}
		mbps = parsed
	}
	if mbps == 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(float64(mbps)*1e6), verifyReadBurst)
}

// verifyReader paces reads against the shared verification budget and stops
// at cancellation. Every content digest reads through one of these.
type verifyReader struct {
	ctx     context.Context
	r       io.Reader
	limiter *rate.Limiter
}

func newVerifyReader(ctx context.Context, r io.Reader) *verifyReader {
	return &verifyReader{ctx: ctx, r: r, limiter: verifyReadLimiter}
}

func (v *verifyReader) Read(p []byte) (int, error) {
	if err := v.ctx.Err(); err != nil {
		return 0, err
	}
	if v.limiter != nil && len(p) > verifyReadBurst {
		p = p[:verifyReadBurst]
	}
	n, err := v.r.Read(p)
	if n > 0 && v.limiter != nil {
		if waitErr := v.limiter.WaitN(v.ctx, n); waitErr != nil {
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
}

// joinDigest runs read once per key among concurrent callers. The leader's
// context governs the read; a waiter whose own context ends stops waiting.
func joinDigest(ctx context.Context, key string, read func() (string, int64, string, error)) (string, int64, string, error) {
	sharedDigests.mu.Lock()
	if call, ok := sharedDigests.inflight[key]; ok {
		sharedDigests.mu.Unlock()
		select {
		case <-call.done:
			// The leader's cancellation is not this caller's: read again.
			if call.err != nil && (errors.Is(call.err, context.Canceled) || errors.Is(call.err, context.DeadlineExceeded)) && ctx.Err() == nil {
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

	call.digest, call.size, call.generation, call.err = read()

	sharedDigests.mu.Lock()
	delete(sharedDigests.inflight, key)
	sharedDigests.mu.Unlock()
	close(call.done)
	return call.digest, call.size, call.generation, call.err
}
