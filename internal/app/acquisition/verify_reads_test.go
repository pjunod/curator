package acquisition

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestVerifyReadBudgetParsing(t *testing.T) {
	limit := func(b *verifyBudget) rate.Limit {
		if b == nil {
			return 0
		}
		return b.background.Limit()
	}
	if b := newVerifyBudget(""); b == nil || limit(b) != rate.Limit(defaultVerifyReadMBps*1e6) || b.foreground.Limit() != limit(b) || b.yield.Limit() != limit(b)/10 {
		t.Fatalf("unset budget = %v, want the %d MB/s default", limit(b), defaultVerifyReadMBps)
	}
	if b := newVerifyBudget("0"); b != nil {
		t.Fatal("0 must mean unlimited")
	}
	if b := newVerifyBudget("250"); limit(b) != rate.Limit(250e6) {
		t.Fatalf("250 = %v", limit(b))
	}
	for _, bad := range []string{"fast", "-5", "1.5"} {
		if b := newVerifyBudget(bad); limit(b) != rate.Limit(defaultVerifyReadMBps*1e6) {
			t.Fatalf("%q must keep the default, not turn the budget off: %v", bad, limit(b))
		}
	}
}

func testBudget(bytesPerSecond float64) *verifyBudget {
	l := rate.Limit(bytesPerSecond)
	return &verifyBudget{foreground: rate.NewLimiter(l, 1<<20), background: rate.NewLimiter(l, 1<<20), yield: rate.NewLimiter(l/10, 1<<20)}
}

// Review finding: an import holds its title's target while it hashes, and
// background proofs drawing on the same budget starved it. Foreground reads
// have their own budget, and background reads yield while one is active.
func TestForegroundVerificationIsNotStarvedByBackgroundReads(t *testing.T) {
	b := testBudget(10 << 20)
	held, release := (&Service{}).lockImportTarget(context.Background(), 1)
	defer release()
	if !foregroundVerification(held) || !foregroundVerification(withForegroundVerification(context.Background())) || foregroundVerification(context.Background()) {
		t.Fatal("foreground classification is wrong")
	}
	// Exhaust the background budget; foreground must not wait on it.
	_ = b.background.WaitN(context.Background(), 1<<20)
	start := time.Now()
	fg := &verifyReader{ctx: held, r: bytes.NewReader(make([]byte, 1<<20)), budget: b, foreground: true}
	if _, err := io.Copy(io.Discard, fg); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("a foreground read waited %v on the background budget", elapsed)
	}
	// With a foreground read just seen, background reads also pay the yield
	// budget (1 MiB/s here): 2 MiB past the burst takes about a second.
	_ = b.yield.WaitN(context.Background(), 1<<20)
	start = time.Now()
	bg := &verifyReader{ctx: context.Background(), r: bytes.NewReader(make([]byte, 1<<19)), budget: b}
	if _, err := io.Copy(io.Discard, bg); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("a background read beside a foreground one took %v; it did not yield", elapsed)
	}
}

// rate.Limiter refuses a wait that would pass the deadline with an error
// that does not wrap context.DeadlineExceeded; callers must still see one.
func TestVerifyReadDeadlineIsReportedAsDeadline(t *testing.T) {
	b := testBudget(1)
	_ = b.background.WaitN(context.Background(), 1<<20)
	// The wait (64 bytes at 1 B/s) is far past the deadline, so the limiter
	// refuses at once rather than waiting.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v := &verifyReader{ctx: ctx, r: bytes.NewReader(make([]byte, 64)), budget: b}
	_, err := v.Read(make([]byte, 64))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// A leader that panics must not strand waiters on its entry.
func TestDigestLeaderPanicReleasesWaiters(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		defer func() { _ = recover() }()
		_, _, _, _ = joinDigest(context.Background(), "panicking-file", func() (string, int64, string, error) {
			close(started)
			<-release
			panic("read exploded")
		})
	}()
	<-started
	result := make(chan string, 1)
	go func() {
		d, _, _, _ := joinDigest(context.Background(), "panicking-file", func() (string, int64, string, error) { return "reread", 1, "g", nil })
		result <- d
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	select {
	case d := <-result:
		if d != "reread" {
			t.Fatalf("waiter got %q, want its own read after the leader panicked", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter stranded by a panicking leader")
	}
}

// The budget is real: reading past the burst waits for it.
func TestVerifyReaderPacesToTheBudget(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 3<<20)
	v := &verifyReader{ctx: context.Background(), r: bytes.NewReader(data), budget: testBudget(10 << 20)}
	start := time.Now()
	n, err := io.Copy(io.Discard, v)
	if err != nil || n != int64(len(data)) {
		t.Fatalf("copied %d err=%v", n, err)
	}
	// 1 MiB burst, then 2 MiB at 10 MiB/s: at least ~200 ms.
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("3 MiB at a 10 MiB/s budget took %v; the budget is not applied", elapsed)
	}
}

func TestVerifyReaderStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := newVerifyReader(ctx, bytes.NewReader([]byte("x")))
	if _, err := v.Read(make([]byte, 1)); err == nil {
		t.Fatal("a cancelled verification read must stop")
	}
}

// Field report: three redundancy proofs hashed the same 2.9 GB library file
// at once. Concurrent digests of one unchanged file now share a single read.
func TestConcurrentDigestsOfOneFileShareARead(t *testing.T) {
	var reads atomic.Int64
	release := make(chan struct{})
	read := func() (string, int64, string, error) {
		reads.Add(1)
		<-release
		return "digest", 4, "gen", nil
	}
	var wg sync.WaitGroup
	results := make(chan string, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, _, _, err := joinDigest(context.Background(), "same-file", read)
			if err != nil {
				t.Error(err)
			}
			results <- d
		}()
	}
	// Let all three reach the shared call before the read finishes.
	deadline := time.Now().Add(2 * time.Second)
	for {
		sharedDigests.mu.Lock()
		_, inflight := sharedDigests.inflight["same-file"]
		sharedDigests.mu.Unlock()
		if inflight || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	for d := range results {
		if d != "digest" {
			t.Fatalf("digest = %q", d)
		}
	}
	if got := reads.Load(); got != 1 {
		t.Fatalf("reads = %d, want one shared read", got)
	}
	// Not a cache: the next digest reads again.
	release = make(chan struct{})
	close(release)
	if _, _, _, err := joinDigest(context.Background(), "same-file", read); err != nil || reads.Load() != 2 {
		t.Fatalf("a later digest must read the file again: reads=%d err=%v", reads.Load(), err)
	}
}

// A leader whose own context ended must not fail a waiter that is still live.
func TestDigestWaiterRereadsWhenTheLeaderWasCancelled(t *testing.T) {
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	started := make(chan struct{})
	var reads atomic.Int64
	go func() {
		_, _, _, _ = joinDigest(leaderCtx, "leader-cancel", func() (string, int64, string, error) {
			reads.Add(1)
			close(started)
			<-leaderCtx.Done()
			return "", 0, "", leaderCtx.Err()
		})
	}()
	<-started
	done := make(chan error, 1)
	go func() {
		d, _, _, err := joinDigest(context.Background(), "leader-cancel", func() (string, int64, string, error) {
			reads.Add(1)
			return "mine", 1, "g", nil
		})
		if err == nil && d != "mine" {
			err = io.ErrUnexpectedEOF
		}
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	if err := <-done; err != nil {
		t.Fatalf("live waiter failed with the leader's cancellation: %v", err)
	}
}

// fileDigestSnapshot binds a shared result to the generation this caller saw.
func TestFileDigestSnapshotStillDetectsChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.mkv")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	d1, n, gen, err := fileDigestSnapshot(context.Background(), path)
	if err != nil || n != 3 || gen == "" || d1 == "" {
		t.Fatalf("digest=%q n=%d gen=%q err=%v", d1, n, gen, err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("two!"), 0o644); err != nil {
		t.Fatal(err)
	}
	d2, n2, gen2, err := fileDigestSnapshot(context.Background(), path)
	if err != nil || d2 == d1 || n2 != 4 || gen2 == gen {
		t.Fatalf("second digest = %q n=%d gen=%q err=%v, want the new bytes", d2, n2, gen2, err)
	}
}
