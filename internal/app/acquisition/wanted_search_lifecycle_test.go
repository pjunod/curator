package acquisition

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

type wantedDBQueue struct{ db *sqlite.DB }

func (q wantedDBQueue) Enqueue(ctx context.Context, job domain.Job) (int64, error) {
	return q.db.EnqueueJob(ctx, job)
}

func TestWantedSearchEmptyScopeCompletesAndNoIndexersStayAnExplicitError(t *testing.T) {
	t.Run("empty reason", func(t *testing.T) {
		svc, db, _ := autoSetup(t, nil, &fakeClient{})
		svc.WithWantedSearchQueue(wantedDBQueue{db: db})
		run, err := svc.StartWantedSearch(context.Background(), WantedSearchRequest{
			Scope: "reason", Reason: WantedUpgrade,
		})
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "completed" || run.Selected != 0 || run.Processed != 0 {
			t.Fatalf("empty run = %+v", run)
		}
	})

	t.Run("no indexers", func(t *testing.T) {
		svc, db, _ := autoSetup(t, nil, &fakeClient{})
		indexers, _ := db.ListIndexers(context.Background())
		for _, indexer := range indexers {
			if err := db.DeleteIndexer(context.Background(), indexer.ID); err != nil {
				t.Fatal(err)
			}
		}
		svc.WithWantedSearchQueue(wantedDBQueue{db: db})
		if _, err := svc.StartWantedSearch(context.Background(), WantedSearchRequest{Scope: "all"}); !errors.Is(err, ErrNoIndexers) {
			t.Fatalf("StartWantedSearch = %v, want ErrNoIndexers", err)
		}
	})
}

func TestWantedSearchRunDrainsEveryTargetBeyondBacklogCapOneJobAtATime(t *testing.T) {
	svc, db, _ := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	for i := 0; i < backlogPerRun+5; i++ {
		if _, err := db.CreateMediaItem(ctx, domain.MediaItem{
			Kind: domain.KindMovie, Title: fmt.Sprintf("Wanted %02d", i),
			SortTitle: fmt.Sprintf("wanted %02d", i), Year: 2000 + i,
			IDs: domain.ExternalIDs{TMDB: int64(8000 + i)}, Monitored: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	svc.WithWantedSearchQueue(wantedDBQueue{db: db})
	delay := 0
	run, err := svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "all", TargetDelay: &delay})
	if err != nil {
		t.Fatal(err)
	}
	if run.Selected <= backlogPerRun {
		t.Fatalf("selected = %d, fixture must exceed backlog cap %d", run.Selected, backlogPerRun)
	}

	jobsRun := 0
	for {
		run, err = svc.WantedSearch(ctx, run.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == "completed" {
			break
		}
		job, err := db.ClaimJob(ctx, "test", nil, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.handleWantedSearchChunk(ctx, job); err != nil {
			t.Fatal(err)
		}
		if err := db.CompleteJob(ctx, job.ID); err != nil {
			t.Fatal(err)
		}
		jobsRun++
		if err := svc.ReconcileWantedSearches(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if jobsRun != run.Selected || run.Processed != run.Selected || run.Searched != run.Selected {
		t.Fatalf("run = %+v jobs = %d; want exactly one job per target", run, jobsRun)
	}
}

func TestWantedSearchQueuedCancellationCompletesWithoutStarting(t *testing.T) {
	svc, db, _ := autoSetup(t, nil, &fakeClient{})
	svc.WithWantedSearchQueue(wantedDBQueue{db: db})
	ctx := context.Background()
	run, err := svc.StartWantedSearch(ctx, WantedSearchRequest{Scope: "all"})
	if err != nil {
		t.Fatal(err)
	}
	run, err = svc.CancelWantedSearch(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "cancelled" || !run.StartedAt.IsZero() || run.Processed != run.Selected || run.Skipped != run.Selected {
		t.Fatalf("cancelled run = %+v", run)
	}
}
