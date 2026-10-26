package library

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/jobs"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestMetadataSweepDoesNotUseReservedWorkerButManualRefreshDoes(t *testing.T) {
	svc, db, b := newService(t)
	ctx := context.Background()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Fight Club", IDs: domain.ExternalIDs{TMDB: 550}, Source: "tmdb"})
	if err != nil {
		t.Fatal(err)
	}
	svc.WithQueue(jobs.New(db, b, nil, jobs.Options{}))
	if err := svc.RefreshAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimInteractiveJob(ctx, "worker", nil, time.Minute); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("automatic refresh used reserved capacity: %v", err)
	}
	background, err := db.ClaimJob(ctx, "worker", nil, time.Minute)
	if err != nil || background.Kind != JobIdentityRefresh || background.Priority <= domain.JobPriorityInteractive {
		t.Fatalf("missing background identity refresh: %+v %v", background, err)
	}
	if err := db.CompleteJob(ctx, background.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RefreshItem(domain.WithInteractiveJob(ctx), itemID); err != nil {
		t.Fatal(err)
	}
	interactive, err := db.ClaimInteractiveJob(ctx, "worker", nil, time.Minute)
	if err != nil || interactive.Kind != JobIdentityRefresh {
		t.Fatalf("manual refresh did not use reserved capacity: %+v %v", interactive, err)
	}
}
