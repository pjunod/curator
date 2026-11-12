package acquisition

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// Review of 5d173a7, finding 1: a salvage that arrives after the row's
// completed publication was imported is imported in a fresh epoch, and the
// row stays imported (never downloaded, so never overdue and failed).
func TestSalvageAfterAnImportedCompletedKeepsTheRowImported(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	f.complete(dl, f.publish(epOne+".job-1.runner-c1", "c1", map[string]string{epOne + ".mkv": "video"}))
	before := f.row(dl.ID)
	if before.State != "imported" && before.State != "notified" {
		t.Fatalf("completed publication: row = %s (%s), want imported", before.State, before.Error)
	}
	info := f.publish(epOne+".job-1.runner-s1", "s1", map[string]string{epOne + ".mkv": "salvaged video"})
	id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "s1", Kind: "salvage", Path: info.Path, Source: "file"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.svc.enqueuePublicationImport(f.ctx, f.row(dl.ID), f.cfg, id) {
		t.Fatal("salvage not offered")
	}
	got := f.row(dl.ID)
	if got.State != before.State || got.Handoff[len(got.Handoff)-1].Step == "" {
		t.Fatalf("after the salvage: row = %s, want %s kept", got.State, before.State)
	}
	if a, err := f.db.GetImportAttempt(f.ctx, dl.ID, dl.GenerationKey); err != nil || a.Epoch < 1 {
		t.Fatalf("attempt = %+v (%v), want a fresh epoch for the salvage", a, err)
	}
	if rows, err := f.db.OverdueDownloadsAnyClient(f.ctx, got.UpdatedAt.Add(24*365*3600e9)); err != nil || len(rows) != 0 {
		t.Fatalf("overdue rows = %d (%v), want none: the row has no deadline", len(rows), err)
	}
}

// Review of 5d173a7, finding 2: a stage an earlier epoch left prepared is
// swept by reconciliation — its temporary removed, the placement rolled back
// — and the library file at its target, which a later epoch published with
// the same bytes, is never touched. A stage an authorization names is kept.
func TestSupersededStageIsSweptWithoutTouchingTheLibrary(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	f.complete(dl, f.publish(epOne+".job-1.runner-c1", "c1", map[string]string{epOne + ".mkv": "video"}))
	var target string
	if err := f.db.R.QueryRowContext(f.ctx, `SELECT path FROM media_files LIMIT 1`).Scan(&target); err != nil {
		t.Fatal(err)
	}
	library, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE import_attempt_controls SET attempt_epoch=1 WHERE download_id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	stage := func(id string) string {
		tmp := filepath.Join(filepath.Dir(target), ".monarr-stage-"+id)
		if err := os.WriteFile(tmp, library, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO import_placements(id,target,state,data,updated_at) VALUES(?,?,'prepared',
			json_object('id',?,'download_id',?,'source_generation',?,'import_attempt_epoch',0,'item_id',?,'source','/src/x.mkv','target',?,'temporary',?,
			'backup',?,'sha256','x','state','prepared'),0)`,
			id, target, id, dl.ID, dl.GenerationKey, f.item, target, tmp, filepath.Join(filepath.Dir(target), ".monarr-prior-"+id)); err != nil {
			t.Fatal(err)
		}
		return tmp
	}
	swept, kept := stage("old0"), stage("auth0")
	if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO import_publication_authorizations(download_id,generation_key,attempt_epoch,publication_id,placement_id,source,target,payload,authorized_at,outcome)
		VALUES(?,?,0,'placement:auth0','auth0','/src/x.mkv',?,'{}',1,'pending')`, dl.ID, dl.GenerationKey, target); err != nil {
		t.Fatal(err)
	}
	f.svc.reconcilePlacements(f.ctx)
	if exists(swept) {
		t.Fatal("the superseded stage's temporary is still on disk")
	}
	if n := f.count(`SELECT COUNT(*) FROM import_placements WHERE id='old0' AND state='rolled_back'`); n != 1 {
		t.Fatal("the superseded stage was not rolled back")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(library) {
		t.Fatalf("library file touched: %q (%v)", got, err)
	}
	if !exists(kept) || f.count(`SELECT COUNT(*) FROM import_placements WHERE id='auth0' AND state='prepared'`) != 1 {
		t.Fatal("a stage an authorization names was swept")
	}
}

// Review of 5d173a7, finding 3: completing an import of a row whose
// publications are all salvage (a person's manual import) writes imported.
func TestCompletingAnImportOfASalvageOnlyRowWritesImported(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET state='failed', runner_outcome='failed', runner_outcome_reason='publish', provisional=0 WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "s1", Kind: "salvage", Path: filepath.Join(f.root, "s1"), Source: "none"}); err != nil {
		t.Fatal(err)
	}
	a, err := f.db.EnsureImportAttempt(f.ctx, dl.ID, dl.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SealImportAttempt(f.ctx, a.ImportAttemptKey, []sqlite.ImportAttemptFile{{Source: "/manual/x.mkv", Kind: "media"}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE import_attempt_files SET outcome='committed' WHERE download_id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.CompleteImportAttempt(f.ctx, a.ImportAttemptKey, "manually imported 1 selected file(s)"); err != nil {
		t.Fatal(err)
	}
	if got := f.row(dl.ID); got.State != "imported" {
		t.Fatalf("row = %s, want imported", got.State)
	}
}

// Review of 5d173a7, finding 4: retrying the import of a failed row with no
// publication records the step and leaves it failed; the import moves it.
func TestImportNowOnAFailedRowKeepsItFailedUntilTheImportMovesIt(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET state='failed', error='import failed: mount' WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.BeginImportRetry(f.ctx, dl.ID, dl.GenerationKey); err != nil {
		t.Fatal(err)
	}
	got := f.row(dl.ID)
	if got.State != "failed" || got.Error != "import failed: mount" || got.Handoff[len(got.Handoff)-1].Step != "import_queued" || !got.DeadlineAt.IsZero() {
		t.Fatalf("row = %s (%q) step %q deadline %v, want failed kept with import_queued", got.State, got.Error, got.Handoff[len(got.Handoff)-1].Step, got.DeadlineAt)
	}
}
