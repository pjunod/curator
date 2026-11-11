package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Acceptance (Fable plan, Curator B): an import killed mid-copy imports on
// its next attempt with no operator action. The stage it left is scratch:
// the retry discards it and writes it again.
func TestRetryDiscardsAnInterruptedStage(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	original := placeFile
	t.Cleanup(func() { placeFile = original })
	var stage string
	placeFile = func(ctx context.Context, src, dest string, progress func(int64, int64)) error {
		// The process "dies" mid-copy: half the bytes under the stage's
		// name, and the copy never returns normally.
		stage = dest
		if err := os.WriteFile(dest, []byte("vid"), 0o644); err != nil {
			return err
		}
		return context.Canceled
	}
	pub := f.publish("Test.Show.S01E01.job-601.runner-kill", "kill", map[string]string{epOne + ".mkv": "video"})
	f.complete(dl, pub)
	p, files := f.publicationFiles(pub.Path)
	if p.State != "discovered" || files[epOne+".mkv"].Outcome == "placed" {
		t.Fatalf("after the interrupted copy: publication %s (%s), file %+v", p.State, p.Error, files[epOne+".mkv"])
	}
	if !strings.Contains(filepath.Base(stage), ".monarr-stage-") || !exists(stage) {
		t.Fatalf("fixture: no interrupted stage left (%q)", stage)
	}

	// The restarted process: the poll offers the publication again.
	placeFile = original
	f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	p, files = f.publicationFiles(pub.Path)
	placed := files[epOne+".mkv"]
	if placed.Outcome != "placed" || f.row(dl.ID).State != "imported" {
		t.Fatalf("retry: publication %s (%s), row %s, file %+v; want imported with no operator action", p.State, p.Error, f.row(dl.ID).State, placed)
	}
	if body, err := os.ReadFile(placed.LibraryPath); err != nil || string(body) != "video" {
		t.Fatalf("library file = %q (%v), want the whole payload", body, err)
	}
	if exists(stage) {
		t.Fatal("the interrupted stage was left behind")
	}
}

// Acceptance (Fable plan, Curator C): startup does not wait for the
// placement journal. Reconciliation runs in the background and waits only
// for the title it is reconciling; once that title is free, a published but
// uncommitted placement is committed by stat.
func TestPlacementReconciliationRunsInTheBackgroundPerTitle(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	if _, err := f.db.W.ExecContext(f.ctx, `CREATE TRIGGER fail_commit BEFORE INSERT ON media_files BEGIN SELECT RAISE(ABORT,'injected commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	pub := f.publish("Test.Show.S01E01.job-602.runner-reconcile", "reconcile", map[string]string{epOne + ".mkv": "video"})
	f.complete(dl, pub)
	pending, err := f.db.PendingPlacements(f.ctx)
	if err != nil || len(pending) != 1 || pending[0].State != "prepared" || pending[0].PublishedFileGeneration == "" {
		t.Fatalf("fixture: want one published, uncommitted placement; got %+v (%v)", pending, err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `DROP TRIGGER fail_commit`); err != nil {
		t.Fatal(err)
	}

	// The title is busy (an import of it is running).
	_, release := f.svc.lockImportTarget(f.ctx, pending[0].ItemID)
	workers, stop := context.WithCancel(context.Background())
	t.Cleanup(func() { stop(); f.svc.WaitImporters() })
	started := make(chan struct{})
	go func() { f.svc.StartImporters(workers); close(started) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("startup waited for the placement journal")
	}
	if p, _ := f.db.GetPlacement(f.ctx, pending[0].ID); p.State != "prepared" {
		release()
		t.Fatalf("a busy title's placement was reconciled under its import: %s", p.State)
	}
	// Another title is not held up by reconciliation (no whole-library lock).
	_, releaseOther := f.svc.lockImportTarget(f.ctx, pending[0].ItemID+1000)
	releaseOther()
	release()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, _ := f.db.GetPlacement(f.ctx, pending[0].ID)
		if p.State == "committed" || p.State == "cleaned" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("placement still %s after its title was released", p.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := f.count(`SELECT COUNT(*) FROM media_files WHERE path=?`, pending[0].Target); n != 1 {
		t.Fatalf("library rows for the reconciled target = %d, want 1", n)
	}
}

// Review: an attempt that keeps failing on the payload does not loop. Only a
// typed payload failure is counted, each is spaced on the retry ladder, and
// past MaxPublicationImportAttempts the publication ends import_failed and
// its want is grabbed again; an untyped error (a busy database) is not
// counted.
func TestRepeatedPayloadFailuresEndInARegrab(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	original := placeFile
	t.Cleanup(func() { placeFile = original })
	failWith := errors.New("injected busy database")
	placeFile = func(context.Context, string, string, func(int64, int64)) error { return failWith }
	pub := f.publish("Test.Show.S01E01.job-603.runner-loop", "loop", map[string]string{epOne + ".mkv": "video"})
	f.complete(dl, pub)
	for i := 0; i < sqlite.MaxPublicationImportAttempts+2; i++ {
		f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	}
	if n := f.count(`SELECT import_attempts FROM publications WHERE path=?`, pub.Path); n != 0 {
		t.Fatalf("an untyped error was counted %d time(s)", n)
	}
	failWith = fmt.Errorf("%w: injected", errPlacementConflict)
	f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	var retryAt int64
	_ = f.db.R.QueryRowContext(f.ctx, `SELECT retry_at FROM publications WHERE path=?`, pub.Path).Scan(&retryAt)
	if retryAt <= time.Now().UnixMilli() {
		t.Fatal("a counted attempt was not spaced on the retry ladder")
	}
	for i := 0; i < sqlite.MaxPublicationImportAttempts+1; i++ {
		if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publications SET retry_at=0`); err != nil { // the ladder's wait, elapsed
			t.Fatal(err)
		}
		f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	}
	// Ended (and its discarded file cleaned up): the row says why.
	row := f.row(dl.ID)
	if row.State != "failed" || !strings.Contains(row.Error, "attempts") {
		t.Fatalf("row = %s %q, want failed after the attempts ran out", row.State, row.Error)
	}
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs WHERE download_id=?`, dl.ID); n != 1 {
		t.Fatalf("replacement grabs %d, want the want grabbed again", n)
	}
}

// Review r2 (1): a library that cannot take the files holds the publication;
// its deadline never ends it, and the sweep never discards its media.
func TestEnvironmentHoldIsNeverEndedOrDiscarded(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	restore := failPlacement(t, syscall.ENOSPC)
	pub := f.publish("Test.Show.S01E01.job-606.runner-full", "full", map[string]string{epOne + ".mkv": "video"})
	f.complete(dl, pub)
	p, files := f.publicationFiles(pub.Path)
	if p.State != "discovered" || files[epOne+".mkv"].Outcome != "pending" {
		t.Fatalf("publication %s file %+v, want held pending", p.State, files[epOne+".mkv"])
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publications SET deadline_at=1`); err != nil {
		t.Fatal(err)
	}
	f.svc.sweepDeadlines(f.ctx, f.cfg, f.runner)
	if err := f.svc.CleanupPayloads(f.ctx); err != nil {
		t.Fatal(err)
	}
	p, files = f.publicationFiles(pub.Path)
	if p.State != "discovered" || f.count(`SELECT COUNT(*) FROM replacement_grabs`) != 0 || files[epOne+".mkv"].Outcome != "pending" || !exists(filepath.Join(pub.Path, epOne+".mkv")) {
		t.Fatalf("held publication: %s %q file %+v", p.State, p.Error, files[epOne+".mkv"])
	}
	// The library has room again: the next attempt imports it.
	restore()
	f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	if f.row(dl.ID).State != "imported" {
		t.Fatalf("after the hold cleared: row %s (%s)", f.row(dl.ID).State, f.row(dl.ID).Error)
	}
}

// Review r2 (3): a manifest that cannot be read for now (an I/O error, not
// a malformed manifest) holds the publication instead of ending it.
func TestUnreadableManifestHoldsThePublication(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-607.runner-eio", "eio", map[string]string{epOne + ".mkv": "video"})
	manifest := filepath.Join(pub.Path, ports.ManifestFileName)
	raw, _ := os.ReadFile(manifest)
	_ = os.Remove(manifest)
	if err := os.Mkdir(manifest, 0o755); err != nil { // reading it fails, and not with "no such file"
		t.Fatal(err)
	}
	f.complete(dl, pub)
	p, _ := f.publicationFiles(pub.Path)
	if p.State != "discovered" || f.count(`SELECT COUNT(*) FROM replacement_grabs`) != 0 {
		t.Fatalf("publication %s %q, want held for the environment", p.State, p.Error)
	}
	_ = os.Remove(manifest)
	if err := os.WriteFile(manifest, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	if f.row(dl.ID).State != "imported" {
		t.Fatalf("after the manifest came back: %s (%s)", f.row(dl.ID).State, f.row(dl.ID).Error)
	}
}

// Review r2 (5): a pack that placed some files before its import had to end
// records them (placed, notified), discards the rest, and grabs the rest of
// the want again.
func TestEndedPackRecordsWhatItPlaced(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(0, seasonPack)
	pub := f.publish("Test.Show.S01.job-608.runner-part", "part", map[string]string{"Test.Show.S01E01.mkv": "one", "Test.Show.S01E02.mkv": "two"})
	if err := os.WriteFile(filepath.Join(pub.Path, "Test.Show.S01E02.mkv"), []byte("TWO"), 0o644); err != nil { // same size, other bytes
		t.Fatal(err)
	}
	pub.CrossVolume = true // the linked copies are hashed once, so E02 is caught
	f.complete(dl, pub)
	p, files := f.publicationFiles(pub.Path)
	if files["Test.Show.S01E01.mkv"].Outcome != "placed" || files["Test.Show.S01E02.mkv"].Outcome != "discarded" {
		t.Fatalf("files %+v, want E01 placed and E02 discarded", files)
	}
	row := f.row(dl.ID)
	if last := row.Handoff[len(row.Handoff)-1]; row.State != "imported" || !strings.Contains(last.Detail, "the rest was not imported") {
		t.Fatalf("row %s (%q), publication %s, want imported with the rest named", row.State, last.Detail, p.State)
	}
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs WHERE download_id=?`, dl.ID); n != 1 {
		t.Fatalf("replacement grabs = %d, want the rest of the want grabbed again", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM media_files`); n != 1 {
		t.Fatalf("library files = %d", n)
	}
}

// Review r2 (6): a person's retry starts an ended publication over.
func TestApprovedRetryResetsAttemptsAndDeadline(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-609.runner-retry", "retry", map[string]string{epOne + ".mkv": "video"})
	restore := failPlacement(t, syscall.ENOSPC)
	f.complete(dl, pub)
	restore()
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publications SET state='import_failed', import_attempts=?, deadline_at=1, last_stop='payload'`, sqlite.MaxPublicationImportAttempts); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET state='failed' WHERE id=?`, dl.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ImportNow(f.ctx, dl.ID); err != nil {
		t.Fatal(err)
	}
	if f.row(dl.ID).State != "imported" {
		t.Fatalf("retry: row %s (%s), want imported", f.row(dl.ID).State, f.row(dl.ID).Error)
	}
}

// Review: a publication file that is not the manifest's size is never
// placed; the publication ends and its want is grabbed again.
func TestTruncatedPublicationFileIsRefused(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-604.runner-short", "short", map[string]string{epOne + ".mkv": "the whole video"})
	if err := os.Truncate(filepath.Join(pub.Path, epOne+".mkv"), 3); err != nil {
		t.Fatal(err)
	}
	f.complete(dl, pub)
	row := f.row(dl.ID)
	if row.State != "failed" || !strings.Contains(row.Error, "does not match its manifest") || f.count(`SELECT COUNT(*) FROM media_files`) != 0 {
		t.Fatalf("row = %s %q, want refused with nothing placed", row.State, row.Error)
	}
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs WHERE download_id=?`, dl.ID); n != 1 {
		t.Fatalf("replacement grabs = %d", n)
	}
}

// Review: a same-filesystem placement links Runner's file and never changes
// its mode (a chmod of an inode Curator does not own fails, and is Runner's
// to decide).
func TestLinkedPlacementNeverChmodsTheSource(t *testing.T) {
	t.Setenv(FileModeEnv, "0664")
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-605.runner-mode", "mode", map[string]string{epOne + ".mkv": "video"})
	src := filepath.Join(pub.Path, epOne+".mkv")
	if err := os.Chmod(src, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(src)
	f.holdCleanup()
	f.complete(dl, pub)
	_, files := f.publicationFiles(pub.Path)
	lib, err := os.Stat(files[epOne+".mkv"].LibraryPath)
	if err != nil || !os.SameFile(before, lib) {
		t.Fatalf("library %v (%v), want the linked source", lib, err)
	}
	if lib.Mode().Perm() != 0o600 {
		t.Fatalf("linked inode mode = %v, want Runner's 0600 untouched", lib.Mode().Perm())
	}
}

// Review r3 (3): a hold is retried on the ladder, not every poll, and is
// recorded once when it begins and once when it clears.
func TestEnvironmentHoldIsRecordedOnceAndBacksOff(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	restore := failPlacement(t, syscall.ENOSPC)
	pub := f.publish("Test.Show.S01E01.job-611.runner-once", "once", map[string]string{epOne + ".mkv": "video"})
	f.complete(dl, pub)
	var retryAt int64
	_ = f.db.R.QueryRowContext(f.ctx, `SELECT retry_at FROM publications WHERE path=?`, pub.Path).Scan(&retryAt)
	if retryAt <= time.Now().UnixMilli() {
		t.Fatal("a hold is offered again on the next poll; want the retry ladder")
	}
	for i := 0; i < 3; i++ {
		f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	}
	if n := f.count(`SELECT COUNT(*) FROM history_events WHERE type='import_held'`); n != 1 {
		t.Fatalf("import_held history rows = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM history_events WHERE type=?`, HistoryImportBlocked); n != 0 {
		t.Fatalf("import_blocked history rows = %d, want 0: a publication's hold is recorded as import_held only", n)
	}
	held := 0
	for _, h := range f.row(dl.ID).Handoff {
		if h.Step == stepImportHeld {
			held++
		}
	}
	if held != 1 {
		t.Fatalf("handoff import_held entries = %d, want 1", held)
	}
	restore()
	f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	if f.row(dl.ID).State != "imported" || f.count(`SELECT COUNT(*) FROM history_events WHERE type='import_hold_cleared'`) != 1 {
		t.Fatalf("after the hold cleared: row %s, hold_cleared rows %d", f.row(dl.ID).State, f.count(`SELECT COUNT(*) FROM history_events WHERE type='import_hold_cleared'`))
	}
}

// Review r3 (1): ending a publication that finished meanwhile writes
// nothing: no second history row, outbox entry or grab.
func TestEndingAFinishedPublicationWritesNothing(t *testing.T) {
	f := setupV2(t, nil)
	f.holdCleanup()
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-612.runner-race", "race", map[string]string{epOne + ".mkv": "video"})
	f.complete(dl, pub)
	p, _ := f.publicationFiles(pub.Path)
	if p.State != "imported" {
		t.Fatalf("fixture: %s", p.State)
	}
	history := f.count(`SELECT COUNT(*) FROM history_events WHERE type='imported'`)
	outbox := f.count(`SELECT COUNT(*) FROM notifier_deliveries`)
	stale := p
	stale.State = "importing" // what the sweep listed before the import finished
	f.svc.endPublication(f.ctx, f.row(dl.ID), f.cfg, stale, nil, "stalled")
	if f.count(`SELECT COUNT(*) FROM history_events WHERE type='imported'`) != history || f.count(`SELECT COUNT(*) FROM notifier_deliveries`) != outbox ||
		f.count(`SELECT COUNT(*) FROM replacement_grabs`) != 0 {
		t.Fatal("ending a finished publication wrote a second import, notification or grab")
	}
	if got, _ := f.publicationFiles(pub.Path); got.State != "imported" {
		t.Fatalf("publication = %s", got.State)
	}
}

// Review r3 (4): ending without being able to read the manifest holds the
// publication instead of forgetting what it placed.
func TestEndingWithAnUnreadableManifestHolds(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-613.runner-endio", "endio", map[string]string{epOne + ".mkv": "video"})
	restore := failPlacement(t, syscall.ENOSPC)
	f.complete(dl, pub)
	restore()
	manifest := filepath.Join(pub.Path, ports.ManifestFileName)
	_ = os.Remove(manifest)
	if err := os.Mkdir(manifest, 0o755); err != nil {
		t.Fatal(err)
	}
	p, _ := f.publicationFiles(pub.Path)
	f.svc.endPublication(f.ctx, f.row(dl.ID), f.cfg, p, nil, "stalled")
	p, _ = f.publicationFiles(pub.Path)
	if p.State != "discovered" || !p.Held() || f.count(`SELECT COUNT(*) FROM replacement_grabs`) != 0 {
		t.Fatalf("publication = %s held=%v %q, want held, not ended", p.State, p.Held(), p.Error)
	}
}

// Review r3 (5): a directory that never gets its manifest becomes a hold,
// shown like any other, after the grace.
func TestMissingManifestBecomesAHold(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-614.runner-nomf", "nomf", map[string]string{epOne + ".mkv": "video"})
	_ = os.Remove(filepath.Join(pub.Path, ports.ManifestFileName))
	f.complete(dl, pub)
	p, _ := f.publicationFiles(pub.Path)
	if p.Held() {
		t.Fatal("a publication mid-publish was held at once")
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publications SET discovered_at=? WHERE id=?`, time.Now().Add(-time.Hour).UnixMilli(), p.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	p, _ = f.publicationFiles(pub.Path)
	if !p.Held() || !strings.Contains(p.Error, "no "+ports.ManifestFileName) {
		t.Fatalf("publication held=%v %q, want held for its missing manifest", p.Held(), p.Error)
	}
}

// Follow-up (1): a hold for a missing manifest ends, and the want is grabbed
// again, after six hours.
func TestMissingManifestHoldEndsAfterSixHours(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-615.runner-nomf6", "nomf6", map[string]string{epOne + ".mkv": "video"})
	_ = os.Remove(filepath.Join(pub.Path, ports.ManifestFileName))
	f.complete(dl, pub)
	p, _ := f.publicationFiles(pub.Path)
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publications SET discovered_at=? WHERE id=?`, time.Now().Add(-8*time.Hour).UnixMilli(), p.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	if p, _ = f.publicationFiles(pub.Path); !p.Held() {
		t.Fatalf("publication %s held=%v, want held", p.State, p.Held())
	}
	// Five hours held: still waiting.
	_, _ = f.db.W.ExecContext(f.ctx, `UPDATE publications SET hold_since=?, retry_at=0 WHERE id=?`, time.Now().Add(-5*time.Hour).UnixMilli(), p.ID)
	f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	if p, _ = f.publicationFiles(pub.Path); !p.Held() || f.count(`SELECT COUNT(*) FROM replacement_grabs`) != 0 {
		t.Fatalf("held 5h: publication %s held=%v, want still held", p.State, p.Held())
	}
	_, _ = f.db.W.ExecContext(f.ctx, `UPDATE publications SET hold_since=?, retry_at=0 WHERE id=?`, time.Now().Add(-7*time.Hour).UnixMilli(), p.ID)
	f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	p, _ = f.publicationFiles(pub.Path)
	if p.State != "import_failed" || f.count(`SELECT COUNT(*) FROM replacement_grabs`) != 1 {
		t.Fatalf("held 7h: publication %s (%s), grabs %d, want import_failed and a regrab", p.State, p.Error, f.count(`SELECT COUNT(*) FROM replacement_grabs`))
	}
}

func (f *v2Fixture) heldByFullLibrary(t *testing.T, name, gen string) (sqlite.Download, ports.RunnerPublication, func()) {
	t.Helper()
	dl := f.grab(1, epOne)
	restore := failPlacement(t, syscall.ENOSPC)
	pub := f.publish(name, gen, map[string]string{epOne + ".mkv": "video"})
	f.complete(dl, pub)
	if p, _ := f.publicationFiles(pub.Path); !p.Held() {
		t.Fatalf("fixture: publication %s held=%v", p.State, p.Held())
	}
	return dl, pub, restore
}

// Follow-up (4): a hold that ends without completing is recorded once, and
// the stale error leaves the download row.
func TestHoldEndingWithoutCompletionIsRecordedOnce(t *testing.T) {
	for _, counted := range []bool{false, true} {
		f := setupV2(t, nil)
		dl, pub, _ := f.heldByFullLibrary(t, "Test.Show.S01E01.job-616.runner-end", "end")
		if got := f.row(dl.ID); !strings.HasPrefix(got.Error, "import held:") {
			t.Fatalf("fixture: row error %q", got.Error)
		}
		p, _ := f.publicationFiles(pub.Path)
		if _, err := f.db.StartPublicationImport(f.ctx, p.ID); err != nil {
			t.Fatal(err)
		}
		row := f.row(dl.ID)
		f.svc.returnPublication(f.ctx, &row, p, "boom", counted)
		f.svc.returnPublication(f.ctx, &row, p, "boom", counted)
		if n := f.count(`SELECT COUNT(*) FROM history_events WHERE type='import_hold_cleared'`); n != 1 {
			t.Fatalf("counted=%v: import_hold_cleared rows = %d, want 1", counted, n)
		}
		if got := f.row(dl.ID); strings.HasPrefix(got.Error, "import held:") {
			t.Fatalf("counted=%v: stale error left on the row: %q", counted, got.Error)
		}
		if p, _ = f.publicationFiles(pub.Path); p.Held() || p.State != "discovered" {
			t.Fatalf("counted=%v: publication %s held=%v", counted, p.State, p.Held())
		}
	}
}

// Follow-up (5): a held publication cancelled mid-retry is not held any more.
func TestCancelledRetryOfAHeldPublicationEndsTheHold(t *testing.T) {
	f := setupV2(t, nil)
	dl, pub, restore := f.heldByFullLibrary(t, "Test.Show.S01E01.job-617.runner-cancel", "cancel")
	restore()
	prev := placeFile
	placeFile = func(context.Context, string, string, func(int64, int64)) error { return context.Canceled }
	t.Cleanup(func() { placeFile = prev })
	p, _ := f.publicationFiles(pub.Path)
	_, _ = f.db.W.ExecContext(f.ctx, `UPDATE publications SET retry_at=0 WHERE id=?`, p.ID)
	f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	p, _ = f.publicationFiles(pub.Path)
	if p.State != "discovered" || p.Held() || p.LastStop != "" {
		t.Fatalf("publication %s held=%v last_stop=%q (%s), want discovered and not held", p.State, p.Held(), p.LastStop, p.Error)
	}
	if n := f.count(`SELECT COUNT(*) FROM history_events WHERE type='import_hold_cleared'`); n != 1 {
		t.Fatalf("import_hold_cleared rows = %d, want 1", n)
	}
}
