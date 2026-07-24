package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// The 0014 backfill is the one part of ADR 0009 that touches data people
// already have, so it is tested against the real pre-0014 schema: migrate up
// to 0013, insert roots and items, then let 0014 run.
func TestRootFolderKindBackfillInfersFromItems(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpToContext(ctx, db.W, "migrations", 13); err != nil {
		t.Fatal(err)
	}

	// Four roots: all-movies, all-series, mixed, and empty.
	for _, path := range []string{"/m", "/s", "/x", "/empty"} {
		if _, err := db.W.ExecContext(ctx,
			`INSERT INTO root_folders (path, added_at) VALUES (?, 0)`, path); err != nil {
			t.Fatal(err)
		}
	}
	add := func(root int64, kind, title string) {
		t.Helper()
		if _, err := db.W.ExecContext(ctx,
			`INSERT INTO media_items (kind, title, sort_title, root_folder_id, added_at, updated_at)
			 VALUES (?, ?, ?, ?, 0, 0)`, kind, title, title, root); err != nil {
			t.Fatal(err)
		}
	}
	add(1, "movie", "Arrival")
	add(1, "movie", "Dune")
	add(2, "series", "Severance")
	add(3, "movie", "Akira")
	add(3, "series", "Cowboy Bebop")

	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		"/m":     "movie",
		"/s":     "series",
		"/x":     "mixed", // spans kinds — guessing here is the mistake
		"/empty": "mixed", // nothing to infer from
	}
	rows, err := db.R.QueryContext(ctx, `SELECT path, kind FROM root_folders`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var path, kind string
		if err := rows.Scan(&path, &kind); err != nil {
			t.Fatal(err)
		}
		got[path] = kind
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for path, wantKind := range want {
		if got[path] != wantKind {
			t.Errorf("root %s: kind = %q, want %q", path, got[path], wantKind)
		}
	}
}
