package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/monarr-media/monarr/internal/domain/mediainfo"
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
