package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// Migration 0073 (pipeline contract v2, M4.3): the count check refuses while
// an operator's v1 deletion request is unfinished; once it is settled the v1
// tables go, the deliveries cursor moves onto the history cursor's row, the
// storage-change path fences survive in their own table, and the 0040 review
// cohort becomes historical publications (nothing is freed for deletion).
func TestMigration0073RefusesWithLiveDependents(t *testing.T) {
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
	if err := goose.UpToContext(ctx, db.W, "migrations", 72); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := db.R.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	exec(`INSERT INTO media_items(id,kind,title,sort_title,added_at,updated_at) VALUES(1,'movie','M','m',0,0)`)
	exec(`INSERT INTO downloads(id,media_item_id,protocol,release_title,state,added_at,updated_at,transfer,generation_key,import_path)
		VALUES(1,1,'usenet','old','imported',1,1,'t-1','gen-old','/completed/old-by-generation')`)
	// The 0040 cohort: a path entry, a generation-only entry, and a
	// storage-change orphan path fence.
	exec(`INSERT INTO legacy_cleanup_review(generation_key,path) VALUES('gen-x','/completed/old-by-path')`)
	exec(`INSERT INTO legacy_cleanup_review(generation_key,path) VALUES('gen-old','')`)
	exec(`INSERT INTO legacy_cleanup_review(generation_key,path) VALUES('storage-change:tok1','/completed/orphan')`)
	exec(`INSERT INTO runner_delivery_cursors(client_id,after_sequence,updated_at) VALUES(7,42,1)`)
	exec(`INSERT INTO runner_admission_poll_schedule(download_id,generation_key) VALUES(1,'gen-old')`)
	exec(`INSERT INTO runner_delivery_receipts(id,client_id,delivery_id,sequence,publication_generation,manifest_digest,artifact_id,job_id,ready_at,publication_path,files_json,created_at,updated_at)
		VALUES(1,1,'d-1',1,'g','dig','a','1',1,'/completed/pub','[]',1,1)`)
	exec(`INSERT INTO runner_delivery_explicit_delete_batches(batch_id,receipt_id,payload_path,inventory_fingerprint,publication_directory_identity,storage_identity,generation_key,request_token,publication_generation,created_at)
		VALUES('batch-pending',1,'/completed/pub','fp','dir','st','g','t','g',1)`)
	exec(`INSERT INTO runner_delivery_explicit_delete_files(batch_id,path,reviewed_path,snapshot_identity,parent_identity) VALUES('batch-pending','a.mkv','/completed/pub/a.mkv','s','p')`)

	err = db.Migrate(ctx)
	if !errors.Is(err, ErrLiveV1Dependents) || !strings.Contains(err.Error(), "batch-pending") {
		t.Fatalf("Migrate = %v, want a refusal naming the pending batch", err)
	}
	if count(`SELECT COUNT(*) FROM sqlite_master WHERE name='legacy_cleanup_review'`) != 1 {
		t.Fatal("the refused migration dropped a table")
	}

	// The operator cancels the request on the previous build: now it may go.
	exec(`UPDATE runner_delivery_explicit_delete_batches SET abandoned_at=5 WHERE batch_id='batch-pending'`)
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"legacy_cleanup_review", "legacy_inflight_import_evidence", "runner_delivery_cursors",
		"runner_admission_poll_schedule", "runner_delivery_explicit_delete_batches", "runner_delivery_explicit_delete_files"} {
		if count(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table) != 0 {
			t.Fatalf("%s still exists", table)
		}
	}
	for _, path := range []string{"/completed/old-by-path", "/completed/old-by-generation"} {
		p, ok, err := db.PublicationByPath(ctx, path)
		if err != nil || !ok || p.Cohort != "historical" || !p.CleanupApprovedAt.IsZero() || p.ManifestSource != "none" {
			t.Fatalf("0040 cohort path %s = %+v ok=%v err=%v, want a historical publication", path, p, ok, err)
		}
	}
	if _, ok, _ := db.PublicationByPath(ctx, "/completed/orphan"); ok {
		t.Fatal("a storage-change fence became a publication")
	}
	held, err := db.StorageReviewPath(ctx, "/completed/orphan/file.mkv")
	if err != nil || !held {
		t.Fatalf("storage-change path fence lost: held=%v err=%v", held, err)
	}
	if after, err := db.DeliveryCursor(ctx, 7); err != nil || after != 42 {
		t.Fatalf("deliveries cursor = %d (%v), want 42", after, err)
	}
	c, err := db.GetHistoryCursor(ctx, 7)
	if err != nil || c.Stored {
		t.Fatalf("history cursor = %+v (%v): a row made for the deliveries cursor must not count as a stored history cursor", c, err)
	}
	if err := db.UpsertHistoryCursor(ctx, RunnerHistoryCursor{ClientID: 7, Incarnation: "inc", SinceSeq: 9}); err != nil {
		t.Fatal(err)
	}
	if c, _ := db.GetHistoryCursor(ctx, 7); !c.Stored || c.SinceSeq != 9 {
		t.Fatalf("history cursor after write = %+v", c)
	}
	if after, _ := db.DeliveryCursor(ctx, 7); after != 42 {
		t.Fatal("writing the history cursor reset the deliveries cursor")
	}
	// The receipt-bound delete guard is gone: a row is deletable unless a
	// publication still holds files for it.
	if count(`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='prevent_download_delete_with_runner_delivery'`) != 0 {
		t.Fatal("receipt delete trigger survived")
	}
}
