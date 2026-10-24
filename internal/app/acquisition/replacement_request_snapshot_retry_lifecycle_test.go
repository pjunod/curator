package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestReplacementRequestSnapshotFailureRetriesExactReviewAfterRestart(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := autoSetup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, path := acq2AddFile(t, svc, itemID, 0, item.Path, "snapshot-retry.mkv", "Test.Movie.2024.720p.WEB-DL.OLD")
	if _, err := db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='snapshot-retry-generation',source_transfer='snapshot-retry-transfer' WHERE id=?`, fileID); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var generation string
	for _, file := range files {
		if file.ID == fileID {
			generation = file.FileGeneration
		}
	}
	if generation == "" {
		t.Fatal("replacement review fixture has no file generation")
	}
	observation := observeReplacementFile(t, svc, itemID, fileID, generation, "snapshot-retry-observation")
	blacklist := false
	req := ReplacementRequest{
		RequestID: "snapshot-retry-request", SourceGeneration: "snapshot-retry-generation", SourceTransfer: "snapshot-retry-transfer",
		Scope: "download", MediaItemID: itemID, SourceFileID: fileID, Intent: "replace_bad",
		BlacklistSourceRelease: &blacklist, ReasonCode: "bad_media", Reason: "verified corrupt source",
		ObservedFiles: []ReplacementObservedFile{{ID: fileID, FileGeneration: generation, ObservationID: observation.ID}},
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_reviewed_replacement_snapshot BEFORE INSERT ON replacement_operation_files BEGIN SELECT RAISE(ABORT,'injected reviewed snapshot persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CreateReplacement(ctx, req); err == nil {
		t.Fatal("replacement request passed an injected snapshot persistence failure")
	}
	if _, err := db.GetReplacementOperationByRequestID(ctx, req.RequestID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("failed snapshot transaction left an idempotency record: %v", err)
	}
	if operations, err := db.ListReplacementOperationsForItem(ctx, itemID, 0); err != nil || len(operations) != 0 {
		t.Fatalf("failed snapshot transaction left partial operations=%+v err=%v", operations, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "bytes" {
		t.Fatalf("failed replacement admission changed source bytes: %q err=%v", got, err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_reviewed_replacement_snapshot`); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, nil, nil, nil, nil)
	created, replay, err := restarted.CreateReplacement(ctx, req)
	if err != nil || replay || created.RequestID != req.RequestID || created.State != "accepted" {
		t.Fatalf("exact reviewed request after restart=%+v replay=%v err=%v", created, replay, err)
	}
	createdAgain, replay, err := restarted.CreateReplacement(ctx, req)
	if err != nil || !replay || createdAgain.ID != created.ID {
		t.Fatalf("committed request replay=%+v replay=%v err=%v, want same durable operation", createdAgain, replay, err)
	}
	operation, reviewedFiles, err := restarted.GetReplacement(ctx, created.ID)
	if err != nil || operation.ID != created.ID || len(reviewedFiles) != 1 || reviewedFiles[0].MediaFileID != fileID || reviewedFiles[0].ObservationID != observation.ID || reviewedFiles[0].FileGeneration != generation {
		t.Fatalf("restarted replacement review=%+v files=%+v err=%v", operation, reviewedFiles, err)
	}
	if operations, err := reopened.ListReplacementOperationsForItem(ctx, itemID, 0); err != nil || len(operations) != 1 || operations[0].ID != created.ID {
		t.Fatalf("exact retry created duplicate durable requests: %+v err=%v", operations, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("reviewed source disappeared before replacement worker approval: %v", err)
	}
}
