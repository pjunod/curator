package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestReplacementObservationStaleTupleTerminatesAndAllowsFreshRequest(t *testing.T) {
	for _, scenario := range []string{"path_changed", "generation_changed", "row_deleted"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			svc, db, itemID := autoSetup(t, nil, &fakeClient{})
			item, err := db.GetMediaItemFull(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			fileID, _ := acq2AddFile(t, svc, itemID, 0, item.Path, "stale-tuple-source.mkv", "Test.Movie.2024.720p.WEB-DL-GROUP")
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
				t.Fatal("fixture library file has no persisted generation")
			}
			first, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, "stale-first-"+scenario)
			if err != nil {
				t.Fatal(err)
			}

			var freshGeneration string
			switch scenario {
			case "path_changed":
				path := filepath.Join(item.Path, "renamed-after-observation.mkv")
				if err := os.WriteFile(path, []byte("newly reviewed source bytes"), 0o644); err != nil {
					t.Fatal(err)
				}
				if _, err := db.W.ExecContext(ctx, `UPDATE media_files SET path=? WHERE id=?`, path, fileID); err != nil {
					t.Fatal(err)
				}
				freshGeneration = generation
			case "generation_changed":
				freshGeneration = generation + "-new-generation"
				if _, err := db.W.ExecContext(ctx, `UPDATE media_files SET file_generation=? WHERE id=?`, freshGeneration, fileID); err != nil {
					t.Fatal(err)
				}
			case "row_deleted":
				if _, err := db.W.ExecContext(ctx, `DELETE FROM media_files WHERE id=?`, fileID); err != nil {
					t.Fatal(err)
				}
			}

			svc.processReplacementFileObservations(ctx)
			stale, err := svc.GetReplacementFileObservation(ctx, first.ID)
			if err != nil || stale.State != "stale" || stale.Error != "selected file changed before observation" {
				t.Fatalf("changed authority did not terminate the old request: observation=%+v err=%v", stale, err)
			}
			if stale.SHA256 != "" || stale.FileIdentity != "" || stale.FilesystemGeneration != "" || stale.Size != 0 {
				t.Fatalf("stale request retained file authority: %+v", stale)
			}
			if _, err := svc.GetReplacementFileObservation(ctx, first.ID); err != nil {
				t.Fatal(err)
			}
			if scenario == "row_deleted" {
				if _, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, "must-not-revive-deleted-row"); err == nil {
					t.Fatal("fresh observation unexpectedly started without the selected library row")
				}
				return
			}

			fresh, replayed, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, freshGeneration, "fresh-after-stale-"+scenario)
			if err != nil || replayed {
				t.Fatalf("fresh request could not observe the current tuple: observation=%+v replayed=%v err=%v", fresh, replayed, err)
			}
			svc.processReplacementFileObservations(ctx)
			ready, err := svc.GetReplacementFileObservation(ctx, fresh.ID)
			if err != nil || ready.State != "ready" || ready.SHA256 == "" {
				t.Fatalf("fresh current tuple did not become ready: observation=%+v err=%v", ready, err)
			}
		})
	}
}
