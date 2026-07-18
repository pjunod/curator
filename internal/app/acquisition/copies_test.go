package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// The multi-copy contract, end to end at the service level: a movie whose
// primary (profile 1, 1080p file at cutoff) is satisfied while its 720p…
// er, HD-1080p copy in a separate folder is empty. The copy must be
// wanted under its own identity, grab under its own identity, import into
// its own folder, and upgrade without ever touching the primary's file.
func TestCopiesWantGrabImportIndependently(t *testing.T) {
	client := &fakeClient{}
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	b := bus.New(nil)
	t.Cleanup(b.Close)
	svc := New(db, b, nil,
		func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} },
		func(ports.ClientConfig) ports.DownloadClient { return client },
	)

	// Movie with a 1080p primary file at profile 1's cutoff.
	primaryDir := filepath.Join(t.TempDir(), "Test Movie (2024)")
	copyDir := filepath.Join(t.TempDir(), "copies", "Test Movie (2024)")
	_ = os.MkdirAll(primaryDir, 0o755)
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Test Movie", SortTitle: "test movie", Year: 2024,
		IDs: domain.ExternalIDs{TMDB: 601}, Monitored: true, Path: primaryDir,
		QualityProfileID: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	primaryFile := filepath.Join(primaryDir, "Test Movie (2024) [WEBDL-1080p].mkv")
	if err := os.WriteFile(primaryFile, []byte("primary"), 0o644); err != nil {
		t.Fatal(err)
	}
	fid, err := db.UpsertFile(ctx, itemID, 0, primaryFile, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQuality(ctx, fid, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddDownloadClient(ctx, ports.ClientConfig{Type: "qbittorrent", Name: "qb", URL: "http://x", Enabled: true}); err != nil {
		t.Fatal(err)
	}

	copyID, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: itemID, Name: "for dad", QualityProfileID: 2,
		Path: copyDir, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. The copy is wanted under its own suffixed identity, with ITS profile;
	//    the satisfied primary is not.
	wanted, err := svc.Wanted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var copyWant domain.Wantable
	for _, w := range wanted {
		if w.MediaItemID() != itemID {
			continue
		}
		if strings.HasSuffix(string(w.ID()), ":c"+itoa(copyID)) {
			copyWant = w
		} else {
			t.Errorf("satisfied primary should not be wanted: %s", w.ID())
		}
	}
	if copyWant == nil {
		t.Fatalf("copy not wanted: %+v", wanted)
	}
	if copyWant.ProfileID() != 2 {
		t.Errorf("copy wantable profile = %d, want 2", copyWant.ProfileID())
	}
	if _, ok := copyWant.CurrentQuality(); ok {
		t.Error("copy should be missing — the primary's file must not count for it")
	}
	if domain.WantableCopyName(copyWant) != "for dad" {
		t.Errorf("copy name = %q", domain.WantableCopyName(copyWant))
	}

	// 2. Grabbing for the copy records the copy on the download row.
	dlID, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, CopyID: copyID, Season: -1,
		Title:       "Test.Movie.2024.720p.WEB-DL.x264-GRP",
		DownloadURL: "http://idx/dl/720.torrent", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}
	active, err := db.ListActiveDownloads(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("active = %v err %v", active, err)
	}
	dl := active[0]
	if dl.ID != dlID || dl.CopyID != copyID {
		t.Fatalf("download copy = %d, want %d", dl.CopyID, copyID)
	}
	if len(dl.WantableIDs) != 1 || !strings.HasSuffix(dl.WantableIDs[0], ":c"+itoa(copyID)) {
		t.Errorf("wantable ids = %v", dl.WantableIDs)
	}

	// …and the in-flight filter now suppresses the copy want.
	svc.InvalidateWanted()
	wanted, _ = svc.Wanted(ctx)
	if len(svc.notInFlight(ctx, wanted)) != 0 {
		t.Errorf("copy want should be in flight: %v", wanted)
	}

	// 3. Import routes the payload into the COPY's folder and attributes it.
	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, "Test.Movie.2024.720p.WEB-DL.x264-GRP.mkv"),
		[]byte("seven-twenty"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.importDownload(ctx, dl, payload); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %+v", files)
	}
	var copyFile string
	for _, f := range files {
		switch f.CopyID {
		case 0:
			if f.Path != primaryFile {
				t.Errorf("primary file moved: %q", f.Path)
			}
		case copyID:
			copyFile = f.Path
			if !strings.HasPrefix(f.Path, copyDir) {
				t.Errorf("copy file outside copy folder: %q", f.Path)
			}
		default:
			t.Errorf("unexpected attribution: %+v", f)
		}
	}
	if copyFile == "" {
		t.Fatal("copy file not recorded")
	}
	if _, err := os.Stat(primaryFile); err != nil {
		t.Error("primary file must survive a copy import")
	}

	// 4. A copy upgrade replaces only the copy's file.
	dl2ID, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, CopyID: copyID, Season: -1,
		Title:       "Test.Movie.2024.1080p.BluRay.x264-GRP",
		DownloadURL: "http://idx/dl/1080.torrent", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload2, "Test.Movie.2024.1080p.BluRay.x264-GRP.mkv"),
		[]byte("ten-eighty"), 0o644); err != nil {
		t.Fatal(err)
	}
	active, _ = db.ListActiveDownloads(ctx)
	var dl2 sqlite.Download
	for _, d := range active {
		if d.ID == dl2ID {
			dl2 = d
		}
	}
	if err := svc.importDownload(ctx, dl2, payload2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(copyFile); !os.IsNotExist(err) {
		t.Error("copy's 720p should be replaced by its 1080p upgrade")
	}
	if _, err := os.Stat(primaryFile); err != nil {
		t.Error("primary file must survive a copy upgrade")
	}
	files, _ = db.ListFilesForItem(ctx, itemID)
	if len(files) != 2 {
		t.Errorf("after upgrade files = %+v", files)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
