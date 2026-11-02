package jobs_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/maintenance"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/jobs"
)

func TestDeploymentDrainPausesClaimsWithoutConsumingQueuedJobs(t *testing.T) {
	db := newStore(t)
	gate := maintenance.New()
	owner := strings.Repeat("ea", 32)
	status, err := gate.Start(owner)
	if err != nil {
		t.Fatal(err)
	}
	q := jobs.New(db, nil, nil, jobs.Options{Workers: 1, PollInterval: 5 * time.Millisecond, Admission: gate})
	started := make(chan struct{}, 1)
	finish := make(chan struct{})
	if err = q.Register("drain", func(ctx context.Context, _ domain.Job) error {
		started <- struct{}{}
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	q.Start(ctx)
	t.Cleanup(func() { cancel(); q.Wait() })
	if _, err = q.Enqueue(ctx, domain.Job{Kind: "drain"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
		t.Fatal("job claimed through closed admission")
	case <-time.After(40 * time.Millisecond):
	}
	if gate.Status().Active != 0 {
		t.Fatal("idle claim waiter prevents drain")
	}
	if _, err = gate.Abort(owner, status.Generation); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("job was lost or failed to resume")
	}
	if _, err = gate.Start(owner); err != nil {
		t.Fatal(err)
	}
	if gate.Status().Phase != "draining" {
		t.Fatal("running handler lost drain ownership")
	}
	close(finish)
	waitFor(t, "actual handler and bookkeeping completion", func() bool { return gate.Status().Phase == "drained" })
	counts, err := q.Counts(ctx)
	if err != nil || counts["done"] != 1 {
		t.Fatalf("%+v %v", counts, err)
	}
}
