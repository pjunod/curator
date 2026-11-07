package sqlite

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

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
