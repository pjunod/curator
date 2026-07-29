package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/jobs"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func newStore(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestQueueRunsAJob(t *testing.T) {
	db := newStore(t)
	q := jobs.New(db, nil, nil, jobs.Options{Workers: 2, PollInterval: 10 * time.Millisecond})

	var ran atomic.Int64
	if err := q.Register("test.ok", func(context.Context, domain.Job) error {
		ran.Add(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	if _, err := q.Enqueue(ctx, domain.Job{Kind: "test.ok"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to run", func() bool { return ran.Load() == 1 })

	waitFor(t, "the job to be marked done", func() bool {
		counts, err := q.Counts(ctx)
		return err == nil && counts["done"] == 1
	})
}

// The claim is the whole point: N workers must never run one job twice.
func TestClaimIsExclusiveUnderConcurrency(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	const total = 40
	for i := 0; i < total; i++ {
		if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "test.race"}); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			owner := "node-" + string(rune('a'+n))
			for {
				job, err := db.ClaimJob(ctx, owner, nil, time.Minute)
				if errors.Is(err, sqlite.ErrNotFound) {
					return
				}
				if err != nil {
					// SQLite serialises writers; a busy database is
					// contention, not corruption — try again.
					continue
				}
				mu.Lock()
				seen[job.ID]++
				mu.Unlock()
				if err := db.CompleteJob(ctx, job.ID); err != nil {
					t.Errorf("complete: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("claimed %d distinct jobs, want %d", len(seen), total)
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("job %d claimed %d times — a claim must be exclusive", id, times)
		}
	}
}

// A dedupe key gives "exactly one" without electing a leader.
func TestDedupeKeyPreventsStacking(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()
	q := jobs.New(db, nil, nil, jobs.Options{})

	added, err := q.EnqueueUnique(ctx, domain.Job{Kind: "rss.sync"})
	if err != nil || !added {
		t.Fatalf("first enqueue: added=%v err=%v", added, err)
	}
	added, err = q.EnqueueUnique(ctx, domain.Job{Kind: "rss.sync"})
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Fatal("a second identical job must not stack up behind the first")
	}

	// Once the first is done, the next run may be enqueued again.
	job, err := db.ClaimJob(ctx, "node", nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if added, err := q.EnqueueUnique(ctx, domain.Job{Kind: "rss.sync"}); err != nil || !added {
		t.Fatalf("after completion the next run should enqueue: added=%v err=%v", added, err)
	}
}

func TestFailingJobRetriesThenFails(t *testing.T) {
	db := newStore(t)
	q := jobs.New(db, nil, nil, jobs.Options{Workers: 1, PollInterval: 5 * time.Millisecond})

	var attempts atomic.Int64
	if err := q.Register("test.flaky", func(context.Context, domain.Job) error {
		attempts.Add(1)
		return errors.New("nope")
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	// MaxAttempts 1 so the first failure is terminal — the backoff on a
	// real retry is deliberately slower than a test should wait for.
	if _, err := q.Enqueue(ctx, domain.Job{Kind: "test.flaky", MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to fail", func() bool {
		counts, err := q.Counts(ctx)
		return err == nil && counts["failed"] == 1
	})
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

// ErrPermanent means retrying cannot help, so the attempts budget is not
// spent pretending otherwise.
func TestPermanentFailureSkipsRetries(t *testing.T) {
	db := newStore(t)
	q := jobs.New(db, nil, nil, jobs.Options{Workers: 1, PollInterval: 5 * time.Millisecond})

	var attempts atomic.Int64
	if err := q.Register("test.permanent", func(context.Context, domain.Job) error {
		attempts.Add(1)
		return fmt.Errorf("%w: malformed payload", jobs.ErrPermanent)
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	if _, err := q.Enqueue(ctx, domain.Job{Kind: "test.permanent", MaxAttempts: 5}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to fail immediately", func() bool {
		counts, err := q.Counts(ctx)
		return err == nil && counts["failed"] == 1
	})
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 — a permanent failure must not retry", got)
	}
}

// A node that dies mid-job strands nothing: the lease expires and the work
// returns to the queue.
func TestExpiredLeaseReturnsWorkToTheQueue(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "test.orphan", MaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	// Claim with a lease that is already expired, simulating a node that
	// took the work and died.
	if _, err := db.ClaimJob(ctx, "dead-node", nil, -time.Second); err != nil {
		t.Fatal(err)
	}
	n, err := db.ReclaimExpiredLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reclaimed %d, want 1", n)
	}
	job, err := db.ClaimJob(ctx, "live-node", nil, time.Minute)
	if err != nil {
		t.Fatalf("reclaimed work should be claimable: %v", err)
	}
	if job.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (the dead node's attempt counts)", job.Attempts)
	}
}

// Reclaiming past the attempt budget fails the job rather than looping it
// forever — a job that kills its worker would otherwise be immortal.
func TestReclaimFailsExhaustedJobs(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "test.poison", MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJob(ctx, "dead", nil, -time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReclaimExpiredLeases(ctx); err != nil {
		t.Fatal(err)
	}
	counts, err := db.CountJobsByState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["failed"] != 1 {
		t.Fatalf("counts = %v, want one failed", counts)
	}
}

// Capability routing: a job requiring a tag only runs on a node that has it.
func TestCapabilityAndAffinityRouting(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	if _, err := db.EnqueueJob(ctx, domain.Job{
		Kind: "test.mounted", RequiredCapability: "mount:media-nas",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJob(ctx, "no-mounts", nil, time.Minute); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("a node without the tag must not claim it, got %v", err)
	}
	if _, err := db.ClaimJob(ctx, "nas-node", []string{"mount:media-nas"}, time.Minute); err != nil {
		t.Fatalf("a node with the tag should claim it: %v", err)
	}

	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "test.pinned", AffinityNode: "node-b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJob(ctx, "node-a", nil, time.Minute); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatal("pinned work must not run on another node")
	}
	if _, err := db.ClaimJob(ctx, "node-b", nil, time.Minute); err != nil {
		t.Fatalf("pinned work should run on its node: %v", err)
	}
}

// Priority decides order, so a user pressing a button outranks a sweep.
func TestPriorityOrdersTheQueue(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "background", Priority: 200}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "interactive", Priority: 10}); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimJob(ctx, "node", nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != "interactive" {
		t.Fatalf("claimed %q first, want the higher-priority interactive job", job.Kind)
	}
}

// run_after is honoured, which is what makes retry backoff work.
func TestRunAfterDefersWork(t *testing.T) {
	db := newStore(t)
	ctx := context.Background()

	if _, err := db.EnqueueJob(ctx, domain.Job{
		Kind: "later", RunAfter: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJob(ctx, "node", nil, time.Minute); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatal("work scheduled for later must not be claimable now")
	}
}

// An unknown kind is permanent by definition — no retry will teach this
// binary about it. This is what a rolling restart mid-upgrade looks like.
func TestUnknownKindFailsWithoutRetrying(t *testing.T) {
	db := newStore(t)
	q := jobs.New(db, nil, nil, jobs.Options{Workers: 1, PollInterval: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	if _, err := q.Enqueue(ctx, domain.Job{Kind: "kind.from.the.future", MaxAttempts: 5}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the unknown kind to fail", func() bool {
		counts, err := q.Counts(ctx)
		return err == nil && counts["failed"] == 1
	})
}
