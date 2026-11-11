package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Publication import paths (pipeline contract v2, section 3.4): where the
// manifest comes from, what refuses it, the per-file outcomes, the outbox
// written with the import, the historical cohort and Resolve.

// lookupRunner is a Runner that also answers a v1 delivery-record lookup.
type lookupRunner struct {
	*fakeRunner
	delivery ports.RunnerDelivery
	err      error
	calls    int
}

func (l *lookupRunner) GetDelivery(_ context.Context, id string) (ports.RunnerDelivery, error) {
	l.calls++
	if l.err != nil {
		return ports.RunnerDelivery{}, l.err
	}
	if id != l.delivery.DeliveryID {
		return ports.RunnerDelivery{}, errors.New("no such delivery")
	}
	return l.delivery, nil
}

// completedAt observes a completed terminal for a publication at dir.
func (f *v2Fixture) completedAt(dl sqlite.Download, dir, generation string) sqlite.Publication {
	f.t.Helper()
	pub := ports.RunnerPublication{Path: dir, Generation: generation, Kind: "completed"}
	f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
	p, ok, err := f.db.PublicationByPath(f.ctx, dir)
	if err != nil || !ok {
		f.t.Fatalf("publication at %s: ok=%v err=%v", dir, ok, err)
	}
	return p
}

// What refuses a manifest before anything is placed.
func TestPublicationManifestRefusals(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(f *v2Fixture, dir string)
		want    string
	}{
		{name: "a manifest file that is not JSON",
			prepare: func(f *v2Fixture, dir string) {
				writeCompleted(f.t, filepath.Join(dir, epOne+".mkv"), 5)
				if err := os.WriteFile(filepath.Join(dir, ports.ManifestFileName), []byte("{not json"), 0o644); err != nil {
					f.t.Fatal(err)
				}
			},
			want: "is not a manifest"},
		{name: "a manifest for another generation",
			prepare: func(f *v2Fixture, dir string) {
				f.publish(filepath.Base(dir), "someone-else", map[string]string{epOne + ".mkv": "video"})
			},
			want: "does not match publication generation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setupV2(t, nil)
			dl := f.grab(1, epOne)
			dir := filepath.Join(f.root, "Test.Show.S01E01.job-101.runner-ref")
			tc.prepare(f, dir)
			p := f.completedAt(dl, dir, "ref")
			got := f.row(dl.ID)
			if p.State != "import_failed" || !strings.Contains(p.Error, "manifest refused") || !strings.Contains(p.Error, tc.want) {
				t.Fatalf("publication = %s %q, want import_failed naming %q", p.State, p.Error, tc.want)
			}
			if got.State != "failed" || f.count(`SELECT COUNT(*) FROM media_files`) != 0 {
				t.Fatalf("row = %s with %d library files, want failed and nothing placed", got.State, f.count(`SELECT COUNT(*) FROM media_files`))
			}
		})
	}
}

// A publication with no manifest file but a stored v1 record that names its
// delivery is looked up on Runner; the record decides the import.
func TestV1RecordLookedUpByDeliveryID(t *testing.T) {
	setupLookup := func(t *testing.T) (*v2Fixture, *lookupRunner, sqlite.Download, string) {
		f := setupV2(t, nil)
		lr := &lookupRunner{fakeRunner: f.runner}
		f.svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return lr }
		dl := f.grab(1, epOne)
		pub := f.publish("Test.Show.S01E01.job-101.runner-lkp", "lkp", map[string]string{epOne + ".mkv": "video"})
		m := mustReadManifest(t, pub.Path)
		_ = os.Remove(filepath.Join(pub.Path, ports.ManifestFileName))
		lr.delivery = ports.RunnerDelivery{DeliveryID: "d-77", PublicationGeneration: "lkp", ManifestDigest: m.Digest}
		for _, file := range m.Files {
			lr.delivery.Files = append(lr.delivery.Files, ports.RunnerFile{Path: file.Path, SizeBytes: file.Bytes, SHA256: file.SHA256})
		}
		// What the deliveries cache stored: the delivery id, no files yet.
		if _, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{DownloadID: dl.ID, Generation: "lkp", Kind: "completed", Path: pub.Path,
			ManifestJSON: `{"generation":"lkp","delivery_id":"d-77"}`, Source: "v1_record", Cohort: "live"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.W.ExecContext(f.ctx, `UPDATE downloads SET state='downloaded', runner_outcome='completed', import_path=?, save_path=? WHERE id=?`, pub.Path, pub.Path, dl.ID); err != nil {
			t.Fatal(err)
		}
		return f, lr, dl, pub.Path
	}
	t.Run("the record imports", func(t *testing.T) {
		f, lr, dl, path := setupLookup(t)
		if err := f.svc.RefreshQueue(f.ctx); err != nil {
			t.Fatal(err)
		}
		p, _, _ := f.db.PublicationByPath(f.ctx, path)
		if lr.calls == 0 || p.State != "imported" || f.row(dl.ID).State != "imported" {
			t.Fatalf("lookups=%d publication %s %q row %s, want imported from Runner's record", lr.calls, p.State, p.Error, f.row(dl.ID).State)
		}
	})
	t.Run("an unreachable Runner leaves it to retry", func(t *testing.T) {
		f, lr, dl, path := setupLookup(t)
		lr.err = errors.New("runner unreachable")
		if err := f.svc.RefreshQueue(f.ctx); err != nil {
			t.Fatal(err)
		}
		p, _, _ := f.db.PublicationByPath(f.ctx, path)
		if p.State != "import_failed" || !strings.Contains(p.Error, "runner unreachable") || f.row(dl.ID).State == "imported" {
			t.Fatalf("publication = %s %q, want the lookup failure recorded", p.State, p.Error)
		}
	})
	t.Run("a record for another generation is refused", func(t *testing.T) {
		f, lr, _, path := setupLookup(t)
		lr.delivery.PublicationGeneration = "other"
		if err := f.svc.RefreshQueue(f.ctx); err != nil {
			t.Fatal(err)
		}
		p, _, _ := f.db.PublicationByPath(f.ctx, path)
		if p.State != "import_failed" || !strings.Contains(p.Error, "does not match publication generation") {
			t.Fatalf("publication = %s %q, want the record refused", p.State, p.Error)
		}
	})
	t.Run("a stored record of the wrong generation is refused", func(t *testing.T) {
		f := setupV2(t, nil)
		dl := f.grab(1, epOne)
		dir := filepath.Join(f.root, "Test.Show.S01E01.job-101.runner-v1x")
		writeCompleted(t, filepath.Join(dir, epOne+".mkv"), 5)
		m := ports.Manifest{Generation: "other", Files: []ports.ManifestFile{{Path: epOne + ".mkv", Bytes: 5}}, Source: "v1_record"}
		pub := ports.RunnerPublication{Path: dir, Generation: "v1x", Kind: "completed", Manifest: &m}
		f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "deliveries 1"))
		p, _, _ := f.db.PublicationByPath(f.ctx, dir)
		if p.State != "import_failed" || !strings.Contains(p.Error, "manifest refused") {
			t.Fatalf("publication = %s %q, want the stored record refused", p.State, p.Error)
		}
	})
}

// A cross-volume publication is verified once against its manifest, by the
// placement (in its copy stream, or one read of a same-filesystem link).
func TestCrossVolumeCopyIsVerified(t *testing.T) {
	for _, tc := range []struct {
		name    string
		corrupt func(path string)
		want    string
	}{
		{name: "intact copy imports"},
		{name: "a changed byte fails it", corrupt: func(p string) { _ = os.WriteFile(p, []byte("vidźo"), 0o644) }, want: "no longer matches its accepted delivery manifest"},
		{name: "a missing file fails it", corrupt: func(p string) { _ = os.Remove(p) }, want: epOne + ".mkv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupV2(t, nil)
			dl := f.grab(1, epOne)
			pub := f.publish("Test.Show.S01E01.job-101.runner-xv", "xv", map[string]string{epOne + ".mkv": "video"})
			pub.CrossVolume = true
			if tc.corrupt != nil {
				tc.corrupt(filepath.Join(pub.Path, epOne+".mkv"))
			}
			f.observe(dl, terminalFact(dl, ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Publication: &pub, Salvage: "none"}, "event 1"))
			p, _, _ := f.db.PublicationByPath(f.ctx, pub.Path)
			if tc.want == "" {
				// The verified copy is placement evidence, so the per-file
				// cleanup (M4) removes the publication once it imports.
				if p.State != "removed" || !p.CrossVolume || f.count(`SELECT COUNT(*) FROM media_files`) != 1 {
					t.Fatalf("publication = %s %q cross=%v, want imported and cleaned up with the library copy kept", p.State, p.Error, p.CrossVolume)
				}
				return
			}
			if p.State != "import_failed" || !strings.Contains(p.Error, tc.want) || f.count(`SELECT COUNT(*) FROM media_files`) != 0 {
				t.Fatalf("publication = %s %q, want import_failed naming %q and nothing placed", p.State, p.Error, tc.want)
			}
		})
	}
}

// A publication outside every completed root is refused, not imported from
// wherever it happens to be.
func TestPublicationOutsideTheCompletedRootsFails(t *testing.T) {
	f := setupV2(t, nil)
	dl := f.grab(1, epOne)
	outside := t.TempDir()
	dir := filepath.Join(outside, "Test.Show.S01E01.job-101.runner-out")
	writeCompleted(t, filepath.Join(dir, epOne+".mkv"), 5)
	f.root = outside
	f.publish(filepath.Base(dir), "out", map[string]string{epOne + ".mkv": "video"})
	p := f.completedAt(dl, dir, "out")
	if p.State != "import_failed" || f.count(`SELECT COUNT(*) FROM media_files`) != 0 {
		t.Fatalf("publication = %s %q, want refused outside the completed roots", p.State, p.Error)
	}
}

// ManualApproval: the row waits for a person, and ImportNow imports exactly
// that publication.
func TestManualApprovalHoldsThePublication(t *testing.T) {
	f := setupV2(t, nil)
	cfg := f.cfg
	cfg.ManualApproval = true
	if err := f.db.UpdateDownloadClient(f.ctx, cfg); err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-101.runner-ok", "ok", map[string]string{epOne + ".mkv": "video"})
	p := f.completedAt(dl, pub.Path, "ok")
	if got := f.row(dl.ID); got.State != "awaiting_import" || p.State != "discovered" {
		t.Fatalf("row %s publication %s, want awaiting approval with nothing imported", got.State, p.State)
	}
	if f.count(`SELECT COUNT(*) FROM media_files`) != 0 {
		t.Fatal("a held publication was imported")
	}
	if err := f.svc.ImportNow(f.ctx, dl.ID); err != nil {
		t.Fatal(err)
	}
	p, _ = f.db.GetPublication(f.ctx, p.ID)
	// Imported, then removed by the per-file cleanup (M4).
	if got := f.row(dl.ID); got.State != "imported" || p.State != "removed" {
		t.Fatalf("after approval row %s publication %s (%s), want imported and the publication cleaned up", got.State, p.State, got.Error)
	}
}

// Per-file outcomes for what the importer does not place automatically.
func TestPublicationFileOutcomes(t *testing.T) {
	t.Run("samples and sidecars only is no importable media", func(t *testing.T) {
		f := setupV2(t, nil)
		dl := f.grab(1, epOne)
		pub := f.publish("Test.Show.S01E01.job-101.runner-smp", "smp", map[string]string{
			"sample/" + epOne + "-sample.mkv": "s", epOne + ".nfo": "info", epOne + ".en.srt": "subs"})
		p := f.completedAt(dl, pub.Path, "smp")
		files, _ := f.db.ListPublicationFiles(f.ctx, p.ID)
		out := map[string]string{}
		for _, file := range files {
			out[file.Path] = file.Outcome + "/" + file.DiscardReason
		}
		if p.State != "import_failed" || !strings.Contains(p.Error, "no importable media") || f.row(dl.ID).State != "failed" {
			t.Fatalf("publication = %s %q row %s", p.State, p.Error, f.row(dl.ID).State)
		}
		if len(files) != 3 || out["sample/"+epOne+"-sample.mkv"] != "pending/" {
			t.Fatalf("files = %v, want every manifest file seeded and left for Resolve", out)
		}
	})
	t.Run("a non-media file beside the media is discarded, the media placed", func(t *testing.T) {
		f := setupV2(t, nil)
		dl := f.grab(1, epOne)
		pub := f.publish("Test.Show.S01E01.job-101.runner-txt", "txt", map[string]string{epOne + ".mkv": "video", "setup.exe": "hello",
			"sample/" + epOne + "-sample.mkv": "s"})
		p := f.completedAt(dl, pub.Path, "txt")
		files, _ := f.db.ListPublicationFiles(f.ctx, p.ID)
		got := map[string]sqlite.PublicationFile{}
		for _, file := range files {
			got[file.Path] = file
		}
		if got[epOne+".mkv"].Outcome != "placed" || got[epOne+".mkv"].PlacedSHA256 == "" {
			t.Fatalf("media = %+v, want placed with its digest", got[epOne+".mkv"])
		}
		if txt := got["setup.exe"]; txt.Outcome != "discarded" || txt.DiscardReason != "rejected" || !strings.Contains(txt.Reason, "not media") {
			t.Fatalf("setup.exe = %+v, want discarded{rejected} as not media (never left for Resolve)", txt)
		}
		if s := got["sample/"+epOne+"-sample.mkv"]; s.Outcome != "discarded" || s.DiscardReason != "rejected" {
			t.Fatalf("sample = %+v, want discarded{rejected}", s)
		}
		if p.State != "removed" || f.row(dl.ID).State != "imported" {
			t.Fatalf("publication %s row %s, want imported and cleaned up (non-media does not block)", p.State, f.row(dl.ID).State)
		}
	})
	t.Run("a file the library already holds at equal quality is discarded as a duplicate", func(t *testing.T) {
		f := setupV2(t, nil)
		first := f.grab(1, epOne)
		pub := f.publish("Test.Show.S01E01.job-101.runner-one", "one", map[string]string{epOne + ".mkv": "video"})
		f.completedAt(first, pub.Path, "one")
		second := f.grab(1, epOne)
		if second.ID == first.ID {
			t.Fatalf("second grab of %q returned the settled row %d; an imported acquisition must not satisfy the in-flight guard — want a new download row", epOne, first.ID)
		}
		again := f.publish("Test.Show.S01E01.job-102.runner-two", "two", map[string]string{epOne + ".mkv": "video"})
		p := f.completedAt(second, again.Path, "two")
		files, _ := f.db.ListPublicationFiles(f.ctx, p.ID)
		if len(files) != 1 || files[0].Outcome != "discarded" || files[0].DiscardReason != "duplicate" {
			t.Fatalf("files = %+v, want discarded{duplicate}", files)
		}
		// A discarded file is cleanup evidence too (M4): the settled
		// publication is removed.
		if p.State != "removed" || f.row(second.ID).State != "imported" {
			t.Fatalf("publication %s row %s, want settled with nothing placed and cleaned up", p.State, f.row(second.ID).State)
		}
		if last := f.row(second.ID).Handoff; !strings.Contains(last[len(last)-1].Detail, "nothing placed") {
			t.Fatalf("handoff = %+v, want the nothing-placed detail", last[len(last)-1])
		}
	})
}

// The import's outbox rows are written in its own transaction, only for
// enabled, import-subscribed notifiers: a queued notifier (plurx) gets a keyed
// sub-delivery per directory, any other one a single one-shot row.
func TestImportWritesTheOutboxInItsTransaction(t *testing.T) {
	f := setupV2(t, nil)
	ids := map[string]int64{}
	for _, n := range []ports.NotifierConfig{
		{Type: "plurx", Name: "cinema", OnImport: true, Enabled: true, Settings: map[string]string{"url": "http://plurx"}},
		{Type: "plurx", Name: "off", OnImport: true, Enabled: false, Settings: map[string]string{"url": "http://plurx"}},
		{Type: "plurx", Name: "grab-only", OnGrab: true, Enabled: true, Settings: map[string]string{"url": "http://plurx"}},
		{Type: "webhook", Name: "chat", OnImport: true, Enabled: true, Settings: map[string]string{"url": "http://hook"}},
	} {
		id, err := f.db.AddNotifier(f.ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		ids[n.Name] = id
	}
	dl := f.grab(1, epOne)
	pub := f.publish("Test.Show.S01E01.job-101.runner-obx", "obx", map[string]string{epOne + ".mkv": "video"})
	f.completedAt(dl, pub.Path, "obx")
	if f.row(dl.ID).State != "imported" {
		t.Fatalf("row = %s", f.row(dl.ID).State)
	}
	if n := f.count(`SELECT COUNT(*) FROM notifier_deliveries WHERE download_id=?`, dl.ID); n != 2 {
		t.Fatalf("outbox rows = %d, want two: the plurx sub-delivery and the webhook's one-shot row", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM notifier_deliveries WHERE download_id=? AND notifier_id=? AND idempotency_key != ''`, dl.ID, ids["cinema"]); n != 1 {
		t.Fatalf("plurx keyed sub-deliveries = %d, want one for the one directory", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM notifier_deliveries WHERE download_id=? AND notifier_id=? AND idempotency_key = ''`, dl.ID, ids["chat"]); n != 1 {
		t.Fatalf("webhook one-shot rows = %d, want one, unkeyed", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM notifier_deliveries WHERE download_id=? AND notifier_id IN (?, ?)`, dl.ID, ids["off"], ids["grab-only"]); n != 0 {
		t.Fatalf("rows for the disabled or grab-only notifier = %d, want none", n)
	}
}

// The notification a book import sends carries its book identity only when
// it has all of it.
func TestImportNotificationShape(t *testing.T) {
	full := ImportCompleted{MediaItemID: 3, DownloadID: 9, Release: "A Book", Files: 2, Upgrade: true, MediaItemKind: "book",
		BookTitle: "Dune", BookAuthor: "Herbert", BookMedium: "ebook", BookWorkID: "W1", BookEditionID: "E1", BookCoverURL: "http://c"}
	n := ImportNotification(full)
	if n.Event != "import" || n.Body != "A Book" || n.Fields["files"] != "2" || n.Fields["upgrade"] != "true" {
		t.Fatalf("notification = %+v", n)
	}
	if n.Import == nil || n.Import.Book == nil || n.Import.Book.Title != "Dune" || n.Import.Book.EditionID != "E1" || n.Import.DownloadID != 9 {
		t.Fatalf("import info = %+v, want the book identity", n.Import)
	}
	partial := full
	partial.BookEditionID = ""
	if n := ImportNotification(partial); n.Import.Book != nil {
		t.Fatalf("a book without its edition carried book info: %+v", n.Import.Book)
	}
}

// The wantable ids a placed file answers to.
func TestWantableKeys(t *testing.T) {
	cases := []struct {
		item     domain.MediaItem
		copyID   int64
		season   int
		episodes []int
		keys     []string
		primary  string
	}{
		{domain.MediaItem{ID: 4, Kind: domain.KindMovie}, 0, 0, nil, []string{"movie:4"}, "movie:4"},
		{domain.MediaItem{ID: 4, Kind: domain.KindMovie}, 7, 0, nil, []string{"movie:4:c7"}, "movie:4:c7"},
		{domain.MediaItem{ID: 5, Kind: domain.KindBook}, 0, 0, nil, []string{"book:5"}, "book:5"},
		{domain.MediaItem{ID: 6, Kind: domain.KindSeries}, 0, 2, []int{3, 4}, []string{"episode:6:2:3", "episode:6:2:4", "season:6:2"}, "episode:6:2:3"},
		{domain.MediaItem{ID: 6, Kind: domain.KindSeries}, 2, 1, nil, []string{"season:6:1:c2"}, ""},
	}
	for _, tc := range cases {
		keys, primary := wantableKeys(tc.item, tc.copyID, tc.season, tc.episodes)
		if strings.Join(keys, ",") != strings.Join(tc.keys, ",") || primary != tc.primary {
			t.Errorf("wantableKeys(%s %d) = %v %q, want %v %q", tc.item.Kind, tc.item.ID, keys, primary, tc.keys, tc.primary)
		}
	}
	if !duplicateRefusal("existing file does not improve on it") || !duplicateRefusal("already present in the library") || duplicateRefusal("unknown episode") {
		t.Fatal("duplicateRefusal misclassifies importer reasons")
	}
	if got := dlStateOr(sqlite.Download{State: "failed"}, sqlite.Publication{Kind: "salvage"}, "downloaded"); got != "failed" {
		t.Fatalf("salvage row state = %s, want kept", got)
	}
}

// The historical cohort skips claimed directories, files, and an unreadable
// root, and is marked once.
func TestHistoricalCohortSkipsWhatItMustNotMark(t *testing.T) {
	f := setupV2(t, nil)
	past := time.Now().Add(-time.Hour)
	claimed := filepath.Join(f.root, "Claimed.Payload")
	writeCompleted(t, filepath.Join(claimed, "a.mkv"), 3)
	_ = os.Chtimes(claimed, past, past)
	if _, err := f.db.InsertPublication(f.ctx, sqlite.PublicationWrite{Kind: "completed", Path: claimed, Cohort: "live", Source: "none"}); err != nil {
		t.Fatal(err)
	}
	writeCompleted(t, filepath.Join(f.root, "loose-file.mkv"), 3)
	if err := f.db.SetMeta(f.ctx, CompletedRootsSetting, f.root+"\n"+filepath.Join(f.root, "missing-root")); err != nil {
		t.Fatal(err)
	}
	n, err := f.svc.MarkHistoricalCohort(f.ctx)
	if err != nil || n != 0 {
		t.Fatalf("marked %d (%v), want nothing: the only directory is claimed", n, err)
	}
	if p, _, _ := f.db.PublicationByPath(f.ctx, claimed); p.Cohort != "live" {
		t.Fatalf("a claimed directory became %s", p.Cohort)
	}
	if !f.svc.publicationManaged(f.ctx, 0, claimed) {
		t.Fatal("a publication's path is not publication-managed")
	}
	if f.svc.publicationManaged(f.ctx, 0, filepath.Join(f.root, "loose-file.mkv")) {
		t.Fatal("a loose file is publication-managed")
	}
	dl := f.grab(1, epOne)
	if f.svc.publicationManaged(f.ctx, dl.ID, "") {
		t.Fatal("a row with no publication is publication-managed")
	}
	pub := f.publish("Test.Show.S01E01.job-101.runner-mgd", "mgd", map[string]string{epOne + ".mkv": "video"})
	f.completedAt(dl, pub.Path, "mgd")
	if !f.svc.publicationManaged(f.ctx, dl.ID, "") {
		t.Fatal("a row with a publication is not publication-managed")
	}
}

// Resolve: decisions that do not apply are refused; place imports exactly
// the file; keep settles; a person's discard is recorded.
func TestResolvePublicationFileMatrix(t *testing.T) {
	setupUnresolved := func(t *testing.T) (*v2Fixture, sqlite.Download, sqlite.Publication) {
		f := setupV2(t, nil)
		dl := f.grab(1, epOne)
		if _, err := f.db.W.ExecContext(f.ctx, `DELETE FROM wantable_attempts`); err != nil {
			t.Fatal(err)
		}
		pub := f.publish("Test.Show.S01E01.job-101.runner-res", "res", map[string]string{epOne + ".mkv": "video", "notes.txt": "n"})
		p := f.completedAt(dl, pub.Path, "res")
		if p.State != "import_failed" {
			t.Fatalf("publication = %s, want import_failed with the media unresolved", p.State)
		}
		return f, dl, p
	}
	t.Run("refusals", func(t *testing.T) {
		f, dl, p := setupUnresolved(t)
		if err := f.svc.ResolvePublicationFile(f.ctx, dl.ID+100, p.ID, epOne+".mkv", "keep"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("another download's publication: %v, want not found", err)
		}
		if err := f.svc.ResolvePublicationFile(f.ctx, dl.ID, p.ID, "absent.mkv", "keep"); !errors.Is(err, ErrInvalidResolution) {
			t.Fatalf("an absent file: %v, want invalid", err)
		}
		if err := f.svc.ResolvePublicationFile(f.ctx, dl.ID, p.ID, epOne+".mkv", "frobnicate"); !errors.Is(err, ErrInvalidResolution) {
			t.Fatalf("an unknown action: %v, want invalid", err)
		}
		if err := f.svc.ResolvePublicationFile(f.ctx, dl.ID, p.ID, "notes.txt", "place"); !errors.Is(err, ErrInvalidResolution) {
			t.Fatalf("placing non-media: %v, want invalid", err)
		}
		if _, err := f.db.GetPublication(f.ctx, p.ID+100); err == nil {
			t.Fatal("fixture has a second publication")
		}
		if err := f.svc.ResolvePublicationFile(f.ctx, dl.ID, p.ID+100, epOne+".mkv", "keep"); err == nil {
			t.Fatal("an unknown publication was resolved")
		}
	})
	t.Run("the import discards the non-media; the media stays unresolved", func(t *testing.T) {
		f, dl, p := setupUnresolved(t)
		files, _ := f.db.ListPublicationFiles(f.ctx, p.ID)
		for _, file := range files {
			if file.Path == "notes.txt" && (file.Outcome != "discarded" || file.DiscardReason != "rejected") {
				t.Fatalf("notes.txt = %+v, want discarded{rejected} by the import", file)
			}
		}
		if err := f.svc.ResolvePublicationFile(f.ctx, dl.ID, p.ID, "notes.txt", "discard"); !errors.Is(err, ErrInvalidResolution) {
			t.Fatalf("discarding an already discarded file: %v, want invalid", err)
		}
		p, _ = f.db.GetPublication(f.ctx, p.ID)
		if p.State != "import_failed" || f.row(dl.ID).State != "failed" {
			t.Fatalf("publication %s row %s, want still unresolved media", p.State, f.row(dl.ID).State)
		}
	})
	t.Run("placing the media imports exactly that file", func(t *testing.T) {
		f, dl, p := setupUnresolved(t)
		if err := f.svc.ResolvePublicationFile(f.ctx, dl.ID, p.ID, epOne+".mkv", "place"); err != nil {
			t.Fatal(err)
		}
		files, _ := f.db.ListPublicationFiles(f.ctx, p.ID)
		got := map[string]sqlite.PublicationFile{}
		for _, file := range files {
			got[file.Path] = file
		}
		if got[epOne+".mkv"].Outcome != "placed" {
			t.Fatalf("media = %+v (%s), want placed by the operator", got[epOne+".mkv"], f.row(dl.ID).Error)
		}
		if got["notes.txt"].Outcome == "placed" {
			t.Fatal("placing one file placed another")
		}
		if n := f.count(`SELECT COUNT(*) FROM media_files WHERE media_item_id=?`, f.item); n != 1 {
			t.Fatalf("library files = %d, want one", n)
		}
	})
}
