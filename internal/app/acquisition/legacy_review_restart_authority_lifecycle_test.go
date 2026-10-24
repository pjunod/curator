package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// An explicit legacy review approval is durable work. Startup recovery must
// replay that exact selection after a process restart, while still checking
// the approved target identity before it moves any source bytes.
func TestLegacyReviewManualImportReplaysExactAuthorityAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		name           string
		changeTarget   bool
		wantState      string
		wantLibraryRow bool
	}{
		{name: "approved target unchanged", wantState: "imported", wantLibraryRow: true},
		{name: "target changed after approval", changeTarget: true, wantState: "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, db, itemID := setup(t, nil, &fakeClient{})
			completedRoot := t.TempDir()
			payload := filepath.Join(completedRoot, "legacy-review")
			selected := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv")
			sibling := filepath.Join(payload, "Test.Show.S01E02.1080p.WEB-DL.mkv")
			if err := os.MkdirAll(payload, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(selected, []byte("selected legacy video"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sibling, []byte("unselected sibling"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES('',?)`, payload); err != nil {
				t.Fatal(err)
			}
			if err := svc.SetCompletedRoots(ctx, completedRoot); err != nil {
				t.Fatal(err)
			}
			if err := svc.ScanCompleted(ctx); err != nil {
				t.Fatal(err)
			}
			entry := inventoryRoot(t, svc, completedRoot).Entries[0]
			var approved CompletedFileSelection
			for _, file := range entry.ReviewFiles {
				if canonicalCompletedPath(file.Path) == canonicalCompletedPath(selected) {
					approved = CompletedFileSelection{Path: file.Path, SnapshotIdentity: file.SnapshotIdentity}
				}
			}
			if approved.SnapshotIdentity == "" {
				t.Fatalf("selected file missing from reviewed snapshot: %+v", entry.ReviewFiles)
			}

			// Queue without a consumer so shutdown leaves the durable manual row
			// at the same point a process crash would.
			svc.importMu.Lock()
			svc.importCh = make(chan importJob, 1)
			svc.importCtx = ctx
			svc.importJobs = map[int64]*importTicket{}
			svc.importMu.Unlock()
			id, err := svc.QueueManualImport(ctx, ManualImportRequest{
				Path: payload, Paths: []string{approved.Path}, MediaItemID: itemID,
				ReviewLegacy: true, ReviewLegacyFingerprint: entry.Fingerprint,
				ReviewLegacyFiles: []CompletedFileSelection{approved},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.changeTarget {
				if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET monitored=0 WHERE id=?`, itemID); err != nil {
					t.Fatal(err)
				}
			}

			dataDir := filepath.Dir(db.Path)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := sqlite.Open(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			if err := reopened.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			fresh := New(reopened, nil, nil,
				func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} },
				func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} },
			)
			workerCtx, stopWorkers := context.WithCancel(ctx)
			fresh.StartImporters(workerCtx)
			t.Cleanup(func() { stopWorkers(); fresh.WaitImporters() })

			deadline := time.After(5 * time.Second)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				row, loadErr := reopened.GetDownload(ctx, id)
				if loadErr == nil && row.State == tc.wantState {
					if tc.changeTarget && !strings.Contains(row.Error, "legacy payload review") {
						t.Fatalf("changed target was not reported as an expired review approval: %+v", row)
					}
					break
				}
				select {
				case <-deadline:
					t.Fatalf("startup replay did not reach %q: row=%+v err=%v", tc.wantState, row, loadErr)
				case <-ticker.C:
				}
			}

			if got, err := os.ReadFile(selected); err != nil || string(got) != "selected legacy video" {
				t.Fatalf("startup replay changed selected source bytes: content=%q err=%v", got, err)
			}
			if got, err := os.ReadFile(sibling); err != nil || string(got) != "unselected sibling" {
				t.Fatalf("startup replay changed unselected sibling: content=%q err=%v", got, err)
			}
			var libraryRows int
			if err := reopened.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE media_item_id=?`, itemID).Scan(&libraryRows); err != nil {
				t.Fatal(err)
			}
			if tc.wantLibraryRow && libraryRows != 1 {
				t.Fatalf("exactly selected file was not placed after restart: library rows=%d", libraryRows)
			}
			if tc.wantLibraryRow {
				var libraryPath string
				if err := reopened.R.QueryRowContext(ctx, `SELECT path FROM media_files WHERE media_item_id=?`, itemID).Scan(&libraryPath); err != nil {
					t.Fatal(err)
				}
				if canonicalCompletedPath(libraryPath) == canonicalCompletedPath(selected) {
					t.Fatalf("imported file still points at the reviewed source: %q", libraryPath)
				}
				if got, err := os.ReadFile(libraryPath); err != nil || string(got) != "selected legacy video" {
					t.Fatalf("library placement has wrong bytes: path=%q content=%q err=%v", libraryPath, got, err)
				}
			}
			if !tc.wantLibraryRow && libraryRows != 0 {
				t.Fatalf("expired approval placed files after restart: library rows=%d", libraryRows)
			}
		})
	}
}

func TestManualImportQueueFullCanRetrySameDurableRow(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	payload := t.TempDir()
	selected := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv")
	if err := os.WriteFile(selected, []byte("manual import source"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A full bounded worker queue must leave an explicit, visible failure that
	// the user can retry on the same Activity row after capacity returns.
	svc.importMu.Lock()
	svc.importCh = make(chan importJob, importQueue)
	svc.importCtx = ctx
	svc.importJobs = map[int64]*importTicket{}
	for range importQueue {
		svc.importCh <- importJob{}
	}
	svc.importMu.Unlock()
	if id, err := svc.QueueManualImport(ctx, ManualImportRequest{
		Path: payload, Paths: []string{selected}, MediaItemID: itemID,
	}); err == nil || id != 0 || !strings.Contains(err.Error(), "queue is full") {
		t.Fatalf("saturated queue admission = id %d, err %v; want a retryable queue-full response", id, err)
	}
	rows, err := db.ListRecentDownloads(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("queue-full admission did not retain one durable Activity row: rows=%+v err=%v", rows, err)
	}
	failed := rows[0]
	if failed.State != "failed" || !strings.Contains(failed.Error, "queue is full") {
		t.Fatalf("queue-full request was not visible as failed: %+v", failed)
	}
	if got, err := os.ReadFile(selected); err != nil || string(got) != "manual import source" {
		t.Fatalf("queue-full response changed source bytes: %q err=%v", got, err)
	}
	for range importQueue {
		<-svc.importCh
	}

	retriedID, err := svc.QueueManualImport(ctx, ManualImportRequest{
		Path: payload, Paths: []string{selected}, MediaItemID: itemID, DownloadID: failed.ID,
	})
	if err != nil || retriedID != failed.ID {
		t.Fatalf("same-row import retry = id %d err %v; want row %d", retriedID, err, failed.ID)
	}
	if len(svc.importCh) != 1 {
		t.Fatalf("retry was not queued after capacity returned: queued=%d", len(svc.importCh))
	}
	job := <-svc.importCh
	svc.runImportTracked(job.ctx, job)

	finished, err := db.GetDownload(ctx, failed.ID)
	if err != nil || finished.State != "imported" {
		t.Fatalf("retried Activity row did not finish import: row=%+v err=%v", finished, err)
	}
	var count int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE media_item_id=?`, itemID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry duplicated or omitted library placement: rows=%d err=%v", count, err)
	}
	var libraryPath string
	if err := db.R.QueryRowContext(ctx, `SELECT path FROM media_files WHERE media_item_id=?`, itemID).Scan(&libraryPath); err != nil {
		t.Fatal(err)
	}
	if canonicalCompletedPath(libraryPath) == canonicalCompletedPath(selected) {
		t.Fatalf("retried file still points at the reviewed source: %q", libraryPath)
	}
	if got, err := os.ReadFile(libraryPath); err != nil || string(got) != "manual import source" {
		t.Fatalf("retried library placement has wrong bytes: path=%q content=%q err=%v", libraryPath, got, err)
	}
}
