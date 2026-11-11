package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/pressly/goose/v3"
)

// Migration 0076: publications that failed only because Curator wedged itself
// (a retained stage, a queue-time stall, an obfuscated single-episode name)
// go back to discovered with a fresh deadline; every other row is untouched.
func TestMigration0076RequeuesOnlyWedgedImports(t *testing.T) {
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
	exec(`INSERT INTO media_items(id,kind,title,sort_title,added_at,updated_at) VALUES(1,'series','S','s',0,0)`)
	rows := []struct {
		state, error string
		requeue      bool
	}{
		{"import_failed", "1 file(s) unresolved: a.mkv: existing stage retained; explicit retry or custody review required: /lib/.monarr-stage-x", true},
		{"import_failed", "unbound stage requires explicit custody review: /lib/.monarr-stage-y", true},
		{"import_failed", "stalled: not imported within 6 h of discovery", true},
		{"import_failed", "1 file(s) unresolved: 321d96.mkv: cannot determine episodes from \"321d96.mkv\"", true},
		{"import_failed", "the publication holds no importable media (samples and sidecars only)", false},
		{"import_failed", "manifest refused: manifest lists no files", false},
		{"imported", "existing stage retained; explicit retry or custody review required", false},
		{"removed", "stalled: not imported within 6 h of discovery", false},
	}
	for i, r := range rows {
		exec(`INSERT INTO downloads(id,media_item_id,protocol,release_title,state,added_at,updated_at,transfer,generation_key) VALUES(?,1,'usenet','r','failed',1,1,?,?)`,
			i+1, "t-"+string(rune('a'+i)), "g-"+string(rune('a'+i)))
		exec(`INSERT INTO publications(id,download_id,generation,kind,path,state,error,discovered_at,deadline_at,updated_at) VALUES(?,?,?, 'completed', ?, ?, ?, 1, 2, 1)`,
			i+1, i+1, "gen", "/completed/p"+string(rune('a'+i)), r.state, r.error)
	}
	before := time.Now().UnixMilli()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i, r := range rows {
		var state, errMsg string
		var deadline int64
		if err := db.R.QueryRowContext(ctx, `SELECT state,error,deadline_at FROM publications WHERE id=?`, i+1).Scan(&state, &errMsg, &deadline); err != nil {
			t.Fatal(err)
		}
		if r.requeue {
			if state != "discovered" || errMsg != "" || deadline < before+int64(5*time.Hour/time.Millisecond) {
				t.Errorf("row %d (%q) = %s %q deadline %d, want requeued as discovered with a fresh deadline", i+1, r.error, state, errMsg, deadline)
			}
			continue
		}
		if state != r.state || errMsg != r.error || deadline != 2 {
			t.Errorf("row %d (%s %q) was changed to %s %q deadline %d", i+1, r.state, r.error, state, errMsg, deadline)
		}
	}
	var indexes int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='import_placements_target'`).Scan(&indexes); err != nil || indexes != 1 {
		t.Fatalf("target index = %d (%v)", indexes, err)
	}
}
