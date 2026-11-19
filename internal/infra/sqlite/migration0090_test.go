package sqlite

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestMigration0090RetiresOnlyLegacyUnsentReplacementWork(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err = goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, db.W, "migrations", 89); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO media_items (id,kind,title,sort_title,added_at,updated_at) VALUES (7,'series','Series','series',0,0)`,
		`INSERT INTO jobs(id,kind,payload,dedupe_key,run_after,created_at,updated_at) VALUES
			(1,'acquisition.season','{"trigger":"failed_acquisition","fingerprint":"unbound"}','season:7:0:1',0,0,0),
			(2,'acquisition.season','{"trigger":"failed_acquisition","fingerprint":"manual","manualGeneration":1}','season:7:0:2',0,0,0),
			(3,'acquisition.season','{"trigger":"backlog","replanGeneration":1}','season:7:0:3',0,0,0)`,
		`INSERT INTO acquisition_plans(id,media_item_id,season,state,snapshot_fingerprint,decision,added_at,updated_at) VALUES
			(1,7,1,'active','unbound','{"trigger":"failed_acquisition"}',0,0),
			(2,7,2,'admitted','manual','{"trigger":"failed_acquisition"}',0,0),
			(3,7,3,'admitted','independent','{"trigger":"backlog"}',0,0),
			(4,7,4,'admitted','unbound','{"trigger":"failed_acquisition"}',0,0)`,
		`INSERT INTO downloads(id,media_item_id,season,release_title,protocol,state,plan_id,candidate_key,submission_phase,wantables,added_at,updated_at) VALUES
			(1,7,1,'unsent replacement','torrent','planned',1,'one','pending','["episode:7:1:1"]',0,0),
			(2,7,1,'submitted replacement','torrent','grabbed',1,'two','submitted','["episode:7:1:2"]',0,0),
			(3,7,2,'manual','torrent','planned',2,'one','pending','["episode:7:2:1"]',0,0),
			(4,7,3,'independent','torrent','planned',3,'one','pending','["episode:7:3:1"]',0,0),
			(5,7,4,'unsent only','torrent','planned',4,'one','pending','["episode:7:4:1"]',0,0)`,
	} {
		if _, err := db.W.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if err = goose.UpToContext(ctx, db.W, "migrations", 90); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct {
		query string
		want  string
	}{
		{`SELECT group_concat(state,',') FROM (SELECT state FROM jobs ORDER BY id)`, "done,queued,queued"},
		{`SELECT group_concat(state,',') FROM (SELECT state FROM acquisition_plans ORDER BY id)`, "active,admitted,admitted,completed"},
		{`SELECT group_concat(submission_phase,',') FROM (SELECT submission_phase FROM downloads ORDER BY id)`, "rejected,submitted,pending,pending,rejected"},
		{`SELECT group_concat(superseded,',') FROM (SELECT superseded FROM downloads ORDER BY id)`, "1,0,0,0,1"},
	} {
		var got string
		if err := db.R.QueryRowContext(ctx, check.query).Scan(&got); err != nil || got != check.want {
			t.Fatalf("%s: got %q, want %q, err=%v", check.query, got, check.want, err)
		}
	}
}
