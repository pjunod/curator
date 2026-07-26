package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/domain/mediainfo"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// corpusFile returns bytes from the prober's fixture corpus, so these tests
// exercise the real walkers rather than a mock that always agrees with us.
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
		corpusFile(t, "mkv-720p-h264-aac.mkv"), 0o644); err != nil {
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
		corpusFile(t, "mkv-1080p-h264-ac3.mkv"), 0o644); err != nil {
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
