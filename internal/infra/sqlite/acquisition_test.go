package sqlite

import (
	"context"
	"testing"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
)

func TestSeededProfiles(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	profiles, err := db.ListProfiles(ctx)
	if err != nil || len(profiles) != 5 {
		t.Fatalf("profiles = %d, err %v", len(profiles), err)
	}
	// Migration 0019 rewrote the seeds in place. Ids are stable because
	// media_items and media_copies reference them; only the shape and two of
	// the names changed (ADR 0014 section 4).
	hd, err := db.GetProfile(ctx, 2)
	if err != nil || hd.Name != "HD-1080p" {
		t.Fatalf("hd profile = %+v err %v", hd, err)
	}
	if hd.Target != (quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}) {
		t.Errorf("target = %+v", hd.Target)
	}
	if hd.Floor == nil || *hd.Floor != (quality.Quality{Source: quality.SourceHDTV, Resolution: 1080}) {
		t.Errorf("floor = %+v, want hdtv-1080 (never accept below 1080p)", hd.Floor)
	}

	// "Any" is retired. What it actually did -- stop at WEB-DL 1080p -- is
	// what the profile now says it does, under a name that says it.
	def, err := db.GetProfile(ctx, 1)
	if err != nil || def.Name != "1080p" {
		t.Fatalf("default profile = %+v err %v", def, err)
	}
	if def.Floor != nil {
		t.Errorf("the default profile should have no floor, got %+v", def.Floor)
	}
	if def.Acceptable(quality.Quality{Source: quality.SourceRemux, Resolution: 2160}) {
		t.Error("the 1080p profile must not accept a 2160p remux -- that is the whole fix")
	}

	uhd, err := db.GetProfile(ctx, 3)
	if err != nil || uhd.Name != "4K" {
		t.Fatalf("uhd profile = %+v err %v", uhd, err)
	}

	ebook, err := db.GetProfile(ctx, quality.EbookProfileID)
	if err != nil || ebook.Name != "Ebook" {
		t.Fatalf("ebook profile = %+v err %v", ebook, err)
	}
	if ebook.Target != (quality.Quality{Source: quality.SourceEPUB}) {
		t.Errorf("ebook target = %+v", ebook.Target)
	}
}

func TestProfileDownloadPriorityRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	p := quality.Profile{
		Name: "Urgent 1080p", Target: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		UpgradesAllowed: true, DownloadPriority: 100,
	}
	id, err := db.AddProfile(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetProfile(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.DownloadPriority != 100 {
		t.Fatalf("download priority = %d, want 100", got.DownloadPriority)
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

	// Files exist but nothing is known about them: HasFiles yes, Best nil.
	// That distinction is the whole of ADR 0013 -- "on disk, unverified" is
	// not "missing", and the old single ok flag could not say so.
	state, err := db.DiskStateForItem(ctx, itemID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !state.HasFiles {
		t.Error("two files were just upserted; HasFiles should be true")
	}
	if state.Best != nil {
		t.Errorf("no qualities set yet, but Best = %v", state.Best)
	}

	db.SetFileQuality(ctx, f1, quality.Quality{Source: quality.SourceHDTV, Resolution: 720})
	db.SetFileQuality(ctx, f2, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080})
	state, err = db.DiskStateForItem(ctx, itemID, 0)
	if err != nil || state.Best == nil || state.Best.Resolution != 1080 {
		t.Errorf("best = %+v err=%v", state.Best, err)
	}
	if state.SourceVerified {
		t.Error("quality written with no provenance should not count as verified")
	}

	// The same quality with a provenance a human or a name supplied IS
	// verified, which is what stops the don't-churn rule from firing.
	if err := db.SetFileQualityFrom(ctx, f2,
		quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080},
		mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	state, err = db.DiskStateForItem(ctx, itemID, 0)
	if err != nil || !state.SourceVerified {
		t.Errorf("filename provenance should be verified: %+v err=%v", state, err)
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
