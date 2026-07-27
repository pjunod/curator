package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// ---- fakes ----

type fakeIndexer struct{ releases []ports.Release }

func (f fakeIndexer) Search(ctx context.Context, q domain.SearchQuery) ([]ports.Release, error) {
	return f.releases, nil
}
func (f fakeIndexer) FetchRSS(ctx context.Context) ([]ports.Release, error) {
	return f.releases, nil
}
func (f fakeIndexer) Test(ctx context.Context) error { return nil }

// fakeClient stands in for a download client.
//
// Its recording fields are guarded because the real thing is called from
// several goroutines at once and this double must not be the reason a test
// passes. Two sweeps overlapping on one client is not a contrived scenario —
// it is what the poll and the event stream do to each other by design, and
// TestPollAndEventCannotBothImport provokes it deliberately. An unguarded
// counter here reports a race in the fake and hides whatever the test was
// actually asking about.
//
// Fields are still read directly after a wg.Wait() or in single-goroutine
// tests, which is safe: the join is the happens-before edge.
type fakeClient struct {
	mu       sync.Mutex
	added    []string
	statuses []ports.DownloadStatus
	// removed records (handle, deleteData) for every Remove, so a test can
	// assert monarr asked the client to throw the payload away — and, just as
	// importantly, that it did not.
	removed []removeCall
	// polls counts Statuses calls, so a test can assert Monarr talked to the
	// client at all — which is what the Connections panel and the contact
	// clock actually report on.
	polls int
}

type removeCall struct {
	Handle     ports.Handle
	DeleteData bool
}

func (f *fakeClient) Add(ctx context.Context, url, cat string) (ports.Handle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added = append(f.added, url)
	return "h1", nil
}
func (f *fakeClient) Statuses(ctx context.Context) ([]ports.DownloadStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	return f.statuses, nil
}
func (f *fakeClient) Remove(ctx context.Context, h ports.Handle, del bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, removeCall{Handle: h, DeleteData: del})
	return nil
}
func (f *fakeClient) Test(ctx context.Context) error { return nil }

func setup(t *testing.T, releases []ports.Release, client *fakeClient) (*Service, *sqlite.DB, int64) {
	t.Helper()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := bus.New(nil)
	t.Cleanup(b.Close)

	svc := New(db, b, nil,
		func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{releases: releases} },
		func(ports.ClientConfig) ports.DownloadClient { return client },
	)

	ctx := context.Background()
	itemDir := filepath.Join(t.TempDir(), "Test Show (2020)")
	os.MkdirAll(itemDir, 0o755)
	item := domain.MediaItem{
		Kind: domain.KindSeries, Title: "Test Show", SortTitle: "test show", Year: 2020,
		IDs: domain.ExternalIDs{TMDB: 100}, Monitored: true, Path: itemDir,
		Seasons: []domain.Season{{Number: 1, Monitored: true, Episodes: []domain.Episode{
			{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot", Monitored: true},
			{SeasonNumber: 1, EpisodeNumber: 2, Title: "Finale", Monitored: true},
		}}},
	}
	itemID, err := db.CreateMediaItem(ctx, item)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "idx", URL: "http://x",
		Protocol: "torrent", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddDownloadClient(ctx, ports.ClientConfig{Type: "qbittorrent",
		Name: "qb", URL: "http://qb", Category: "monarr", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return svc, db, itemID
}

func TestSearchRanksAndRejects(t *testing.T) {
	releases := []ports.Release{
		{Title: "Test.Show.S01E01.720p.HDTV.x264-A", DownloadURL: "u1", Protocol: "torrent", Indexer: "idx", Seeders: 5, PublishDate: time.Now().Add(-2 * time.Hour)},
		{Title: "Test.Show.S01E01.1080p.WEB-DL.x264-B", DownloadURL: "u2", Protocol: "torrent", Indexer: "idx", Seeders: 50},
		{Title: "Other.Show.S01E01.1080p.WEB-DL-C", DownloadURL: "u3", Protocol: "torrent", Indexer: "idx"},
		{Title: "Test.Show.S01E01.4K.WEB-DL-D", DownloadURL: "u4", Protocol: "torrent", Indexer: "idx"},
	}
	svc, _, itemID := setup(t, releases, &fakeClient{})

	cands, err := svc.Search(context.Background(), itemID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 4 {
		t.Fatalf("candidates = %d", len(cands))
	}
	// Best accepted first: 1080 WEB-DL (profile Any cutoff webdl-1080; 4K allowed too but... default profile 'Any' allows 2160 → 2160 ranks higher).
	if !cands[0].Accepted {
		t.Errorf("top candidate should be accepted: %+v", cands[0])
	}
	var notMatched, accepted int
	for _, c := range cands {
		if c.Accepted {
			accepted++
		}
		for _, r := range c.Rejections {
			if r.Code == "not_matched" {
				notMatched++
			}
		}
	}
	if notMatched != 1 {
		t.Errorf("expected exactly one not_matched (Other.Show), got %d", notMatched)
	}
	if accepted < 2 {
		t.Errorf("expected multiple accepted candidates, got %d", accepted)
	}
}

func TestGrabAndImportEpisode(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL.x264-GRP", DownloadURL: "magnet:x",
		Indexer: "idx", Protocol: "torrent", Size: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 {
		t.Fatal("client should have received the add")
	}

	// Simulate the client finishing with a payload on disk.
	payload := filepath.Join(t.TempDir(), "Test.Show.S01E01.1080p.WEB-DL.x264-GRP")
	os.MkdirAll(payload, 0o755)
	os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.x264-GRP.mkv"), []byte("video"), 0o644)
	os.WriteFile(filepath.Join(payload, "sample.mkv"), []byte("s"), 0o644)
	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: "Test.Show.S01E01.1080p.WEB-DL.x264-GRP",
		State: ports.StateCompleted, Progress: 1, SavePath: payload,
	}}

	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}

	dl, _ := db.GetDownload(ctx, id)
	if dl.State != "imported" {
		t.Fatalf("download state = %s (%s)", dl.State, dl.Error)
	}
	item, _ := db.GetMediaItemFull(ctx, itemID)
	if len(item.Files) != 1 {
		t.Fatalf("files = %+v", item.Files)
	}
	f := item.Files[0]
	if !strings.Contains(f.Path, filepath.Join("Season 1", "Test Show - S01E01 - Pilot [WEB-DL 1080p].mkv")) {
		t.Errorf("renamed path = %q", f.Path)
	}
	if len(f.EpisodeIDs) != 1 {
		t.Errorf("episode links = %v", f.EpisodeIDs)
	}
	if _, err := os.Stat(f.Path); err != nil {
		t.Errorf("imported file missing on disk: %v", err)
	}
	if !item.Seasons[0].Episodes[0].HasFile || item.Seasons[0].Episodes[1].HasFile {
		t.Errorf("episode file flags: %+v", item.Seasons[0].Episodes)
	}
}

func TestSeasonPackImportFansOut(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()

	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 0,
		Title: "Test.Show.S01.1080p.WEB-DL-PACK", DownloadURL: "magnet:pack",
		Indexer: "idx", Protocol: "torrent",
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := filepath.Join(t.TempDir(), "Test.Show.S01.1080p.WEB-DL-PACK")
	os.MkdirAll(payload, 0o755)
	os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.mkv"), []byte("e1"), 0o644)
	os.WriteFile(filepath.Join(payload, "Test.Show.S01E02.1080p.mkv"), []byte("e2"), 0o644)
	client.statuses = []ports.DownloadStatus{{
		Handle: "h1", Name: "Test.Show.S01.1080p.WEB-DL-PACK",
		State: ports.StateCompleted, Progress: 1, SavePath: payload,
	}}

	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, id)
	if dl.State != "imported" {
		t.Fatalf("state = %s (%s)", dl.State, dl.Error)
	}
	item, _ := db.GetMediaItemFull(ctx, itemID)
	if len(item.Files) != 2 {
		t.Fatalf("files = %d", len(item.Files))
	}
	for _, e := range item.Seasons[0].Episodes {
		if !e.HasFile {
			t.Errorf("episode %d should have a file after pack import", e.EpisodeNumber)
		}
	}
}

func TestImportUpgradeReplacesFile(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, itemID)

	// Existing 720p HDTV file for S01E01.
	oldDir := filepath.Join(item.Path, "Season 1")
	os.MkdirAll(oldDir, 0o755)
	oldPath := filepath.Join(oldDir, "Test Show - S01E01 - Pilot [HDTV 720p].mkv")
	os.WriteFile(oldPath, []byte("old"), 0o644)
	fid, _ := db.UpsertFile(ctx, itemID, 0, oldPath, 3)
	ep1, _ := db.GetEpisodeID(ctx, itemID, 1, 1)
	db.ReplaceFileEpisodeLinks(ctx, fid, []int64{ep1})
	db.SetFileQuality(ctx, fid, mustQ("hdtv-720"))

	id, _ := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1,
		Title: "Test.Show.S01E01.1080p.WEB-DL-NEW", DownloadURL: "m", Indexer: "idx", Protocol: "torrent",
	})
	payload := filepath.Join(t.TempDir(), "p")
	os.MkdirAll(payload, 0o755)
	os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL-NEW.mkv"), []byte("new"), 0o644)
	client.statuses = []ports.DownloadStatus{{Handle: "h1", Name: "Test.Show.S01E01.1080p.WEB-DL-NEW",
		State: ports.StateCompleted, Progress: 1, SavePath: payload}}

	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	dl, _ := db.GetDownload(ctx, id)
	if dl.State != "imported" {
		t.Fatalf("state = %s (%s)", dl.State, dl.Error)
	}
	item, _ = db.GetMediaItemFull(ctx, itemID)
	if len(item.Files) != 1 {
		t.Fatalf("old file should be replaced, files = %+v", item.Files)
	}
	if !strings.Contains(item.Files[0].Path, "WEB-DL 1080p") {
		t.Errorf("surviving file = %q", item.Files[0].Path)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("old file should be deleted from disk")
	}
}

func mustQ(s string) quality.Quality { return quality.FromString(s) }
