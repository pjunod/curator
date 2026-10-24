package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyManualImportConsentExpiresWhenTargetIdentityChanges(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	completedRoot := t.TempDir()
	payload := filepath.Join(completedRoot, "reviewed-payload")
	if err := os.MkdirAll(payload, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(payload, "Reviewed.Show.S01E01.1080p.WEB-DL.mkv")
	if err := os.WriteFile(source, []byte("legacy selected bytes"), 0o644); err != nil {
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
	if len(entry.ReviewFiles) != 1 {
		t.Fatalf("review inventory=%+v", entry.ReviewFiles)
	}
	selection := CompletedFileSelection{Path: entry.ReviewFiles[0].Path, SnapshotIdentity: entry.ReviewFiles[0].SnapshotIdentity}

	// Keep the durable job queued so the destination can change between user
	// approval and worker execution.
	svc.importMu.Lock()
	svc.importCh = make(chan importJob, 1)
	svc.importCtx = ctx
	svc.importJobs = map[int64]*importTicket{}
	svc.importMu.Unlock()
	id, err := svc.QueueManualImport(ctx, ManualImportRequest{
		Path: payload, Paths: []string{selection.Path}, MediaItemID: itemID,
		ReviewLegacy: true, ReviewLegacyFingerprint: entry.Fingerprint,
		ReviewLegacyFiles: []CompletedFileSelection{selection},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_items SET monitored=0 WHERE id=?`, itemID); err != nil {
		t.Fatal(err)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.runImport(ctx, dl)
	if err == nil || !strings.Contains(err.Error(), "legacy payload review") {
		t.Fatalf("worker honored approval for a changed target identity: %v", err)
	}
	if got, readErr := os.ReadFile(source); readErr != nil || string(got) != "legacy selected bytes" {
		t.Fatalf("expired approval changed source bytes: %q err=%v", got, readErr)
	}
	var files int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE media_item_id=?`, itemID).Scan(&files); err != nil || files != 0 {
		t.Fatalf("expired approval created library rows: count=%d err=%v", files, err)
	}
}

func TestSelectedLegacyDeletePrevalidatesAllFilesBeforeRemovingAny(t *testing.T) {
	svc, db, itemID, root := completedFixture(t)
	ctx := context.Background()
	payload := filepath.Join(root, "review", "legacy-payload")
	first := filepath.Join(payload, "a.mkv")
	second := filepath.Join(payload, "b.mkv")
	writeCompleted(t, first, 10)
	writeCompleted(t, second, 11)
	dl := insertImportable(t, db, itemID, payload)
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES(?,?)`, dl.GenerationKey, payload); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDownloadState(ctx, dl.ID, "failed", 0, "held for review"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	selections := make([]CompletedFileSelection, 0, 2)
	for _, file := range entry.ReviewFiles {
		selections = append(selections, CompletedFileSelection{Path: file.Path, SnapshotIdentity: file.SnapshotIdentity})
	}
	if len(selections) != 2 || selections[0].Path != canonicalCompletedPath(first) || selections[1].Path != canonicalCompletedPath(second) {
		t.Fatalf("unexpected review selection order: %+v", selections)
	}
	changed := filepath.Join(payload, "b-replacement")
	writeCompleted(t, changed, 11)
	if err := os.Rename(changed, second); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedLegacyFiles(ctx, entry.Path, entry.Fingerprint, selections); err == nil {
		t.Fatal("multi-file delete accepted a stale later selection")
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("a file was removed before all selected identities were validated (%s): %v", path, err)
		}
	}
}

func TestSelectedLegacyDeleteRechecksLibraryReferenceAfterReview(t *testing.T) {
	svc, db, itemID, root := completedFixture(t)
	ctx := context.Background()
	payload := filepath.Join(root, "review", "legacy-payload")
	selected := filepath.Join(payload, "selected.mkv")
	sibling := filepath.Join(payload, "sibling.mkv")
	writeCompleted(t, selected, 15)
	writeCompleted(t, sibling, 16)
	dl := insertImportable(t, db, itemID, payload)
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES(?,?)`, dl.GenerationKey, payload); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDownloadState(ctx, dl.ID, "failed", 0, "held for review"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	var selection CompletedFileSelection
	for _, file := range entry.ReviewFiles {
		if file.Path == canonicalCompletedPath(selected) {
			selection = CompletedFileSelection{Path: file.Path, SnapshotIdentity: file.SnapshotIdentity}
		}
	}
	if selection.SnapshotIdentity == "" {
		t.Fatalf("selected file missing from review: %+v", entry.ReviewFiles)
	}
	// A later import may establish library ownership after the inventory scan.
	// The final deletion boundary must repeat that protection check.
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files(media_item_id,path,size,added_at) VALUES(?,?,15,0)`, itemID, selected); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedLegacyFiles(ctx, entry.Path, entry.Fingerprint, []CompletedFileSelection{selection}); err == nil {
		t.Fatal("selected legacy delete removed a file that became library-referenced after review")
	}
	for _, path := range []string{selected, sibling} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("refused selected deletion changed payload member %s: %v", path, err)
		}
	}
	var count int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE media_item_id=? AND path=?`, itemID, selected).Scan(&count); err != nil || count != 1 {
		t.Fatalf("library reference was not retained: count=%d err=%v", count, err)
	}
}
