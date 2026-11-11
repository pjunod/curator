package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
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

// Review: an attempt that keeps failing on the payload does not loop. Past
// MaxPublicationImportAttempts the publication ends import_failed and its
// want is grabbed again; no one decides anything.
func TestRepeatedPayloadFailuresEndInARegrab(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	original := placeFile
	t.Cleanup(func() { placeFile = original })
	placeFile = func(context.Context, string, string, func(int64, int64)) error {
		return errors.New("injected payload failure")
	}
	pub := f.publish("Test.Show.S01E01.job-603.runner-loop", "loop", map[string]string{epOne + ".mkv": "video"})
	f.complete(dl, pub)
	for i := 0; i < sqlite.MaxPublicationImportAttempts+1; i++ {
		f.svc.kickPublicationImports(f.ctx, f.row(dl.ID), f.cfg)
	}
	p, _ := f.publicationFiles(pub.Path)
	if p.State != "import_failed" || !strings.Contains(p.Error, "attempts") {
		t.Fatalf("publication = %s %q, want import_failed after the attempts ran out", p.State, p.Error)
	}
	if n := f.count(`SELECT COUNT(*) FROM replacement_grabs WHERE download_id=?`, dl.ID); n != 1 || f.row(dl.ID).State != "failed" {
		t.Fatalf("row %s replacement grabs %d, want failed and grabbed again", f.row(dl.ID).State, n)
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
	p, _ := f.publicationFiles(pub.Path)
	if p.State != "import_failed" || !strings.Contains(p.Error, "does not match its manifest") || f.count(`SELECT COUNT(*) FROM media_files`) != 0 {
		t.Fatalf("publication = %s %q, want refused with nothing placed", p.State, p.Error)
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
