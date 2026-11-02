package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestDeliveryPlacementLedgerFailureRetriesAfterRestartThroughCleanup(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_import_provenance BEFORE INSERT ON runner_delivery_file_imports BEGIN SELECT RAISE(ABORT,'injected provenance boundary failure'); END`); err != nil {
		t.Fatal(err)
	}
	err := svc.runDeliveryImport(ctx, dl, cfg, receiptID)
	if err == nil || !strings.Contains(err.Error(), "injected provenance boundary failure") {
		t.Fatalf("did not reach actual placement ledger failure: %v", err)
	}
	source := filepath.Join(publication, epRelease+".mkv")
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_import_provenance`); err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, svc.bus, svc.log, svc.newIndexer, svc.newClient)
	fresh, err := reopened.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.runDeliveryImport(ctx, fresh, cfg, receiptID); err != nil {
		t.Fatalf("restart import: %v", err)
	}
	receipt, err := reopened.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.cleanupDeliveryFiles(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	outcomes, err := reopened.DeliveryFileOutcomes(ctx, receiptID)
	if err != nil || len(outcomes) != 1 || outcomes[0].CleanupState != "removed" {
		t.Fatalf("retry cleanup=%+v err=%v", outcomes, err)
	}
	files, err := reopened.ListFilesForItem(ctx, dl.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("library files=%+v err=%v", files, err)
	}
	after, err := os.ReadFile(files[0].Path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("library bytes changed: %q %v", after, err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source remains after atomic retry/cleanup: %v", err)
	}
}

func TestHistoricalPlacedRowWithoutProvenanceRepairsAndCleansExactSource(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, dl.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("library=%+v err=%v", files, err)
	}
	before, err := os.ReadFile(files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce an old partial commit: placement/disposition persisted, cleanup
	// and provenance were lost. The original exact placement receipt survives.
	if _, err := db.W.ExecContext(ctx, `DELETE FROM runner_delivery_file_imports WHERE receipt_id=?`, receiptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_state='protected',cleanup_error='' WHERE receipt_id=?`, receiptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='ready' WHERE id=?`, receiptID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET state='downloaded' WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, svc.bus, svc.log, svc.newIndexer, svc.newClient)
	fresh, err := reopened.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.runDeliveryImport(ctx, fresh, cfg, receiptID); err != nil {
		t.Fatal(err)
	}
	receipt, err := reopened.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.cleanupDeliveryFiles(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	rows, err := reopened.DeliveryFileOutcomes(ctx, receiptID)
	if err != nil || len(rows) != 1 || rows[0].CleanupState != "removed" {
		t.Fatalf("historical partial not settled: %+v %v", rows, err)
	}
	after, err := os.ReadFile(files[0].Path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("historical retry republished or lost library bytes: %q %v", after, err)
	}
	if _, err := os.Stat(filepath.Join(publication, epRelease+".mkv")); !os.IsNotExist(err) {
		t.Fatalf("old exact source remains: %v", err)
	}
}
