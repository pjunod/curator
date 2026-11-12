package sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// Migration 0077: publications an older build failed with a reason the
// current rules no longer have ("N file(s) unresolved: ...", "holds no
// importable media") are decided again — requeued with fresh limits when an
// enabled Runner client owns them (the import decides each file's want), or
// ended with their want grabbed again when no client can import them — and a
// removed publication's undecided files are closed. A person's decision in
// Resolve, a publication already fully decided and every non-failed row are
// untouched, and running it again changes nothing.
func TestMigration0077RedecidesLegacyFailedImports(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db.W, "migrations", 76); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO media_items(id,kind,title,sort_title,added_at,updated_at) VALUES(1,'series','S','s',0,0)`)
	exec(`INSERT INTO download_clients(id,type,name,url,category,added_at,enabled) VALUES(1,'nzbd','runner','http://r','tv',0,1)`)
	exec(`INSERT INTO download_clients(id,type,name,url,category,added_at,enabled) VALUES(2,'nzbd','old runner','http://o','tv',0,0)`)
	type file struct{ path, outcome, discard, reason string }
	rows := []struct {
		state, cohort, error string
		files                []file
		want                 string // requeue | end | keep
	}{
		// 1: the live wedge (pub 735): a superseded file left unresolved
		// with a sidecar.
		{"import_failed", "live", "1 file(s) unresolved: runner-swc-s01e04.avi: superseded by download 1250 which has not imported yet",
			[]file{{"runner-swc-s01e04.avi", "unresolved", "", "superseded by download 1250 which has not imported yet"},
				{"runner-swc-s01e04.nfo", "unresolved", "", "sidecar with no automatic placement; place or discard it in Resolve"}}, "requeue"},
		// 2: a truncated file (pub 501) beside one already placed.
		{"import_failed", "live", "1 file(s) unresolved: c48c.mkv: the file is cut short: the container says 147 MB, only 16 MB is here",
			[]file{{"c48c.mkv", "unresolved", "", "the file is cut short"}, {"ok.mkv", "placed", "", ""}}, "requeue"},
		// 3: the old no-media wording with pending files (pub 715).
		{"import_failed", "live", "the publication holds no importable media (samples and sidecars only)",
			[]file{{"abc.m2ts", "pending", "", ""}}, "requeue"},
		// 4: historical, with a download row (pub 13).
		{"import_failed", "historical", "1 file(s) unresolved: Musk.mkv: superseded by download 3 which has not imported yet",
			[]file{{"Musk.mkv", "unresolved", "", "superseded by download 3 which has not imported yet"}}, "requeue"},
		// 5: every file already decided (pub 666): the sweep retires it.
		{"import_failed", "live", "1 file(s) unresolved: d443.mkv: superseded by download 1155 which has not imported yet",
			[]file{{"d443.mkv", "discarded", "superseded", "superseded by the current attempt"}}, "keep"},
		// 6: a person deciding in Resolve (manual import).
		{"import_failed", "live", "1 file(s) unresolved: x.mkv: not selected",
			[]file{{"x.mkv", "unresolved", "", "not selected for import; place or discard it in Resolve"}}, "keep"},
		// 7: a current-build failure: already terminal and decided.
		{"import_failed", "live", "nothing importable: y.mkv: cannot determine episodes",
			[]file{{"y.mkv", "discarded", "rejected", "cannot determine episodes"}}, "keep"},
		// 8: imported rows are never re-opened.
		{"imported", "live", "1 file(s) unresolved: z.mkv: superseded by download 4 which has not imported yet",
			[]file{{"z.mkv", "unresolved", "", "superseded by download 4 which has not imported yet"}}, "keep"},
		// 9: owned by a disabled client: nothing can import it, so it ends
		// and its want is grabbed again.
		{"import_failed", "live", "1 file(s) unresolved: e.mkv: superseded by download 10 which has not imported yet",
			[]file{{"e.mkv", "unresolved", "", "superseded by download 10 which has not imported yet"}}, "end"},
	}
	for i, r := range rows {
		id := i + 1
		wantables, client, kind := `[]`, 1, "salvage"
		if id == 9 {
			wantables, client, kind = `["episode:1:1:9"]`, 2, "completed"
		}
		exec(`INSERT INTO downloads(id,media_item_id,protocol,release_title,state,added_at,updated_at,transfer,generation_key,wantables,client_id) VALUES(?,1,'usenet','r','failed',1,1,?,?,?,?)`,
			id, "t-"+string(rune('a'+i)), "g-"+string(rune('a'+i)), wantables, client)
		exec(`INSERT INTO publications(id,download_id,generation,kind,path,state,cohort,error,discovered_at,deadline_at,updated_at,import_attempts,last_stop,retry_at,hold_since,holds)
			VALUES(?,?,'gen',?,?,?,?,?,1,2,1,3,'payload',99,7,2)`,
			id, id, kind, "/completed/p"+string(rune('a'+i)), r.state, r.cohort, r.error)
		for _, f := range r.files {
			exec(`INSERT INTO publication_files(publication_id,path,bytes,sha256,outcome,discard_reason,reason,updated_at) VALUES(?,?,1,'d',?,?,?,1)`, id, f.path, f.outcome, f.discard, f.reason)
		}
	}
	// A removed publication's leftover unresolved sidecar (pub 538).
	exec(`INSERT INTO downloads(id,media_item_id,protocol,release_title,state,added_at,updated_at,transfer,generation_key) VALUES(20,1,'usenet','r','imported',1,1,'t-20','g-20')`)
	exec(`INSERT INTO publications(id,download_id,generation,kind,path,state,discovered_at,updated_at) VALUES(20,20,'gen','completed','/completed/gone','removed',1,1)`)
	exec(`INSERT INTO publication_files(publication_id,path,bytes,sha256,outcome,reason,updated_at) VALUES(20,'gone.nfo',1,'d','unresolved','sidecar with no automatic placement; place or discard it in Resolve',1)`)
	// Custody an interrupted attempt of a requeued row left.
	exec(`INSERT INTO import_attempt_controls(download_id,generation_key) VALUES(1,'g-a')`)
	exec(`INSERT INTO import_attempt_stages(download_id,generation_key,attempt_epoch,stage,source,target) VALUES(1,'g-a',0,'/lib/.monarr-stage-a','/c/a.avi','/lib/a.avi')`)

	type snapshot struct {
		state, error, lastStop string
		deadline, retry, hold  int64
		attempts, holds        int
		files                  map[string]string
	}
	read := func() map[int]snapshot {
		t.Helper()
		out := map[int]snapshot{}
		for _, id := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 20} {
			var s snapshot
			if err := db.R.QueryRowContext(ctx, `SELECT state,error,last_stop,deadline_at,retry_at,hold_since,import_attempts,holds FROM publications WHERE id=?`, id).
				Scan(&s.state, &s.error, &s.lastStop, &s.deadline, &s.retry, &s.hold, &s.attempts, &s.holds); err != nil {
				t.Fatal(err)
			}
			s.files = map[string]string{}
			fr, err := db.R.QueryContext(ctx, `SELECT path, outcome||'/'||discard_reason||'/'||CASE WHEN removed_at>0 THEN 'removed' ELSE '' END FROM publication_files WHERE publication_id=?`, id)
			if err != nil {
				t.Fatal(err)
			}
			for fr.Next() {
				var p, o string
				if err := fr.Scan(&p, &o); err != nil {
					t.Fatal(err)
				}
				s.files[p] = o
			}
			_ = fr.Close()
			out[id] = s
		}
		return out
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got := read()
	for i, r := range rows {
		id := i + 1
		s := got[id]
		switch r.want {
		case "requeue":
			if s.state != "discovered" || s.error != "" || s.deadline != 0 || s.retry != 0 || s.hold != 0 || s.attempts != 0 || s.holds != 0 || s.lastStop != "" {
				t.Errorf("row %d (%q) = %+v, want discovered with fresh limits", id, r.error, s)
			}
			for _, f := range r.files {
				want := f.outcome + "/" + f.discard + "/"
				if f.outcome == "unresolved" {
					want = "pending//"
				}
				if s.files[f.path] != want {
					t.Errorf("row %d file %s = %s, want %s", id, f.path, s.files[f.path], want)
				}
			}
		case "end":
			if s.state != "import_failed" || !strings.Contains(s.error, "download client") || s.files["e.mkv"] != "discarded/rejected/" {
				t.Errorf("clientless row %d = %+v, want ended with its files discarded", id, s)
			}
			var grabs int
			if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM replacement_grabs WHERE download_id=? AND wantable_ids='["episode:1:1:9"]'`, id).Scan(&grabs); err != nil || grabs != 1 {
				t.Errorf("clientless row %d: replacement grabs = %d (%v), want its want grabbed again", id, grabs, err)
			}
		case "keep":
			if s.state != r.state || s.error != r.error || s.attempts != 3 || s.retry != 99 {
				t.Errorf("row %d (%s %q) was changed: %+v", id, r.state, r.error, s)
			}
			for _, f := range r.files {
				if want := f.outcome + "/" + f.discard + "/"; s.files[f.path] != want {
					t.Errorf("row %d file %s = %s, want untouched %s", id, f.path, s.files[f.path], want)
				}
			}
		}
	}
	if o := got[20].files["gone.nfo"]; o != "discarded/rejected/removed" {
		t.Errorf("removed publication's unresolved file = %s, want discarded and removed", o)
	}
	var stage string
	if err := db.R.QueryRowContext(ctx, `SELECT state FROM import_attempt_stages WHERE stage='/lib/.monarr-stage-a'`).Scan(&stage); err != nil || stage != "released" {
		t.Errorf("held stage of a requeued row = %q (%v), want released", stage, err)
	}

	// Idempotent: the same statements a second time change nothing.
	raw, err := migrationsFS.ReadFile("migrations/0077_unwedge_legacy_imports.sql")
	if err != nil {
		t.Fatal(err)
	}
	up := strings.SplitN(strings.SplitN(string(raw), "-- +goose Up", 2)[1], "-- +goose Down", 2)[0]
	exec(up)
	again := read()
	for id, before := range got {
		after := again[id]
		if before.state != after.state || before.error != after.error || len(before.files) != len(after.files) {
			t.Errorf("second run changed row %d: %+v -> %+v", id, before, after)
		}
		for p, o := range before.files {
			if after.files[p] != o {
				t.Errorf("second run changed row %d file %s: %s -> %s", id, p, o, after.files[p])
			}
		}
	}
}
