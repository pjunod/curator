package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestPendingCleanupSurvivesActivityDismissal(t *testing.T) {
	for _, name := range []string{"clear_finished", "retention", "individual"} {
		t.Run(name, func(t *testing.T) {
			svc, db, item := setupWithClientType(t, &fakeClient{}, "sabnzbd")
			ctx := context.Background()
			root := t.TempDir()
			payload := filepath.Join(root, "release")
			writeCompleted(t, filepath.Join(payload, "video.mkv"), 15)
			if err := svc.SetCompletedRoots(ctx, root); err != nil {
				t.Fatal(err)
			}
			dl := insertImportable(t, db, item, payload)
			dl.State = "imported"
			if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
				t.Fatal(err)
			}
			var n int64
			var err error
			switch name {
			case "retention":
				n, err = db.PruneTerminalDownloads(ctx, time.Now().Add(time.Hour))
			case "individual":
				err = db.DeleteDownload(ctx, dl.ID)
				n = 1
			default:
				n, err = svc.ClearFinished(ctx)
			}
			if err != nil || n != 1 {
				t.Fatalf("dismissed=%d: %v", n, err)
			}
			if _, err := db.GetDownload(ctx, dl.ID); err != nil {
				t.Fatalf("cleanup ownership lost: %v", err)
			}
			counts, err := svc.QueueCounts(ctx)
			if err != nil || counts["imported"] != 0 {
				t.Fatalf("dismissed row visible: %v %v", counts, err)
			}
			rows, err := svc.QueuePage(ctx, sqlite.QueueFilter(""), "", 25, 0)
			if err != nil || len(rows) != 0 {
				t.Fatalf("dismissed row in page: %v %v", rows, err)
			}
			if n, err := svc.ClearFinished(ctx); err != nil || n != 0 {
				t.Fatalf("repeated dismissal=%d: %v", n, err)
			}
			if name == "individual" {
				if err := db.DeleteDownload(ctx, dl.ID); err != nil {
					t.Fatal(err)
				}
			}
			if rows, err := db.ListRecentDownloads(ctx); err != nil || len(rows) != 0 {
				t.Fatalf("legacy queue exposes dismissed row: %v %v", rows, err)
			}
			if _, err := os.Stat(payload); err != nil {
				t.Fatalf("dismissal touched files: %v", err)
			}
			// A new service must retry using durable ownership, without the UI row.
			again := New(db, svc.bus, svc.log, svc.newIndexer, svc.newClient)
			if err := again.ScanCompleted(ctx); err != nil {
				t.Fatal(err)
			}
			if got := inventoryRoot(t, again, root).Entries[0].Status; got != "cleanup_pending" {
				t.Fatalf("status=%s", got)
			}
			if err := again.CleanupPayloads(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(payload); !os.IsNotExist(err) {
				t.Fatalf("payload still exists: %v", err)
			}
			if _, err := db.GetDownload(ctx, dl.ID); err == nil {
				t.Fatal("cleaned dismissed row was not retired")
			}
			receipts, err := db.CompletedReceipts(ctx)
			if err != nil || len(receipts) != 1 || !receipts[0].PayloadRemoved || receipts[0].Live {
				t.Fatalf("removal receipt=%+v: %v", receipts, err)
			}
		})
	}
}

func TestDismissedCleanupStillRespectsOwnershipAndClientSetting(t *testing.T) {
	for _, blocked := range []string{"client_disabled", "overlapping_download"} {
		t.Run(blocked, func(t *testing.T) {
			svc, db, item := setupWithClientType(t, &fakeClient{}, "sabnzbd")
			ctx := context.Background()
			root := t.TempDir()
			payload := filepath.Join(root, "release")
			writeCompleted(t, filepath.Join(payload, "video.mkv"), 15)
			if err := svc.SetCompletedRoots(ctx, root); err != nil {
				t.Fatal(err)
			}
			dl := insertImportable(t, db, item, payload)
			dl.State = "imported"
			if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
				t.Fatal(err)
			}
			if _, err := svc.ClearFinished(ctx); err != nil {
				t.Fatal(err)
			}
			var otherID int64
			if blocked == "client_disabled" {
				if _, err := db.W.ExecContext(ctx, "UPDATE download_clients SET remove_completed=0 WHERE id=?", dl.ClientID); err != nil {
					t.Fatal(err)
				}
			} else {
				other := insertImportable(t, db, item, payload)
				otherID = other.ID
				if otherID == dl.ID {
					t.Fatal("dismissal allowed reuse of pending cleanup ID")
				}
				if err := db.UpdateDownloadHandoff(ctx, other); err != nil {
					t.Fatal(err)
				}
				err := svc.payloadPathError(ctx, downloadRef{ID: dl.ID, ImportPath: payload})
				if err == nil || !strings.Contains(err.Error(), "overlaps download") {
					t.Fatalf("missing blocker explanation: %v", err)
				}
			}
			if err := svc.CleanupPayloads(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(payload); err != nil {
				t.Fatalf("blocked payload removed: %v", err)
			}
			if _, err := db.GetDownload(ctx, dl.ID); err != nil {
				t.Fatalf("blocked retry lost: %v", err)
			}
			if otherID != 0 {
				other, err := db.GetDownload(ctx, otherID)
				if err != nil || other.PayloadRemoved {
					t.Fatalf("unrelated row changed: %+v %v", other, err)
				}
			} else {
				if _, err := db.W.ExecContext(ctx, "UPDATE download_clients SET remove_completed=1 WHERE id=?", dl.ClientID); err != nil {
					t.Fatal(err)
				}
				if err := svc.CleanupPayloads(ctx); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(payload); !os.IsNotExist(err) {
					t.Fatalf("reenabled cleanup lost dismissed download: %v", err)
				}
			}
		})
	}
}

func TestDismissedLegacyImportsKeepCleanupHandleAndSavePath(t *testing.T) {
	for _, source := range []string{"handle_only", "save_path"} {
		for _, dismissal := range []string{"clear_finished", "retention", "individual"} {
			t.Run(source+"/"+dismissal, func(t *testing.T) {
				client := &fakeClient{}
				svc, db, item := setupWithClientType(t, client, "sabnzbd")
				ctx := context.Background()
				payload := ""
				if source == "save_path" {
					root := t.TempDir()
					payload = filepath.Join(root, "legacy")
					writeCompleted(t, filepath.Join(payload, "video.mkv"), 15)
					if err := svc.SetCompletedRoots(ctx, root); err != nil {
						t.Fatal(err)
					}
				}
				dl := insertImportable(t, db, item, payload)
				dl.State, dl.ImportPath = "imported", ""
				if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
					t.Fatal(err)
				}
				if source == "save_path" {
					if _, err := db.W.ExecContext(ctx, "UPDATE downloads SET handle='' WHERE id=?", dl.ID); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				switch dismissal {
				case "clear_finished":
					_, err = svc.ClearFinished(ctx)
				case "retention":
					_, err = db.PruneTerminalDownloads(ctx, time.Now().Add(time.Hour))
				case "individual":
					err = db.DeleteDownload(ctx, dl.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.GetDownload(ctx, dl.ID); err != nil {
					t.Fatalf("legacy cleanup ownership lost: %v", err)
				}
				counts, err := svc.QueueCounts(ctx)
				if err != nil || counts["imported"] != 0 {
					t.Fatalf("dismissed row visible: %v %v", counts, err)
				}
				if err := svc.CleanupPayloads(ctx); err != nil {
					t.Fatal(err)
				}
				if source == "handle_only" && (len(client.removed) != 1 || !client.removed[0].DeleteData) {
					t.Fatalf("client payload cleanup not requested: %+v", client.removed)
				}
				if payload != "" {
					if _, err := os.Stat(payload); !os.IsNotExist(err) {
						t.Fatalf("save_path fallback left files behind: %v", err)
					}
				}
				if _, err := db.GetDownload(ctx, dl.ID); err == nil {
					t.Fatal("cleaned legacy row was not retired")
				}
			})
		}
	}
}
