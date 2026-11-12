package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"
)

// Migration 0078 (pipeline contract v2, M6b): nothing writes the v1_record
// source any more, and nothing is refused. A database at goose 77 — what
// v0.45.x leaves, with v1_record stamped on live publications — migrates; a
// live (imported) v1_record row keeps its stored manifest so it drains
// through its cleanup, and only a removed one is normalised.
func TestMigration0078MigratesLiveV1RecordRowsAndNormalisesOnlyRemoved(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db.W, "migrations", 77); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	manifest := `{"generation":"g","files":[{"path":"a.mkv","bytes":1,"sha256":"d"}],"digest":"x"}`
	// The live row's directory is on disk with its .runner-manifest.json.
	live := filepath.Join(t.TempDir(), "live-v1")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, ".runner-manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, row := range map[string][2]string{
		live:                    {"v1_record", "imported"},
		"/completed/removed-v1": {"v1_record", "removed"},
		"/completed/file":       {"file", "imported"},
		"/completed/frozen":     {"none", "import_failed"},
	} {
		id, err := db.InsertPublication(ctx, PublicationWrite{Generation: "g", Kind: "completed", Path: path, ManifestJSON: manifest, Source: row[0], Cohort: "live"})
		if err != nil {
			t.Fatal(err)
		}
		exec(`UPDATE publications SET state=? WHERE id=?`, row[1], id)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("Migrate at goose 77 with a live v1_record publication = %v, want it to run", err)
	}
	source := func(path string) (string, string) {
		t.Helper()
		var src, body string
		if err := db.R.QueryRowContext(ctx, `SELECT manifest_source, manifest_json FROM publications WHERE path=?`, path).Scan(&src, &body); err != nil {
			t.Fatal(err)
		}
		return src, body
	}
	for path, want := range map[string]string{
		live: "v1_record", "/completed/file": "file", "/completed/frozen": "none", "/completed/removed-v1": "none",
	} {
		src, body := source(path)
		if src != want {
			t.Errorf("%s source = %s, want %s", path, src, want)
		}
		if path == "/completed/removed-v1" && body != "{}" {
			t.Errorf("removed row kept its manifest body %q", body)
		}
		if path == live && body != manifest {
			t.Errorf("live v1_record row lost its stored manifest: %q", body)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(live, ".runner-manifest.json")); err != nil || string(raw) != manifest {
		t.Fatalf("the live publication's manifest file changed: %q (%v)", raw, err)
	}
	var state string
	if err := db.R.QueryRowContext(ctx, `SELECT state FROM publications WHERE path=?`, live).Scan(&state); err != nil || state != "imported" {
		t.Fatalf("live row state = %q (%v), want untouched", state, err)
	}
	var version int64
	if err := db.R.QueryRowContext(ctx, `SELECT MAX(version_id) FROM goose_db_version`).Scan(&version); err != nil || version < retireV1RecordVersion {
		t.Fatalf("goose version = %d (%v), want 0078 applied", version, err)
	}
}
