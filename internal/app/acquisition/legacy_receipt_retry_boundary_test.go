package acquisition

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyReceiptFallbackValidatesCachedSnapshotBeforeManualImport(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	root := t.TempDir()
	payload := filepath.Join(root, "orphaned-legacy-payload")
	if err := os.MkdirAll(payload, 0o755); err != nil {
		t.Fatal(err)
	}
	mediaPath := filepath.Join(payload, "Reviewed.Show.S01E01.1080p.WEB-DL.mkv")
	if err := os.WriteFile(mediaPath, []byte("legacy bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	dl := insertImportable(t, db, itemID, payload)
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDownloadState(ctx, dl.ID, "imported", 1, ""); err != nil {
		t.Fatal(err)
	}
	dl, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES(?,?)`, dl.GenerationKey, payload); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	cached := inventoryRoot(t, svc, root)
	if len(cached.Entries) != 1 || !cached.Entries[0].LegacyReview || len(cached.Entries[0].Receipts) != 1 || !cached.Entries[0].Receipts[0].LegacyReview {
		t.Fatalf("expected a durable legacy receipt in inventory: %+v", cached.Entries)
	}
	entry := cached.Entries[0]
	if len(entry.ReviewFiles) != 1 || entry.Fingerprint == "" {
		t.Fatalf("legacy receipt did not carry a complete file snapshot: %+v", entry)
	}
	selection := []CompletedFileSelection{{Path: entry.ReviewFiles[0].Path, SnapshotIdentity: entry.ReviewFiles[0].SnapshotIdentity}}

	// Simulate an older cached inventory that has the durable receipt but lost
	// the denormalized LegacyReview flag. The receipt must still fence the import.
	entry.LegacyReview = false
	cached.Entries[0] = entry
	report, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCompletedScan(ctx, root, string(report)); err != nil {
		t.Fatal(err)
	}
	if err := svc.validateLegacyReviewSnapshot(ctx, payload, entry.Fingerprint, selection); err != nil {
		t.Fatalf("cached receipt did not authorize exact reviewed snapshot: %v", err)
	}
	if !hasLegacyReviewReceipt(entry.Receipts) {
		t.Fatal("durable legacy receipt was not recognized")
	}

	if err := os.WriteFile(mediaPath, []byte("replaced bytes with a new snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.validateLegacyReviewSnapshot(ctx, payload, entry.Fingerprint, selection); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("legacy receipt allowed stale file generation after cache reload: %v", err)
	}
}
