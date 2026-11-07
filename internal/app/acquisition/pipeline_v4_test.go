package acquisition

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Pipeline contract v2, M4 acceptance tests (plan M4 (c)-(g)): per-file,
// evidence-bound cleanup of publications. The fixture is M2's: a real
// database, reducer, importer and completed root, with a fake Runner.

// holdCleanup makes the post-import cleanup queue (with no worker draining
// it), so a test can change the world between the import and the cleanup;
// the hourly sweep then runs the same implementation.
func (f *v2Fixture) holdCleanup() {
	f.svc.importMu.Lock()
	f.svc.cleanupCh = make(chan importJob, 16)
	f.svc.importCtx = f.ctx
	f.svc.importMu.Unlock()
}

func (f *v2Fixture) publicationFiles(path string) (sqlite.Publication, map[string]sqlite.PublicationFile) {
	f.t.Helper()
	p, ok, err := f.db.PublicationByPath(f.ctx, path)
	if err != nil || !ok {
		f.t.Fatalf("publication %s: ok=%v err=%v", path, ok, err)
	}
	files, err := f.db.ListPublicationFiles(f.ctx, p.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]sqlite.PublicationFile{}
	for _, file := range files {
		out[file.Path] = file
	}
	return p, out
}

func (f *v2Fixture) complete(dl sqlite.Download, pub ports.RunnerPublication) {
	f.t.Helper()
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (f *v2Fixture) noRunnerCleanup() {
	f.t.Helper()
	f.runner.mu.Lock()
	defer f.runner.mu.Unlock()
	if len(f.runner.removed) != 0 {
		f.t.Fatalf("cleanup asked Runner to delete: %+v", f.runner.removed)
	}
}

// (c) A publication from a completed terminal imports, every file gets an
// outcome, the evidenced ones leave, and the directory goes — without asking
// Runner for anything.
func TestCompletedPublicationIsRemovedPerFileWithoutRunner(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-201.runner-c0ffee", "c0ffee", map[string]string{
		epOne + ".mkv": "the episode", "Sample/" + epOne + "-sample.mkv": "sample"})
	f.complete(dl, pub)
	if got := f.row(dl.ID); got.State != "imported" || !got.PayloadRemoved {
		t.Fatalf("row = %s payload_removed=%v (%s), want imported with its payload gone", got.State, got.PayloadRemoved, got.Error)
	}
	p, files := f.publicationFiles(pub.Path)
	if p.State != "removed" {
		t.Fatalf("publication = %s (%s), want removed", p.State, p.Error)
	}
	ep, sample := files[epOne+".mkv"], files["Sample/"+epOne+"-sample.mkv"]
	if ep.Outcome != "placed" || ep.RemovedAt.IsZero() || sample.Outcome != "discarded" || sample.RemovedAt.IsZero() {
		t.Fatalf("files = %+v / %+v, want placed and discarded, both removed", ep, sample)
	}
	if exists(pub.Path) {
		t.Fatal("the publication directory is still there")
	}
	body, err := os.ReadFile(ep.LibraryPath)
	if err != nil || string(body) != "the episode" {
		t.Fatalf("library file %s = %q (%v)", ep.LibraryPath, body, err)
	}
	f.noRunnerCleanup()
}

// (d) A partially selected season pack: the one authorized episode is placed
// and leaves; the other episodes are unresolved, retained, and keep the
// directory. A person discarding one of them removes exactly that file.
func TestPartiallySelectedSeasonPack(t *testing.T) {
	f := setupV2(t, nil)
	cfg := f.cfg
	cfg.ManualApproval = true
	if err := f.db.UpdateDownloadClient(f.ctx, cfg); err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	dl := f.grab(0, seasonPack)
	pub := f.publish("Test.Show.S01.job-202.runner-pack", "pack", map[string]string{
		"Test.Show.S01E01.mkv": "episode one", "Test.Show.S01E02.mkv": "episode two", "Test.Show.S01E03.mkv": "episode three"})
	f.complete(dl, pub)
	if got := f.row(dl.ID); got.State != "awaiting_import" {
		t.Fatalf("row = %s (%s), want awaiting_import under manual approval", got.State, got.Error)
	}
	p, _ := f.publicationFiles(pub.Path)
	// The person authorizes exactly one episode.
	if err := f.svc.ResolvePublicationFile(f.ctx, dl.ID, p.ID, "Test.Show.S01E01.mkv", "place"); err != nil {
		t.Fatal(err)
	}
	p, files := f.publicationFiles(pub.Path)
	if p.State != "imported" {
		t.Fatalf("publication = %s (%s), want imported", p.State, p.Error)
	}
	if e1 := files["Test.Show.S01E01.mkv"]; e1.Outcome != "placed" || e1.RemovedAt.IsZero() || exists(filepath.Join(pub.Path, "Test.Show.S01E01.mkv")) {
		t.Fatalf("E01 = %+v, want placed and removed", e1)
	}
	for _, name := range []string{"Test.Show.S01E02.mkv", "Test.Show.S01E03.mkv"} {
		if g := files[name]; g.Outcome != "unresolved" || !g.RemovedAt.IsZero() || !exists(filepath.Join(pub.Path, name)) {
			t.Fatalf("%s = %+v, want unresolved and retained", name, g)
		}
	}
	if !exists(filepath.Join(pub.Path, ports.ManifestFileName)) {
		t.Fatal("the directory (and its manifest) must stay while unresolved files remain")
	}
	// The sweep changes nothing: unresolved files have no evidence.
	if err := f.svc.CleanupPayloads(f.ctx); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(pub.Path, "Test.Show.S01E02.mkv")) {
		t.Fatal("the sweep deleted an unresolved file")
	}
	// "Delete this leftover" in Storage: discard E02 only.
	if err := f.svc.DiscardPublicationFiles(f.ctx, pub.Path, []CompletedFileSelection{{Path: filepath.Join(pub.Path, "Test.Show.S01E02.mkv")}}); err != nil {
		t.Fatal(err)
	}
	_, files = f.publicationFiles(pub.Path)
	if e2 := files["Test.Show.S01E02.mkv"]; e2.Outcome != "discarded" || e2.RemovedAt.IsZero() || exists(filepath.Join(pub.Path, "Test.Show.S01E02.mkv")) {
		t.Fatalf("E02 = %+v, want discarded and removed", e2)
	}
	if !exists(filepath.Join(pub.Path, "Test.Show.S01E03.mkv")) {
		t.Fatal("discarding E02 touched E03")
	}
	f.noRunnerCleanup()
}

// (e) The completed root's storage identity changed (an unmounted share):
// nothing is deleted, the publication stays imported and says why, and the
// next sweep after the storage is back removes it.
func TestStorageIdentityChangeDeletesNothing(t *testing.T) {
	f := setupV2(t, nil)
	f.holdCleanup()
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-203.runner-mnt", "mnt", map[string]string{epOne + ".mkv": "the episode"})
	f.complete(dl, pub)
	if got := f.row(dl.ID); got.State != "imported" {
		t.Fatalf("row = %s (%s)", got.State, got.Error)
	}
	original := observeStorageIdentity
	observeStorageIdentity = func(string) (string, error) { return "some-other-device", nil }
	t.Cleanup(func() { observeStorageIdentity = original })
	if err := f.svc.CleanupPayloads(f.ctx); err != nil {
		t.Fatal(err)
	}
	p, files := f.publicationFiles(pub.Path)
	if !exists(filepath.Join(pub.Path, epOne+".mkv")) || !files[epOne+".mkv"].RemovedAt.IsZero() {
		t.Fatal("a file was deleted from a root whose storage identity changed")
	}
	if p.State != "imported" || !strings.Contains(p.Error, "storage identity") {
		t.Fatalf("publication = %s %q, want imported with the storage reason", p.State, p.Error)
	}
	if files[epOne+".mkv"].Outcome != "placed" {
		t.Fatalf("a storage outage changed the file's evidence: %+v", files[epOne+".mkv"])
	}
	observeStorageIdentity = original
	if err := f.svc.CleanupPayloads(f.ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.publicationFiles(pub.Path); p.State != "removed" || exists(pub.Path) {
		t.Fatalf("after the storage came back: %s (%s)", p.State, p.Error)
	}
}

// (f) The library destination now holds a same-size file with different
// content: the publication's copy may be the only good one, so it is kept
// and shown in Resolve (digest, never size).
func TestSameSizeDifferentContentIsNotDeleted(t *testing.T) {
	f := setupV2(t, nil)
	f.holdCleanup()
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-204.runner-dig", "dig", map[string]string{epOne + ".mkv": "the episode"})
	f.complete(dl, pub)
	_, files := f.publicationFiles(pub.Path)
	lib := files[epOne+".mkv"].LibraryPath
	if lib == "" {
		t.Fatalf("not placed: %+v", files[epOne+".mkv"])
	}
	// A different inode with equal size and different bytes.
	if err := os.Remove(lib); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lib, []byte("THE EPISODE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.CleanupPayloads(f.ctx); err != nil {
		t.Fatal(err)
	}
	p, files := f.publicationFiles(pub.Path)
	src := filepath.Join(pub.Path, epOne+".mkv")
	if body, err := os.ReadFile(src); err != nil || string(body) != "the episode" {
		t.Fatalf("the publication file was deleted or changed: %q %v", body, err)
	}
	if g := files[epOne+".mkv"]; g.Outcome != "unresolved" || !strings.Contains(g.Reason, "differs from the manifest digest") {
		t.Fatalf("file = %+v, want unresolved with the digest reason", g)
	}
	if p.State == "removed" || !strings.Contains(p.Error, "kept 1 file") {
		t.Fatalf("publication = %s %q, want kept and surfaced", p.State, p.Error)
	}
	// It is no longer evidence, so later sweeps do not re-read it.
	if pubs, _ := f.db.PublicationsToSweep(f.ctx, 10); len(pubs) != 0 {
		t.Fatalf("the sweep would revisit %d publication(s)", len(pubs))
	}
}

// (g) A historical publication with every file placed survives the sweep
// until its review batch is approved; then the same rule removes it.
func TestHistoricalCohortWaitsForApproval(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-9.runner-hist", "hist", map[string]string{epOne + ".mkv": "video"})
	id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "hist", Kind: "completed", Path: pub.Path, Cohort: "historical"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET runner_outcome='completed', state='downloaded', import_path=?, save_path=? WHERE id=?`, pub.Path, pub.Path, dl.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.RefreshQueue(f.ctx); err != nil {
		t.Fatal(err)
	}
	p, files := f.publicationFiles(pub.Path)
	if p.State != "imported" || files[epOne+".mkv"].Outcome != "placed" {
		t.Fatalf("historical publication did not import: %s %+v (%s)", p.State, files, f.row(dl.ID).Error)
	}
	for i := 0; i < 2; i++ {
		if err := f.svc.CleanupPayloads(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !exists(filepath.Join(pub.Path, epOne+".mkv")) {
		t.Fatal("automatic cleanup deleted a historical publication before approval")
	}
	if err := f.svc.DiscardPublicationFiles(f.ctx, pub.Path, []CompletedFileSelection{{Path: filepath.Join(pub.Path, epOne+".mkv")}}); err == nil {
		t.Fatal("an operator discard bypassed the historical cohort")
	}
	n, err := f.svc.ApprovePublicationCleanup(f.ctx, []int64{id})
	if err != nil || n != 1 {
		t.Fatalf("approve = %d %v", n, err)
	}
	if p, _ := f.publicationFiles(pub.Path); p.State != "removed" || exists(pub.Path) {
		t.Fatalf("approved historical publication = %s (%s), want removed", p.State, p.Error)
	}
	f.noRunnerCleanup()
}

// A directory whose manifest now names another generation was replaced:
// nothing in it is this publication's to delete.
func TestReplacedManifestGenerationDeletesNothing(t *testing.T) {
	f := setupV2(t, nil)
	f.holdCleanup()
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-205.runner-gen1", "gen1", map[string]string{epOne + ".mkv": "the episode"})
	f.complete(dl, pub)
	f.publish("Test.Show.S01E01.job-205.runner-gen1", "gen2", map[string]string{epOne + ".mkv": "the episode"})
	if err := f.svc.CleanupPayloads(f.ctx); err != nil {
		t.Fatal(err)
	}
	p, _ := f.publicationFiles(pub.Path)
	if !exists(filepath.Join(pub.Path, epOne+".mkv")) || !strings.Contains(p.Error, "generation") {
		t.Fatalf("publication = %s %q: a replaced directory was cleaned", p.State, p.Error)
	}
}

