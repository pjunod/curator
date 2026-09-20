package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWantedSearchStorageOwnsOneActiveRunAndCheckpointsOneOrdinal(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	run := WantedSearchRun{RunID: "run-1", Scope: "all", ScopeLabel: "All wanted items", CreatedAt: now, TargetDelay: time.Second}
	targets := []WantedSearchTarget{
		{WantableID: "movie:1", SelectedReason: "missing", Label: "One"},
		{WantableID: "movie:2", SelectedReason: "upgrade", Label: "Two"},
	}
	if err := db.CreateWantedSearch(ctx, run, targets); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateWantedSearch(ctx, WantedSearchRun{RunID: "run-2", Scope: "all", ScopeLabel: "All", CreatedAt: now}, nil); !errors.Is(err, ErrWantedSearchActive) {
		t.Fatalf("second active run = %v, want ErrWantedSearchActive", err)
	}

	committed, err := db.CommitWantedSearchTarget(ctx, "run-1", 0, WantedSearchTarget{
		State: "searched", Seen: 4, Matched: 2, Accepted: 1, Grabbed: "Release.One",
	}, now.Add(time.Second))
	if err != nil || !committed {
		t.Fatalf("commit = %v, %v", committed, err)
	}
	if replayed, err := db.CommitWantedSearchTarget(ctx, "run-1", 0, WantedSearchTarget{State: "failed"}, now); err != nil || replayed {
		t.Fatalf("stale ordinal replay = %v, %v", replayed, err)
	}
	got, err := db.GetWantedSearch(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Cursor != 1 || got.Processed != 1 || got.Searched != 1 || got.Grabbed != 1 {
		t.Fatalf("checkpoint counters = %+v", got)
	}
}

func TestWantedSearchCancellationTurnsEveryPendingTargetIntoASkip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.CreateWantedSearch(ctx, WantedSearchRun{
		RunID: "cancel-me", Scope: "reason", Reason: "missing",
		ScopeLabel: "All missing items", CreatedAt: now,
	}, []WantedSearchTarget{
		{WantableID: "movie:1", SelectedReason: "missing", Label: "One"},
		{WantableID: "movie:2", SelectedReason: "missing", Label: "Two"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RequestWantedSearchCancel(ctx, "cancel-me"); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishWantedSearch(ctx, "cancel-me", "cancelled", "", true); err != nil {
		t.Fatal(err)
	}
	run, _ := db.GetWantedSearch(ctx, "cancel-me")
	if run.Status != "cancelled" || run.Processed != 2 || run.Skipped != 2 || run.Cursor != 2 {
		t.Fatalf("cancelled run = %+v", run)
	}
	results, err := db.ListWantedSearchResults(ctx, "cancel-me", 100, 0)
	if err != nil || len(results) != 2 {
		t.Fatalf("results = %+v, %v", results, err)
	}
	for _, result := range results {
		if result.State != "skipped" || result.Skipped != "cancelled" {
			t.Errorf("cancel result = %+v", result)
		}
	}
}
