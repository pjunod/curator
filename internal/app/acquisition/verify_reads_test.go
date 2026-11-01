package acquisition

import (
	"bytes"
	"context"
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
	if l := newVerifyReadLimiter(""); l == nil || l.Limit() != rate.Limit(defaultVerifyReadMBps*1e6) {
		t.Fatalf("unset budget = %v, want the %d MB/s default", l, defaultVerifyReadMBps)
	}
	if l := newVerifyReadLimiter("0"); l != nil {
		t.Fatal("0 must mean unlimited")
	}
	if l := newVerifyReadLimiter("250"); l == nil || l.Limit() != rate.Limit(250e6) {
		t.Fatalf("250 = %v", l)
	}
	for _, bad := range []string{"fast", "-5", "1.5"} {
		if l := newVerifyReadLimiter(bad); l == nil || l.Limit() != rate.Limit(defaultVerifyReadMBps*1e6) {
			t.Fatalf("%q must keep the default, not turn the budget off: %v", bad, l)
		}
	}
}

// The budget is real: reading past the burst waits for it.
func TestVerifyReaderPacesToTheBudget(t *testing.T) {
	data := bytes.Repeat([]byte{7}, 3<<20)
	v := &verifyReader{ctx: context.Background(), r: bytes.NewReader(data), limiter: rate.NewLimiter(rate.Limit(10<<20), 1<<20)}
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
