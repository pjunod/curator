package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestSelectedDeliveryDeleteRefusesReceiptThatBecameImporting(t *testing.T) {
	ctx := context.Background()
	svc, db, receipt, publication := reviewedDeliveryDeleteFixture(t, map[string][]byte{
		"keep.txt": []byte("unselected sibling"),
	})
	entry := completedEntryForPath(t, svc, publication)
	fingerprint := entry.Fingerprint
	selectedPath := filepath.Join(publication, epRelease+".mkv")
	selection := completedSelectionForPath(t, entry, selectedPath)

	// The UI inventory was reviewed while the delivery was idle. Import starts
	// before the destructive request reaches its final admission check.
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='importing' WHERE id=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, []CompletedFileSelection{selection}); err == nil {
		t.Fatal("selected deletion proceeded after the receipt entered importing")
	}
	if got, err := os.ReadFile(selectedPath); err != nil || len(got) == 0 {
		t.Fatalf("active import's selected source changed: bytes=%d err=%v", len(got), err)
	}
	if got, err := os.ReadFile(filepath.Join(publication, "keep.txt")); err != nil || string(got) != "unselected sibling" {
		t.Fatalf("active import's unselected sibling changed: %q err=%v", got, err)
	}
	var batches int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_explicit_delete_batches WHERE receipt_id=?`, receipt.ID).Scan(&batches); err != nil || batches != 0 {
		t.Fatalf("refused active request created deletion intent: count=%d err=%v", batches, err)
	}
}

func TestLegacySelectedDeletePreservesPayloadOwnedByActiveDownload(t *testing.T) {
	svc, db, itemID, root := completedFixture(t)
	ctx := context.Background()
	payload := filepath.Join(root, "active-run", "release")
	selectedPath := filepath.Join(payload, "selected.mkv")
	siblingPath := filepath.Join(payload, "sibling.mkv")
	writeCompleted(t, selectedPath, 31)
	writeCompleted(t, siblingPath, 37)
	dl := insertImportable(t, db, itemID, payload)
	aliasParent := filepath.Join(t.TempDir(), "download-root-alias")
	if err := os.Symlink(filepath.Dir(payload), aliasParent); err != nil {
		t.Fatalf("create explicit storage alias: %v", err)
	}
	aliasPayload := filepath.Join(aliasParent, filepath.Base(payload))
	dl.State, dl.ImportPath, dl.SavePath = "downloading", aliasPayload, aliasPayload
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	reservations, err := db.ListDownloadReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var reservationFound bool
	for _, reservation := range reservations {
		if reservation.ID == dl.ID && reservation.State == "downloading" && reservation.ImportPath == aliasPayload {
			reservationFound = true
		}
	}
	if !reservationFound {
		t.Fatalf("active payload reservation was not persisted: %+v", reservations)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES(?,?)`, dl.GenerationKey, payload); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	selection := completedSelectionForPath(t, &entry, selectedPath)
	reservations, err = db.ListDownloadReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reservations) == 0 {
		t.Fatal("completed inventory removed the active download reservation")
	}

	if err := svc.DeleteSelectedLegacyFiles(ctx, entry.Path, entry.Fingerprint, []CompletedFileSelection{selection}); err == nil {
		t.Fatalf("selected legacy deletion proceeded while a live download reserved payload=%q entry=%q reservations=%+v", payload, entry.Path, reservations)
	}
	for _, path := range []string{selectedPath, siblingPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("active download payload member %q was changed: %v", path, err)
		}
	}

	// An active job elsewhere must not block the next check, so the pending
	// placement below independently exercises alias-safe placement fencing.
	unrelated := filepath.Join(t.TempDir(), "another-release")
	dl.ImportPath, dl.SavePath = unrelated, unrelated
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}

	// An unresolved placement's source is another ownership fence. Store the
	// source through the explicit alias and ensure it protects these exact
	// payload bytes too.
	target := filepath.Join(t.TempDir(), "library", "target.mkv")
	placement := sqlite.Placement{
		ID: "legacy-delete-pending-placement", Source: aliasPayload, Target: target,
		Temporary: target + ".stage", Backup: target + ".prior", State: "prepared",
		ItemID: itemID, CopyID: 0, EpisodeIDs: []int64{1}, Release: "pending placement",
	}
	if err := db.PreparePlacement(ctx, placement); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedLegacyFiles(ctx, entry.Path, entry.Fingerprint, []CompletedFileSelection{selection}); err == nil {
		t.Fatal("selected deletion proceeded while a prepared placement retained the aliased payload")
	}
	for _, path := range []string{selectedPath, siblingPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("pending placement changed payload member %q: %v", path, err)
		}
	}
	if _, err := db.W.ExecContext(ctx, `DELETE FROM import_placements WHERE id=?`, placement.ID); err != nil {
		t.Fatal(err)
	}

	// With the unrelated active job and placement both outside this payload,
	// the exact unchanged review selection can proceed.
	if err := svc.DeleteSelectedLegacyFiles(ctx, entry.Path, entry.Fingerprint, []CompletedFileSelection{selection}); err != nil {
		t.Fatalf("unrelated active download blocked selected legacy deletion: %v", err)
	}
	if _, err := os.Stat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("reviewed selected file remains after unrelated reservation moved away: %v", err)
	}
	if _, err := os.Stat(siblingPath); err != nil {
		t.Fatalf("unselected sibling changed on retry: %v", err)
	}
}

func TestDeliveryFileCleanupRejectsSamePathPublicationReplacement(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted delivery: %v", err)
	}
	receipt, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	row := mustDeliveryOutcome(t, db, receiptID, epRelease+".mkv")
	current := filepath.Join(publication, filepath.FromSlash(row.Path))
	contents, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}

	// Replacing the directory at the same lexical path must not inherit the
	// accepted publication's cleanup authority, even if its media has identical
	// bytes and the independently committed library copy remains valid.
	held := publication + ".held"
	if err := os.Rename(publication, held); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(publication, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(current, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.removeVerifiedDeliveryFile(ctx, receipt, row); err == nil {
		t.Fatal("cleanup accepted a same-path replacement publication directory")
	}
	if got, err := os.ReadFile(current); err != nil || string(got) != string(contents) {
		t.Fatalf("replacement publication bytes were removed or changed: err=%v", err)
	}
	if got, err := os.ReadFile(filepath.Join(held, filepath.FromSlash(row.Path))); err != nil || string(got) != string(contents) {
		t.Fatalf("original publication bytes changed: err=%v", err)
	}
	files, err := db.ListFilesForItem(ctx, dl.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("committed library copy was changed: files=%+v err=%v", files, err)
	}
	if _, err := os.Stat(files[0].Path); err != nil {
		t.Fatalf("committed library bytes disappeared: %v", err)
	}
}
