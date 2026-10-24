package sqlite

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestLegacyCleanupReviewMigrationCoversRowsAndOrphanReceipts(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, db.W, "migrations", 39); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id, kind, title, sort_title, quality_profile_id, added_at, updated_at)
		VALUES(1, 'movie', 'Test', 'test', 1, 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads
		(id, media_item_id, release_title, client_id, protocol, handle, transfer, state, import_path, added_at, updated_at)
		VALUES(71, 1, 'Old completed', 8, 'usenet', 'old-handle', 'old-transfer', 'imported', '/completed/old', 1000, 1000),
		       (72, 1, 'Old failed', 8, 'usenet', 'failed-handle', '', 'failed', '', 1001, 1001)`); err != nil {
		t.Fatal(err)
	}
	// The receipt remains after its queue row is gone and must also enter the
	// cohort, independently of Activity retention.
	if _, err := db.W.ExecContext(ctx, `DELETE FROM downloads WHERE id=71`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var cohortRows int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM legacy_cleanup_review`).Scan(&cohortRows); err != nil {
		t.Fatal(err)
	}
	if cohortRows != 2 {
		t.Fatalf("legacy cohort rows: got %d, want 2", cohortRows)
	}
	receipts, err := db.CompletedReceipts(ctx)
	if err != nil || len(receipts) != 1 || !receipts[0].LegacyReview || receipts[0].Live {
		t.Fatalf("orphan receipt review custody: %+v err=%v", receipts, err)
	}

	// Reusing the old integer ID with a different generation snapshot does not
	// inherit its review hold, even when the old timestamp and title collide.
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads
		(id, media_item_id, release_title, client_id, protocol, handle, transfer, state, import_path, added_at, updated_at)
		VALUES(71, 1, 'Old completed', 8, 'usenet', 'new-handle', 'new-transfer', 'imported', '/completed/new', 1000, 2000)`); err != nil {
		t.Fatal(err)
	}
	newDownload, err := db.GetDownload(ctx, 71)
	if err != nil {
		t.Fatal(err)
	}
	var oldGeneration string
	if err := db.R.QueryRowContext(ctx, `SELECT generation_key FROM legacy_cleanup_review WHERE path='/completed/old'`).Scan(&oldGeneration); err != nil {
		t.Fatal(err)
	}
	if newDownload.GenerationKey == "" || newDownload.GenerationKey == oldGeneration {
		t.Fatalf("reused ID did not receive fresh generation: new=%q old=%q", newDownload.GenerationKey, oldGeneration)
	}
	var receiptGeneration string
	if err := db.R.QueryRowContext(ctx, `SELECT generation_key FROM completed_receipts WHERE download_id=71 AND path='/completed/new'`).Scan(&receiptGeneration); err != nil || receiptGeneration == "" || receiptGeneration != newDownload.GenerationKey {
		t.Fatalf("raw insert receipt generation: got %q, download generation %q, err=%v", receiptGeneration, newDownload.GenerationKey, err)
	}
	review, err := db.LegacyCleanupReview(ctx, newDownload.GenerationKey, newDownload.ImportPath)
	if err != nil || review {
		t.Fatalf("reused ID inherited review: review=%v err=%v", review, err)
	}
	// Reusing ID, timestamp, title, and even the old path must add a second
	// receipt generation rather than replacing the orphaned receipt row.
	if _, err := db.W.ExecContext(ctx, `DELETE FROM downloads WHERE id=71`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads
		(id, media_item_id, release_title, client_id, protocol, handle, transfer, state, import_path, added_at, updated_at)
		VALUES(71, 1, 'Old completed', 8, 'usenet', 'third-handle', 'third-transfer', 'imported', '/completed/old', 1000, 3000)`); err != nil {
		t.Fatal(err)
	}
	var samePathReceipts int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM completed_receipts WHERE download_id=71 AND added_at=1000 AND path='/completed/old'`).Scan(&samePathReceipts); err != nil || samePathReceipts != 2 {
		t.Fatalf("same-path receipt generations collapsed: rows=%d err=%v", samePathReceipts, err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET activity_dismissed=1 WHERE id=72`); err != nil {
		t.Fatal(err)
	}
	var failedGeneration string
	if err := db.R.QueryRowContext(ctx, `SELECT generation_key FROM downloads WHERE id=72`).Scan(&failedGeneration); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM legacy_cleanup_review WHERE generation_key=?`, failedGeneration).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("Activity dismissal erased legacy custody: rows=%d err=%v", retained, err)
	}
}

func TestCompletedMigrationPreservesPreexistingPathsAndDeletedHistory(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, db.W, "migrations", 29); err != nil {
		t.Fatal(err)
	}
	_, err = db.W.ExecContext(ctx, `INSERT INTO media_items(id, kind, title, sort_title, quality_profile_id, added_at, updated_at)
		VALUES(1, 'movie', 'Test', 'test', 1, 0, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.W.ExecContext(ctx, `INSERT INTO downloads(id, media_item_id, release_title, protocol, state, import_path, added_at, updated_at)
		VALUES(1, 1, 'Old release', 'usenet', 'imported', '/completed/old', 100, 100)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := db.CompletedReceipts(ctx)
	if err != nil || len(rows) != 1 || rows[0].Path != "/completed/old" || !rows[0].Live {
		t.Fatalf("backfill: %+v, %v", rows, err)
	}
	// A row with pending cleanup now survives dismissal. Confirm removal to
	// exercise ID reuse after the row can actually be retired.
	if err := db.MarkPayloadRemoved(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClearImportedDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	// SQLite can reuse queue IDs after clearing. A new row at the same path
	// must not turn the old historical receipt into a live ownership claim.
	_, err = db.W.ExecContext(ctx, `INSERT INTO downloads(id, media_item_id, release_title, protocol, state, import_path, added_at, updated_at)
		VALUES(1, 1, 'New release', 'usenet', 'downloading', '/completed/old', 200, 200)`)
	if err != nil {
		t.Fatal(err)
	}
	rows, err = db.CompletedReceipts(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("receipts: %+v, %v", rows, err)
	}
	for _, r := range rows {
		if r.Title == "Old release" && r.Live {
			t.Fatal("reused ID resurrected ownership")
		}
		if r.Title == "New release" && !r.Live {
			t.Fatal("missing new ownership")
		}
	}
	if err := db.UpdateDownloadState(ctx, 1, "failed", 0, "failure"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClearFailedDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err = db.CompletedReceipts(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("clearing erased receipts: %+v, %v", rows, err)
	}
}
