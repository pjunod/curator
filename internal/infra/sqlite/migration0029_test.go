package sqlite

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

// The edition identity backfill runs against the real pre-0029 schema. A
// profile was the only durable indication older databases had, so migration
// 0029 must translate it once and preserve that answer thereafter.
func TestBookEditionMigrationBackfillsExistingBooks(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db.W, "migrations", 28); err != nil {
		t.Fatal(err)
	}

	for _, row := range []struct {
		title     string
		profileID int64
	}{
		{title: "Written", profileID: 4},
		{title: "Narrated", profileID: 5},
	} {
		if _, err := db.W.ExecContext(ctx,
			`INSERT INTO media_items
			 (kind, title, sort_title, quality_profile_id, added_at, updated_at)
			 VALUES ('book', ?, ?, ?, 0, 0)`, row.title, row.title, row.profileID); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	assertColumn(t, ctx, db, "media_items", "book_type")
	assertColumn(t, ctx, db, "media_copies", "book_type")

	rows, err := db.R.QueryContext(ctx, `SELECT title, book_type FROM media_items ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var title, bookType string
		if err := rows.Scan(&title, &bookType); err != nil {
			t.Fatal(err)
		}
		got[title] = bookType
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got["Written"] != "ebook" || got["Narrated"] != "audiobook" {
		t.Fatalf("edition backfill = %#v, want Written=ebook and Narrated=audiobook", got)
	}
}
