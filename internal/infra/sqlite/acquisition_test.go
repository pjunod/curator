package sqlite

import (
	"context"
	"testing"

	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/ports"
)

func TestSeededProfiles(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	profiles, err := db.ListProfiles(ctx)
	if err != nil || len(profiles) != 5 {
		t.Fatalf("profiles = %d, err %v", len(profiles), err)
	}
	hd, err := db.GetProfile(ctx, 2)
	if err != nil || hd.Name != "HD-1080p" || len(hd.Allowed) != 5 {
		t.Fatalf("hd profile = %+v err %v", hd, err)
	}
	if hd.Cutoff != (quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}) {
		t.Errorf("cutoff = %+v", hd.Cutoff)
	}
	ebook, err := db.GetProfile(ctx, quality.EbookProfileID)
	if err != nil || ebook.Name != "Ebook" || len(ebook.Allowed) != 4 {
		t.Fatalf("ebook profile = %+v err %v", ebook, err)
	}
	if ebook.Cutoff != (quality.Quality{Source: quality.SourceEPUB}) {
		t.Errorf("ebook cutoff = %+v", ebook.Cutoff)
	}
}

func TestIndexerAndClientCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.AddIndexer(ctx, ports.IndexerConfig{
		Name: "NZBTest", URL: "http://idx", APIKey: "k", Protocol: "usenet",
		Categories: []int{5030, 5040}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetIndexer(ctx, id)
	if err != nil || got.Name != "NZBTest" || len(got.Categories) != 2 || !got.Enabled {
		t.Fatalf("indexer = %+v err %v", got, err)
	}
	list, _ := db.ListIndexers(ctx)
	if len(list) != 1 {
		t.Fatal("list should have 1")
	}
	if err := db.DeleteIndexer(ctx, id); err != nil {
		t.Fatal(err)
	}

	cid, err := db.AddDownloadClient(ctx, ports.ClientConfig{
		Type: "qbittorrent", Name: "qbit", URL: "http://qb", Username: "admin",
		Password: "pass", Category: "monarr", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := db.GetDownloadClient(ctx, cid)
	if err != nil || c.Type != "qbittorrent" || c.Category != "monarr" {
		t.Fatalf("client = %+v err %v", c, err)
	}
}

func TestDownloadQueueRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	itemID, _ := db.CreateMediaItem(ctx, sampleSeries())

	id, err := db.InsertDownload(ctx, Download{
		MediaItemID: itemID, WantableIDs: []string{"episode:1:1:1", "episode:1:1:2"},
		Season: 1, ReleaseTitle: "Test.Show.S01.1080p", Protocol: "torrent",
		Quality:  quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		ClientID: 1, Handle: "abc123", State: "grabbed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDownloadState(ctx, id, "downloading", 0.5, ""); err != nil {
		t.Fatal(err)
	}
	active, err := db.ListActiveDownloads(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("active = %v err %v", active, err)
	}
	dl := active[0]
	if dl.State != "downloading" || dl.Progress != 0.5 || len(dl.WantableIDs) != 2 ||
		dl.Quality.Resolution != 1080 {
		t.Errorf("download = %+v", dl)
	}
	if err := db.UpdateDownloadState(ctx, id, "imported", 1, ""); err != nil {
		t.Fatal(err)
	}
	active, _ = db.ListActiveDownloads(ctx)
	if len(active) != 0 {
		t.Error("imported rows are not active")
	}
	recent, _ := db.ListRecentDownloads(ctx)
	if len(recent) != 1 {
		t.Error("recent should still show it")
	}
}

func TestFileQualityAndHistory(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	itemID, _ := db.CreateMediaItem(ctx, sampleSeries())
	f1, _ := db.UpsertFile(ctx, itemID, 0, "/x/a.mkv", 1)
	f2, _ := db.UpsertFile(ctx, itemID, 0, "/x/b.mkv", 1)

	if _, ok, _ := db.BestQualityForItem(ctx, itemID, 0); ok {
		t.Error("no qualities set yet")
	}
	db.SetFileQuality(ctx, f1, quality.Quality{Source: quality.SourceHDTV, Resolution: 720})
	db.SetFileQuality(ctx, f2, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080})
	best, ok, err := db.BestQualityForItem(ctx, itemID, 0)
	if err != nil || !ok || best.Resolution != 1080 {
		t.Errorf("best = %+v ok=%v err=%v", best, ok, err)
	}

	if err := db.AddHistory(ctx, "grabbed", itemID, "Some.Release", map[string]any{"indexer": "x"}); err != nil {
		t.Fatal(err)
	}
}

// TestEveryClientTypeInsertable guards schema/code drift: every type the
// factory knows must pass the download_clients CHECK constraint. (The
// Phase 5 client zoo once outran the Phase 2 CHECK — never again.)
func TestEveryClientTypeInsertable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, typ := range []string{"qbittorrent", "sabnzbd", "transmission", "deluge", "nzbget"} {
		if _, err := db.AddDownloadClient(ctx, ports.ClientConfig{
			Type: typ, Name: typ + "-test", URL: "http://x:1", Enabled: true,
		}); err != nil {
			t.Errorf("type %q rejected by schema: %v", typ, err)
		}
	}
	list, err := db.ListDownloadClients(ctx)
	if err != nil || len(list) != 5 {
		t.Fatalf("clients = %d err %v", len(list), err)
	}
}
