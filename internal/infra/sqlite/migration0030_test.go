package sqlite

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

// Release identity upgrades the real prior schema with deliberately
// duplicated non-unique IDs, existing downloads, manual rows, and pending
// jobs. Migration must preserve every row while making ambiguity inspectable.
func TestReleaseIdentityMigrationPreservesLegacyState(t *testing.T) {
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

	for _, row := range []struct {
		title, source string
		tmdb, tvdb    int64
		imdb          string
	}{
		{"First", "", 0, 453187, "tt33096993"},
		{"Duplicate", "", 0, 453187, "tt33096993"},
		{"Manual", "manual", 0, 0, ""},
	} {
		if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items
			(kind,title,sort_title,source,tmdb_id,tvdb_id,imdb_id,added_at,updated_at)
			VALUES ('series',?,?,?,?,?,?,0,0)`, row.title, row.title, row.source, row.tmdb, row.tvdb, row.imdb); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads
		(media_item_id,release_title,indexer,state,protocol,quality,added_at,updated_at)
		VALUES (1,'Release','indexer','grabbed','torrent','1080p',0,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO jobs
		(kind,payload,state,priority,run_after,attempts,max_attempts,lease_owner,lease_expires_at,created_at,updated_at,finished_at)
		VALUES ('library.scan','{}','queued',100,0,0,3,'',0,0,0,0)`); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, check := range [][2]string{{"downloads", "match_evidence"}, {"media_aliases", "normalized_title"}, {"media_identity_sources", "countries"}} {
		assertColumn(t, ctx, db, check[0], check[1])
	}
	var count int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_items WHERE tvdb_id=453187`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("duplicate TVDB rows = %d, err %v", count, err)
	}
	var source string
	if err := db.R.QueryRowContext(ctx, `SELECT source FROM media_items WHERE title='First'`).Scan(&source); err != nil || source != "tvmaze" {
		t.Fatalf("source backfill = %q, err %v", source, err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT source FROM media_items WHERE title='Manual'`).Scan(&source); err != nil || source != "manual" {
		t.Fatalf("manual source = %q, err %v", source, err)
	}
	var evidence string
	if err := db.R.QueryRowContext(ctx, `SELECT match_evidence FROM downloads`).Scan(&evidence); err != nil || evidence != "{}" {
		t.Fatalf("download evidence = %q, err %v", evidence, err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE state='queued'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pending jobs = %d, err %v", count, err)
	}
}
