package acquisition

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// runnerDir writes a committed Runner publication directory: its files and a
// .runner-manifest.json naming the job and transfer, as Runner writes it.
func (f *v2Fixture) runnerDir(name, generation, transfer string, job int64, files map[string]string) string {
	f.t.Helper()
	dir := filepath.Join(f.root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	var mf []ports.ManifestFile
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			f.t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(body))
		mf = append(mf, ports.ManifestFile{Path: rel, Bytes: int64(len(body)), SHA256: hex.EncodeToString(sum[:])})
	}
	digest, err := ports.ManifestDigest(mf)
	if err != nil {
		f.t.Fatal(err)
	}
	raw, _ := json.Marshal(runnerManifestFile{Generation: generation, Job: job, Transfer: transfer, Digest: digest, CrossVolume: true, Files: mf})
	if err := os.WriteFile(filepath.Join(dir, ports.ManifestFileName), raw, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

// Field data 2026-11-12 (Runner jobs 2707, 2755): a job failed at publish
// with its salvage pending; Runner committed the directory later, but the
// fact found no row (or the feed moved past it), so no publication was ever
// made. The completed-root pass attributes the directory by the manifest's
// transfer to its row, as a salvage, and imports it — exactly once.
func TestAdoptionAttributesUnclaimedDirectoryToItsRowOnce(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	if dl.Transfer == "" {
		t.Fatal("grab minted no transfer token")
	}
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: "publish", Message: "publication file changed before commit",
		Salvage: "pending"}, "history 7"))
	if got := f.row(dl.ID); got.State != "failed" {
		t.Fatalf("row = %s, want failed with its salvage pending", got.State)
	}
	dir := f.runnerDir(epOne+".job-2707.runner-236d505c", "236d505c", dl.Transfer, 2707, map[string]string{epOne + ".mkv": "video"})

	adopted, orphaned, err := f.svc.adoptUnclaimedPublications(f.ctx)
	if err != nil || adopted != 1 || orphaned != 0 {
		t.Fatalf("adoption = %d attributed, %d orphaned, %v; want 1, 0", adopted, orphaned, err)
	}
	p, ok, err := f.db.PublicationByPath(f.ctx, dir)
	if err != nil || !ok || p.DownloadID != dl.ID || p.Kind != "salvage" || p.Cohort != "live" {
		t.Fatalf("publication = %+v (%v %v), want a live salvage of download %d", p, ok, err, dl.ID)
	}
	if p.State != "imported" && p.State != "removed" { // removed: imported, then retired by its cleanup
		t.Fatalf("publication state = %s (%s), want imported", p.State, p.Error)
	}
	if n := f.count(`SELECT COUNT(*) FROM media_files`); n != 1 {
		t.Fatalf("media files = %d, want the salvage placed", n)
	}
	// Exactly once: the next pass, and a replay of the feed fact, find it.
	if adopted, orphaned, err := f.svc.adoptUnclaimedPublications(f.ctx); err != nil || adopted != 0 || orphaned != 0 {
		t.Fatalf("second pass = %d, %d, %v; want nothing", adopted, orphaned, err)
	}
	pub := ports.RunnerPublication{Path: dir, Generation: "236d505c", Kind: "salvage"}
	f.observe(dl, ports.RunnerFact{Kind: ports.FactPublicationReady, Transfer: dl.Transfer, Publication: &pub, Source: "deliveries 549"})
	if n := f.count(`SELECT COUNT(*) FROM publications WHERE path=?`, dir); n != 1 {
		t.Fatalf("publications for the directory = %d, want 1", n)
	}
}

// A committed directory whose Curator transfer token has no row left is an
// orphan: recorded (visible) as a live publication whose files are not kept,
// and retired by the same hourly pass. A directory that names no Curator
// token, or has no manifest, is not Curator's and is left alone.
func TestAdoptionOrphanIsRecordedDiscardedAndRetired(t *testing.T) {
	f := setupV2(t, nil)
	orphan := f.runnerDir("Victor.Frankenstein.2015.job-2707.runner-aaa", "aaa", "t-0-0afae4913ed01f00", 2707, map[string]string{"v.mkv": "video"})
	foreign := f.runnerDir("Manual.Job.job-3000.runner-bbb", "bbb", "", 3000, map[string]string{"m.mkv": "video"})
	bare := filepath.Join(f.root, "not-a-publication")
	writeCompleted(t, filepath.Join(bare, "x.mkv"), 3)

	if err := f.svc.CleanupPayloads(f.ctx); err != nil {
		t.Fatal(err)
	}
	p, ok, err := f.db.PublicationByPath(f.ctx, orphan)
	if err != nil || !ok || p.DownloadID != 0 || p.State != "removed" || p.Cohort != "live" {
		t.Fatalf("orphan publication = %+v (%v %v)", p, ok, err)
	}
	files, err := f.db.ListPublicationFiles(f.ctx, p.ID)
	if err != nil || len(files) != 1 || files[0].Outcome != "discarded" || files[0].DiscardReason != "rejected" {
		t.Fatalf("orphan files = %+v (%v), want discarded", files, err)
	}
	if files[0].RemovedAt.IsZero() {
		t.Fatalf("the orphan's file was not retired by the same pass: %+v", files[0])
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan directory still present: %v", err)
	}
	for _, dir := range []string{foreign, bare} {
		if _, ok, _ := f.db.PublicationByPath(f.ctx, dir); ok {
			t.Fatalf("%s was adopted; it is not Curator's", dir)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s was touched: %v", dir, err)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM history_events WHERE type='publication_orphaned' AND data LIKE ?`, "%"+orphanReasonPrefix+"%"); n != 1 {
		t.Fatalf("orphan history events = %d, want 1", n)
	}
	if !strings.HasPrefix(files[0].Reason, orphanReasonPrefix) {
		t.Fatalf("orphan file reason = %q", files[0].Reason)
	}
}

// Media an ended import left pending (an earlier build's import, or a stall
// that ended it before it reached the file) is decided by the cleanup pass:
// not selected, not kept. Nothing is left for a person.
func TestSweepDiscardsMediaAnEndedImportLeftPending(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	pubInfo := f.publish(epOne+".job-1.runner-ccc", "ccc", map[string]string{"0ff7cab5.mkv": "extra", epOne + ".nfo": "n"})
	id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "ccc", Kind: "salvage", Path: pubInfo.Path, Source: "file"})
	if err != nil {
		t.Fatal(err)
	}
	m, _, err := f.svc.publicationManifest(f.ctx, f.cfg, sqlite.Publication{Path: pubInfo.Path, Generation: "ccc"})
	if err != nil {
		t.Fatal(err)
	}
	var seed []sqlite.PublicationFile
	for _, mf := range m.Files {
		seed = append(seed, sqlite.PublicationFile{Path: mf.Path, Bytes: mf.Bytes, SHA256: mf.SHA256})
	}
	if err := f.db.SeedPublicationFiles(f.ctx, id, seed); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetPublicationState(f.ctx, id, "import_failed", "stalled: not imported within 6 h of its start"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.sweepPublications(f.ctx); err != nil {
		t.Fatal(err)
	}
	files, err := f.db.ListPublicationFiles(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.Outcome != "discarded" || file.RemovedAt.IsZero() {
			t.Errorf("%s = %s removed=%v, want discarded and removed", file.Path, file.Outcome, !file.RemovedAt.IsZero())
		}
	}
}
