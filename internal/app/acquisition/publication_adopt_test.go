package acquisition

import (
	"crypto/sha256"
	"encoding/hex"
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

var errFake = errors.New("runner unreachable")

// age makes a committed directory old enough to be an orphan.
func (f *v2Fixture) age(dir string) {
	f.t.Helper()
	old := time.Now().Add(-2 * orphanMinAge)
	if err := os.Chtimes(filepath.Join(dir, ports.ManifestFileName), old, old); err != nil {
		f.t.Fatal(err)
	}
}

// Field data 2026-11-12 (Runner jobs 2707, 2755): a job failed at publish
// with its salvage pending; Runner committed the directory later, but the
// fact found no row (or the feed moved past it), so no publication was ever
// made. The completed-root pass attributes the directory by the manifest's
// transfer to its row — whose terminal Runner gave — as a salvage, and
// imports it, exactly once.
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
	if adopted, orphaned, err := f.svc.adoptUnclaimedPublications(f.ctx); err != nil || adopted != 0 || orphaned != 0 {
		t.Fatalf("second pass = %d, %d, %v; want nothing", adopted, orphaned, err)
	}
	pub := ports.RunnerPublication{Path: dir, Generation: "236d505c", Kind: "salvage"}
	f.observe(dl, ports.RunnerFact{Kind: ports.FactPublicationReady, Transfer: dl.Transfer, Publication: &pub, Source: "deliveries 549"})
	if n := f.count(`SELECT COUNT(*) FROM publications WHERE path=?`, dir); n != 1 {
		t.Fatalf("publications for the directory = %d, want 1", n)
	}
}

// A row with no terminal yet is attributed only on Runner's word: an
// unreachable Runner or a receipt that is not terminal waits; a receipt
// that names this directory as the completed publication completes the row;
// a failed receipt records the failure and adopts the directory as its
// salvage. Nothing forges a terminal from the directory alone.
func TestAdoptionAsksRunnerBeforeAttributingAnOpenRow(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	dir := f.runnerDir(epOne+".job-5.runner-c1", "c1", dl.Transfer, 5, map[string]string{epOne + ".mkv": "video"})
	f.runner.admitErr = errFake
	if a, o, err := f.svc.adoptUnclaimedPublications(f.ctx); err != nil || a != 0 || o != 0 {
		t.Fatalf("unreachable Runner: %d, %d, %v; want a wait", a, o, err)
	}
	f.runner.admitErr = nil
	f.runner.admissions[dl.Transfer] = ports.RequestAdmission{RequestToken: dl.Transfer, JobID: "5", Outcome: "accepted"}
	if a, _, _ := f.svc.adoptUnclaimedPublications(f.ctx); a != 0 {
		t.Fatal("a receipt that is not terminal was taken as a completion")
	}
	if _, ok, _ := f.db.PublicationByPath(f.ctx, dir); ok {
		t.Fatal("a publication was made before Runner confirmed the terminal")
	}
	f.runner.admissions[dl.Transfer] = ports.RequestAdmission{RequestToken: dl.Transfer, JobID: "5", Outcome: "completed", PublicationPath: dir, SourceGeneration: "c1"}
	if a, _, err := f.svc.adoptUnclaimedPublications(f.ctx); err != nil || a != 1 {
		t.Fatalf("confirmed completion: %d, %v", a, err)
	}
	if p, ok, _ := f.db.PublicationByPath(f.ctx, dir); !ok || p.Kind != "completed" || p.DownloadID != dl.ID {
		t.Fatalf("publication = %+v, want the row's completed publication", p)
	}

	// Runner says the transfer failed: the failure is recorded and the
	// directory is its salvage.
	dl2 := f.grab(2, epTwo)
	dir2 := f.runnerDir(epTwo+".job-6.runner-c2", "c2", dl2.Transfer, 6, map[string]string{epTwo + ".mkv": "video"})
	f.runner.admissions[dl2.Transfer] = ports.RequestAdmission{RequestToken: dl2.Transfer, JobID: "6", Outcome: "failed", FailureReason: "publish",
		FailureCause: "publication file changed before commit", Salvage: "pending"}
	if a, _, err := f.svc.adoptUnclaimedPublications(f.ctx); err != nil || a != 1 {
		t.Fatalf("failed receipt: %d, %v", a, err)
	}
	if got := f.row(dl2.ID); got.State != "failed" {
		t.Fatalf("row = %s, want Runner's failure recorded", got.State)
	}
	if p, ok, _ := f.db.PublicationByPath(f.ctx, dir2); !ok || p.Kind != "salvage" {
		t.Fatalf("publication = %+v, want the salvage", p)
	}
}

// A committed directory whose Curator transfer token has no row left is an
// orphan only once it is old enough and Runner says the token is unknown or
// ended: recorded (visible) as a live publication whose files are not kept,
// and retired by the same hourly pass. A directory that names no Curator
// token, or has no manifest, is not Curator's and is left alone.
func TestAdoptionOrphanIsRecordedDiscardedAndRetired(t *testing.T) {
	f := setupV2(t, nil)
	orphan := f.runnerDir("Victor.Frankenstein.2015.job-2707.runner-aaa", "aaa", "t-0-0afae4913ed01f00", 2707, map[string]string{"v.mkv": "video"})
	young := f.runnerDir("Young.job-2800.runner-ddd", "ddd", "t-0-young", 2800, map[string]string{"y.mkv": "video"})
	running := f.runnerDir("Running.job-2801.runner-eee", "eee", "t-0-running", 2801, map[string]string{"r.mkv": "video"})
	foreign := f.runnerDir("Manual.Job.job-3000.runner-bbb", "bbb", "", 3000, map[string]string{"m.mkv": "video"})
	bare := filepath.Join(f.root, "not-a-publication")
	writeCompleted(t, filepath.Join(bare, "x.mkv"), 3)
	f.age(orphan)
	f.age(running)
	f.age(foreign)
	f.runner.admissions["t-0-0afae4913ed01f00"] = ports.RequestAdmission{RequestToken: "t-0-0afae4913ed01f00", JobID: "2707", Outcome: "failed", FailureReason: "publish", Salvage: "pending"}
	f.runner.admissions["t-0-running"] = ports.RequestAdmission{RequestToken: "t-0-running", JobID: "2801", Outcome: "accepted"}

	f.runner.admitErr = errFake
	if err := f.svc.CleanupPayloads(f.ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.db.PublicationByPath(f.ctx, orphan); ok {
		t.Fatal("an orphan was recorded while Runner was unreachable")
	}
	f.runner.admitErr = nil
	if err := f.svc.CleanupPayloads(f.ctx); err != nil {
		t.Fatal(err)
	}
	p, ok, err := f.db.PublicationByPath(f.ctx, orphan)
	if err != nil || !ok || p.DownloadID != 0 || p.State != "removed" || p.Cohort != "live" {
		t.Fatalf("orphan publication = %+v (%v %v)", p, ok, err)
	}
	files, err := f.db.ListPublicationFiles(f.ctx, p.ID)
	if err != nil || len(files) != 1 || files[0].Outcome != "discarded" || files[0].DiscardReason != "rejected" || files[0].RemovedAt.IsZero() {
		t.Fatalf("orphan files = %+v (%v), want discarded and retired", files, err)
	}
	if !strings.HasPrefix(files[0].Reason, orphanReasonPrefix) {
		t.Fatalf("orphan file reason = %q", files[0].Reason)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan directory still present: %v", err)
	}
	for _, dir := range []string{young, running, foreign, bare} {
		if _, ok, _ := f.db.PublicationByPath(f.ctx, dir); ok {
			t.Fatalf("%s was adopted; it is too young, still running, or not Curator's", dir)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s was touched: %v", dir, err)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM history_events WHERE type='publication_orphaned' AND data LIKE ?`, "%"+orphanReasonPrefix+"%"); n != 1 {
		t.Fatalf("orphan history events = %d, want 1", n)
	}
}

// pendingMediaFixture is an ended publication of the episode-1 grab whose
// obfuscated extra video an earlier build's import left pending.
func (f *v2Fixture) pendingMediaFixture(dl sqlite.Download, state string) int64 {
	f.t.Helper()
	info := f.publish(epOne+".job-1.runner-ccc", "ccc", map[string]string{"0ff7cab5.mkv": "extra", epOne + ".nfo": "n"})
	id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "ccc", Kind: "salvage", Path: info.Path, Source: "file"})
	if err != nil {
		f.t.Fatal(err)
	}
	m, _, err := f.svc.publicationManifest(f.ctx, f.cfg, sqlite.Publication{Path: info.Path, Generation: "ccc"})
	if err != nil {
		f.t.Fatal(err)
	}
	var seed []sqlite.PublicationFile
	for _, mf := range m.Files {
		seed = append(seed, sqlite.PublicationFile{Path: mf.Path, Bytes: mf.Bytes, SHA256: mf.SHA256})
	}
	if err := f.db.SeedPublicationFiles(f.ctx, id, seed); err != nil {
		f.t.Fatal(err)
	}
	if err := f.db.SetPublicationState(f.ctx, id, state, "1 file(s) unresolved: 0ff7cab5.mkv: cannot determine episodes"); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *v2Fixture) outcome(pub int64, path string) string {
	f.t.Helper()
	files, err := f.db.ListPublicationFiles(f.ctx, pub)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, file := range files {
		if file.Path == path {
			return file.Outcome
		}
	}
	return ""
}

// ADR 0026 decision 27: media an ended import left pending is imported
// again while its want is open (the requeue counts as an attempt, so it is
// bounded), discarded once its want is filled or a replacement imported it,
// and discarded when no attempt is left. The sidecar beside it is decided by
// the sweep as before.
func TestSweepDecidesMediaAnEndedImportLeftPending(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	id := f.pendingMediaFixture(dl, "import_failed")
	if err := f.svc.sweepPublications(f.ctx); err != nil {
		t.Fatal(err)
	}
	pub, err := f.db.GetPublication(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if pub.State != "discovered" || f.outcome(id, "0ff7cab5.mkv") != "pending" || f.outcome(id, epOne+".nfo") != "discarded" {
		t.Fatalf("open want: publication %s, media %s, sidecar %s; want requeued with the media pending", pub.State,
			f.outcome(id, "0ff7cab5.mkv"), f.outcome(id, epOne+".nfo"))
	}
	if n, _ := f.db.PublicationImportAttempts(f.ctx, id); n != 1 {
		t.Fatalf("attempts = %d, want the requeue counted", n)
	}

	// Out of attempts: discarded, so nothing waits.
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE publications SET state='import_failed', import_attempts=? WHERE id=?`, sqlite.MaxPublicationImportAttempts, id); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.sweepPublications(f.ctx); err != nil {
		t.Fatal(err)
	}
	if o := f.outcome(id, "0ff7cab5.mkv"); o != "discarded" {
		t.Fatalf("out of attempts: media = %s, want discarded", o)
	}
}

func TestSweepDiscardsPendingMediaWhoseWantAReplacementImported(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	id := f.pendingMediaFixture(dl, "import_failed")
	// A later download for the same want imported.
	if _, err := f.db.W.ExecContext(f.ctx, `INSERT INTO downloads(media_item_id,protocol,release_title,state,added_at,updated_at,transfer,generation_key,wantables,client_id)
		VALUES(?,'torrent','replacement','imported',?,?,'t-r','g-r',?,1)`, f.item, time.Now().UnixMilli()+1000, time.Now().UnixMilli(), `["`+dl.WantableIDs[0]+`"]`); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.sweepPublications(f.ctx); err != nil {
		t.Fatal(err)
	}
	pub, _ := f.db.GetPublication(f.ctx, id)
	if o := f.outcome(id, "0ff7cab5.mkv"); o != "discarded" || pub.State == "discovered" {
		t.Fatalf("satisfied want: media %s, publication %s; want discarded, not requeued", o, pub.State)
	}
}

// Every waiting publication has an exit: the hourly pass offers one whose
// client is an enabled Runner (it imports), and ends one whose client is
// disabled (nothing could ever offer it).
func TestHourlyPassOffersOrEndsEveryWaitingPublication(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	info := f.publish(epOne+".job-7.runner-f1", "f1", map[string]string{epOne + ".mkv": "video"})
	id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "f1", Kind: "salvage", Path: info.Path, Source: "file"})
	if err != nil {
		t.Fatal(err)
	}
	if offered, ended, err := f.svc.offerWaitingPublications(f.ctx); err != nil || offered != 1 || ended != 0 {
		t.Fatalf("enabled Runner: offered %d ended %d (%v)", offered, ended, err)
	}
	if p, _ := f.db.GetPublication(f.ctx, id); p.State != "imported" && p.State != "removed" {
		t.Fatalf("offered publication = %s (%s), want imported", p.State, p.Error)
	}

	dl2 := f.grab(2, epTwo)
	info2 := f.publish(epTwo+".job-8.runner-f2", "f2", map[string]string{epTwo + ".mkv": "video"})
	id2, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl2.ID, Generation: "f2", Kind: "completed", Path: info2.Path, Source: "file"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.W.ExecContext(f.ctx, `UPDATE download_clients SET enabled=0 WHERE id=?`, dl2.ClientID); err != nil {
		t.Fatal(err)
	}
	if _, ended, err := f.svc.offerWaitingPublications(f.ctx); err != nil || ended != 1 {
		t.Fatalf("disabled client: ended %d (%v), want 1", ended, err)
	}
	p, _ := f.db.GetPublication(f.ctx, id2)
	if p.State != "import_failed" && p.State != "removed" {
		t.Fatalf("publication of a disabled client = %s, want ended", p.State)
	}
	if f.outcome(id2, epTwo+".mkv") != "discarded" {
		t.Fatalf("its media = %s, want discarded", f.outcome(id2, epTwo+".mkv"))
	}
}

// Rows hidden for a pending salvage leave: Runner no longer knowing the
// receipt, or its salvage no longer pending, settles the row.
func TestPendingSalvageIsSettledFromRunner(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: "publish", Salvage: "pending"}, "history 9"))
	f.runner.admissions[dl.Transfer] = ports.RequestAdmission{RequestToken: dl.Transfer, Outcome: "failed", Salvage: "pending"}
	if n, err := f.svc.settlePendingSalvages(f.ctx); err != nil || n != 0 {
		t.Fatalf("still pending at Runner: settled %d (%v)", n, err)
	}
	delete(f.runner.admissions, dl.Transfer)
	if n, err := f.svc.settlePendingSalvages(f.ctx); err != nil || n != 1 {
		t.Fatalf("Runner no longer knows it: settled %d (%v)", n, err)
	}
	if got := f.row(dl.ID); got.RunnerSalvage != "none" {
		t.Fatalf("salvage = %q, want none", got.RunnerSalvage)
	}
}

// Production shape: the configured root is the working tree, and payloads
// sit in its completed container. Adoption reads the container's children.
func TestAdoptionReadsTheCompletedContainer(t *testing.T) {
	f := setupV2(t, nil)
	if err := os.MkdirAll(filepath.Join(f.root, "completed"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := f.runnerDir(filepath.Join("completed", "Good.Trouble.S04E10.job-2755.runner-98b8"), "98b8", "t-0-faec6d5a", 2755, map[string]string{"g.mkv": "video"})
	f.age(dir)
	adopted, orphaned, err := f.svc.adoptUnclaimedPublications(f.ctx)
	if err != nil || adopted != 0 || orphaned != 1 {
		t.Fatalf("adoption = %d, %d, %v; want the container's payload orphaned", adopted, orphaned, err)
	}
	if _, ok, _ := f.db.PublicationByPath(f.ctx, dir); !ok {
		t.Fatal("payload inside the completed container was not adopted")
	}
	if _, ok, _ := f.db.PublicationByPath(f.ctx, filepath.Join(f.root, "completed")); ok {
		t.Fatal("the completed container itself was recorded as a publication")
	}
}

// Publication 344 (field data 2026-11-12): a record-only historical
// publication at the completed container claimed every folder beside it. It
// claims nothing and its record is deleted (freeing the path, history kept);
// so is a record whose directory is gone from verified storage; a record
// whose directory is still there keeps waiting for its review. Nothing on
// disk is deleted. A path reached through an alias of the container is a
// container too.
func TestRecordOnlyPublicationsEndWhenTheEvidenceAllows(t *testing.T) {
	f := setupV2(t, nil)
	container := filepath.Join(f.root, "completed")
	sibling := filepath.Join(container, "Some.Payload.job-1.runner-aa")
	kept := filepath.Join(f.root, "Old.Payload")
	writeCompleted(t, filepath.Join(sibling, "a.mkv"), 3)
	writeCompleted(t, filepath.Join(kept, "b.mkv"), 3)
	gone := filepath.Join(f.root, "Gone.Payload")
	outside := filepath.Join(t.TempDir(), "failed", "Gone.Elsewhere")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "downloads")
	if err := os.Symlink(container, alias); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ScanCompleted(f.ctx); err != nil {
		t.Fatal(err)
	}
	roots, _ := f.svc.completedRoots(f.ctx, nil)
	if !payloadContainer(alias, roots) {
		t.Fatal("an alias of the completed container is not recognized as a container")
	}
	ids := map[string]int64{}
	for _, p := range []string{container, kept, gone, outside} {
		id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{Kind: "completed", Path: p, Cohort: "historical", Source: "none"})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.SetPublicationState(f.ctx, id, "import_failed", "pre-delivery acquisition (migration 0040 review cohort): no manifest and no download row"); err != nil {
			t.Fatal(err)
		}
		ids[p] = id
	}
	if _, claimed, err := f.svc.publicationClaiming(f.ctx, sibling); err != nil || claimed {
		t.Fatalf("the container record claims its sibling: claimed=%v err=%v", claimed, err)
	}
	closed, err := f.svc.closeRecordOnlyPublications(f.ctx)
	if err != nil || closed != 3 {
		t.Fatalf("closed = %d (%v), want the container, the gone and the outside records", closed, err)
	}
	for p, id := range ids {
		pub, err := f.db.GetPublication(f.ctx, id)
		if p == kept {
			if err != nil || pub.State != "import_failed" {
				t.Errorf("kept record = %+v (%v), want still waiting", pub, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s record still present (%s), want deleted", p, pub.State)
		}
	}
	for _, p := range []string{sibling, kept} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was touched: %v", p, err)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM history_events WHERE type='publication_closed'`); n != 3 {
		t.Fatalf("publication_closed events = %d, want 3", n)
	}
	// A Runner fact naming the container (or its alias) is refused.
	dl := f.grab(1, epOne)
	for _, path := range []string{container, alias} {
		pub := ports.RunnerPublication{Path: path, Generation: "zz", Kind: "completed"}
		f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
		if _, ok, _ := f.db.PublicationByPath(f.ctx, path); ok {
			t.Fatalf("a terminal made a publication at %s", path)
		}
	}
	if got := f.row(dl.ID); got.State == "downloaded" || got.State == "imported" {
		t.Fatalf("row = %s: a terminal naming the container was accepted", got.State)
	}
}
