package acquisition

import (
	"context"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// autoSetup builds a service with one monitored, missing movie and one
// enabled indexer/client pair — the minimal automation scenario.
func autoSetup(t *testing.T, releases []ports.Release, client *fakeClient) (*Service, *sqlite.DB, int64) {
	t.Helper()
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
		func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{releases: releases} },
		func(ports.ClientConfig) ports.DownloadClient { return client },
	)

	movieID, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Test Movie", SortTitle: "test movie",
		Year: 2024, IDs: domain.ExternalIDs{TMDB: 601}, Monitored: true,
		Path: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "idx", URL: "http://x",
		Protocol: "torrent", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddDownloadClient(ctx, ports.ClientConfig{Type: "qbittorrent",
		Name: "qb", URL: "http://qb", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return svc, db, movieID
}

func rel(title string, seeders int) ports.Release {
	return ports.Release{
		Title: title, DownloadURL: "http://dl/" + title, Indexer: "idx",
		Protocol: "torrent", Seeders: seeders, Size: 1000,
	}
}

func TestWantedIndexAndRSSAutoGrab(t *testing.T) {
	client := &fakeClient{}
	svc, db, movieID := autoSetup(t,
		[]ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-GRP", 50)}, client)
	ctx := context.Background()

	// The movie is wanted (missing).
	wanted, err := svc.WantedList(ctx)
	if err != nil || len(wanted) != 1 || !wanted[0].Missing || wanted[0].MediaItemID != movieID {
		t.Fatalf("wanted = %+v err %v", wanted, err)
	}

	// RSS pass grabs it.
	if err := svc.SyncRSS(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 {
		t.Fatalf("adds after rss = %d, want 1", len(client.added))
	}
	dls, _ := db.ListRecentDownloads(ctx)
	if len(dls) != 1 || dls[0].State != "grabbed" {
		t.Fatalf("downloads = %+v", dls)
	}

	// Second pass: the wantable is in flight — no duplicate grab.
	if err := svc.SyncRSS(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 {
		t.Errorf("adds after second rss = %d, want still 1", len(client.added))
	}
}

func TestBacklogSearchGrabsBest(t *testing.T) {
	client := &fakeClient{}
	svc, _, _ := autoSetup(t, []ports.Release{
		rel("Test.Movie.2024.720p.WEB-DL.x264-LOW", 90),
		rel("Test.Movie.2024.1080p.BluRay.x264-BEST", 10),
		rel("Unrelated.Film.2020.1080p.WEB-DL", 99),
	}, client)
	ctx := context.Background()

	if err := svc.BacklogSearch(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 {
		t.Fatalf("adds = %v, want exactly the best release", client.added)
	}
	if client.added[0] != "http://dl/Test.Movie.2024.1080p.BluRay.x264-BEST" {
		t.Errorf("grabbed %q, want the Bluray 1080p", client.added[0])
	}
}

func TestFailedDownloadBlocklistsAndResearches(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := autoSetup(t, []ports.Release{
		rel("Test.Movie.2024.1080p.WEB-DL.x264-AAA", 50),
		rel("Test.Movie.2024.1080p.WEB-DL.x264-BBB", 10),
	}, client)
	ctx := context.Background()

	// RSS grabs the higher-seeded AAA first (rss iterates releases in order;
	// both match, but the first grab wins the wantable for the run).
	if err := svc.SyncRSS(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 || client.added[0] != "http://dl/Test.Movie.2024.1080p.WEB-DL.x264-AAA" {
		t.Fatalf("initial grab = %v", client.added)
	}

	// The client reports it failed → blocklist + automatic re-search grabs BBB.
	client.statuses = []ports.DownloadStatus{{Handle: "h1", State: ports.StateFailed, Message: "stalled"}}
	if err := svc.RefreshQueue(ctx); err != nil {
		t.Fatal(err)
	}
	blocked, err := db.IsBlocklisted(ctx, "Test.Movie.2024.1080p.WEB-DL.x264-AAA", "idx")
	if err != nil || !blocked {
		t.Errorf("AAA blocklisted = %v err %v", blocked, err)
	}
	if len(client.added) != 2 || client.added[1] != "http://dl/Test.Movie.2024.1080p.WEB-DL.x264-BBB" {
		t.Fatalf("re-search adds = %v, want BBB grabbed", client.added)
	}

	// Blocklist survives into future searches: AAA never comes back.
	list, err := db.ListBlocklist(ctx)
	if err != nil || len(list) != 1 || list[0].Reason != "stalled" {
		t.Errorf("blocklist = %+v err %v", list, err)
	}
}

func TestAutoSearchItemGrabsBest(t *testing.T) {
	client := &fakeClient{}
	svc, db, movieID := autoSetup(t, []ports.Release{
		rel("Test.Movie.2024.720p.WEB-DL.x264-LOW", 90),
		rel("Test.Movie.2024.1080p.BluRay.x264-BEST", 10),
	}, client)
	ctx := context.Background()

	if err := svc.AutoSearchItem(ctx, movieID); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 || client.added[0] != "http://dl/Test.Movie.2024.1080p.BluRay.x264-BEST" {
		t.Fatalf("auto search adds = %v, want the best release", client.added)
	}
	dls, _ := db.ListRecentDownloads(ctx)
	if len(dls) != 1 || dls[0].State != "grabbed" {
		t.Fatalf("downloads = %+v", dls)
	}

	// Second call: the wantable is in flight — nothing double-grabbed.
	if err := svc.AutoSearchItem(ctx, movieID); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 {
		t.Errorf("second auto search grabbed again: %v", client.added)
	}
}

// The backlog has a per-run cap, so ordering is not cosmetic: what is
// searched first is, on a large backlog, what is searched at all. Being three
// episodes into a series and waiting a week for the upgrade — while a film
// nobody has opened in two years is retried nightly — is the case this fixes.
func TestTheBacklogSearchesWhatSomebodyIsWatchingFirst(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := autoSetup(t, nil, client)
	ctx := context.Background()

	cold, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Alien", Year: 1979, Monitored: true,
		IDs: domain.ExternalIDs{TMDB: 348},
	})
	if err != nil {
		t.Fatal(err)
	}
	hot, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Heat", Year: 1995, Monitored: true,
		IDs: domain.ExternalIDs{TMDB: 949},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nothing watched: the order is whatever it was.
	before := svc.watchedFirst(ctx, []domain.Wantable{
		domain.MovieWantable{Item: cold},
		domain.MovieWantable{Item: hot},
	})
	if before[0].MediaItemID() != cold {
		t.Errorf("with no signal the order must not change, got %d first", before[0].MediaItemID())
	}

	// Somebody watched Heat this morning.
	if err := db.RecordPlurxWatched(ctx, sqlite.PlurxWatch{
		MediaItemID: hot, Username: "paul", WatchedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	after := svc.watchedFirst(ctx, []domain.Wantable{
		domain.MovieWantable{Item: cold},
		domain.MovieWantable{Item: hot},
	})
	if after[0].MediaItemID() != hot {
		t.Errorf("the watched item was not searched first: %d", after[0].MediaItemID())
	}
	if len(after) != 2 || after[1].MediaItemID() != cold {
		t.Errorf("reordering dropped or duplicated something: %v", after)
	}
}
