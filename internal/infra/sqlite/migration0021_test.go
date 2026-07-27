package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// The exact failure this exists for, reproduced.
//
// A second branch adds its own `0020_*.sql`, its build runs first, and
// goose — which knows a migration only by the number in its filename —
// records version 20. `0020_nzbd_native.sql` is then skipped forever, with
// no error anywhere. The binary is correct, the code is correct, and adding
// a download client fails with "table download_clients has no column named
// mode" in the settings UI.
func TestRepairsASchemaGooseWronglyBelievesIsAt20(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db.W, "migrations", 19); err != nil {
		t.Fatal(err)
	}
	// A client configured before any of this, which must survive the rebuild.
	if _, err := db.W.ExecContext(ctx,
		`INSERT INTO download_clients (id, type, name, url, category, added_at)
		 VALUES (1, 'sabnzbd', 'sab', 'http://sab:8080', 'monarr', 7)`); err != nil {
		t.Fatal(err)
	}
	// Another branch's 0020, recorded. goose stores the number, not the name,
	// so this is indistinguishable from ours having run.
	if _, err := db.W.ExecContext(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied, tstamp)
		 VALUES (20, 1, CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	assertColumn(t, ctx, db, "downloads", "transfer")
	assertColumn(t, ctx, db, "download_clients", "mode")
	ddl, err := tableDDL(ctx, db.R, "download_clients")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "'nzbd'") {
		t.Errorf("download_clients still rejects type 'nzbd':\n%s", ddl)
	}

	// The pre-existing client is intact, with the default mode.
	var name, mode string
	if err := db.R.QueryRowContext(ctx,
		`SELECT name, mode FROM download_clients WHERE id = 1`).Scan(&name, &mode); err != nil {
		t.Fatal(err)
	}
	if name != "sab" || mode != "poll" {
		t.Errorf("client not carried across the rebuild: name=%q mode=%q", name, mode)
	}

	// And the thing that was failing now works.
	if _, err := db.W.ExecContext(ctx,
		`INSERT INTO download_clients (type, name, url, category, added_at, mode)
		 VALUES ('nzbd', 'nzbd', 'http://host.docker.internal:6789', 'monarr', 8, 'push')`); err != nil {
		t.Fatalf("adding a native nzbd client still fails: %v", err)
	}
}

// On a database where 0020 did apply, the repair must touch nothing — and
// must not quietly reset a client an operator switched to push.
func TestRepairIsANoOpOnACorrectlyMigratedDatabase(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx,
		`INSERT INTO download_clients (id, type, name, url, category, added_at, mode)
		 VALUES (1, 'nzbd', 'nzbd', 'http://nzbd:6789', 'monarr', 7, 'push')`); err != nil {
		t.Fatal(err)
	}
	before, err := tableDDL(ctx, db.R, "download_clients")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	after, err := tableDDL(ctx, db.R, "download_clients")
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Errorf("the table was rebuilt when it did not need to be:\nbefore %s\nafter  %s", before, after)
	}
	var mode string
	if err := db.R.QueryRowContext(ctx,
		`SELECT mode FROM download_clients WHERE id = 1`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "push" {
		t.Errorf("mode reset to %q — a repair must not undo an operator's choice", mode)
	}
}

// The tripwire: a database that lies about its version and that migration 21
// cannot reach must stop the server at startup, not surface as a SQL error in
// the UI hours later.
func TestStartupRefusesADatabaseMissingAColumnTheCodeWritesTo(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Simulate the next collision: a column the query layer needs, gone,
	// with the version table insisting everything is applied.
	if _, err := db.W.ExecContext(ctx,
		`ALTER TABLE downloads DROP COLUMN transfer`); err != nil {
		t.Fatal(err)
	}

	err = db.Migrate(ctx)
	if err == nil {
		t.Fatal("startup accepted a database missing downloads.transfer")
	}
	if !strings.Contains(err.Error(), "downloads.transfer") {
		t.Errorf("the error must name the missing column, got: %v", err)
	}
}

func assertColumn(t *testing.T, ctx context.Context, db *DB, table, column string) {
	t.Helper()
	ok, err := columnExists(ctx, db.R, table, column)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("%s.%s is missing", table, column)
	}
}
