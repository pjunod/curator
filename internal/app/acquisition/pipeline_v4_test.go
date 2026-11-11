package acquisition

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// sweep runs the hourly pass and then the cleanup lane's queued jobs, as a
// cleanup worker would.
func (f *v2Fixture) sweep() {
	f.t.Helper()
	if err := f.svc.CleanupPayloads(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	f.drainCleanup()
}

func (f *v2Fixture) drainCleanup() {
	for {
		select {
		case job := <-f.svc.cleanupCh:
			f.svc.runImportOwned(job.ctx, job)
		default:
			return
		}
	}
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
	f.sweep()
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
	f.sweep()
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
	f.sweep()
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
	f.sweep()
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
	f.sweep()
	p, _ := f.publicationFiles(pub.Path)
	if !exists(filepath.Join(pub.Path, epOne+".mkv")) || !strings.Contains(p.Error, "generation") {
		t.Fatalf("publication = %s %q: a replaced directory was cleaned", p.State, p.Error)
	}
}

// entryFor returns the inventory entry for a publication directory after a
// fresh scan.
func (f *v2Fixture) entryFor(dir string) CompletedEntry {
	f.t.Helper()
	if err := f.svc.ScanCompleted(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	for _, e := range inventoryRoot(f.t, f.svc, f.root).Entries {
		if canonicalCompletedPath(e.Path) == canonicalCompletedPath(dir) {
			return e
		}
	}
	f.t.Fatalf("no inventory entry for %s", dir)
	return CompletedEntry{}
}

func (f *v2Fixture) importHeld(name, generation string, files map[string]string) ports.RunnerPublication {
	f.t.Helper()
	f.holdCleanup()
	dl := f.grab(1, epOne)
	pub := f.publish(name, generation, files)
	f.complete(dl, pub)
	if got := f.row(dl.ID); got.State != "imported" {
		f.t.Fatalf("row = %s (%s)", got.State, got.Error)
	}
	return pub
}

// Review finding 1: an accepted storage change records the new identity (so
// the identity check passes) and fences what was on the old volume. The
// per-file cleanup honours the fence, the inventory still calls the
// directory a publication, and the storage-review delete refuses it.
func TestStorageChangeFenceStopsAutomaticCleanup(t *testing.T) {
	f := setupV2(t, nil)
	pub := f.importHeld("Test.Show.S01E01.job-301.runner-fence", "fence", map[string]string{epOne + ".mkv": "the episode"})
	if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO storage_change_review_paths(review_token,path) VALUES('tok',?)`, pub.Path); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	src := filepath.Join(pub.Path, epOne+".mkv")
	p, files := f.publicationFiles(pub.Path)
	if !exists(src) || !files[epOne+".mkv"].RemovedAt.IsZero() || !strings.Contains(p.Error, "held for review") {
		t.Fatalf("fenced publication = %s %q: the sweep must delete nothing", p.State, p.Error)
	}
	e := f.entryFor(pub.Path)
	if !e.DeliveryManaged || !e.LegacyReview {
		t.Fatalf("fenced publication entry = %+v, want publication-managed and held for review", e)
	}
	err := f.svc.DeleteSelectedLegacyFiles(f.ctx, e.Path, e.Fingerprint, []CompletedFileSelection{{Path: src}})
	if err == nil || !exists(src) {
		t.Fatalf("the storage-review delete reached into a publication: %v", err)
	}
}

// The generation fence written by AcceptCompletedStorageChange (row-bound
// publications) is honoured too, not only path fences.
func TestStorageChangeGenerationFenceStopsCleanup(t *testing.T) {
	f := setupV2(t, nil)
	pub := f.importHeld("Test.Show.S01E01.job-302.runner-genf", "genf", map[string]string{epOne + ".mkv": "the episode"})
	p, _ := f.publicationFiles(pub.Path)
	dl := f.row(p.DownloadID)
	if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO storage_change_review_fences(generation_key,root) VALUES(?,?)`, dl.GenerationKey, canonicalCompletedPath(f.root)); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	if p, _ := f.publicationFiles(pub.Path); !exists(filepath.Join(pub.Path, epOne+".mkv")) || !strings.Contains(p.Error, "held for review") {
		t.Fatalf("generation-fenced publication = %s %q", p.State, p.Error)
	}
}

// A crash between the unlink and recording it: the next pass finds the file
// gone under verified storage and records it removed.
func TestCrashBetweenUnlinkAndRecordMarksRemoved(t *testing.T) {
	f := setupV2(t, nil)
	pub := f.importHeld("Test.Show.S01E01.job-303.runner-crash", "crash", map[string]string{epOne + ".mkv": "the episode"})
	if err := os.Remove(filepath.Join(pub.Path, epOne+".mkv")); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	p, files := f.publicationFiles(pub.Path)
	if files[epOne+".mkv"].RemovedAt.IsZero() || p.State != "removed" || exists(pub.Path) {
		t.Fatalf("after the replayed pass: %s %+v", p.State, files[epOne+".mkv"])
	}
}

// The generation is re-checked inside the destructive step, not only at the
// start of the pass: a directory replaced between the hash and the unlink
// keeps its file.
func TestPreUnlinkGenerationRecheck(t *testing.T) {
	f := setupV2(t, nil)
	name := "Test.Show.S01E01.job-304.runner-pre"
	pub := f.importHeld(name, "pre", map[string]string{epOne + ".mkv": "the episode"})
	publicationUnlinkHook = func(sqlite.Publication, string) {
		f.publish(name, "other", map[string]string{epOne + ".mkv": "the episode"})
	}
	t.Cleanup(func() { publicationUnlinkHook = func(sqlite.Publication, string) {} })
	f.sweep()
	p, files := f.publicationFiles(pub.Path)
	if !exists(filepath.Join(pub.Path, epOne+".mkv")) || !files[epOne+".mkv"].RemovedAt.IsZero() || !strings.Contains(p.Error, "generation") {
		t.Fatalf("publication = %s %q: the pre-unlink generation check did not hold", p.State, p.Error)
	}
}

// A placement that is a hard link of the publication file (same inode) is
// verified by that identity and one digest; removing the publication's name
// leaves the library's.
func TestHardlinkedPlacementKeepsLibraryBytes(t *testing.T) {
	f := setupV2(t, nil)
	pub := f.importHeld("Test.Show.S01E01.job-305.runner-link", "link", map[string]string{epOne + ".mkv": "the episode"})
	_, files := f.publicationFiles(pub.Path)
	lib, src := files[epOne+".mkv"].LibraryPath, filepath.Join(pub.Path, epOne+".mkv")
	li, _ := os.Stat(lib)
	si, _ := os.Stat(src)
	if !os.SameFile(li, si) {
		if err := os.Remove(lib); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(src, lib); err != nil {
			t.Skipf("hard links unsupported here: %v", err)
		}
	}
	f.sweep()
	if p, _ := f.publicationFiles(pub.Path); p.State != "removed" || exists(src) {
		t.Fatalf("hard-linked publication = %s (%s)", p.State, p.Error)
	}
	if body, err := os.ReadFile(lib); err != nil || string(body) != "the episode" {
		t.Fatalf("library bytes after removing the publication's name: %q %v", body, err)
	}
}

// A manifest entry without a digest is not evidence (size alone never is):
// the file is kept and becomes unresolved.
func TestDigestlessManifestFileIsKept(t *testing.T) {
	f := setupV2(t, nil)
	pub := f.importHeld("Test.Show.S01E01.job-306.runner-nodig", "nodig", map[string]string{epOne + ".mkv": "the episode"})
	raw, _ := json.Marshal(ports.Manifest{Generation: "nodig", Files: []ports.ManifestFile{{Path: epOne + ".mkv", Bytes: int64(len("the episode"))}}})
	if err := os.WriteFile(filepath.Join(pub.Path, ports.ManifestFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	_, files := f.publicationFiles(pub.Path)
	if g := files[epOne+".mkv"]; g.Outcome != "unresolved" || !strings.Contains(g.Reason, "size alone is not evidence") || !exists(filepath.Join(pub.Path, epOne+".mkv")) {
		t.Fatalf("digest-less file = %+v", g)
	}
}

// Review finding 2: a stray (.DS_Store, an NFS silly-rename) keeps the rmdir
// from finishing. The publication is not stranded: with every manifest file
// removed it may be deleted whole from Storage, even after its manifest is
// already gone.
func TestStrayFileLeavesPublicationDeletableWhole(t *testing.T) {
	f := setupV2(t, nil)
	pub := f.importHeld("Test.Show.S01E01.job-307.runner-stray", "stray", map[string]string{epOne + ".mkv": "the episode"})
	if err := os.WriteFile(filepath.Join(pub.Path, ".DS_Store"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	p, files := f.publicationFiles(pub.Path)
	if files[epOne+".mkv"].RemovedAt.IsZero() || p.State == "removed" || !strings.Contains(p.Error, ".DS_Store") {
		t.Fatalf("after the pass: %s %q", p.State, p.Error)
	}
	// The worst case: the manifest went too before the rmdir failed.
	_ = os.Remove(filepath.Join(pub.Path, ports.ManifestFileName))
	f.sweep()
	if p, _ := f.publicationFiles(pub.Path); strings.Contains(p.Error, "missing") {
		t.Fatalf("a removed manifest stranded the publication: %q", p.Error)
	}
	e := f.entryFor(pub.Path)
	if !e.WholeDeletable {
		t.Fatalf("entry = %+v, want deletable whole", e)
	}
	if err := f.svc.DeleteCompletedEntry(f.ctx, e.Path, e.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.publicationFiles(pub.Path); p.State != "removed" || exists(pub.Path) {
		t.Fatalf("after the whole delete: %s", p.State)
	}
}

// Review finding 3: a directory frozen with no manifest and no row is not
// "discovered" (nothing can import it); approved, it is deleted whole from
// Storage, and not before.
func TestApprovedManifestlessHistoricalDeletesWhole(t *testing.T) {
	f := setupV2(t, nil)
	old := filepath.Join(f.root, "Old.Payload")
	writeCompleted(t, filepath.Join(old, "a.mkv"), 3)
	past := time.Now().Add(-time.Hour)
	_ = os.Chtimes(old, past, past)
	if n, err := f.svc.MarkHistoricalCohort(f.ctx); err != nil || n != 1 {
		t.Fatalf("marked %d %v", n, err)
	}
	p, _ := f.publicationFiles(old)
	if p.State != "import_failed" || !strings.Contains(p.Error, "no manifest") {
		t.Fatalf("frozen directory = %s %q", p.State, p.Error)
	}
	e := f.entryFor(old)
	if e.WholeDeletable || f.svc.DeleteCompletedEntry(f.ctx, e.Path, e.Fingerprint) == nil || !exists(old) {
		t.Fatal("an unapproved historical directory was deletable")
	}
	if _, err := f.svc.ApprovePublicationCleanup(f.ctx, []int64{p.ID}); err != nil {
		t.Fatal(err)
	}
	if !exists(old) {
		t.Fatal("approval alone deleted a directory with no manifest evidence")
	}
	e = f.entryFor(old)
	if !e.WholeDeletable {
		t.Fatalf("approved entry = %+v", e)
	}
	if err := f.svc.DeleteCompletedEntry(f.ctx, e.Path, e.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.publicationFiles(old); p.State != "removed" || exists(old) {
		t.Fatalf("after the whole delete: %s", p.State)
	}
}

// Review findings 4 and 6: a pass that gets through clears an earlier stop;
// a Discard whose evidence does not hold says the file stayed. A non-media
// sidecar is discarded by the import itself, never left for Resolve.
func TestRecoveredPassClearsErrorAndDiscardReportsKept(t *testing.T) {
	f := setupV2(t, nil)
	pub := f.importHeld("Test.Show.S01E01.job-308.runner-nfo", "nfo", map[string]string{epOne + ".mkv": "the episode", epOne + ".nfo": "info", "notes.txt": "keep me?"})
	if _, files := f.publicationFiles(pub.Path); files[epOne+".nfo"].Outcome != "discarded" || files[epOne+".nfo"].DiscardReason != "rejected" {
		t.Fatalf("sidecar = %+v, want discarded{rejected} by the import", files[epOne+".nfo"])
	}
	original := observeStorageIdentity
	observeStorageIdentity = func(string) (string, error) { return "elsewhere", nil }
	f.sweep()
	observeStorageIdentity = original
	if p, _ := f.publicationFiles(pub.Path); !strings.Contains(p.Error, "storage identity") {
		t.Fatalf("error = %q", p.Error)
	}
	// The text file grew after the manifest: its size is no longer the
	// manifest's, so it stays (demoted to unresolved); everything else goes.
	if err := os.WriteFile(filepath.Join(pub.Path, "notes.txt"), []byte("keep me? edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	p, files := f.publicationFiles(pub.Path)
	if files[epOne+".mkv"].RemovedAt.IsZero() || files[epOne+".nfo"].RemovedAt.IsZero() || files["notes.txt"].Outcome != "unresolved" ||
		strings.Contains(p.Error, "storage identity") {
		t.Fatalf("after recovery: error %q files %+v", p.Error, files)
	}
	// Discarding it records the decision, keeps the file and says so.
	err := f.svc.ResolvePublicationFile(f.ctx, p.DownloadID, p.ID, "notes.txt", "discard")
	if !errors.Is(err, ErrDiscardedButKept) || !exists(filepath.Join(pub.Path, "notes.txt")) {
		t.Fatalf("discard = %v: want ErrDiscardedButKept and the file kept", err)
	}
}

// Review finding 5: a pass is queued on the cleanup lane where it is
// visible, and an operator can pause it; resume queues it again.
func TestPublicationCleanupCanBePausedAndResumed(t *testing.T) {
	f := setupV2(t, nil)
	pub := f.importHeld("Test.Show.S01E01.job-309.runner-pause", "pause", map[string]string{epOne + ".mkv": "the episode"})
	p, _ := f.publicationFiles(pub.Path)
	if state, why := f.svc.PublicationCleanupState(p.ID); state != "queued" || why != "after its import" {
		t.Fatalf("cleanup state = %q %q, want queued after its import", state, why)
	}
	if err := f.svc.PausePublicationCleanup(f.ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	f.drainCleanup()
	f.sweep()
	if !exists(filepath.Join(pub.Path, epOne+".mkv")) {
		t.Fatal("a paused publication was cleaned")
	}
	if pubs, _ := f.db.PublicationsToSweep(f.ctx, 10); len(pubs) != 0 {
		t.Fatal("the sweep would pick a paused publication")
	}
	if err := f.svc.PausePublicationCleanup(f.ctx, p.ID, false); err != nil {
		t.Fatal(err)
	}
	f.drainCleanup()
	if p, _ := f.publicationFiles(pub.Path); p.State != "removed" {
		t.Fatalf("after resume: %s (%s)", p.State, p.Error)
	}
}

// Review finding 7: a path inside a publication's directory is claimed by it.
func TestPublicationClaimsItsChildren(t *testing.T) {
	f := setupV2(t, nil)
	pub := f.importHeld("Test.Show.S01E01.job-310.runner-child", "child", map[string]string{"Sub/" + epOne + ".mkv": "the episode"})
	if !f.svc.publicationManaged(f.ctx, 0, filepath.Join(pub.Path, "Sub")) {
		t.Fatal("a directory inside a publication was not publication-managed")
	}
}
