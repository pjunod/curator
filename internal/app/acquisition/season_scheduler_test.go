package acquisition

import (
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestFeasibleRollingRequests(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	b := sqlite.RequestBudget(250)
	finish := feasibleFinish(now, b, nil, nil, 63, time.Second)
	if finish.Before(now.Add(12*time.Hour)) || finish.After(now.Add(23*time.Hour)) {
		t.Fatalf("63 requests finish %v", finish)
	}
	if !feasibleFinish(now, b, nil, nil, 126, time.Second).After(now.Add(23 * time.Hour)) {
		t.Fatal("oversized two-page season admitted as complete")
	}
	// Four season comparisons opened sequentially retain fresh clocks. Three
	// indexers have the same release timeline, so per-indexer accounting suffices.
	day, half := []time.Time{}, []time.Time{}
	at := now
	for season := 0; season < 4; season++ {
		started := at
		done := feasibleFinish(at, b, day, half, 33, time.Second)
		if done.After(started.Add(23 * time.Hour)) {
			t.Fatal("scheduler manufactured stale comparison")
		}
		for n := 0; n < 33; n++ {
			sent := feasibleFinish(at, b, day, half, 1, 0)
			day = append(day, sent.Add(24*time.Hour))
			half = append(half, sent.Add(12*time.Hour))
			at = sent.Add(time.Second)
		}
	}
	if at.After(now.Add(25 * time.Hour)) {
		t.Fatalf("four seasons finish too late: %v", at)
	}
}
