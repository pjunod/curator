package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// Migration 0078 (pipeline contract v2, M6b): the v1_record manifest source is
// retired. The count check refuses while a publication that is not removed
// still depends on it, naming it; once they are all removed their source is
// normalised and nothing else moves.
func TestMigration0078RefusesWhileAPublicationStillNeedsTheV1Record(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db.W, "migrations", 74); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	source := func(path string) (string, string) {
		t.Helper()
		var src, manifest string
		if err := db.R.QueryRowContext(ctx, `SELECT manifest_source, manifest_json FROM publications WHERE path=?`, path).Scan(&src, &manifest); err != nil {
			t.Fatal(err)
		}
		return src, manifest
	}
	for path, row := range map[string][2]string{
		"/completed/live-v1":    {"v1_record", "imported"},
		"/completed/removed-v1": {"v1_record", "removed"},
		"/completed/file":       {"file", "imported"},
		"/completed/frozen":     {"none", "import_failed"},
	} {
		id, err := db.InsertPublication(ctx, PublicationWrite{Generation: "g", Kind: "completed", Path: path, ManifestJSON: `{"generation":"g"}`, Source: row[0], Cohort: "historical"})
		if err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE publications SET state=? WHERE id=?`, row[1], id)
	}

	err = db.Migrate(ctx)
	if !errors.Is(err, ErrLiveV1Records) || !strings.Contains(err.Error(), "/completed/live-v1") || strings.Contains(err.Error(), "/completed/removed-v1") {
		t.Fatalf("Migrate = %v, want a refusal naming only the live v1_record publication", err)
	}
	if src, _ := source("/completed/removed-v1"); src != "v1_record" {
		t.Fatal("the refused migration changed a row")
	}

	// The publication is removed (its import and review finished): now it may go.
	exec(`UPDATE publications SET state='removed' WHERE path='/completed/live-v1'`)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/completed/live-v1", "/completed/removed-v1"} {
		if src, manifest := source(path); src != "none" || manifest != "{}" {
			t.Fatalf("%s = %s %s, want the source normalised", path, src, manifest)
		}
	}
	if src, manifest := source("/completed/file"); src != "file" || manifest != `{"generation":"g"}` {
		t.Fatalf("a file-manifest publication changed: %s %s", src, manifest)
	}
	if src, _ := source("/completed/frozen"); src != "none" {
		t.Fatalf("a manifest-less publication changed: %s", src)
	}
}

// A database older than 0072 gets its v1_record publications from 0072 itself
// (one per old receipt); the upgrade stops once they exist rather than
// stranding them.
func TestMigration0078RefusesAnUpgradeThatCrossesTheReceiptMigration(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db.W, "migrations", 71); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO runner_delivery_receipts(id,client_id,delivery_id,sequence,publication_generation,manifest_digest,artifact_id,job_id,ready_at,publication_path,files_json,created_at,updated_at)
		VALUES(1,1,'d-1',1,'g','dig','a','1',1,'/completed/pub','[]',1,1)`); err != nil {
		t.Fatal(err)
	}
	err = db.Migrate(ctx)
	if !errors.Is(err, ErrLiveV1Records) || !strings.Contains(err.Error(), "/completed/pub") {
		t.Fatalf("Migrate = %v, want a refusal naming the migrated receipt's publication", err)
	}
}
