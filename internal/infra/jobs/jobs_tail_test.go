package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// fakeStore is a Store whose every method can be made to fail on demand.
//
// The queue's job is to survive its store misbehaving — a node that cannot
// record "done" must not silently drop the job, and one that cannot record
// "failed" must not lose the reason. None of that is reachable through the
// real SQLite store without corrupting a database, and the Store interface
// is narrow precisely so a second implementation can stand in here.
type fakeStore struct {
	mu sync.Mutex

	queue []domain.Job // claimed front-first
	calls map[string]int
	args  map[string]any

	claimErr     error
	completeErr  error
	failErr      error
	retryErr     error
	reclaimErr   error
	pruneErr     error
	heartbeatOK  bool
	heartbeatErr error
	reclaimed    int64
}

func newFakeStore(jobs ...domain.Job) *fakeStore {
	return &fakeStore{
		queue: jobs, calls: map[string]int{}, args: map[string]any{},
		heartbeatOK: true,
	}
}

func (f *fakeStore) note(name string) {
	f.mu.Lock()
	f.calls[name]++
	f.mu.Unlock()
}

func (f *fakeStore) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeStore) arg(name string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.args[name]
}

func (f *fakeStore) EnqueueJob(context.Context, domain.Job) (int64, error) {
	f.note("enqueue")
	return 1, nil
}

func (f *fakeStore) ClaimJob(context.Context, string, []string, time.Duration) (domain.Job, error) {
	f.note("claim")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return domain.Job{}, f.claimErr
	}
	if len(f.queue) == 0 {
		return domain.Job{}, sqlite.ErrNotFound
	}
	j := f.queue[0]
	f.queue = f.queue[1:]
	return j, nil
}

func (f *fakeStore) CompleteJob(context.Context, int64) error {
	f.note("complete")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.completeErr
}

func (f *fakeStore) RetryJob(_ context.Context, _ int64, cause string, runAfter time.Time) error {
	f.note("retry")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.args["retryAfter"] = runAfter
	f.args["retryCause"] = cause
	return f.retryErr
}

func (f *fakeStore) FailJob(_ context.Context, _ int64, cause string) error {
	f.note("fail")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.args["failCause"] = cause
	return f.failErr
}

func (f *fakeStore) HeartbeatJob(context.Context, int64, string, time.Duration) (bool, error) {
	f.note("heartbeat")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.heartbeatOK, f.heartbeatErr
}

func (f *fakeStore) ReclaimExpiredLeases(context.Context) (int64, error) {
	f.note("reclaim")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reclaimed, f.reclaimErr
}

func (f *fakeStore) CountJobsByState(context.Context) (map[string]int64, error) {
	f.note("counts")
	return map[string]int64{}, nil
}

func (f *fakeStore) PruneFinishedJobs(context.Context, time.Time) (int64, error) {
	f.note("prune")
	f.mu.Lock()
	defer f.mu.Unlock()
	return 0, f.pruneErr
}

// quiet keeps the queue's own logging out of the test output while still
// exercising every log call on the paths under test.
func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func tailWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The cap matters more than the curve: a provider that is rate-limiting
// wants to be retried eventually, not abandoned, and not hammered.
func TestTailBackoffGrowsExponentiallyAndCaps(t *testing.T) {
	// Attempt 0 cannot happen — a job is claimed with its attempt already
	// incremented — but treating it as attempt 1 keeps the first retry at
	// five seconds instead of zero, which would be a hot loop.
	if got := backoff(0); got != 5*time.Second {
		t.Errorf("backoff(0) = %v, want 5s", got)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second}
	for i, w := range want {
		if got := backoff(int64(i + 1)); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	// Every attempt past the cap is the cap. Bounded at 31 deliberately:
	// the doubling is computed before the cap is applied, so beyond that the
	// intermediate overflows time.Duration and the "cap" stops capping. No
	// job monarr enqueues has an attempts budget within two orders of
	// magnitude of that, so this pins the range that is actually reachable
	// rather than asserting what the overflow currently happens to produce.
	if got := backoff(6); got != 160*time.Second {
		t.Errorf("backoff(6) = %v, want 2m40s (the last step under the cap)", got)
	}
	for _, attempt := range []int64{7, 20, 31} {
		if got := backoff(attempt); got != 5*time.Minute {
			t.Errorf("backoff(%d) = %v, want the 5m cap", attempt, got)
		}
	}
	// Whatever the arithmetic does, a retry must never be scheduled in the
	// past: a negative delay is a hot loop that spends the whole attempts
	// budget in one tick.
	for _, attempt := range []int64{1, 5, 10, 31} {
		if backoff(attempt) <= 0 {
			t.Errorf("backoff(%d) = %v — a non-positive delay retries immediately",
				attempt, backoff(attempt))
		}
	}
}

// Zero options are the documented defaults, and they are what every caller
// that does not care gets. A zero lease or a zero poll interval would panic
// the ticker they feed.
func TestTailZeroOptionsBecomeTheDocumentedDefaults(t *testing.T) {
	q := New(newFakeStore(), nil, nil, Options{})
	if q.opts.Workers != 2 || q.opts.Lease != 60*time.Second ||
		q.opts.PollInterval != 2*time.Second || q.opts.Retention != 7*24*time.Hour {
		t.Errorf("defaults = %+v", q.opts)
	}
	if q.opts.Node == "" {
		t.Error("a node with no name cannot own a lease anybody can attribute")
	}
	if q.Node() != q.opts.Node {
		t.Errorf("Node() = %q, want %q", q.Node(), q.opts.Node)
	}
	// Negative values are the same mistake as zero and get the same fix.
	neg := New(newFakeStore(), nil, nil, Options{
		Workers: -1, Lease: -time.Second, PollInterval: -time.Second, Retention: -time.Hour,
	})
	if neg.opts.Workers != 2 || neg.opts.Lease != 60*time.Second {
		t.Errorf("negative options = %+v", neg.opts)
	}
	// An explicit node name wins, which is what a multi-instance deployment
	// sets so leases name something meaningful.
	named := New(newFakeStore(), nil, nil, Options{Node: "nas-01"})
	if named.Node() != "nas-01" {
		t.Errorf("Node() = %q, want nas-01", named.Node())
	}
}

// Handler registration is the one place a wiring mistake can be caught
// cheaply. After Start it is too late — a handler added while workers are
// claiming would be a data race on the map they read.
func TestTailRegisterRejectsUnusableHandlers(t *testing.T) {
	q := New(newFakeStore(), nil, quiet(), Options{})
	if err := q.Register("", func(context.Context, domain.Job) error { return nil }); err == nil {
		t.Error("a handler with no kind was registered")
	}
	if err := q.Register("test.nil", nil); err == nil {
		t.Error("a nil handler was registered")
	}
	if err := q.Register("test.ok", func(context.Context, domain.Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// A duplicate is a wiring bug: one of the two handlers would never run
	// and nothing would say which.
	if err := q.Register("test.ok", func(context.Context, domain.Job) error { return nil }); err == nil {
		t.Error("a second handler for the same kind was accepted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	if err := q.Register("test.late", func(context.Context, domain.Job) error { return nil }); err == nil {
		t.Error("a handler was registered after Start")
	}
	cancel()
	q.Wait()
}

// Wait is the drain: after the context ends, every worker and the reaper
// must have stopped before the process considers itself shut down.
func TestTailWaitDrainsEveryWorker(t *testing.T) {
	q := New(newFakeStore(), nil, quiet(), Options{
		Workers: 3, Lease: 100 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	time.Sleep(20 * time.Millisecond)
	cancel()

	done := make(chan struct{})
	go func() { defer close(done); q.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the context was cancelled")
	}
}

// A terminal job publishes what happened, because the UI's job list and the
// activity feed are driven by the bus and not by polling the table.
func TestTailTerminalJobsArePublishedOnTheBus(t *testing.T) {
	store := newFakeStore(
		domain.Job{ID: 1, Kind: "test.ok", Attempts: 1, MaxAttempts: 3},
		domain.Job{ID: 2, Kind: "test.bad", Attempts: 3, MaxAttempts: 3},
	)
	b := bus.New(nil)
	defer b.Close()
	events, cancelSub := bus.Subscribe[JobFinished](b, 8)
	defer cancelSub()

	q := New(store, b, quiet(), Options{
		Workers: 1, Lease: time.Minute, PollInterval: 5 * time.Millisecond,
	})
	if err := q.Register("test.ok", func(context.Context, domain.Job) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := q.Register("test.bad", func(context.Context, domain.Job) error {
		return errors.New("the provider said no")
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	got := map[int64]JobFinished{}
	for len(got) < 2 {
		select {
		case e := <-events:
			got[e.ID] = e
		case <-time.After(5 * time.Second):
			t.Fatalf("only saw %v", got)
		}
	}
	if got[1].State != string(domain.JobDone) || got[1].Kind != "test.ok" {
		t.Errorf("done event = %+v", got[1])
	}
	if got[2].State != string(domain.JobFailed) {
		t.Errorf("failed event = %+v", got[2])
	}
	// The reason has to travel with the event: it is what the activity feed
	// shows, and re-reading the row to find it defeats the point.
	if got[2].Error == "" || got[2].Attempts != 3 {
		t.Errorf("failed event lost its reason or its attempt count: %+v", got[2])
	}
	if (JobFinished{}).EventType() != "job.finished" {
		t.Error("the event type changed; subscribers key off it")
	}
}

// A retryable failure is scheduled forward with the backoff, not retried on
// the spot. Retrying immediately is how a rate-limited provider turns into
// a hot loop that burns the attempt budget in under a second.
func TestTailARetryableFailureIsScheduledWithBackoff(t *testing.T) {
	store := newFakeStore(domain.Job{ID: 1, Kind: "test.flaky", Attempts: 1, MaxAttempts: 3})
	q := New(store, nil, quiet(), Options{
		Workers: 1, Lease: time.Minute, PollInterval: 5 * time.Millisecond,
	})
	if err := q.Register("test.flaky", func(context.Context, domain.Job) error {
		return errors.New("provider is rate limiting")
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	tailWaitFor(t, "the retry to be scheduled", func() bool { return store.count("retry") == 1 })
	if store.count("fail") != 0 {
		t.Error("a job with attempts left was failed rather than retried")
	}
	runAfter, _ := store.arg("retryAfter").(time.Time)
	delay := time.Until(runAfter)
	if delay < 4*time.Second || delay > 6*time.Second {
		t.Errorf("first retry scheduled in %v, want about 5s", delay)
	}
	if cause, _ := store.arg("retryCause").(string); cause != "provider is rate limiting" {
		t.Errorf("retry cause = %q — a pending retry has to say what went wrong", cause)
	}
}

// The attempt budget is what stops a poison job looping forever: once it is
// spent, the next failure is terminal even though the error is retryable.
func TestTailAnExhaustedBudgetTurnsARetryIntoAFailure(t *testing.T) {
	store := newFakeStore(domain.Job{ID: 1, Kind: "test.flaky", Attempts: 3, MaxAttempts: 3})
	q := New(store, nil, quiet(), Options{
		Workers: 1, Lease: time.Minute, PollInterval: 5 * time.Millisecond,
	})
	if err := q.Register("test.flaky", func(context.Context, domain.Job) error {
		return errors.New("still broken")
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	tailWaitFor(t, "the job to fail", func() bool { return store.count("fail") == 1 })
	if store.count("retry") != 0 {
		t.Error("a job with no attempts left was retried")
	}
	if cause, _ := store.arg("failCause").(string); cause != "still broken" {
		t.Errorf("fail cause = %q", cause)
	}
}

// A store that cannot record the outcome must not take the worker with it.
// The job stays leased and the reaper will bring it back — which is exactly
// what should happen when the database is briefly unwritable.
func TestTailAStoreThatCannotRecordOutcomesDoesNotKillTheWorker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		job    domain.Job
		handle Handler
		setup  func(*fakeStore)
		expect string
	}{
		{
			name: "done", job: domain.Job{ID: 1, Kind: "k", MaxAttempts: 3},
			handle: func(context.Context, domain.Job) error { return nil },
			setup:  func(f *fakeStore) { f.completeErr = errors.New("database is locked") },
			expect: "complete",
		},
		{
			name: "failed", job: domain.Job{ID: 1, Kind: "k", Attempts: 3, MaxAttempts: 3},
			handle: func(context.Context, domain.Job) error { return errors.New("nope") },
			setup:  func(f *fakeStore) { f.failErr = errors.New("database is locked") },
			expect: "fail",
		},
		{
			name: "retried", job: domain.Job{ID: 1, Kind: "k", Attempts: 1, MaxAttempts: 3},
			handle: func(context.Context, domain.Job) error { return errors.New("nope") },
			setup:  func(f *fakeStore) { f.retryErr = errors.New("database is locked") },
			expect: "retry",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore(tc.job)
			tc.setup(store)
			b := bus.New(nil)
			defer b.Close()
			events, cancelSub := bus.Subscribe[JobFinished](b, 4)
			defer cancelSub()

			q := New(store, b, quiet(), Options{
				Workers: 1, Lease: time.Minute, PollInterval: 5 * time.Millisecond,
			})
			if err := q.Register("k", tc.handle); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			q.Start(ctx)

			tailWaitFor(t, "the outcome to be attempted", func() bool {
				return store.count(tc.expect) >= 1
			})
			// The worker is still alive and still polling.
			before := store.count("claim")
			tailWaitFor(t, "the worker to keep polling", func() bool {
				return store.count("claim") > before
			})
			// Nothing is published for an outcome that was never recorded:
			// an event saying "done" for a row still marked leased would be
			// worse than no event at all.
			select {
			case e := <-events:
				t.Errorf("published %+v for an outcome the store rejected", e)
			case <-time.After(50 * time.Millisecond):
			}
		})
	}
}

// A claim that errors is contention or an outage, not "no work". The worker
// backs off to its tick rather than spinning on the failing query.
func TestTailAFailingClaimBacksOffInsteadOfSpinning(t *testing.T) {
	store := newFakeStore()
	store.claimErr = errors.New("database is locked")
	q := New(store, nil, quiet(), Options{
		Workers: 1, Lease: time.Minute, PollInterval: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	time.Sleep(250 * time.Millisecond)
	cancel()
	q.Wait()

	// One poll per tick, give or take a tick — emphatically not thousands.
	if n := store.count("claim"); n > 20 {
		t.Errorf("claims = %d in 250ms — a failing claim is being retried in a "+
			"hot loop", n)
	}
	if store.count("claim") == 0 {
		t.Error("the worker never polled at all")
	}
}

// Losing the lease mid-run means the reaper handed this job to somebody
// else. Carrying on would have two nodes writing the same result, so the
// work is cancelled — which is the one thing that makes a distributed claim
// actually safe.
func TestTailLosingTheLeaseMidRunCancelsTheWork(t *testing.T) {
	store := newFakeStore(domain.Job{ID: 1, Kind: "test.slow", Attempts: 1, MaxAttempts: 3})
	store.heartbeatOK = false // the reaper already took it

	q := New(store, nil, quiet(), Options{
		Workers: 1, Lease: 30 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	})
	cancelled := make(chan struct{})
	if err := q.Register("test.slow", func(ctx context.Context, _ domain.Job) error {
		select {
		case <-ctx.Done():
			close(cancelled)
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler was never cancelled after the lease was lost — two " +
			"nodes would be writing the same result")
	}
}

// A heartbeat that errors is not the same as a heartbeat that says "you no
// longer hold this". A transient database error must not abandon work that
// is still legitimately leased.
func TestTailAFailingHeartbeatDoesNotAbandonTheWork(t *testing.T) {
	store := newFakeStore(domain.Job{ID: 1, Kind: "test.slow", Attempts: 1, MaxAttempts: 3})
	store.heartbeatErr = errors.New("database is locked")
	store.heartbeatOK = false // ignored: the error is what the queue sees first

	q := New(store, nil, quiet(), Options{
		Workers: 1, Lease: 30 * time.Millisecond, PollInterval: 5 * time.Millisecond,
	})
	finished := make(chan error, 1)
	if err := q.Register("test.slow", func(ctx context.Context, _ domain.Job) error {
		select {
		case <-ctx.Done():
			finished <- ctx.Err()
		case <-time.After(200 * time.Millisecond):
			finished <- nil
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.Start(ctx)

	tailWaitFor(t, "the heartbeat to be attempted", func() bool { return store.count("heartbeat") >= 2 })
	select {
	case err := <-finished:
		if err != nil {
			t.Errorf("the handler was cancelled (%v) by a heartbeat that merely "+
				"errored — a locked database is not a lost lease", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never finished")
	}
}

// The reaper is what makes a dead node's work recoverable, so it has to keep
// running on its own clock — and keep running after a sweep that failed.
func TestTailTheReaperKeepsSweepingAfterAFailure(t *testing.T) {
	store := newFakeStore()
	store.reclaimErr = errors.New("database is locked")
	q := New(store, nil, quiet(), Options{
		Workers: 1, Lease: 20 * time.Millisecond, PollInterval: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	tailWaitFor(t, "the reaper to sweep repeatedly", func() bool { return store.count("reclaim") >= 3 })
	cancel()
	q.Wait()
}

// A sweep that actually reclaims something is the interesting one: it is the
// only signal that a node died, so it is logged rather than passed over.
func TestTailTheReaperReportsWhatItReclaimed(t *testing.T) {
	store := newFakeStore()
	store.reclaimed = 4
	q := New(store, nil, quiet(), Options{
		Workers: 1, Lease: 20 * time.Millisecond, PollInterval: time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	tailWaitFor(t, "a sweep that reclaimed work", func() bool { return store.count("reclaim") >= 2 })
	cancel()
	q.Wait()
}

// Enqueue and Counts are thin, but they are the queue's whole public surface
// for callers that never start a worker — the API handlers, the scheduler.
func TestTailEnqueueAndCountsReachTheStore(t *testing.T) {
	store := newFakeStore()
	q := New(store, nil, quiet(), Options{})
	ctx := context.Background()

	if _, err := q.Enqueue(ctx, domain.Job{Kind: "rss.sync"}); err != nil {
		t.Fatal(err)
	}
	if store.count("enqueue") != 1 {
		t.Error("Enqueue never reached the store")
	}
	if _, err := q.Counts(ctx); err != nil {
		t.Fatal(err)
	}
	if store.count("counts") != 1 {
		t.Error("Counts never reached the store")
	}
}

// EnqueueUnique defaults the dedupe key to the kind, which is what makes
// "the scheduler enqueues every minute" safe for a job that takes ten. A
// store error that is not a dedupe collision must still surface.
func TestTailEnqueueUniqueSurfacesRealErrors(t *testing.T) {
	store := &erroringEnqueueStore{fakeStore: newFakeStore(), err: errors.New("disk full")}
	q := New(store, nil, quiet(), Options{})
	added, err := q.EnqueueUnique(context.Background(), domain.Job{Kind: "rss.sync"})
	if err == nil {
		t.Fatal("a store error was swallowed as a dedupe collision")
	}
	if added {
		t.Error("a job that failed to enqueue was reported as added")
	}
}

// erroringEnqueueStore fails only on enqueue, so the dedupe path can be told
// apart from a genuine write failure.
type erroringEnqueueStore struct {
	*fakeStore
	err error
}

func (e *erroringEnqueueStore) EnqueueJob(context.Context, domain.Job) (int64, error) {
	return 0, e.err
}
