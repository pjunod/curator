// Package jobs is the leased job queue's runtime: a pool of workers that
// claim, execute, heartbeat, and retire jobs (ADR 0008 step 1).
//
// It is deliberately built and proven on SQLite before any storage change,
// because the design risk lives in claiming work rather than in the store
// swap, and because the queue is worth having on a single instance: the
// timer scheduler cannot do durable retries, visible failures, or
// backpressure.
package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// Store is the persistence the queue needs. Narrow on purpose: the Postgres
// implementation of ADR 0008 step 2 has to satisfy exactly this.
type Store interface {
	EnqueueJob(ctx context.Context, j domain.Job) (int64, error)
	ClaimJob(ctx context.Context, owner string, capabilities []string, lease time.Duration) (domain.Job, error)
	CompleteJob(ctx context.Context, id int64) error
	RetryJob(ctx context.Context, id int64, cause string, runAfter time.Time) error
	FailJob(ctx context.Context, id int64, cause string) error
	HeartbeatJob(ctx context.Context, id int64, owner string, lease time.Duration) (bool, error)
	ReclaimExpiredLeases(ctx context.Context) (int64, error)
	CountJobsByState(ctx context.Context) (map[string]int64, error)
	PruneFinishedJobs(ctx context.Context, before time.Time) (int64, error)
}

// Handler executes one job. Returning an error retries the job until its
// attempts are exhausted; returning ErrPermanent fails it immediately.
type Handler func(ctx context.Context, j domain.Job) error

// ErrPermanent marks a failure that retrying cannot fix — a malformed
// payload, or a folder that no longer exists. Retrying those burns attempts
// and hides the real error behind "attempt 3 of 3".
var ErrPermanent = errors.New("permanent job failure")

// JobFinished is published when a job reaches a terminal state.
type JobFinished struct {
	ID       int64  `json:"id"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Attempts int64  `json:"attempts"`
	Error    string `json:"error,omitempty"`
}

// EventType implements bus.Event.
func (JobFinished) EventType() string { return "job.finished" }

// Options configure a Queue. Zero values are replaced with the defaults
// documented on each field.
type Options struct {
	// Workers is how many jobs run concurrently on this node. Default 2:
	// most Monarr work is I/O against a provider or a disk, and a homelab
	// NAS is not helped by more parallel walks.
	Workers int
	// Lease is how long a claim is held before it can be reclaimed.
	// Default 60s, renewed by heartbeat at a third of that.
	Lease time.Duration
	// PollInterval is how often an idle worker looks for work. Default 2s.
	// The queue is poll-based because that degrades identically on SQLite
	// and Postgres; LISTEN/NOTIFY is a step-3 optimisation.
	PollInterval time.Duration
	// Node names this instance in leases. Default: the hostname.
	Node string
	// Capabilities this node advertises — mount and toolchain tags.
	Capabilities []string
	// Retention is how long terminal jobs are kept for inspection.
	// Default 7 days.
	Retention time.Duration
	// PruneHook lets a queue-owned feature share this exact hourly retention
	// clock instead of inventing a second policy or scheduler.
	PruneHook func(context.Context, time.Time) error
}

// Queue claims and runs jobs.
type Queue struct {
	store    Store
	bus      *bus.Bus
	log      *slog.Logger
	opts     Options
	handlers map[string]Handler

	mu      sync.RWMutex
	started bool
	wg      sync.WaitGroup
}

// New returns a Queue. Register handlers before Start.
func New(store Store, b *bus.Bus, log *slog.Logger, opts Options) *Queue {
	if log == nil {
		log = slog.Default()
	}
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.Lease <= 0 {
		opts.Lease = 60 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.Retention <= 0 {
		opts.Retention = 7 * 24 * time.Hour
	}
	if opts.Node == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			opts.Node = h
		} else {
			opts.Node = "monarr"
		}
	}
	return &Queue{
		store: store, bus: b, log: log, opts: opts,
		handlers: map[string]Handler{},
	}
}

// Node reports the name this queue leases work as.
func (q *Queue) Node() string { return q.opts.Node }

// Register wires a handler for a job kind. Must be called before Start.
func (q *Queue) Register(kind string, h Handler) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.started {
		return fmt.Errorf("jobs: cannot register %q after Start", kind)
	}
	if kind == "" || h == nil {
		return fmt.Errorf("jobs: a handler needs a kind and a function")
	}
	if _, dup := q.handlers[kind]; dup {
		return fmt.Errorf("jobs: handler for %q already registered", kind)
	}
	q.handlers[kind] = h
	return nil
}

// Enqueue adds a job. A dedupe collision is reported as
// sqlite.ErrDuplicateJob and is normally not an error to the caller: it
// means the same work is already scheduled.
func (q *Queue) Enqueue(ctx context.Context, j domain.Job) (int64, error) {
	return q.store.EnqueueJob(ctx, j)
}

// EnqueueUnique adds a job unless an identical one is already pending,
// reporting whether it was actually added. This is the common case for
// periodic work — the scheduler enqueues, and the dedupe key means a slow
// run never stacks up behind itself.
func (q *Queue) EnqueueUnique(ctx context.Context, j domain.Job) (bool, error) {
	if j.DedupeKey == "" {
		j.DedupeKey = j.Kind
	}
	_, err := q.store.EnqueueJob(ctx, j)
	if errors.Is(err, sqlite.ErrDuplicateJob) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Counts reports how many jobs sit in each state.
func (q *Queue) Counts(ctx context.Context) (map[string]int64, error) {
	return q.store.CountJobsByState(ctx)
}

// Start launches the workers and the lease reaper. Cancel ctx to stop, then
// Wait for a drain.
func (q *Queue) Start(ctx context.Context) {
	q.mu.Lock()
	q.started = true
	q.mu.Unlock()

	for i := 0; i < q.opts.Workers; i++ {
		q.wg.Add(1)
		go func(n int) {
			defer q.wg.Done()
			q.worker(ctx, n)
		}(i)
	}
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		q.reaper(ctx)
	}()
	q.log.Info("jobs: queue started",
		"node", q.opts.Node, "workers", q.opts.Workers, "lease", q.opts.Lease)
}

// Wait blocks until every worker has stopped.
func (q *Queue) Wait() { q.wg.Wait() }

func (q *Queue) worker(ctx context.Context, n int) {
	// Stagger pollers so N workers don't wake in lockstep and contend for
	// the same write lock on an idle queue.
	stagger := time.Duration(n) * q.opts.PollInterval / time.Duration(max(q.opts.Workers, 1))
	select {
	case <-ctx.Done():
		return
	case <-time.After(stagger):
	}

	ticker := time.NewTicker(q.opts.PollInterval)
	defer ticker.Stop()
	for {
		// Drain everything available before going back to sleep, so a
		// burst of enqueued work is not paced at one job per tick.
		for {
			claimed, err := q.claimAndRun(ctx)
			if err != nil || !claimed {
				break
			}
			if ctx.Err() != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// claimAndRun leases one job and executes it, reporting whether anything was
// claimed.
func (q *Queue) claimAndRun(ctx context.Context) (bool, error) {
	job, err := q.store.ClaimJob(ctx, q.opts.Node, q.opts.Capabilities, q.opts.Lease)
	if errors.Is(err, sqlite.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		if ctx.Err() == nil {
			q.log.Warn("jobs: claim failed", "err", err)
		}
		return false, err
	}

	q.mu.RLock()
	h, ok := q.handlers[job.Kind]
	q.mu.RUnlock()
	if !ok {
		// An unknown kind is permanent by definition: no amount of
		// retrying will make this binary understand it. This is what a
		// rolling restart mid-upgrade looks like, so say so clearly.
		q.finish(ctx, job, fmt.Errorf("%w: no handler for kind %q", ErrPermanent, job.Kind))
		return true, nil
	}

	// Heartbeat while the job runs. If the lease is lost — a stall long
	// enough for the reaper to reclaim it — cancel the work rather than let
	// two nodes write the same result.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	beat := time.NewTicker(q.opts.Lease / 3)
	defer beat.Stop()
	go func() {
		for {
			select {
			case <-runCtx.Done():
				return
			case <-beat.C:
				held, err := q.store.HeartbeatJob(runCtx, job.ID, q.opts.Node, q.opts.Lease)
				if err == nil && !held {
					q.log.Warn("jobs: lease lost mid-run, abandoning",
						"id", job.ID, "kind", job.Kind)
					cancel()
					return
				}
			}
		}
	}()

	err = h(runCtx, job)
	q.finish(ctx, job, err)
	return true, nil
}

// finish retires a job: done, retried with backoff, or failed.
func (q *Queue) finish(ctx context.Context, job domain.Job, runErr error) {
	// Use a context that survives shutdown, or a cancelled run would also
	// lose the record of why it stopped.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	switch {
	case runErr == nil:
		if err := q.store.CompleteJob(recCtx, job.ID); err != nil {
			q.log.Error("jobs: could not mark done", "id", job.ID, "err", err)
			return
		}
		q.publish(JobFinished{ID: job.ID, Kind: job.Kind, State: string(domain.JobDone), Attempts: job.Attempts})

	case errors.Is(runErr, ErrPermanent) || job.Attempts >= job.MaxAttempts:
		if err := q.store.FailJob(recCtx, job.ID, runErr.Error()); err != nil {
			q.log.Error("jobs: could not mark failed", "id", job.ID, "err", err)
			return
		}
		q.log.Warn("jobs: failed", "id", job.ID, "kind", job.Kind,
			"attempts", job.Attempts, "err", runErr)
		q.publish(JobFinished{
			ID: job.ID, Kind: job.Kind, State: string(domain.JobFailed),
			Attempts: job.Attempts, Error: runErr.Error(),
		})

	default:
		next := time.Now().Add(backoff(job.Attempts))
		if err := q.store.RetryJob(recCtx, job.ID, runErr.Error(), next); err != nil {
			q.log.Error("jobs: could not schedule retry", "id", job.ID, "err", err)
			return
		}
		q.log.Info("jobs: retrying", "id", job.ID, "kind", job.Kind,
			"attempt", job.Attempts, "next", next, "err", runErr)
	}
}

// backoff grows exponentially from 5s and caps at 5 minutes. The cap matters
// more than the curve: a provider that is rate-limiting wants to be retried
// eventually, not abandoned, and not hammered.
func backoff(attempt int64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := 5 * time.Second * time.Duration(math.Pow(2, float64(attempt-1)))
	if d > 5*time.Minute {
		return 5 * time.Minute
	}
	return d
}

// reaper returns expired leases to the queue and prunes old terminal jobs.
func (q *Queue) reaper(ctx context.Context) {
	ticker := time.NewTicker(q.opts.Lease / 2)
	defer ticker.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := q.store.ReclaimExpiredLeases(ctx); err != nil {
				if ctx.Err() == nil {
					q.log.Warn("jobs: reclaim failed", "err", err)
				}
			} else if n > 0 {
				q.log.Info("jobs: reclaimed expired leases", "count", n)
			}
		case <-prune.C:
			before := time.Now().Add(-q.opts.Retention)
			if _, err := q.store.PruneFinishedJobs(ctx, before); err != nil && ctx.Err() == nil {
				q.log.Warn("jobs: prune failed", "err", err)
			}
			if q.opts.PruneHook != nil {
				if err := q.opts.PruneHook(ctx, before); err != nil && ctx.Err() == nil {
					q.log.Warn("jobs: feature prune failed", "err", err)
				}
			}
		}
	}
}

func (q *Queue) publish(e bus.Event) {
	if q.bus != nil {
		q.bus.Publish(e)
	}
}
