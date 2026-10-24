package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDeliveryCleanupSettlesOwnedFileAndKeepsUnrelatedPayloadReviewable(t *testing.T) {
	ctx := context.Background()
	const unrelatedName = "release-notes.txt"
	const unrelatedBytes = "operator-provided notes must remain available"
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, &deliveryImportClient{}, map[string][]byte{
		unrelatedName: []byte(unrelatedBytes),
	})
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted media: %v", err)
	}
	receipt, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || receipt.State != "imported" {
		t.Fatalf("accepted receipt=%+v err=%v", receipt, err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, receipt); err != nil {
		t.Fatalf("first cleanup sweep: %v", err)
	}
	mediaPath := filepath.Join(publication, epRelease+".mkv")
	notesPath := filepath.Join(publication, unrelatedName)
	if _, err := os.Stat(mediaPath); !os.IsNotExist(err) {
		t.Fatalf("authorized media cleanup did not settle: %v", err)
	}
	if got, err := os.ReadFile(notesPath); err != nil || string(got) != unrelatedBytes {
		t.Fatalf("unrelated payload was changed during media cleanup: %q err=%v", got, err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, epRelease+".mkv")
	if err != nil || row.CleanupState != "removed" {
		t.Fatalf("owned media cleanup ledger=%+v err=%v", row, err)
	}
	var directoryState string
	if err := db.R.QueryRowContext(ctx, `SELECT directory_cleanup_state FROM runner_delivery_receipts WHERE id=?`, receiptID).Scan(&directoryState); err != nil {
		t.Fatal(err)
	}
	if directoryState != "protected" {
		t.Fatalf("receipt did not retain the blocked directory obligation: state=%q", directoryState)
	}

	if err := os.Remove(notesPath); err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, receipt); err != nil {
		t.Fatalf("retry directory cleanup after unrelated data was removed: %v", err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT directory_cleanup_state FROM runner_delivery_receipts WHERE id=?`, receiptID).Scan(&directoryState); err != nil || directoryState != "removed" {
		t.Fatalf("directory cleanup did not settle after the external obstruction was removed: state=%q err=%v", directoryState, err)
	}
	if _, err := os.Lstat(publication); !os.IsNotExist(err) {
		t.Fatalf("accepted empty publication directory remained after retry: %v", err)
	}
	files, err := db.ListFilesForItem(ctx, dl.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("library placement was changed by payload cleanup: files=%+v err=%v", files, err)
	}
	if _, err := os.Stat(files[0].Path); err != nil {
		t.Fatalf("library media was removed while settling the delivery directory: %v", err)
	}
}
