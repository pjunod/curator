package sqlite

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestAcquisitionMigrationPreservesLegacyQueueAndRefusesCustodyDowngrade(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err = goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, db.W, "migrations", 37); err != nil {
		t.Fatal(err)
	}
	_, err = db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,added_at) VALUES(1,'series','Legacy','legacy',0)`)
	if err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{"grabbed", "downloading", "downloaded", "awaiting_import", "importing", "imported", "failed"} {
		_, err = db.W.ExecContext(ctx, `INSERT INTO downloads(id,media_item_id,protocol,release_title,state,added_at,updated_at,transfer,runner_control,save_path,import_path,match_evidence,handoff_log) VALUES(?,1,'torrent','legacy',?,1,2,'trace','{"version":1,"lifecycle":"held"}','/client/partial','/local/partial','{"reason":"match"}','[{"step":"held"}]')`, i+1, state)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 7; i++ {
		dl, e := db.GetDownload(ctx, int64(i))
		if e != nil {
			t.Fatal(e)
		}
		if dl.Transfer != "trace" || dl.RunnerControl == "" || dl.ImportPath != "/local/partial" || len(dl.Handoff) != 1 || dl.SubmissionPhase != "submitted" {
			t.Fatalf("custody lost: %+v", dl)
		}
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE downloads SET submission_phase='uncertain' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err = goose.DownToContext(ctx, db.W, "migrations", 37); err == nil {
		t.Fatal("downgrade erased uncertainty")
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE downloads SET submission_phase='submitted' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err = goose.DownToContext(ctx, db.W, "migrations", 37); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM downloads`).Scan(&n); err != nil || n != 7 {
		t.Fatal("downgrade lost legacy rows", err)
	}
}
