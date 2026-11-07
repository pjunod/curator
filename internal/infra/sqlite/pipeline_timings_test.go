package sqlite

import (
	"context"
	"testing"
)

func TestPipelineTimingsFirstStartWinsAndEndsOnce(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(db.StartPipelineTiming(ctx, TimingRunnerDoneToImport, "t-1", 1, 1000))
	must(db.StartPipelineTiming(ctx, TimingRunnerDoneToImport, "t-1", 1, 9000)) // ignored
	must(db.EndPipelineTiming(ctx, TimingRunnerDoneToImport, "t-1", 4000))
	must(db.EndPipelineTiming(ctx, TimingRunnerDoneToImport, "t-1", 8000)) // already closed
	ds, err := db.PipelineDurationsMs(ctx, TimingRunnerDoneToImport, 0)
	if err != nil || len(ds) != 1 || ds[0] != 3000 {
		t.Fatalf("durations = %v err %v, want [3000]", ds, err)
	}
	n, _, err := db.PipelineWaiting(ctx, TimingRunnerDoneToImport, 0)
	if err != nil || n != 0 {
		t.Fatalf("waiting = %d err %v, want 0", n, err)
	}
	if err := db.StartPipelineTiming(ctx, "bogus", "t-2", 2, 1); err == nil {
		t.Fatal("an unknown kind must be refused by the CHECK constraint")
	}
}
