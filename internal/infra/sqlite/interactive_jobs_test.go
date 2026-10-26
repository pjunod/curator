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
	if _, err := db.EnqueueJob(domain.WithInteractiveJob(ctx), domain.Job{Kind: "scan", DedupeKey: "scan", Priority: 50}); !errors.Is(err, ErrDuplicateJob) {
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

// Season acquisition writes directly through the store, without jobs.Queue.
func TestDirectStoreEnqueueHonorsInteractiveOrigin(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, priority := range []int64{0, 50, 110} {
		id, err := db.EnqueueJob(domain.WithInteractiveJob(ctx), domain.Job{Kind: "direct", Priority: priority})
		if err != nil {
			t.Fatal(err)
		}
		job, err := db.ClaimInteractiveJob(ctx, "worker", nil, time.Minute)
		if err != nil || job.ID != id || job.Priority != domain.JobPriorityInteractive {
			t.Fatalf("direct enqueue bypassed interactive priority: %+v %v", job, err)
		}
		if err := db.CompleteJob(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGuardedDeferralKeepsQuotaDeadlineWithoutANewerManualRequest(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, payload := range []string{`{}`, `{"manual":true,"manualGeneration":2}`, `{"manual":false,"manualGeneration":3}`} {
		id, err := db.EnqueueJob(ctx, domain.Job{Kind: "quota", Payload: payload, Priority: domain.JobPriorityInteractive})
		if err != nil {
			t.Fatal(err)
		}
		job, err := db.ClaimInteractiveJob(ctx, "worker", nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(2 * time.Hour).Truncate(time.Millisecond)
		if err := db.DeferClaimedJob(ctx, job, "quota exhausted", deadline); err != nil {
			t.Fatal(err)
		}
		saved, err := db.GetJob(ctx, id)
		if err != nil || !saved.RunAfter.Equal(deadline) || saved.State != domain.JobQueued || saved.Attempts != 0 || saved.LeaseOwner != "" {
			t.Fatalf("quota deferral changed deadline or lease: %+v %v", saved, err)
		}
	}
	if _, err := db.ClaimInteractiveJob(ctx, "reserved", nil, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("quota-bound job claimed early: %v", err)
	}
}

func TestGuardedDeferralCannotAlterAnotherLeaseOrCompletedJob(t *testing.T) {
	for _, state := range []string{"expired", "reclaimed", "completed"} {
		t.Run(state, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			id, err := db.EnqueueJob(ctx, domain.Job{Kind: "quota", Priority: domain.JobPriorityInteractive})
			if err != nil {
				t.Fatal(err)
			}
			job, err := db.ClaimInteractiveJob(ctx, "old-worker", nil, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if state == "completed" {
				if err := db.CompleteJob(ctx, id); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := db.W.ExecContext(ctx, `UPDATE jobs SET lease_expires_at=? WHERE id=?`, time.Now().Add(-time.Second).UnixMilli(), id); err != nil {
					t.Fatal(err)
				}
				if state == "reclaimed" {
					if _, err := db.ReclaimExpiredLeases(ctx); err != nil {
						t.Fatal(err)
					}
					if _, err := db.ClaimInteractiveJob(ctx, "new-worker", nil, time.Hour); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := db.GetJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.DeferClaimedJob(ctx, job, "stale deferral", time.Now().Add(time.Hour)); err == nil {
				t.Fatal("stale claim changed durable scheduling")
			}
			after, err := db.GetJob(ctx, id)
			if err != nil || after.State != before.State || after.LeaseOwner != before.LeaseOwner || after.Attempts != before.Attempts || !after.RunAfter.Equal(before.RunAfter) {
				t.Fatalf("stale worker altered job: before=%+v after=%+v err=%v", before, after, err)
			}
		})
	}
}
