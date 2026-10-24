package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

func explicitDeleteFixture(t *testing.T) (*DB, context.Context, DeliveryReceipt, ExplicitDeliveryDeleteBatch) {
	t.Helper()
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	receipt, err := db.SaveDiscoveredDelivery(ctx, 81, coverageDelivery("explicit-delete-journal"), "/mounted/release")
	if err != nil {
		t.Fatal(err)
	}
	manifest := receipt.Delivery.Files[0]
	file := DeliveryManifestFile{Path: manifest.Path, SizeBytes: manifest.SizeBytes, SHA256: manifest.SHA256, FileGeneration: "journal-file-generation"}
	if err := db.EnsureExplicitDeliveryFileOutcome(ctx, receipt.ID, file); err != nil {
		t.Fatal(err)
	}
	batch := ExplicitDeliveryDeleteBatch{
		ID: "explicit-delete-batch", ReceiptID: receipt.ID, PayloadPath: "/mounted/release",
		InventoryFingerprint: "inventory-fingerprint", PublicationDirectoryIdentity: "publication-directory",
		StorageIdentity: "storage-identity", GenerationKey: "generation-key", RequestToken: receipt.Delivery.RequestToken,
		PublicationGeneration: receipt.Delivery.PublicationGeneration,
		Files:                 []ExplicitDeliveryDeleteFile{{Path: file.Path, ReviewedPath: filepath.Join("/reviewed/payload", filepath.FromSlash(file.Path)), SnapshotIdentity: "file-stat-snapshot", ParentIdentity: "payload-parent"}},
	}
	return db, ctx, receipt, batch
}

func TestExplicitDeliveryDeleteBatchCannotChangeTupleOrWidenSelection(t *testing.T) {
	db, ctx, receipt, batch := explicitDeleteFixture(t)
	defer func() { _ = db.Close() }()
	if err := db.PrepareExplicitDeliveryDeleteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := db.PrepareExplicitDeliveryDeleteBatch(ctx, batch); err != nil {
		t.Fatalf("exact batch replay failed: %v", err)
	}
	manifest := receipt.Delivery.Files[0]
	if err := db.EnsureExplicitDeliveryFileOutcome(ctx, receipt.ID, DeliveryManifestFile{
		Path: "bonus.mkv", SizeBytes: manifest.SizeBytes, SHA256: manifest.SHA256, FileGeneration: "bonus-file-generation",
	}); err != nil {
		t.Fatalf("prepare valid second manifest path for widening regression: %v", err)
	}

	mutations := []struct {
		name string
		edit func(*ExplicitDeliveryDeleteBatch)
	}{
		{"receipt", func(b *ExplicitDeliveryDeleteBatch) { b.ReceiptID++ }},
		{"payload path", func(b *ExplicitDeliveryDeleteBatch) { b.PayloadPath += "/changed" }},
		{"inventory fingerprint", func(b *ExplicitDeliveryDeleteBatch) { b.InventoryFingerprint += "-changed" }},
		{"publication directory", func(b *ExplicitDeliveryDeleteBatch) { b.PublicationDirectoryIdentity += "-changed" }},
		{"storage identity", func(b *ExplicitDeliveryDeleteBatch) { b.StorageIdentity += "-changed" }},
		{"generation key", func(b *ExplicitDeliveryDeleteBatch) { b.GenerationKey += "-changed" }},
		{"request token", func(b *ExplicitDeliveryDeleteBatch) { b.RequestToken += "-changed" }},
		{"publication generation", func(b *ExplicitDeliveryDeleteBatch) { b.PublicationGeneration += "-changed" }},
		{"reviewed path", func(b *ExplicitDeliveryDeleteBatch) { b.Files[0].ReviewedPath += ".moved" }},
		{"file snapshot", func(b *ExplicitDeliveryDeleteBatch) { b.Files[0].SnapshotIdentity += "-changed" }},
		{"file parent", func(b *ExplicitDeliveryDeleteBatch) { b.Files[0].ParentIdentity += "-changed" }},
		{"widened file membership", func(b *ExplicitDeliveryDeleteBatch) {
			b.Files = append(b.Files, ExplicitDeliveryDeleteFile{Path: "bonus.mkv", ReviewedPath: "/reviewed/payload/bonus.mkv", SnapshotIdentity: "bonus-snapshot", ParentIdentity: "payload-parent"})
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			attempt := batch
			attempt.Files = append([]ExplicitDeliveryDeleteFile(nil), batch.Files...)
			mutation.edit(&attempt)
			if err := db.PrepareExplicitDeliveryDeleteBatch(ctx, attempt); !errors.Is(err, ErrDeliveryReceiptStale) {
				t.Fatalf("changed batch was accepted: %v", err)
			}
		})
	}
	stored, err := db.ExplicitDeliveryDeleteBatch(ctx, batch.ID)
	if err != nil || stored.ReceiptID != receipt.ID || len(stored.Files) != 1 || stored.Files[0].Path != batch.Files[0].Path || stored.Files[0].SnapshotIdentity != batch.Files[0].SnapshotIdentity {
		t.Fatalf("rejected replay changed pinned batch authority: batch=%+v err=%v", stored, err)
	}
}

func TestExplicitDeliveryDeleteCompletionRequiresExactReceiptAndPath(t *testing.T) {
	db, ctx, receipt, batch := explicitDeleteFixture(t)
	defer func() { _ = db.Close() }()
	if err := db.PrepareExplicitDeliveryDeleteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	complete := func(receiptID int64, path string) error {
		return db.CompleteExplicitDeliveryFileDelete(ctx, batch.ID, receiptID, path, "explicit_delivery_file_removed_test", 0, "release", map[string]string{"path": path})
	}
	if err := complete(receipt.ID+1, batch.Files[0].Path); !errors.Is(err, ErrDeliveryReceiptStale) {
		t.Fatalf("completion for another receipt error=%v, want stale receipt", err)
	}
	if err := complete(receipt.ID, "other/path.mkv"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("completion for an unreviewed path error=%v, want no rows", err)
	}
	assertPending := func() {
		t.Helper()
		outcome, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, batch.Files[0].Path)
		if err != nil || outcome.CleanupState != "pending" {
			t.Fatalf("rejected completion changed cleanup state: outcome=%+v err=%v", outcome, err)
		}
		stored, err := db.ExplicitDeliveryDeleteBatch(ctx, batch.ID)
		if err != nil || len(stored.Files) != 1 || stored.Files[0].HistoryRecorded {
			t.Fatalf("rejected completion changed journal: batch=%+v err=%v", stored, err)
		}
	}
	assertPending()
	if err := complete(receipt.ID, batch.Files[0].Path); err != nil {
		t.Fatalf("valid exact completion failed after rejected attempts: %v", err)
	}
}

func TestExplicitDeliveryDeleteHistoryFailureRollsBackAndCanRetry(t *testing.T) {
	db, ctx, receipt, batch := explicitDeleteFixture(t)
	defer func() { _ = db.Close() }()
	if err := db.PrepareExplicitDeliveryDeleteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_explicit_delete_history BEFORE INSERT ON history_events WHEN NEW.type='explicit_delivery_file_removed_retry_test' BEGIN SELECT RAISE(ABORT,'injected history write failure'); END`); err != nil {
		t.Fatal(err)
	}
	complete := func() error {
		return db.CompleteExplicitDeliveryFileDelete(ctx, batch.ID, receipt.ID, batch.Files[0].Path, "explicit_delivery_file_removed_retry_test", 0, "release", map[string]string{"path": batch.Files[0].Path})
	}
	if err := complete(); err == nil || !strings.Contains(err.Error(), "injected history write failure") {
		t.Fatalf("history trigger error=%v, want injected failure", err)
	}
	outcome, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, batch.Files[0].Path)
	if err != nil || outcome.CleanupState != "pending" {
		t.Fatalf("history failure did not roll back removed state: outcome=%+v err=%v", outcome, err)
	}
	stored, err := db.ExplicitDeliveryDeleteBatch(ctx, batch.ID)
	if err != nil || len(stored.Files) != 1 || stored.Files[0].HistoryRecorded || !stored.Files[0].CompletedAt.IsZero() {
		t.Fatalf("history failure did not leave journal pending: batch=%+v err=%v", stored, err)
	}
	var historyCount int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM history_events WHERE type='explicit_delivery_file_removed_retry_test'`).Scan(&historyCount); err != nil || historyCount != 0 {
		t.Fatalf("failed completion left history events: count=%d err=%v", historyCount, err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_explicit_delete_history`); err != nil {
		t.Fatal(err)
	}
	if err := complete(); err != nil {
		t.Fatalf("completion retry after history recovery failed: %v", err)
	}
	outcome, err = db.DeliveryFileOutcomeByPath(ctx, receipt.ID, batch.Files[0].Path)
	if err != nil || outcome.CleanupState != "removed" {
		t.Fatalf("successful retry did not settle file outcome: outcome=%+v err=%v", outcome, err)
	}
	stored, err = db.ExplicitDeliveryDeleteBatch(ctx, batch.ID)
	if err != nil || len(stored.Files) != 1 || !stored.Files[0].HistoryRecorded || stored.Files[0].CompletedAt.IsZero() {
		t.Fatalf("successful retry did not settle exact journal file: batch=%+v err=%v", stored, err)
	}
	if !stored.RetryNextAttemptAt.IsZero() || stored.RetryError != "" {
		t.Fatalf("complete batch retained retry state: %+v", stored)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM history_events WHERE type='explicit_delivery_file_removed_retry_test'`).Scan(&historyCount); err != nil || historyCount != 1 {
		t.Fatalf("successful retry history count=%d err=%v", historyCount, err)
	}
}

func openAtExplicitDeleteMigration(t *testing.T) (*DB, context.Context) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, db.W, "migrations", 61); err != nil {
		t.Fatal(err)
	}
	return db, ctx
}

func TestExplicitDeliveryDeleteJournalMigrationDownRequiresEmptyJournal(t *testing.T) {
	db, ctx := openAtExplicitDeleteMigration(t)
	defer func() { _ = db.Close() }()
	receipt, err := db.SaveDiscoveredDelivery(ctx, 81, coverageDelivery("explicit-delete-migration"), "/mounted/release")
	if err != nil {
		t.Fatal(err)
	}
	manifest := receipt.Delivery.Files[0]
	if err := db.EnsureExplicitDeliveryFileOutcome(ctx, receipt.ID, DeliveryManifestFile{Path: manifest.Path, SizeBytes: manifest.SizeBytes, SHA256: manifest.SHA256, FileGeneration: "migration-file-generation"}); err != nil {
		t.Fatal(err)
	}
	batch := ExplicitDeliveryDeleteBatch{
		ID: "migration-batch", ReceiptID: receipt.ID, PayloadPath: "/mounted/release", InventoryFingerprint: "migration-inventory",
		PublicationDirectoryIdentity: "migration-directory", StorageIdentity: "migration-storage", GenerationKey: "migration-generation-key",
		RequestToken: receipt.Delivery.RequestToken, PublicationGeneration: receipt.Delivery.PublicationGeneration,
		Files: []ExplicitDeliveryDeleteFile{{Path: manifest.Path, ReviewedPath: filepath.Join("/reviewed/payload", filepath.FromSlash(manifest.Path)), SnapshotIdentity: "migration-snapshot", ParentIdentity: "migration-parent"}},
	}
	if err := db.PrepareExplicitDeliveryDeleteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownToContext(ctx, db.W, "migrations", 60); err == nil {
		t.Fatal("migration down discarded an active reviewed deletion journal")
	}
	stored, err := db.ExplicitDeliveryDeleteBatch(ctx, batch.ID)
	if err != nil || stored.ReceiptID != receipt.ID || len(stored.Files) != 1 || stored.Files[0].HistoryRecorded {
		t.Fatalf("refused migration down changed journal data: batch=%+v err=%v", stored, err)
	}
}

func TestExplicitDeliveryDeleteJournalMigrationDownAllowsEmptyJournal(t *testing.T) {
	db, ctx := openAtExplicitDeleteMigration(t)
	defer func() { _ = db.Close() }()
	if err := goose.DownToContext(ctx, db.W, "migrations", 60); err != nil {
		t.Fatalf("empty migration down failed: %v", err)
	}
	var version int64
	if err := db.R.QueryRowContext(ctx, `SELECT version_id FROM goose_db_version WHERE is_applied=1 ORDER BY id DESC LIMIT 1`).Scan(&version); err != nil || version != 60 {
		t.Fatalf("migration version after empty down=%d err=%v, want 60", version, err)
	}
	var tableCount int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('runner_delivery_explicit_delete_batches','runner_delivery_explicit_delete_files')`).Scan(&tableCount); err != nil || tableCount != 0 {
		t.Fatalf("explicit delete tables survived empty migration down: count=%d err=%v", tableCount, err)
	}
}
