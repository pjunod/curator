package acquisition

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// corpusFile returns bytes from the prober's fixture corpus, so these tests
// exercise the real walkers rather than a mock that always agrees with us.
// wholeCorpusFixture is the corpus's one intact MKV. The rest are 64 KiB
// header windows — real, and really truncated, which the prober now reports.
// Use this one whenever a test wants an ordinary file rather than a stump.
const (
	wholeCorpus1080 = "mkv-1080p-h264-ac3-whole.mkv"
	wholeCorpus720  = "mkv-720p-h264-aac-whole.mkv"
)

func corpusFile(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "domain", "mediainfo", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// TestImportRecordsMeasuredQualityNotTheClaim is the import half of ADR 0013:
// the release name gets the grab decision, because before the download that is
// all anyone has. Once the file is on disk the bytes settle it.
//
// Here a release calls itself 2160p and delivers a 720p file — the routine
// mislabelling that makes a quality-managing tool built on names untrustworthy.
func TestImportRecordsMeasuredQualityNotTheClaim(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	release := "Test.Show.S01E01.2160p.WEB-DL-LIAR"
	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: release, DownloadURL: "m", Indexer: "idx", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(t.TempDir(), "p")
	if err := os.MkdirAll(payload, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(payload, release+".mkv"),
		corpusFile(t, wholeCorpus720), 0o644); err != nil {
		t.Fatal(err)
	}
	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: release, State: ports.StateCompleted,
		Progress: 1, SavePath: payload,
	}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	if dl, _ := db.GetDownload(ctx, id); dl.State != "imported" {
		t.Fatalf("state = %s (%s)", dl.State, dl.Error)
	}

	records, err := db.FileQualityRecords(ctx, itemID)
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v err = %v", records, err)
	}
	rec := records[0]
	if rec.Quality.Resolution != 720 {
		t.Errorf("recorded resolution = %d, want 720 — the file, not the name",
			rec.Quality.Resolution)
	}
	if !rec.Probed || rec.Info.Video == nil {
		t.Errorf("import did not measure the placed file: %+v", rec)
	}

	// The disagreement is kept. It is evidence about a release, and the only
	// way a future auto-blocklist design gets data to be designed against.
	events, err := db.ListHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.Type == HistoryQualityMismatch && e.ReleaseTitle == release {
			found = true
		}
	}
	if !found {
		t.Error("no quality_mismatch event recorded for a release that lied about resolution")
	}
}

// TestImportKeepsAnHonestReleaseClaim: when the measurement agrees with the
// name, or has nothing to add on the source axis, the claim stands and the
// provenance says the claim is where it came from. Not everything is a lie.
func TestImportKeepsAnHonestReleaseClaim(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	release := "Test.Show.S01E01.1080p.WEB-DL-HONEST"
	if _, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: release, DownloadURL: "m", Indexer: "idx", Protocol: "torrent",
	}); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(t.TempDir(), "p")
	_ = os.MkdirAll(payload, 0o755)
	// 1080p H.264 with AC-3 at a bitrate that infers WEB-DL only weakly, so
	// the uncontradicted name is what supplies the source.
	if err := os.WriteFile(filepath.Join(payload, release+".mkv"),
		corpusFile(t, wholeCorpus1080), 0o644); err != nil {
		t.Fatal(err)
	}
	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: release, State: ports.StateCompleted,
		Progress: 1, SavePath: payload,
	}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}

	records, err := db.FileQualityRecords(ctx, itemID)
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v err = %v", records, err)
	}
	rec := records[0]
	if rec.Quality.Resolution != 1080 {
		t.Errorf("resolution = %d, want 1080", rec.Quality.Resolution)
	}
	if !rec.SourceVerified() {
		t.Errorf("an uncontradicted release claim should count as verified: %+v", rec)
	}

	events, _ := db.ListHistory(ctx)
	for _, e := range events {
		if e.Type == HistoryQualityMismatch {
			t.Errorf("mismatch recorded for a release that told the truth: %+v", e)
		}
	}
}

// TestImportOfAnUnmeasurableFileKeepsTheClaim: an AVI, or a file the walkers
// cannot read, still imports. It keeps the release's claim and says so — the
// import path must never refuse a file for being unmeasurable.
func TestImportOfAnUnmeasurableFileKeepsTheClaim(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	release := "Test.Show.S01E01.1080p.WEB-DL-ODD"
	if _, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: release, DownloadURL: "m", Indexer: "idx", Protocol: "torrent",
	}); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(t.TempDir(), "p")
	_ = os.MkdirAll(payload, 0o755)
	if err := os.WriteFile(filepath.Join(payload, release+".mkv"),
		[]byte("not really a matroska file"), 0o644); err != nil {
		t.Fatal(err)
	}
	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: release, State: ports.StateCompleted,
		Progress: 1, SavePath: payload,
	}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}

	records, err := db.FileQualityRecords(ctx, itemID)
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v err = %v", records, err)
	}
	rec := records[0]
	if rec.Quality.Resolution != 1080 || rec.Provenance != mediainfo.ProvenanceRelease {
		t.Errorf("record = %+v, want the release's own claim at provenance %q",
			rec, mediainfo.ProvenanceRelease)
	}
}

// TestImportSaysWhyItSkippedEveryFile is the regression for the message a user
// actually got when an import declined everything: "no files imported from
// /working/monarr/completed/…" and nothing else. The reasons were computed and
// even logged, then dropped before reaching the person who needed them — and a
// refusal without a reason is indistinguishable from a bug.
func TestImportSaysWhyItSkippedEveryFile(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, itemID)

	// S01E01 already on disk at exactly the profile's target.
	seasonDir := filepath.Join(item.Path, "Season 1")
	if err := os.MkdirAll(seasonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(seasonDir, "Test Show - S01E01 - Pilot [WEB-DL 1080p].mkv")
	if err := os.WriteFile(existing, []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}
	fid, err := db.UpsertFile(ctx, itemID, 0, existing, 12)
	if err != nil {
		t.Fatal(err)
	}
	ep1, err := db.GetEpisodeID(ctx, itemID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceFileEpisodeLinks(ctx, fid, []int64{ep1}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQualityFrom(ctx, fid,
		quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}

	// A payload offering the same episode at the same quality: legitimately
	// nothing to do, automatically.
	payload := t.TempDir()
	name := "Test.Show.S01E01.1080p.WEB-DL.x264-SAME.mkv"
	if err := os.WriteFile(filepath.Join(payload, name), []byte("same again"), 0o644); err != nil {
		t.Fatal(err)
	}

	dl := sqlite.Download{MediaItemID: itemID, ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL.x264-SAME"}
	result, err := svc.importDownload(ctx, dl, payload, false)
	if err == nil {
		t.Fatal("expected the automatic import to decline")
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("error does not name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "does not improve on") {
		t.Errorf("error does not say why: %v", err)
	}
	skipped := result.Skipped()
	if len(skipped) != 1 || skipped[0].Name != name || skipped[0].Reason == "" {
		t.Fatalf("per-file outcomes = %+v", result.Files)
	}
	if skipped[0].Quality != "WEB-DL 1080p" {
		t.Errorf("outcome should say what it judged the file to be, got %q", skipped[0].Quality)
	}
}

// TestManualImportIsNotGatedByTheProfile: the profile gates AUTOMATION. A
// person who pointed at a folder and pressed Import has already decided, and
// telling them "does not improve on" is the same mistake as gating a manual
// grab — which monarr has never done.
func TestManualImportIsNotGatedByTheProfile(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, itemID)

	seasonDir := filepath.Join(item.Path, "Season 1")
	_ = os.MkdirAll(seasonDir, 0o755)
	existing := filepath.Join(seasonDir, "Test Show - S01E01 - Pilot [WEB-DL 1080p].mkv")
	_ = os.WriteFile(existing, []byte("already here"), 0o644)
	fid, _ := db.UpsertFile(ctx, itemID, 0, existing, 12)
	ep1, _ := db.GetEpisodeID(ctx, itemID, 1, 1)
	_ = db.ReplaceFileEpisodeLinks(ctx, fid, []int64{ep1})
	_ = db.SetFileQualityFrom(ctx, fid,
		quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone)

	payload := t.TempDir()
	name := "Test.Show.S01E01.1080p.WEB-DL.x264-SAME.mkv"
	_ = os.WriteFile(filepath.Join(payload, name), []byte("same again"), 0o644)

	result, err := svc.ManualImport(ctx, ManualImportRequest{
		Path: payload, MediaItemID: itemID,
	})
	if err != nil {
		t.Fatalf("manual import refused what the user explicitly asked for: %v", err)
	}
	if result.Imported != 1 {
		t.Errorf("imported = %d, want 1: %+v", result.Imported, result.Files)
	}
	// Same quality, so it is not an upgrade and must NOT have deleted the
	// file that was already there — a manual import replaces only when the
	// new file actually outranks the old one.
	if result.Upgraded {
		t.Error("a same-quality manual import reported itself as an upgrade")
	}
	if _, statErr := os.Stat(existing); statErr != nil {
		t.Error("the existing file was deleted by a manual import that did not outrank it")
	}
}

// What an import announces decides what a media server can do with it.
//
// "Something was imported" makes plurx (or Plex, or Jellyfin) sweep an
// entire library to find one file, and then identify it by searching for its
// filename — the step that puts the wrong film's poster on a remake. The
// paths and the ids have to leave the importer, because nothing downstream
// can work them out afterwards.
func TestImportAnnouncesTheExactPathsAndIDs(t *testing.T) {
	client := &fakeClient{}
	svc, _, itemID := setup(t, nil, client)
	ctx := context.Background()

	events, cancel := bus.Subscribe[ImportCompleted](svc.bus, 4)
	defer cancel()

	payload := t.TempDir()
	for _, name := range []string{
		"Test.Show.S01E01.1080p.WEB-DL.x264-GRP.mkv",
		"Test.Show.S01E02.1080p.WEB-DL.x264-GRP.mkv",
	} {
		if err := os.WriteFile(filepath.Join(payload, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dl := sqlite.Download{
		MediaItemID:  itemID,
		ReleaseTitle: "Test.Show.S01.1080p.WEB-DL.x264-GRP",
		Transfer:     "t-42-a3f9c1",
	}
	result, err := svc.importDownload(ctx, dl, payload, false)
	if err != nil {
		t.Fatal(err)
	}

	var ev ImportCompleted
	select {
	case ev = <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("no ImportCompleted published")
	}

	if len(ev.Paths) != result.Imported {
		t.Fatalf("announced %d paths for %d imported files: %v", len(ev.Paths), result.Imported, ev.Paths)
	}
	for _, p := range ev.Paths {
		if !filepath.IsAbs(p) {
			t.Errorf("%q is not absolute — a consumer cannot resolve it", p)
		}
		if _, err := os.Stat(p); err != nil {
			t.Errorf("announced a path that is not there: %v", err)
		}
	}
	if ev.MediaItemKind != "series" {
		t.Errorf("kind = %q — it decides whose id TmdbID is and how plurx is told", ev.MediaItemKind)
	}
	if ev.TmdbID != 100 {
		t.Errorf("TmdbID = %d, want the SHOW's id", ev.TmdbID)
	}
	// One directory for a season pack: that is what plurx is asked to index.
	if len(ev.Dirs) != 1 {
		t.Errorf("dirs = %v, want the one season folder both files landed in", ev.Dirs)
	}
	if ev.Dirs[0] != filepath.Dir(ev.Paths[0]) {
		t.Errorf("dirs[0] = %q is not the parent of paths[0] = %q", ev.Dirs[0], ev.Paths[0])
	}
	if ev.DownloadID != dl.ID {
		t.Errorf("downloadId = %d, want %d — a consumer writes back onto that trace", ev.DownloadID, dl.ID)
	}
	if ev.Title != "Test Show" {
		t.Errorf("title = %q", ev.Title)
	}
	if ev.Transfer != "t-42-a3f9c1" {
		t.Errorf("transfer = %q — without it the trail stops at Monarr", ev.Transfer)
	}
}

// A file that was rejected has no path. Announcing one would send a media
// server after a file that is not in the library — or, if the rejected copy
// is still sitting in the download folder, after that one.
func TestImportAnnouncesOnlyTheFilesThatLanded(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, itemID)

	// S01E01 already here at the profile's target, so a second copy of it
	// will be declined while S01E02 lands.
	seasonDir := filepath.Join(item.Path, "Season 1")
	if err := os.MkdirAll(seasonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(seasonDir, "Test Show - S01E01 - Pilot [WEB-DL 1080p].mkv")
	if err := os.WriteFile(existing, []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}
	fid, err := db.UpsertFile(ctx, itemID, 0, existing, 12)
	if err != nil {
		t.Fatal(err)
	}
	ep1, err := db.GetEpisodeID(ctx, itemID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceFileEpisodeLinks(ctx, fid, []int64{ep1}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQualityFrom(ctx, fid,
		quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}

	events, cancel := bus.Subscribe[ImportCompleted](svc.bus, 4)
	defer cancel()

	payload := t.TempDir()
	for _, name := range []string{
		"Test.Show.S01E01.1080p.WEB-DL.x264-SAME.mkv",
		"Test.Show.S01E02.1080p.WEB-DL.x264-GRP.mkv",
	} {
		if err := os.WriteFile(filepath.Join(payload, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	dl := sqlite.Download{MediaItemID: itemID, ReleaseTitle: "Test.Show.S01.1080p"}
	if _, err := svc.importDownload(ctx, dl, payload, false); err != nil {
		t.Fatal(err)
	}

	var ev ImportCompleted
	select {
	case ev = <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("no ImportCompleted published")
	}
	if len(ev.Paths) != 1 {
		t.Fatalf("announced %d paths; only S01E02 landed: %v", len(ev.Paths), ev.Paths)
	}
	if len(ev.Dirs) != 1 {
		t.Errorf("dirs = %v, want one", ev.Dirs)
	}
	if strings.Contains(ev.Paths[0], "S01E01") {
		t.Errorf("announced the file that was declined: %q", ev.Paths[0])
	}
	for _, p := range ev.Paths {
		if strings.HasPrefix(p, payload) {
			t.Errorf("announced a path still in the download folder: %q", p)
		}
	}
}

// TestMoviePayloadPrefersTheLargestFile pins which file wins when a payload
// offers more than one.
//
// Only one can: the rest are declined as "does not improve on" whatever landed
// first, so the ordering IS the decision. Alphabetical order made that
// decision by accident, and a decoy named to sort early takes the slot from
// the feature sitting right next to it.
func TestMoviePayloadPrefersTheLargestFile(t *testing.T) {
	svc, db, _ := setup(t, nil, nil)
	ctx := context.Background()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Test Movie", SortTitle: "test movie",
		Year: 2024, IDs: domain.ExternalIDs{TMDB: 601}, Monitored: true,
		Path: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := t.TempDir()
	// The decoy sorts first and is a fraction of the size.
	decoy := filepath.Join(payload, "AAA.The.Test.Movie.2024.1080p.WEB-DL.mkv")
	feature := filepath.Join(payload, "The.Test.Movie.2024.1080p.WEB-DL.mkv")
	if err := os.WriteFile(decoy, bytes.Repeat([]byte{0x11}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(feature, bytes.Repeat([]byte{0x22}, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	dl := sqlite.Download{
		MediaItemID: itemID, ReleaseTitle: "The.Test.Movie.2024.1080p.WEB-DL.x264-GRP",
		Quality:  quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		SavePath: payload,
	}
	if _, err := svc.importDownload(ctx, dl, payload, false); err != nil {
		t.Fatalf("import: %v", err)
	}

	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected one file, got %d", len(files))
	}
	if files[0].Size != 1<<20 {
		t.Errorf("imported %d bytes; the decoy won over the feature", files[0].Size)
	}
}

// TestTruncatedPayloadIsRefused is the 500 MiB case, at the moment it lands.
//
// Two different films arrived at 499.70 MiB — 61 seconds of a 96-minute
// feature, at a bitrate that proves the content was genuine and the transfer
// stopped. The old behaviour placed that stump in the library, where the
// plausibility rules correctly refused to count it, which put the item back to
// wanted, which had the next backlog pass grab again and land another one.
//
// Refusing at import breaks that loop and puts the failure somewhere a person
// will see it. The release is NOT blocklisted: it was never the problem.
func TestTruncatedPayloadIsRefused(t *testing.T) {
	svc, db, _ := setup(t, nil, nil)
	ctx := context.Background()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Test Movie", SortTitle: "test movie",
		Year: 2024, IDs: domain.ExternalIDs{TMDB: 601}, Monitored: true,
		Path: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// A real MKV cut to a header window — the corpus is built this way, so
	// these bytes are a genuine truncated file rather than a mock of one.
	payload := t.TempDir()
	stump := filepath.Join(payload, "The.Test.Movie.2024.2160p.BluRay.REMUX.mkv")
	if err := os.WriteFile(stump, corpusFile(t, "mkv-2160p-hevc-hdr10-truehd.mkv"), 0o644); err != nil {
		t.Fatal(err)
	}

	dl := sqlite.Download{
		MediaItemID: itemID, ReleaseTitle: "The.Test.Movie.2024.2160p.BluRay.REMUX-GRP",
		Quality:  quality.Quality{Source: quality.SourceRemux, Resolution: 2160},
		SavePath: payload,
	}
	_, err = svc.importDownload(ctx, dl, payload, false)
	if err == nil {
		t.Fatal("a cut-short payload imported; it must fail the import instead")
	}
	if !strings.Contains(err.Error(), "cut short") {
		t.Errorf("the failure must say what happened, got: %v", err)
	}

	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Errorf("%d file(s) landed in the library; a stump must not be placed", len(files))
	}

	// And the release keeps its good name.
	blocked, err := db.IsBlocklisted(ctx, dl.ReleaseTitle, dl.Indexer)
	if err != nil {
		t.Fatal(err)
	}
	if blocked {
		t.Error("the release was blocklisted; a stopped transfer is not a bad release")
	}
}
