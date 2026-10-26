package jobs_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/jobs"
)

func TestInteractiveJobRunsWhileBackgroundWorkersAreOccupied(t *testing.T) {
	db := newStore(t)
	q := jobs.New(db, nil, nil, jobs.Options{Workers: 2, PollInterval: 5 * time.Millisecond})
	started := make(chan struct{}, 3)
	if err := q.Register("background", func(ctx context.Context, _ domain.Job) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	interactive := make(chan domain.Job, 1)
	if err := q.Register("scan", func(ctx context.Context, j domain.Job) error {
		// Automatic follow-up jobs must not accidentally inherit this priority.
		if domain.IsInteractiveJob(ctx) {
			t.Error("worker leaked interactive enqueue context")
		}
		interactive <- j
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); q.Wait() }()
	for range 3 {
		if _, err := q.Enqueue(ctx, domain.Job{Kind: "background"}); err != nil {
			t.Fatal(err)
		}
	}
	q.Start(ctx)
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("background workers did not fill")
		}
	}
	// The third background job must remain queued, leaving capacity for the click.
	id, err := q.Enqueue(domain.WithInteractiveJob(ctx), domain.Job{Kind: "scan", Priority: 50})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case j := <-interactive:
		if j.ID != id || j.Priority != domain.JobPriorityInteractive {
			t.Fatalf("wrong interactive job: %+v", j)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interactive job waited for blocked background work")
	}
	select {
	case <-started:
		t.Fatal("reserved worker consumed background work")
	default:
	}
}

type automaticEpochDeferral struct{ until time.Time }

func (e automaticEpochDeferral) Error() string            { return "automatic epoch deferred" }
func (e automaticEpochDeferral) DeferredUntil() time.Time { return e.until }

func TestNewManualGenerationCannotBeParkedByAnOlderClaim(t *testing.T) {
	db := newStore(t)
	q := jobs.New(db, nil, nil, jobs.Options{Workers: 1, PollInterval: 5 * time.Millisecond})
	reran := make(chan struct{}, 1)
	if err := q.Register("generation", func(ctx context.Context, j domain.Job) error {
		var p struct {
			Generation int `json:"manualGeneration"`
		}
		if err := json.Unmarshal([]byte(j.Payload), &p); err != nil {
			return err
		}
		if p.Generation == 1 {
			// The new request lands after this handler chose a long automatic delay.
			_, err := db.W.ExecContext(ctx, `UPDATE jobs SET payload=?,run_after=? WHERE id=?`, `{"manual":true,"manualGeneration":2}`, time.Now().UnixMilli(), j.ID)
			if err != nil {
				return err
			}
			return automaticEpochDeferral{time.Now().Add(24 * time.Hour)}
		}
		reran <- struct{}{}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); q.Wait() }()
	if _, err := q.Enqueue(domain.WithInteractiveJob(ctx), domain.Job{Kind: "generation", Payload: `{"manual":true,"manualGeneration":1}`}); err != nil {
		t.Fatal(err)
	}
	q.Start(ctx)
	select {
	case <-reran:
	case <-time.After(5 * time.Second):
		t.Fatal("old claim parked the newer manual request for a day")
	}
	waitFor(t, "new request to complete", func() bool { counts, err := q.Counts(ctx); return err == nil && counts["done"] == 1 })
}
