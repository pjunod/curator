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

func TestPipelineWaitingExcludesOnlySettledFailedImportsWithoutInventingSuccess(t *testing.T) {
	f := newAcquisitionStoreFixture(t)
	db := f.db
	ctx := context.Background()
	dl := f.stopped
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET payload_removed=1,runner_salvage='none',transfer='terminal-import' WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.StartPipelineTiming(ctx, TimingRunnerDoneToImport, "terminal-import", dl.ID, 1000); err != nil {
		t.Fatal(err)
	}
	assertWaiting := func(want int) {
		t.Helper()
		n, _, err := db.PipelineWaiting(ctx, TimingRunnerDoneToImport, 0)
		if err != nil || n != want {
			t.Fatalf("waiting=%d want=%d err=%v", n, want, err)
		}
	}
	assertWaiting(0)
	var ended int64
	if err := db.R.QueryRowContext(ctx, `SELECT ended_at FROM pipeline_timings WHERE transfer='terminal-import'`).Scan(&ended); err != nil || ended != 0 {
		t.Fatal("failed import became a success", err)
	}
	for _, update := range []string{"runner_salvage='pending'", "payload_removed=0", "state='importing'"} {
		if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET `+update+` WHERE id=?`, dl.ID); err != nil {
			t.Fatal(err)
		}
		assertWaiting(1)
		if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET runner_salvage='none',payload_removed=1,state='failed' WHERE id=?`, dl.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.EnsureImportAttempt(ctx, dl.ID, dl.GenerationKey); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO import_attempt_stages(download_id,generation_key,attempt_epoch,stage,source,target,state) VALUES(?,?,0,?,?,?,'held')`, dl.ID, dl.GenerationKey, "/stage", "/source", "/target"); err != nil {
		t.Fatal(err)
	}
	assertWaiting(1)
	if _, err := db.W.ExecContext(ctx, `UPDATE import_attempt_stages SET state='operator_disposed' WHERE download_id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	assertWaiting(0)
	if err := db.EndPipelineTiming(ctx, TimingRunnerDoneToImport, "terminal-import", 2000); err != nil {
		t.Fatal(err)
	}
	durations, err := db.PipelineDurationsMs(ctx, TimingRunnerDoneToImport, 0)
	if err != nil || len(durations) != 1 || durations[0] != 1000 {
		t.Fatalf("closed sample altered: %v %v", durations, err)
	}
}
