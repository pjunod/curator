package sqlite

import (
	"context"
	"testing"

	"github.com/pressly/goose/v3"
)

// Migration 0072 (pipeline contract v2, M2; plan M2 tests (k) and (n)):
// parked and superseded rows become provisional failures, every v1 receipt
// becomes a historical publication carrying the per-file outcomes it reached,
// the precedence table is seeded, terminal rows keep their terminal, and the
// rebuild of downloads leaves its child tables untouched.
func TestMigration0072(t *testing.T) {
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
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.W.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`INSERT INTO media_items(id,kind,title,sort_title,added_at,updated_at) VALUES(1,'series','Show','show',0,0)`)
	row := func(id int, state, parked string, superseded int, wants string, added int64) {
		exec(`INSERT INTO downloads(id,media_item_id,protocol,release_title,state,added_at,updated_at,transfer,parked_reason,parked_at,superseded,wantables,generation_key)
			VALUES(?,1,'usenet','r',?,?,?,?,?,?,?,?,?)`, id, state, added, added, "t-"+string(rune('a'+id)), parked, map[bool]int{true: 5, false: 0}[parked != ""], superseded, wants, "g"+string(rune('a'+id)))
	}
	row(1, "downloading", "Runner retains source files; review", 0, `["episode:1:1:1"]`, 10) // parked
	row(2, "grabbed", "", 1, `["episode:1:1:2"]`, 11)                                        // superseded
	row(3, "importing", "", 0, `["season:1:1"]`, 12)                                         // accepted, partially placed
	row(4, "imported", "", 0, `["episode:1:1:3"]`, 13)
	row(5, "failed", "", 0, `["episode:1:1:3"]`, 14)
	row(6, "downloading", "", 0, `["episode:1:1:4"]`, 15)
	row(7, "downloading", "", 0, `["episode:1:1:4"]`, 15) // ambiguous: same added_at for one wantable
	exec(`UPDATE downloads SET runner_control='{"version":1,"lifecycle":"held","message":"waiting for disk"}' WHERE id=6`)
	// Children the rebuild must not cascade-delete.
	exec(`INSERT INTO import_attempt_controls(download_id,generation_key) VALUES(3,'gd')`)
	exec(`INSERT INTO runner_admission_poll_schedule(download_id,generation_key) VALUES(6,'gg')`)
	// An accepted v1 receipt with one placed file and one pending, and an
	// unmatched receipt.
	files := `[{"path":"S01E01.mkv","sizeBytes":5,"sha256":"aa"},{"path":"S01E02.mkv","sizeBytes":6,"sha256":"bb"}]`
	exec(`INSERT INTO runner_delivery_receipts(id,client_id,delivery_id,sequence,publication_generation,manifest_digest,artifact_id,job_id,ready_at,publication_path,mapped_path,files_json,state,ack_state,download_id,created_at,updated_at)
		VALUES(1,1,'d-1',7,'gen1','dig','a','33',100,'/runner/pub1','/local/pub1',?,'importing','accepted',3,1,1)`, files)
	exec(`INSERT INTO runner_delivery_file_outcomes(receipt_id,path,size_bytes,sha256,disposition,library_path,updated_at) VALUES(1,'S01E01.mkv',5,'aa','placed','/lib/e1.mkv',1)`)
	exec(`INSERT INTO runner_delivery_receipts(id,client_id,delivery_id,sequence,publication_generation,manifest_digest,artifact_id,job_id,ready_at,publication_path,files_json,state,ack_state,download_id,created_at,updated_at)
		VALUES(2,1,'d-2',8,'gen2','dig2','b','34',101,'/runner/pub2','[{"path":"x.mkv","sizeBytes":1,"sha256":"cc"}]','unresolved','not_accepted',0,1,1)`)
	// A newer receipt for the same path (a re-published generation): it wins
	// the publication, and each file takes the newest outcome recorded.
	exec(`INSERT INTO runner_delivery_receipts(id,client_id,delivery_id,sequence,publication_generation,manifest_digest,artifact_id,job_id,ready_at,publication_path,mapped_path,files_json,state,ack_state,download_id,created_at,updated_at)
		VALUES(3,1,'d-3',9,'gen3','dig3','c','33',102,'/runner/pub1b','/local/pub1',?,'importing','accepted',3,2,2)`, files)
	exec(`INSERT INTO runner_delivery_file_outcomes(receipt_id,path,size_bytes,sha256,disposition,library_path,updated_at) VALUES(3,'S01E01.mkv',5,'aa','pending','',0)`)
	exec(`INSERT INTO runner_delivery_file_outcomes(receipt_id,path,size_bytes,sha256,disposition,library_path,updated_at) VALUES(3,'S01E02.mkv',6,'bb','placed','/lib/e2.mkv',5)`)
	var children int
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_attempt_controls`).Scan(&children)

	if err := goose.UpToContext(ctx, db.W, "migrations", 72); err != nil {
		t.Fatal(err)
	}

	var after int
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_attempt_controls`).Scan(&after)
	if after != children || after != 1 {
		t.Fatalf("import_attempt_controls %d -> %d: the rebuild cascaded", children, after)
	}
	var schedules int
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_admission_poll_schedule`).Scan(&schedules)
	if schedules != 1 {
		t.Fatalf("runner_admission_poll_schedule rows = %d, want 1", schedules)
	}
	fk, err := db.R.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if fk.Next() {
		t.Fatal("foreign_key_check reports violations after the rebuild")
	}
	_ = fk.Close()

	// The rest reads through the current query layer, which needs the
	// whole schema (0073 adds columns it scans). 0078 is not applied: it
	// refuses while these v1_record publications are not removed.
	if err := goose.UpToContext(ctx, db.W, "migrations", 74); err != nil {
		t.Fatal(err)
	}
	get := func(id int64) Download {
		t.Helper()
		dl, err := db.GetDownload(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return dl
	}
	for _, id := range []int64{1, 2} {
		dl := get(id)
		if dl.State != "failed" || !dl.Provisional || dl.RunnerOutcome != "failed" || dl.RunnerOutcomeReason != "parked" || len(dl.Error) < 8 || dl.Error[:8] != "parked: " {
			t.Fatalf("row %d = %s provisional=%v outcome=%s/%s %q, want a provisional parked failure", id, dl.State, dl.Provisional, dl.RunnerOutcome, dl.RunnerOutcomeReason, dl.Error)
		}
	}
	if dl := get(3); dl.RunnerOutcome != "completed" || dl.Provisional {
		t.Fatalf("importing row kept no terminal: %+v", dl.RunnerOutcome)
	}
	if dl := get(4); dl.RunnerOutcome != "completed" {
		t.Fatal("imported row kept no terminal")
	}
	if dl := get(5); dl.RunnerOutcome != "failed" || dl.Provisional || !dl.RunnerBlamesRelease {
		t.Fatalf("failed row = %+v", dl.RunnerOutcome)
	}
	if dl := get(6); dl.State != "held" || dl.Error != "waiting for disk" || dl.DeadlineAt.IsZero() {
		t.Fatalf("held row = %s %q deadline %v", dl.State, dl.Error, dl.DeadlineAt)
	}

	p, ok, err := db.PublicationByPath(ctx, "/local/pub1")
	if err != nil || !ok || p.DownloadID != 3 || p.Cohort != "historical" || p.State != "discovered" || p.ManifestSource != "v1_record" ||
		p.RemotePath != "/runner/pub1b" || p.Generation != "gen3" {
		t.Fatalf("receipt 1 publication = %+v ok=%v err=%v", p, ok, err)
	}
	pf, err := db.ListPublicationFiles(ctx, p.ID)
	if err != nil || len(pf) != 2 {
		t.Fatalf("files = %+v err %v", pf, err)
	}
	if pf[0].Path != "S01E01.mkv" || pf[0].Outcome != "placed" || pf[0].LibraryPath != "/lib/e1.mkv" || pf[0].PlacedSHA256 != "aa" {
		t.Fatalf("E01 = %+v, want the newer placed outcome over the later receipt's older pending one", pf[0])
	}
	if pf[1].Outcome != "placed" || pf[1].LibraryPath != "/lib/e2.mkv" {
		t.Fatalf("E02 = %+v, want the newest receipt's placed outcome", pf[1])
	}
	p2, ok, _ := db.PublicationByPath(ctx, "/runner/pub2")
	if !ok || p2.DownloadID != 0 || p2.Cohort != "historical" {
		t.Fatalf("unmatched receipt = %+v ok=%v, want a historical publication with no row", p2, ok)
	}
	attempts, err := db.WantableAttempts(ctx, []string{"episode:1:1:3", "season:1:1", "episode:1:1:4", "episode:1:1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if attempts["episode:1:1:3"].DownloadID != 4 || attempts["season:1:1"].DownloadID != 3 {
		t.Fatalf("attempts = %+v, want the newest non-failed download per wantable", attempts)
	}
	if _, ok := attempts["episode:1:1:4"]; ok {
		t.Fatal("an ambiguous wantable was seeded")
	}
	if _, ok := attempts["episode:1:1:1"]; ok {
		t.Fatal("a failed (parked) row was seeded as current")
	}
	// A row inserted after the migration gets its state's deadline and exactly
	// one completed receipt, carrying its generation key: the deadline is set
	// by the same post-insert UPDATE that assigns the key.
	exec(`INSERT INTO downloads(id,media_item_id,protocol,release_title,state,import_path,added_at,updated_at) VALUES(20,1,'usenet','fresh','downloading','/completed/fresh',20,20)`)
	if dl := get(20); dl.DeadlineAt.IsZero() || dl.GenerationKey == "" {
		t.Fatalf("fresh row deadline %v generation %q", dl.DeadlineAt, dl.GenerationKey)
	}
	var receipts, blank int
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(generation_key=''),0) FROM completed_receipts WHERE download_id=20`).Scan(&receipts, &blank)
	if receipts != 1 || blank != 0 {
		t.Fatalf("fresh row has %d completed receipts (%d without a generation key), want one with its key", receipts, blank)
	}
	// A provisional terminal may be re-opened; a final one may not change.
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET runner_outcome='' WHERE id=1`); err != nil {
		t.Fatalf("provisional terminal could not re-open: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET runner_outcome='completed' WHERE id=5`); err == nil {
		t.Fatal("a final runner_outcome was rewritten")
	}
	// The down migration restores the old CHECK and state names.
	if err := goose.DownToContext(ctx, db.W, "migrations", 71); err != nil {
		t.Fatal(err)
	}
	var state string
	_ = db.R.QueryRowContext(ctx, `SELECT state FROM downloads WHERE id=6`).Scan(&state)
	if state != "downloading" {
		t.Fatalf("down migration left state %q", state)
	}
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_attempt_controls`).Scan(&after)
	if after != 1 {
		t.Fatal("the down migration cascaded")
	}
}
