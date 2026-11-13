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

// backupStage is a superseded stage (epoch 0 of a download whose attempt is
// now at epoch 1) that had moved the library's prior file aside to its
// backup: a temporary, a backup holding prior, and the placement row.
func (f *v2Fixture) backupStage(dl sqlite.Download, id, target string, prior []byte, parentIdentity string) (tmp, backup string) {
	f.t.Helper()
	dir := filepath.Dir(target)
	tmp, backup = filepath.Join(dir, ".monarr-stage-"+id), filepath.Join(dir, ".monarr-prior-"+id)
	if err := os.WriteFile(tmp, []byte("stage bytes"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(backup, prior, 0o644); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO import_placements(id,target,state,data,updated_at) VALUES(?,?,'prepared',
		json_object('id',?,'download_id',?,'source_generation',?,'import_attempt_epoch',0,'item_id',?,'source','/src/x.mkv','target',?,'temporary',?,
		'backup',?,'sha256',?,'previous_sha256',?,'target_parent_identity',?,'state','prepared'),0)`,
		id, target, id, dl.ID, dl.GenerationKey, f.item, target, tmp, backup, sha256Hex([]byte("stage bytes")), sha256Hex(prior), parentIdentity); err != nil {
		f.t.Fatal(err)
	}
	return tmp, backup
}

// supersededSetup imports a completed publication (its committed placement
// is the library file) and moves the attempt to epoch 1.
func (f *v2Fixture) supersededSetup() (sqlite.Download, string, []byte) {
	f.t.Helper()
	dl := f.grab(1, epOne)
	f.complete(dl, f.publish(epOne+".job-1.runner-c1", "c1", map[string]string{epOne + ".mkv": "video"}))
	var target string
	if err := f.db.R.QueryRowContext(f.ctx, `SELECT path FROM media_files LIMIT 1`).Scan(&target); err != nil {
		f.t.Fatal(err)
	}
	library, err := os.ReadFile(target)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE import_attempt_controls SET attempt_epoch=1 WHERE download_id=?`, dl.ID); err != nil {
		f.t.Fatal(err)
	}
	return dl, target, library
}

func (f *v2Fixture) stageState(id string) string {
	f.t.Helper()
	var st string
	if err := f.db.R.QueryRowContext(f.ctx, `SELECT state FROM import_placements WHERE id=?`, id).Scan(&st); err != nil {
		f.t.Fatal(err)
	}
	return st
}

// Decision 2026-11-12, case 1: the target verifies against its committed
// placement (the upgrade landed): the backup is superseded and removed with
// the temporary; the library file is untouched.
func TestSupersededStageBackupRemovedWhenTheUpgradeLanded(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl, target, library := f.supersededSetup()
	tmp, backup := f.backupStage(dl, "landed0", target, []byte("the prior file"), "")
	f.svc.sweepAllSupersededStages(f.ctx)
	if exists(tmp) || exists(backup) || f.stageState("landed0") != "rolled_back" {
		t.Fatalf("temporary %v backup %v state %s, want both removed and rolled_back", exists(tmp), exists(backup), f.stageState("landed0"))
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(library) {
		t.Fatalf("library file touched: %q (%v)", got, err)
	}
}

// Case 2: an existing target is never overwritten. A target someone else
// put there (bytes nothing recorded) is kept and our backup removed; a
// missing target gets a backup that is the prior file back; a backup that
// is not the prior file is removed. Every stage ends rolled_back.
func TestSupersededStageBackupNeverOverwritesAnExistingTarget(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl, target, _ := f.supersededSetup()
	dir := filepath.Dir(target)
	prior := []byte("the prior episode file")
	theirs := []byte("a file someone else put here")
	unknown := filepath.Join(dir, "Test Show - S01E08.mkv")
	if err := os.WriteFile(unknown, theirs, 0o644); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(dir, "Test Show - S01E09.mkv")
	strange := filepath.Join(dir, "Test Show - S01E07.mkv")
	cases := []struct {
		id, target string
		want       []byte // nil: the target stays missing
		tmp, bak   string
	}{{id: "theirs0", target: unknown, want: theirs}, {id: "gone0", target: gone, want: prior}, {id: "strange0", target: strange}}
	for i := range cases {
		cases[i].tmp, cases[i].bak = f.backupStage(dl, cases[i].id, cases[i].target, prior, "")
	}
	if err := os.WriteFile(cases[2].bak, []byte("not the prior file"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.reconcileAndSweep()
	for _, c := range cases {
		got, err := os.ReadFile(c.target)
		switch {
		case c.want == nil && !os.IsNotExist(err):
			t.Fatalf("%s: target = %q (%v), want still missing", c.id, got, err)
		case c.want != nil && (err != nil || string(got) != string(c.want)):
			t.Fatalf("%s: target = %q (%v), want %q", c.id, got, err, c.want)
		}
		if exists(c.tmp) || exists(c.bak) || f.stageState(c.id) != "rolled_back" {
			t.Fatalf("%s: temporary %v backup %v state %s, want both removed and rolled_back", c.id, exists(c.tmp), exists(c.bak), f.stageState(c.id))
		}
	}
}

// Case 3: nothing can be proven (here the target directory is not the one
// recorded): the stage is left, untouched, for the next pass; once the
// directory verifies, that pass ends it.
func TestSupersededStageBackupLeftWhenNothingCanBeProven(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl, target, library := f.supersededSetup()
	tmp, backup := f.backupStage(dl, "unproven0", target, []byte("the prior file"), "linux-directory-v1:not-this-one")
	f.reconcileAndSweep()
	if !exists(tmp) || !exists(backup) || f.stageState("unproven0") != "prepared" {
		t.Fatalf("unproven: temporary %v backup %v state %s, want all left", exists(tmp), exists(backup), f.stageState("unproven0"))
	}
	if got, _ := os.ReadFile(target); string(got) != string(library) {
		t.Fatal("library file touched while nothing could be proven")
	}
	_, parent, err := placementParentIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE import_placements SET data=json_set(data,'$.target_parent_identity',?) WHERE id='unproven0'`, parent); err != nil {
		t.Fatal(err)
	}
	f.svc.sweepAllSupersededStages(f.ctx)
	if exists(tmp) || exists(backup) || f.stageState("unproven0") != "rolled_back" {
		t.Fatalf("next pass: temporary %v backup %v state %s, want it ended", exists(tmp), exists(backup), f.stageState("unproven0"))
	}
}

// reconcileAndSweep runs both passes that may end a stage with a backup.
func (f *v2Fixture) reconcileAndSweep() {
	f.svc.reconcilePlacements(f.ctx)
	f.svc.sweepAllSupersededStages(f.ctx)
}
