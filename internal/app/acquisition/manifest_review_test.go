package acquisition

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Review (M6b) #2: a publication made from a feed fact has no manifest body;
// the import that reads .runner-manifest.json records it as 'file', so the
// cleanup guard keeps requiring the file while files remain.
func TestImportRecordsTheFileManifestOnAFeedPublication(t *testing.T) {
	f := setupV2(t, nil)
	f.deferGrabs()
	dl := f.grab(1, epOne)
	pub := f.publish(epOne+".job-1.runner-m1", "m1", map[string]string{epOne + ".mkv": "video", "extra.nfo": "n"})
	// Keep the publication in the database after its import (no cleanup yet).
	id, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "m1", Kind: "completed", Path: pub.Path, RemotePath: pub.Path})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := f.db.GetPublication(f.ctx, id)
	if before.ManifestSource != "none" {
		t.Fatalf("feed publication source = %s, want none before the import", before.ManifestSource)
	}
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	after, _ := f.db.GetPublication(f.ctx, id)
	if after.ManifestSource != "file" || !strings.Contains(after.ManifestJSON, epOne+".mkv") {
		t.Fatalf("after import: source %s manifest %q, want the file's manifest recorded", after.ManifestSource, after.ManifestJSON)
	}
}

// Review (M6b) #1: a publication an earlier build stored a v1 body for
// falls back to it when its file is gone, so it drains.
func TestStoredManifestBodyIsTheFallback(t *testing.T) {
	f := setupV2(t, nil)
	pub := f.publish(epOne+".job-2.runner-m2", "m2", map[string]string{epOne + ".mkv": "video"})
	raw, err := os.ReadFile(filepath.Join(pub.Path, ports.ManifestFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(pub.Path, ports.ManifestFileName)); err != nil {
		t.Fatal(err)
	}
	m, ok, err := f.svc.publicationManifest(sqlite.Publication{Path: pub.Path, Generation: "m2", ManifestSource: "v1_record", ManifestJSON: string(raw)})
	if err != nil || !ok || len(m.Files) != 1 || m.Source != "v1_record" {
		t.Fatalf("fallback = %+v ok=%v err=%v", m, ok, err)
	}
	if _, ok, err := f.svc.publicationManifest(sqlite.Publication{Path: pub.Path, Generation: "m2", ManifestSource: "none", ManifestJSON: "{}"}); ok || err != nil {
		t.Fatalf("no file and no body: ok=%v err=%v, want not a publication yet", ok, err)
	}
}

// Review (M6b) #6: the manifest is read without following a symlink and
// with a size cap.
func TestManifestReadRefusesSymlinksAndOversize(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"generation":"g"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ports.ManifestFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifestFile(dir); err == nil {
		t.Fatal("a symlinked manifest was read")
	}
	big := t.TempDir()
	if err := os.WriteFile(filepath.Join(big, ports.ManifestFileName), make([]byte, maxManifestBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifestFile(big); !errors.Is(err, errManifestTooLarge) {
		t.Fatalf("oversize manifest: %v, want errManifestTooLarge", err)
	}
	if _, err := readManifestFile(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing directory: %v, want not-exist", err)
	}
}

// Review (M6b) #3: a legacy receipt status maps by prefix; a dead release is
// blamed (blocklisted), an unrecognised status is io, not blamed.
func TestLegacyReceiptStatusMapsByPrefix(t *testing.T) {
	for status, want := range map[string]struct {
		reason string
		blamed bool
	}{
		"FAILURE/HEALTH":  {ports.ReasonHealth, true},
		"FAILURE/FETCH":   {ports.ReasonFetch, true},
		"PAR_FAILURE":     {ports.ReasonPar, true},
		"UNPACK_FAILURE":  {ports.ReasonUnpack, true},
		"SCRIPT_FAILURE":  {ports.ReasonScript, true},
		"FAILURE/UNKNOWN": {ports.ReasonIO, false},
	} {
		tm, ok := admissionTerminal(ports.RequestAdmission{Outcome: "failed", TerminalOutcome: status})
		if !ok || tm.Reason != want.reason || tm.BlamesRelease != want.blamed {
			t.Errorf("%s -> %s blamed=%v, want %s blamed=%v", status, tm.Reason, tm.BlamesRelease, want.reason, want.blamed)
		}
	}
}

// Review (M6b) #2: only a record-only publication is labelled as having no
// manifest to clean from; a live one waiting for its manifest is not.
func TestInventoryLabelsOnlyRecordOnlyPublicationsAsManifestless(t *testing.T) {
	var live, record CompletedEntry
	live.Path, record.Path = "/c/p", "/c/q"
	classifyPublication(&live, sqlite.Publication{Path: "/c/p", DownloadID: 7, ManifestSource: "none", State: "imported", Cohort: "live"})
	classifyPublication(&record, sqlite.Publication{Path: "/c/q", ManifestSource: "none", State: "import_failed", Cohort: "live"})
	if strings.Contains(live.Reason, "no manifest") {
		t.Fatalf("live publication labelled manifestless: %q", live.Reason)
	}
	if !strings.Contains(record.Reason, "no manifest") {
		t.Fatalf("record-only publication not labelled: %q", record.Reason)
	}
}
