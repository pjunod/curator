package sqlite

import (
	"context"
	"testing"
	"time"
)

// A timing ended inside a publication's completing transaction keeps
// EndPipelineTiming's semantics: an end before the start (a backwards clock
// step) closes the sample at duration 0 instead of leaving it open forever.
func TestCompletePublicationImportClampsTimingEnd(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour).UnixMilli()
	if err := db.StartPipelineTiming(ctx, TimingRunnerDoneToImport, "tok", 0, future); err != nil {
		t.Fatal(err)
	}
	if err := db.CompletePublicationImport(ctx, PublicationImportWrite{State: "imported",
		TimingEnds: []TimingStart{{Kind: TimingRunnerDoneToImport, Transfer: "tok"}}}); err != nil {
		t.Fatal(err)
	}
	var started, ended int64
	if err := db.R.QueryRowContext(ctx, `SELECT started_at, ended_at FROM pipeline_timings WHERE kind=? AND transfer='tok'`, TimingRunnerDoneToImport).Scan(&started, &ended); err != nil {
		t.Fatal(err)
	}
	if ended != started {
		t.Fatalf("ended_at = %d, want clamped to started_at %d", ended, started)
	}
}
