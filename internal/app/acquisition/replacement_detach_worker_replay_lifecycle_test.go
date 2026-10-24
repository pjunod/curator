package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestReplacementWorkerRetriesSharedEpisodeDetachAfterJournalFailureAndRestart(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := seriesSetup(t, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	episodeOne := item.Seasons[0].Episodes[0].ID
	episodeTwo := item.Seasons[0].Episodes[1].ID
	path := filepath.Join(item.Path, "Season 1", "shared-review.mkv")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const original = "one file contains selected and protected episode bytes"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, itemID, 0, path, int64(len(original)))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{episodeOne, episodeTwo}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='shared-worker-source', source_transfer='shared-worker-transfer' WHERE id=?`, fileID); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var fileGeneration string
	for _, file := range files {
		if file.ID == fileID {
			fileGeneration = file.FileGeneration
		}
	}
	if fileGeneration == "" {
		t.Fatal("shared source did not have a persisted library generation")
	}
	observation := observeReplacementFile(t, svc, itemID, fileID, fileGeneration, "shared-worker-observation")
	blacklist := false
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{
		RequestID: "shared-worker-detach-replay", SourceGeneration: "shared-worker-source", SourceTransfer: "shared-worker-transfer",
		Scope: "episodes", MediaItemID: itemID, SourceFileID: fileID, EpisodeIDs: []int64{episodeOne},
		Intent: "replace_bad", BlacklistSourceRelease: &blacklist, ReasonCode: "corrupt_media",
		Reason:        "remove only the selected bad episode mapping",
		ObservedFiles: []ReplacementObservedFile{{ID: fileID, FileGeneration: fileGeneration, ObservationID: observation.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	reviewed, err := db.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(reviewed) != 1 || reviewed[0].Disposition != "preserved" || reviewed[0].RemovalState != "pending" {
		t.Fatalf("shared-file operation did not retain its scoped detach obligation: files=%+v err=%v", reviewed, err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_shared_detach BEFORE UPDATE OF removal_state ON replacement_operation_files WHEN NEW.removal_state='removed' BEGIN SELECT RAISE(ABORT,'injected shared detach journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.processReplacementOperation(ctx, op); err == nil {
		t.Fatal("worker advanced despite failure to durably journal the selected episode detach")
	}
	blocked, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || blocked.State != "cleanup_blocked" {
		t.Fatalf("failed scoped cleanup was not left visibly retryable: operation=%+v err=%v", blocked, err)
	}
	assertSharedEpisodeState := func(db *sqlite.DB, wantSelected bool) {
		t.Helper()
		current, err := db.ListFilesForItem(ctx, itemID)
		if err != nil || len(current) != 1 || current[0].ID != fileID {
			t.Fatalf("shared media row changed across failed detach: files=%+v err=%v", current, err)
		}
		if data, err := os.ReadFile(path); err != nil || string(data) != original {
			t.Fatalf("shared source bytes changed across scoped detach: data=%q err=%v", data, err)
		}
		gotEpisodes := map[int64]bool{}
		for _, id := range current[0].EpisodeIDs {
			gotEpisodes[id] = true
		}
		if gotEpisodes[episodeTwo] != true || gotEpisodes[episodeOne] != wantSelected || len(gotEpisodes) != map[bool]int{true: 2, false: 1}[wantSelected] {
			t.Fatalf("shared file episode links=%v, want selected=%v and protected sibling=%v", gotEpisodes, wantSelected, episodeTwo)
		}
	}
	assertSharedEpisodeState(db, true)
	// Reopen the durable operation while the failpoint is still installed. The
	// retry must use the original reviewed generation and must not consume the
	// protected sibling episode even though the selected side could now detach.
	dbPath := db.Path
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.W.ExecContext(ctx, `DROP TRIGGER reject_shared_detach`); err != nil {
		t.Fatal(err)
	}
	// Build a fresh service as well as reopening SQLite so the test exercises
	// only durable worker state, not retained in-memory locks or caches.
	svc = New(reopened, nil, nil,
		func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} },
		func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} },
	)
	op, err = reopened.GetReplacementOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatalf("exact reviewed detach did not recover after restart: %v", err)
	}
	assertSharedEpisodeState(reopened, false)
	finalFiles, err := reopened.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(finalFiles) != 1 || finalFiles[0].RemovalState != "removed" {
		t.Fatalf("successful retry did not settle exact selected-source obligation: files=%+v err=%v", finalFiles, err)
	}
	finalOp, err := reopened.GetReplacementOperation(ctx, op.ID)
	if err != nil || (finalOp.State != "searching" && finalOp.State != "waiting_for_release") {
		t.Fatalf("worker did not advance after the exact detach committed: operation=%+v err=%v", finalOp, err)
	}
	if _, err := reopened.GetFileQuality(ctx, fileID); errors.Is(err, sqlite.ErrNotFound) {
		t.Fatal("detaching one episode deleted the still-used shared library row")
	} else if err != nil {
		t.Fatal(err)
	}
}
