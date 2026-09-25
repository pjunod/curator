package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/pjunod/monarr/internal/domain"
)

// A library that already has two items on one folder — two different films
// called Leviticus (2022) — is repaired on upgrade: the item that owns the
// files keeps the folder, the other is pointed at its own, and nothing is
// deleted. Then the index refuses the next attempt.
func TestMigration0032GivesEachSharedItemItsOwnFolder(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db.W, "migrations", 31); err != nil {
		t.Fatal(err)
	}

	const shared = "/movies/Leviticus (2022)"
	for _, row := range []struct {
		title string
		tmdb  int64
		imdb  string
		path  string
	}{
		{"Leviticus", 1001, "", shared},                  // id 1: added first, no files
		{"Leviticus", 1002, "", shared},                  // id 2: owns the files
		{"Manual Twin", 0, "", shared + "/"},             // id 3: same folder, uncleaned spelling, no ids
		{"Lonely", 3003, "", "/movies/Lonely (2001)"},    // id 4: untouched
		{"Unplaced", 4004, "", ""},                       // id 5: no folder at all
		{"Unplaced 2", 5005, "", ""},                     // id 6: '' is not a claim
		{"Slashed", 6006, "", "/movies/Slashed (2003)/"}, // id 7: alone, stored uncleaned
	} {
		if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items
			(kind,title,sort_title,tmdb_id,imdb_id,path,added_at,updated_at)
			VALUES ('movie',?,?,?,?,?,0,0)`, row.title, row.title, row.tmdb, row.imdb, row.path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files (media_item_id,path,size,added_at)
		VALUES (2, ?, 10, 0)`, shared+"/Leviticus (2022).mkv"); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	want := map[int64]string{
		1: shared + " {tmdb-1001}",
		2: shared,
		3: shared + " {monarr-3}",
		4: "/movies/Lonely (2001)",
		5: "",
		6: "",
		7: "/movies/Slashed (2003)",
	}
	for id, path := range want {
		var got string
		if err := db.R.QueryRowContext(ctx, `SELECT path FROM media_items WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != path {
			t.Errorf("item %d path = %q, want %q", id, got, path)
		}
	}
	var files int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE media_item_id = 2`).Scan(&files); err != nil || files != 1 {
		t.Errorf("files on the keeper = %d, %v; the migration must not touch media_files", files, err)
	}

	// The index is the guarantee from here on.
	_, err = db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Intruder", IDs: domain.ExternalIDs{TMDB: 9999}, Path: shared,
	})
	if !errors.Is(err, ErrFolderTaken) {
		t.Fatalf("insert onto a held folder: err = %v, want ErrFolderTaken", err)
	}
	item, err := db.GetMediaItemFull(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	item.Path = shared
	if err := db.UpdateMediaItemPlacement(ctx, item); !errors.Is(err, ErrFolderTaken) {
		t.Fatalf("move onto a held folder: err = %v, want ErrFolderTaken", err)
	}
	// Cleaning is what lets the byte-comparing index see the clean spelling
	// of a folder that was stored with a trailing slash.
	_, err = db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Other", IDs: domain.ExternalIDs{TMDB: 8888}, Path: "/movies/Slashed (2003)",
	})
	if !errors.Is(err, ErrFolderTaken) {
		t.Fatalf("insert onto a formerly-uncleaned folder: err = %v, want ErrFolderTaken", err)
	}
	// A provider-id duplicate is still reported as a duplicate, not a folder
	// clash: the two sentinels mean different things to the caller.
	_, err = db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Lonely", IDs: domain.ExternalIDs{TMDB: 3003}, Path: "/movies/elsewhere",
	})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate tmdb id: err = %v, want ErrDuplicate", err)
	}
}
