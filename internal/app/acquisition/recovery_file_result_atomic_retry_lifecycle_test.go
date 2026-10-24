package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveryPerFileReceiptFailureKeepsPlacementPreparedForExactRetry(t *testing.T) {
	svc, db, request, receipts := recoveryFixture(t)
	ctx := context.Background()
	preview, err := svc.PreviewRecovery(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	markRecoveryImportingForTest(t, db, job.ID)
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_recovery_file_receipt BEFORE INSERT ON recovery_file_results
		BEGIN SELECT RAISE(ABORT,'injected per-file receipt outage'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.runRecovery(ctx, job); err == nil || !strings.Contains(err.Error(), "injected per-file receipt outage") {
		t.Fatalf("failed per-file receipt write error=%v, want injected transaction failure", err)
	}
	files, err := db.ListFilesForItem(ctx, request.MediaItemID)
	if err != nil || len(files) != 0 {
		t.Fatalf("failed receipt transaction left library metadata: files=%+v err=%v", files, err)
	}
	pending, err := db.PendingPlacements(ctx)
	if err != nil || len(pending) != 1 || pending[0].RecoveryImport != job.ID || pending[0].State != "prepared" {
		t.Fatalf("failed receipt transaction lost its exact prepared placement: placements=%+v err=%v", pending, err)
	}
	if _, err := os.Stat(pending[0].Target); err != nil {
		t.Fatalf("fixture did not reach the published-but-uncommitted crash boundary: %v", err)
	}
	var resultRows, outboxRows int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_file_results WHERE import_id=?`, job.ID).Scan(&resultRows); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_receipt_outbox WHERE import_id=?`, job.ID).Scan(&outboxRows); err != nil {
		t.Fatal(err)
	}
	if resultRows != 0 || outboxRows != 0 || receipts.Load() != 0 {
		t.Fatalf("Runner or durable receipt advanced before atomic file commit: fileResults=%d outbox=%d remoteReceipts=%d", resultRows, outboxRows, receipts.Load())
	}
	sourceRoot, err := svc.recoveryPayload(ctx, job.Recovery)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sourceRoot, job.Recovery.Files[0].Path)); err != nil {
		t.Fatalf("failed metadata commit discarded the recovery source: %v", err)
	}

	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_recovery_file_receipt`); err != nil {
		t.Fatal(err)
	}
	if err := svc.runRecovery(ctx, job); err != nil {
		t.Fatalf("exact recovery retry did not reconcile the published placement: %v", err)
	}
	files, err = db.ListFilesForItem(ctx, request.MediaItemID)
	if err != nil || len(files) != 1 || files[0].Path != pending[0].Target {
		t.Fatalf("recovery retry did not commit the exact target: files=%+v err=%v", files, err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_file_results WHERE import_id=?`, job.ID).Scan(&resultRows); err != nil || resultRows != 1 {
		t.Fatalf("recovery retry per-file results=%d err=%v, want one", resultRows, err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_receipt_outbox WHERE import_id=? AND delivered=0`, job.ID).Scan(&outboxRows); err != nil || outboxRows != 1 {
		t.Fatalf("recovery retry outbox pending=%d err=%v, want one", outboxRows, err)
	}
	if receipts.Load() != 0 {
		t.Fatalf("runRecovery sent Runner receipt before the outbox worker: %d", receipts.Load())
	}
	svc.deliverRecoveryReceipts(ctx)
	current, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil || current.State != "imported" || receipts.Load() != 1 {
		t.Fatalf("recovered durable receipt did not settle once: import=%+v remoteReceipts=%d err=%v", current, receipts.Load(), err)
	}
}
