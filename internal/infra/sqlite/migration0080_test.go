package sqlite

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

// Migration 0080 rebuilds replacement_operations for the rematch intent. A
// database with replacement operations and their files and steps keeps every
// row, every child still points at its operation, and the new intent fits.
func TestMigration0080KeepsReplacementOperationsAndChildren(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db.W, "migrations", 79); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO media_items (id, kind, title, sort_title, added_at, updated_at) VALUES (7, 'movie', 'Dune', 'dune', 0, 0)`,
		`INSERT INTO replacement_operations(operation_id,request_id,fingerprint,state,intent,scope,media_item_id,source_generation,blacklist_source_release,reason_code,reason,created_at,updated_at,retry_attempts)
		 VALUES('op-1','req-1','fp','waiting_for_release','replace_bad','download',7,'g',1,'bad','bad file',1,2,3)`,
		`INSERT INTO replacement_operation_files(operation_id,media_file_id,file_generation,source_generation,path) VALUES('op-1',11,'fg','g','/m/Dune.mkv')`,
		`INSERT INTO replacement_operation_steps(operation_id,step,state,started_at) VALUES('op-1','accepted','complete',1)`,
	} {
		if _, err := db.W.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := goose.UpToContext(ctx, db.W, "migrations", 80); err != nil {
		t.Fatal(err)
	}
	var state string
	var retries, files, steps, violations int
	if err := db.R.QueryRowContext(ctx, `SELECT state,retry_attempts FROM replacement_operations WHERE operation_id='op-1'`).Scan(&state, &retries); err != nil || state != "waiting_for_release" || retries != 3 {
		t.Fatalf("operation = %s/%d %v, want it kept as it was", state, retries, err)
	}
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM replacement_operation_files WHERE operation_id='op-1'`).Scan(&files)
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM replacement_operation_steps WHERE operation_id='op-1'`).Scan(&steps)
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations)
	if files != 1 || steps != 1 || violations != 0 {
		t.Fatalf("files %d steps %d violations %d, want the children kept and valid", files, steps, violations)
	}
	var origin string
	_ = db.R.QueryRowContext(ctx, `SELECT origin FROM media_items WHERE id=7`).Scan(&origin)
	if origin != "user" {
		t.Fatalf("origin = %q, want existing items backfilled user", origin)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO replacement_operations(operation_id,request_id,fingerprint,state,intent,scope,media_item_id,source_generation,blacklist_source_release,reason_code,reason,created_at,updated_at)
		VALUES('op-2','req-2','fp','complete','rematch','download',7,'',1,'rematch','re-matched',1,1)`); err != nil {
		t.Fatalf("the rematch intent is refused: %v", err)
	}
	// Down and up again: the rematch rows go, the rest stay.
	if err := goose.DownToContext(ctx, db.W, "migrations", 79); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM replacement_operations`).Scan(&n)
	if n != 1 {
		t.Fatalf("operations after down = %d, want only the replacement", n)
	}
	if err := goose.UpToContext(ctx, db.W, "migrations", 80); err != nil {
		t.Fatal(err)
	}
}
