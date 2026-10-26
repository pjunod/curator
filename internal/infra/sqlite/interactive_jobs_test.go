package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func TestDuplicateRequestPromotesQueuedJobWithoutResettingItsDeadline(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	deadline := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	id, err := db.EnqueueJob(ctx, domain.Job{Kind: "scan", DedupeKey: "scan", Priority: 50, RunAfter: deadline, Payload: `{"original":true}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "scan", DedupeKey: "scan", Priority: domain.JobPriorityInteractive}); !errors.Is(err, ErrDuplicateJob) {
		t.Fatal(err)
	}
	j, err := db.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Priority != domain.JobPriorityInteractive || !j.RunAfter.Equal(deadline) || j.Payload != `{"original":true}` || j.Attempts != 0 {
		t.Fatalf("promotion changed job identity or deadline: %+v", j)
	}
	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "scan", DedupeKey: "scan", Priority: 50}); !errors.Is(err, ErrDuplicateJob) {
		t.Fatal(err)
	}
	j, err = db.GetJob(ctx, id)
	if err != nil || j.Priority != domain.JobPriorityInteractive {
		t.Fatalf("scheduled request demoted job: %+v %v", j, err)
	}
	if _, err := db.ClaimInteractiveJob(ctx, "worker", nil, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deadline bypassed: %v", err)
	}
}

func TestInteractiveClaimsRespectRoutingAndLeaveBackgroundWorkQueued(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, j := range []domain.Job{
		{Kind: "background", Priority: 50},
		{Kind: "future", Priority: domain.JobPriorityInteractive, RunAfter: time.Now().Add(time.Hour)},
		{Kind: "capability", Priority: domain.JobPriorityInteractive, RequiredCapability: "disk"},
		{Kind: "affinity", Priority: domain.JobPriorityInteractive, AffinityNode: "other"},
	} {
		if _, err := db.EnqueueJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ClaimInteractiveJob(ctx, "worker", nil, time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reserved claim ignored eligibility: %v", err)
	}
	j, err := db.ClaimInteractiveJob(ctx, "worker", []string{"disk"}, time.Minute)
	if err != nil || j.Kind != "capability" {
		t.Fatalf("capability claim: %+v %v", j, err)
	}
	j, err = db.ClaimInteractiveJob(ctx, "other", nil, time.Minute)
	if err != nil || j.Kind != "affinity" {
		t.Fatalf("affinity claim: %+v %v", j, err)
	}
	j, err = db.ClaimJob(ctx, "worker", nil, time.Minute)
	if err != nil || j.Kind != "background" {
		t.Fatalf("background work was consumed by reserved worker: %+v %v", j, err)
	}
}

func TestDuplicateRequestDoesNotChangeRunningJob(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	id, err := db.EnqueueJob(ctx, domain.Job{Kind: "scan", DedupeKey: "scan", Priority: 50})
	if err != nil {
		t.Fatal(err)
	}
	leased, err := db.ClaimJob(ctx, "worker", nil, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.EnqueueJob(ctx, domain.Job{Kind: "scan", DedupeKey: "scan", Priority: domain.JobPriorityInteractive}); !errors.Is(err, ErrDuplicateJob) {
		t.Fatal(err)
	}
	got, err := db.GetJob(ctx, id)
	if err != nil || got.Priority != 50 || got.State != domain.JobLeased || got.Attempts != leased.Attempts || !got.LeaseExpiresAt.Equal(leased.LeaseExpiresAt) {
		t.Fatalf("running job changed: %+v %v", got, err)
	}
}
