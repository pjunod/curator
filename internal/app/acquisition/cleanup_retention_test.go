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
			if err := svc.ScanCompleted(ctx); err != nil {
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

func TestLegacyReviewCohortBlocksAutomaticCleanupPaths(t *testing.T) {
	client := &fakeClient{}
	svc, db, item := setupWithClientType(t, client, "sabnzbd")
	ctx := context.Background()
	root := t.TempDir()
	payload := filepath.Join(root, "legacy-release")
	media := filepath.Join(payload, "video.mkv")
	writeCompleted(t, media, 15)
	if err := svc.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	dl := insertImportable(t, db, item, payload)
	dl.State = "imported"
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	dl, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dl.GenerationKey == "" {
		t.Fatal("InsertDownload did not persist a generation key")
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review
		(generation_key, path) VALUES(?, ?)`, dl.GenerationKey, dl.ImportPath); err != nil {
		t.Fatal(err)
	}
	updatedPayload := payload + "-updated"
	if err := os.Rename(payload, updatedPayload); err != nil {
		t.Fatal(err)
	}
	media = filepath.Join(updatedPayload, "video.mkv")
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads
		SET handle='updated-handle', release_title='Updated legacy title', import_path=? WHERE id=?`, updatedPayload, dl.ID); err != nil {
		t.Fatal(err)
	}
	dl, err = db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	ref := downloadRef{ID: dl.ID, MediaItemID: dl.MediaItemID, ClientID: dl.ClientID,
		Handle: dl.Handle, GenerationKey: dl.GenerationKey, ImportPath: dl.ImportPath,
		ReleaseTitle: dl.ReleaseTitle, Size: dl.Size}

	// The scheduled sweep, inline import cleanup, and direct helper calls all
	// honor the same durable cohort fence.
	if err := svc.CleanupPayloads(ctx); err != nil {
		t.Fatal(err)
	}
	svc.cleanupAfterImport(ctx, ref)
	if svc.removePayload(ctx, ref) {
		t.Fatal("client removal bypassed the legacy review cohort")
	}
	svc.removeImportedDir(ctx, ref)
	if _, err := os.Stat(media); err != nil {
		t.Fatalf("automatic cleanup removed a legacy payload: %v", err)
	}
	if len(client.removed) != 0 {
		t.Fatalf("automatic cleanup called client Remove: %+v", client.removed)
	}

	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET state='failed', cleanup_pending=1 WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	svc.cleanupPartialPayloads(ctx)
	if _, err := os.Stat(media); err != nil {
		t.Fatalf("partial cleanup removed a legacy payload: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET activity_dismissed=1 WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM legacy_cleanup_review WHERE generation_key=?`, dl.GenerationKey).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("Activity removal erased review custody: rows=%d err=%v", retained, err)
	}
	// Reuse the same integer ID, timestamp, and title for a distinct generation
	// at a new path. An in-flight stale cleanup ref must not delete it.
	if _, err := db.W.ExecContext(ctx, `DELETE FROM downloads WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	newPayload := filepath.Join(root, "new-generation")
	newMedia := filepath.Join(newPayload, "video.mkv")
	writeCompleted(t, newMedia, 15)
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads
		(id, media_item_id, release_title, protocol, state, import_path, save_path, client_id, handle, added_at, updated_at)
		VALUES(?, ?, ?, ?, 'imported', ?, ?, ?, ?, ?, ?)`, dl.ID, dl.MediaItemID,
		dl.ReleaseTitle, dl.Protocol, newPayload, newPayload, dl.ClientID, dl.Handle,
		dl.AddedAt.UnixMilli(), dl.UpdatedAt.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	newDL, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if newDL.GenerationKey == "" || newDL.GenerationKey == dl.GenerationKey {
		t.Fatalf("reused download ID did not receive a fresh generation key: old=%q new=%q", dl.GenerationKey, newDL.GenerationKey)
	}
	svc.removeImportedDir(ctx, downloadRef{ID: dl.ID, MediaItemID: dl.MediaItemID,
		ClientID: dl.ClientID, Handle: dl.Handle, GenerationKey: dl.GenerationKey,
		ImportPath: newPayload, ReleaseTitle: dl.ReleaseTitle, Size: dl.Size})
	if _, err := os.Stat(newMedia); err != nil {
		t.Fatalf("stale cleanup reference removed a newer generation: %v", err)
	}
}

func TestLegacyReviewPathAncestorBlocksNewGenerationCleanup(t *testing.T) {
	svc, db, item := setupWithClientType(t, &fakeClient{}, "sabnzbd")
	ctx := context.Background()
	root := t.TempDir()
	if err := svc.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(root, "legacy-container")
	legacy := insertImportable(t, db, item, legacyPath)
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review
		(generation_key, path) VALUES(?, ?)`, legacy.GenerationKey, legacyPath); err != nil {
		t.Fatal(err)
	}

	newPath := filepath.Join(legacyPath, "rediscovered-child")
	media := filepath.Join(newPath, "video.mkv")
	writeCompleted(t, media, 15)
	newDL := insertImportable(t, db, item, newPath)
	newDL.State = "imported"
	if err := db.UpdateDownloadHandoff(ctx, newDL); err != nil {
		t.Fatal(err)
	}
	svc.removeImportedDir(ctx, downloadRef{
		ID: newDL.ID, ClientID: newDL.ClientID, Handle: newDL.Handle,
		GenerationKey: newDL.GenerationKey, ImportPath: newDL.ImportPath,
	})
	if _, err := os.Stat(media); err != nil {
		t.Fatalf("new descendant of a reviewed path was removed: %v", err)
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
			if err := svc.ScanCompleted(ctx); err != nil {
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
					if err := svc.ScanCompleted(ctx); err != nil {
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
