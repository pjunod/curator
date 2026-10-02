package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestStorageContainerReceiptsDoNotAssociateSiblingPayloads(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed-root", true: "working-tree"}[nested], func(t *testing.T) {
			root := t.TempDir()
			parent := root
			if nested {
				parent = filepath.Join(root, "completed")
			}
			avatar := filepath.Join(parent, "Avatar.Fire.and.Ash.2025")
			other := filepath.Join(parent, "Other.Movie.2025")
			writeCompleted(t, filepath.Join(avatar, "video.mkv"), 12)
			writeCompleted(t, filepath.Join(other, "video.mkv"), 8)
			receipts := []sqlite.CompletedReceipt{
				{DownloadID: 1, Path: avatar, Title: "Avatar.Fire.and.Ash.2025", State: "downloading", Live: true},
				{DownloadID: 11, Path: parent, Title: "Monster.The.Ed.Gein.Story.S01", State: "failed"},
				{DownloadID: 12, Path: root, Title: "Shared working root", State: "failed"},
			}
			entries, total, err := scanCompletedRoot(context.Background(), root, receipts, nil)
			if err != nil || total != 20 || len(entries) != 2 {
				t.Fatalf("inventory %+v bytes=%d err=%v", entries, total, err)
			}
			for _, e := range entries {
				if e.Path == avatar {
					if e.Status != "active" || len(e.Receipts) != 1 || e.Receipts[0].DownloadID != 1 {
						t.Fatalf("shared receipt contaminated Avatar: %+v", e)
					}
				} else if e.Path != other || e.Status != "untracked" || len(e.Receipts) != 0 {
					t.Fatalf("unrelated payload attributed to history: %+v", e)
				}
			}
			// A specific descendant receipt still identifies its payload, and a
			// second genuinely overlapping download must continue to require review.
			receipts = append(receipts, sqlite.CompletedReceipt{DownloadID: 13, Path: filepath.Join(avatar, "video.mkv"), State: "failed"})
			entries, _, err = scanCompletedRoot(context.Background(), root, receipts, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Path == avatar && (e.Status != "ambiguous" || len(e.Receipts) != 2) {
					t.Fatalf("real ambiguity hidden %+v", e)
				}
			}
		})
	}
}

func TestHistoricalStorageContainerCannotPoisonImportedPayloadCleanup(t *testing.T) {
	for _, location := range []string{"root", "completed", "ancestor", "specific-payload", "live-root"} {
		t.Run(location, func(t *testing.T) {
			svc, db, item, root := completedFixture(t)
			ctx := context.Background()
			payload := filepath.Join(root, "completed", "Avatar.Fire.and.Ash.2025")
			source := filepath.Join(payload, "video.mkv")
			writeCompleted(t, source, 12)
			own := insertImportable(t, db, item, payload)
			own.State = "imported"
			if err := db.UpdateDownloadHandoff(ctx, own); err != nil {
				t.Fatal(err)
			}
			historicalPath := root
			switch location {
			case "completed":
				historicalPath = filepath.Join(root, "completed")
			case "ancestor":
				historicalPath = filepath.Dir(root)
			case "specific-payload":
				historicalPath = payload
			}
			history := insertImportable(t, db, item, historicalPath)
			history.ReleaseTitle = "Monster.The.Ed.Gein.Story.S01"
			history.State = "failed"
			if location == "live-root" {
				history.State = "downloading"
			}
			if err := db.UpdateDownloadHandoff(ctx, history); err != nil {
				t.Fatal(err)
			}
			if location != "live-root" {
				if _, err := db.ClearFailedDownloads(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.ScanCompleted(ctx); err != nil {
				t.Fatal(err)
			}
			entries := inventoryRoot(t, svc, root).Entries
			if len(entries) != 1 {
				t.Fatalf("entries %+v", entries)
			}
			if location == "specific-payload" {
				if entries[0].Status != "ambiguous" || len(entries[0].Receipts) != 2 {
					t.Fatalf("specific history lost %+v", entries)
				}
			} else if len(entries[0].Receipts) != 1 || entries[0].Receipts[0].DownloadID != own.ID {
				t.Fatalf("container matched unrelated payload %+v", entries)
			}
			safe := svc.safePayloadPath(ctx, downloadRef{ID: own.ID, ImportPath: own.ImportPath})
			protected := location == "specific-payload" || location == "live-root"
			if safe == protected {
				t.Fatalf("cleanup authority location=%s safe=%v", location, safe)
			}
			if b, err := os.ReadFile(source); err != nil || len(b) != 12 {
				t.Fatal("inventory changed payload bytes", err)
			}
			receipts, err := db.CompletedReceipts(ctx)
			if err != nil || len(receipts) != 2 {
				t.Fatalf("historical evidence erased %+v %v", receipts, err)
			}
		})
	}
}
