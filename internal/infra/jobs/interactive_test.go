package jobs_test

import (
	"context"
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
