package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

func TestReplacementWorkerRetiresOnlyReviewedAdditionalCopySource(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{}
	svc, db, itemID := seriesSetup(t, client)
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) == 0 {
		t.Fatalf("series fixture item=%+v err=%v", item, err)
	}
	episodeID := item.Seasons[0].Episodes[0].ID
	copyPath := filepath.Join(t.TempDir(), "additional-copy")
	if err := os.MkdirAll(copyPath, 0o700); err != nil {
		t.Fatal(err)
	}
	copyID, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: itemID, Name: "Additional copy", QualityProfileID: 1, Path: copyPath, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	addFile := func(copyID int64, root, name, sourceGeneration string, body []byte) (int64, string, string) {
		t.Helper()
		path := filepath.Join(root, "Season 1", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		id, err := db.UpsertFile(ctx, itemID, copyID, path, int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.ReplaceFileEpisodeLinks(ctx, id, []int64{episodeID}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.W.ExecContext(ctx, `UPDATE media_files SET source_generation=?,source_transfer=? WHERE id=?`,
			sourceGeneration, "transfer-"+sourceGeneration, id); err != nil {
			t.Fatal(err)
		}
		files, err := db.ListFilesForItem(ctx, itemID)
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if file.ID == id {
				return id, path, file.FileGeneration
			}
		}
		t.Fatalf("file %d is absent after upsert", id)
		return 0, "", ""
	}

	primaryBytes := []byte("primary copy source stays untouched")
	primaryID, primaryPath, _ := addFile(0, item.Path, "primary.mkv", "primary-source-generation", primaryBytes)
	selectedBytes := []byte("reviewed bad bytes in the additional copy")
	selectedID, selectedPath, selectedGeneration := addFile(copyID, copyPath, "additional-bad.mkv", "additional-source-generation", selectedBytes)
	observation := observeReplacementFile(t, svc, itemID, selectedID, selectedGeneration, "observe-additional-copy-replacement")
	bl := false
	op, replayed, err := svc.CreateReplacement(ctx, ReplacementRequest{
		RequestID: "additional-copy-replacement", SourceGeneration: "additional-source-generation",
		SourceTransfer: "transfer-additional-source-generation", Scope: "episodes", MediaItemID: itemID,
		CopyID: copyID, SourceFileID: selectedID, EpisodeIDs: []int64{episodeID}, Intent: "replace_bad",
		BlacklistSourceRelease: &bl, ReasonCode: "bad_media", Reason: "replace the reviewed additional-copy source",
		ObservedFiles: []ReplacementObservedFile{{ID: selectedID, FileGeneration: selectedGeneration, ObservationID: observation.ID}},
	})
	if err != nil || replayed {
		t.Fatalf("create copy-scoped replacement operation=%+v replayed=%v err=%v", op, replayed, err)
	}
	files, err := db.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(files) != 1 || files[0].MediaFileID != selectedID || files[0].Disposition != "selected" {
		t.Fatalf("replacement journal crossed copy scope: files=%+v err=%v", files, err)
	}

	release := rel("Test.Show.S01E01.1080p.WEB-DL.x264-COPY-REPLACEMENT", 30)
	indexer := &mutableWorkerIndexer{releases: []ports.Release{release}}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return indexer }
	if err := svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatalf("process additional-copy replacement: %v", err)
	}

	if _, err := os.Stat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("reviewed additional-copy source was not retired: %v", err)
	}
	if _, err := db.GetFileQuality(ctx, selectedID); err == nil {
		t.Fatal("retired additional-copy source still has a library row")
	}
	if got, err := os.ReadFile(primaryPath); err != nil || string(got) != string(primaryBytes) {
		t.Fatalf("primary-copy bytes changed during additional-copy replacement: %q err=%v", got, err)
	}
	primary, err := db.GetFileQuality(ctx, primaryID)
	if err != nil || primary.CopyID != 0 {
		t.Fatalf("primary-copy library row was retired or reassigned: %+v err=%v", primary, err)
	}
	remaining, err := db.ListFilesForItem(ctx, itemID)
	if err != nil || len(remaining) != 1 {
		t.Fatalf("expected only the untouched primary library file before the new attempt imports, files=%+v err=%v", remaining, err)
	}
	if remaining[0].ID != primaryID || remaining[0].CopyID != 0 || len(remaining[0].EpisodeIDs) != 1 || remaining[0].EpisodeIDs[0] != episodeID {
		t.Fatalf("copy-scoped replacement changed the primary copy's ownership or episode link: %+v", remaining)
	}
	operation, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || operation.CopyID != copyID || operation.State != "replacing" {
		t.Fatalf("replacement did not persist its selected-copy attempt: operation=%+v err=%v", operation, err)
	}
	attempts, err := db.ListReplacementDownloads(ctx, op.ID)
	if err != nil || len(attempts) != 1 || attempts[0].CopyID != copyID || attempts[0].ReplacementOperationID != op.ID {
		t.Fatalf("replacement download escaped selected copy: attempts=%+v err=%v", attempts, err)
	}
	journal, err := db.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(journal) != 1 || journal[0].RemovalState != "removed" {
		t.Fatalf("reviewed source retirement was not durably journaled: files=%+v err=%v", journal, err)
	}
}
