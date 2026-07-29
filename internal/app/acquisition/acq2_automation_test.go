package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/format"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
)

// The unattended loops are where monarr spends every night, and every branch
// below is one of the ways a release gets declined without a person watching.
// A loop that silently grabs what it should have declined is the failure that
// costs real bandwidth and a wrong file in the library.

// recordingIndexer answers every query with the same releases and remembers
// what it was asked, so a test can assert what the loop actually searched for.
type recordingIndexer struct {
	mu       sync.Mutex
	releases []ports.Release
	queries  []string
	fail     error
}

func (r *recordingIndexer) Search(ctx context.Context, q domain.SearchQuery) ([]ports.Release, error) {
	r.mu.Lock()
	r.queries = append(r.queries, q.Q)
	r.mu.Unlock()
	if r.fail != nil {
		return nil, r.fail
	}
	return r.releases, nil
}

func (r *recordingIndexer) FetchRSS(ctx context.Context) ([]ports.Release, error) {
	r.mu.Lock()
	r.queries = append(r.queries, "<rss>")
	r.mu.Unlock()
	if r.fail != nil {
		return nil, r.fail
	}
	return r.releases, nil
}

func (r *recordingIndexer) Test(ctx context.Context) error { return nil }

func (r *recordingIndexer) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

// refusingClient is a download client that will not accept an add — a full
// disk, a bad category, an API key that expired overnight.
type refusingClient struct{ fakeClient }

func (c *refusingClient) Add(ctx context.Context, url, cat string) (ports.Handle, error) {
	return "", errors.New("client refused the add")
}

// Three separate reasons to say no, in one pass, on an unattended loop: the
// release is already blocklisted, the release is above what the profile calls
// done, and the advertised size cannot hold what the name claims. Any of them
// leaking through is a grab nobody asked for.
func TestAcq2RSSDeclinesBlocklistedOversizedAndImplausibleReleases(t *testing.T) {
	client := &fakeClient{}
	blocked := rel("Test.Movie.2024.1080p.WEB-DL.x264-BANNED", 80)
	tooBig := rel("Test.Movie.2024.2160p.WEB-DL.x264-UHD", 70)
	runt := rel("Test.Movie.2024.1080p.BluRay.x264-RUNT", 60)
	runt.Size = 40 << 20 // 40 MB cannot be two hours of 1080p BluRay
	runt2 := rel("Test.Movie.2024.1080p.BluRay.x264-RUNT2", 50)
	runt2.Size = 40 << 20
	svc, db, movieID := autoSetup(t, []ports.Release{blocked, tooBig, runt, runt2}, client)
	ctx := context.Background()

	// A runtime is what turns an advertised size into a bitrate, so the movie
	// needs one for the size rule to have anything to say.
	item, _ := db.GetMediaItemFull(ctx, movieID)
	item.Runtime = 120
	if err := db.UpdateMediaItemMetadata(ctx, movieID, item); err != nil {
		t.Fatal(err)
	}
	if err := db.AddBlocklist(ctx, movieID, blocked.Title, "idx", "failed before"); err != nil {
		t.Fatal(err)
	}

	if err := svc.SyncRSS(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 0 {
		t.Fatalf("the RSS pass grabbed something it should have declined: %v", client.added)
	}
	rows, _ := db.ListRecentDownloads(ctx)
	if len(rows) != 0 {
		t.Errorf("download rows created: %+v", rows)
	}

	// And the same pass still grabs a release that clears all three.
	good := rel("Test.Movie.2024.1080p.WEB-DL.x264-GOOD", 10)
	good.Size = 8 << 30
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer {
		return fakeIndexer{releases: []ports.Release{good}}
	}
	svc.InvalidateWanted()
	if err := svc.SyncRSS(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 {
		t.Errorf("a plausible release was not grabbed: %v", client.added)
	}
}

// One indexer being down must not stop the pass. Returning early would mean a
// single flaky tracker suspends automation for the whole library.
func TestAcq2RSSSurvivesAnIndexerThatFails(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := autoSetup(t, nil, client)
	ctx := context.Background()

	broken := &recordingIndexer{fail: errors.New("502 bad gateway")}
	good := &recordingIndexer{releases: []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-OK", 5)}}
	if _, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "second", URL: "http://y",
		Protocol: "torrent", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	svc.newIndexer = func(cfg ports.IndexerConfig) ports.Indexer {
		if cfg.Name == "idx" {
			return broken
		}
		return good
	}

	if err := svc.SyncRSS(ctx); err != nil {
		t.Fatalf("a failing indexer aborted the pass: %v", err)
	}
	if len(client.added) != 1 {
		t.Errorf("the healthy indexer's release was not grabbed: %v", client.added)
	}
}

// Nothing wanted means nothing to do — and, importantly, no indexer traffic.
// A loop that queries every tracker to discover it has nothing to look for is
// how a quiet library still gets rate-limited.
func TestAcq2LoopsDoNotTouchIndexersWhenNothingIsWanted(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	no := false
	if err := db.BulkUpdateItem(ctx, movieID, &no, nil); err != nil {
		t.Fatal(err)
	}
	idx := &recordingIndexer{}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return idx }
	svc.InvalidateWanted()

	if err := svc.SyncRSS(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.BacklogSearch(ctx); err != nil {
		t.Fatal(err)
	}
	if got := idx.asked(); len(got) != 0 {
		t.Errorf("queried indexers with an empty wanted list: %v", got)
	}
}

// With no indexers configured neither loop can do anything, and neither may
// report a failure for it — a fresh install has no indexers yet.
func TestAcq2LoopsAreQuietWithNoIndexersConfigured(t *testing.T) {
	svc, db, _ := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	indexers, _ := db.ListIndexers(ctx)
	for _, ix := range indexers {
		if err := db.DeleteIndexer(ctx, ix.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.SyncRSS(ctx); err != nil {
		t.Errorf("SyncRSS with no indexers = %v", err)
	}
	if err := svc.BacklogSearch(ctx); err != nil {
		t.Errorf("BacklogSearch with no indexers = %v", err)
	}
}

// A client that refuses the add must leave nothing behind. The row is inserted
// before the add (the transfer id is built from it), so a refusal that did not
// undo it would leave a queue row describing a download nobody has — and the
// in-flight filter would then suppress the want forever.
func TestAcq2AGrabTheClientRefusesLeavesNoQueueRow(t *testing.T) {
	client := &refusingClient{}
	svc, db, _ := autoSetup(t,
		[]ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-GRP", 5)}, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()

	if err := svc.SyncRSS(ctx); err != nil {
		t.Fatalf("a refused add aborted the pass: %v", err)
	}
	rows, err := db.ListRecentDownloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("a refused add left %d row(s) behind: %+v", len(rows), rows)
	}
	// The want survives, so the next pass can try a different release.
	svc.InvalidateWanted()
	wanted, _ := svc.Wanted(ctx)
	if len(svc.notInFlight(ctx, wanted)) != 1 {
		t.Error("the wantable was consumed by a grab that never happened")
	}
}

// The per-run cap is what stops a large backlog hammering every indexer in one
// pass. Without it, a first run on a 2000-item library is a denial-of-service
// against the trackers it depends on.
func TestAcq2BacklogStopsAtThePerRunCap(t *testing.T) {
	svc, db, _ := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	idx := &recordingIndexer{}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return idx }

	// autoSetup already contributes one wanted movie.
	for i := 0; i < backlogPerRun+5; i++ {
		if _, err := db.CreateMediaItem(ctx, domain.MediaItem{
			Kind: domain.KindMovie, Title: fmt.Sprintf("Filler %d", i),
			SortTitle: fmt.Sprintf("filler %d", i), Year: 2000 + i,
			IDs: domain.ExternalIDs{TMDB: int64(9000 + i)}, Monitored: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	svc.InvalidateWanted()
	if err := svc.BacklogSearch(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(idx.asked()); got != backlogPerRun {
		t.Errorf("the backlog searched %d wantables in one pass, cap is %d", got, backlogPerRun)
	}
}

// A wantable pointing at a profile that no longer exists cannot be judged, so
// the search has to stop rather than grab under a zero-valued profile — which
// would accept anything at all.
func TestAcq2SearchAndGrabBestRefusesAMissingProfile(t *testing.T) {
	client := &fakeClient{}
	svc, _, movieID := autoSetup(t,
		[]ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-GRP", 5)}, client)
	ctx := context.Background()
	enabled, err := svc.enabledIndexers(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = svc.searchAndGrabBest(ctx, domain.MovieWantable{
		Item: movieID, Profile: 4242, Mon: true, Title: "Test Movie", Year: 2024,
	}, enabled)
	if err == nil {
		t.Fatal("searched under a profile that does not exist")
	}
	if len(client.added) != 0 {
		t.Errorf("grabbed anyway: %v", client.added)
	}
}

// Same quality is the common case — the ladder only has so many rungs — so the
// tie-breaks are what actually choose the release most nights: custom-format
// score first, then seeders.
func TestAcq2BacklogBreaksQualityTiesOnScoreThenSeeders(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := autoSetup(t, []ports.Release{
		rel("Test.Movie.2024.1080p.WEB-DL.x264-PLAIN", 99),
		rel("Test.Movie.2024.1080p.WEB-DL.x264-REMASTERED", 1),
	}, client)
	ctx := context.Background()
	if _, err := db.AddCustomFormat(ctx, format.CustomFormat{
		Name: "Remaster", Pattern: "remastered", Score: 50,
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.BacklogSearch(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 {
		t.Fatalf("adds = %v", client.added)
	}
	if client.added[0] != "http://dl/Test.Movie.2024.1080p.WEB-DL.x264-REMASTERED" {
		t.Errorf("grabbed %q — the 99-seeder release beat a +50 custom format", client.added[0])
	}

	// With no format to separate them, seeders decide.
	client2 := &fakeClient{}
	svc2, _, _ := autoSetup(t, []ports.Release{
		rel("Test.Movie.2024.1080p.WEB-DL.x264-AAA", 3),
		rel("Test.Movie.2024.1080p.WEB-DL.x264-BBB", 300),
	}, client2)
	if err := svc2.BacklogSearch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client2.added) != 1 || client2.added[0] != "http://dl/Test.Movie.2024.1080p.WEB-DL.x264-BBB" {
		t.Errorf("seeder tie-break grabbed %v", client2.added)
	}
}

// The backlog fans out across indexers; one of them failing must not lose the
// releases the others returned.
func TestAcq2BacklogSurvivesAnIndexerThatFails(t *testing.T) {
	client := &fakeClient{}
	svc, db, _ := autoSetup(t, nil, client)
	ctx := context.Background()
	if _, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "second", URL: "http://y",
		Protocol: "torrent", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	broken := &recordingIndexer{fail: errors.New("timeout")}
	good := &recordingIndexer{releases: []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-OK", 4)}}
	svc.newIndexer = func(cfg ports.IndexerConfig) ports.Indexer {
		if cfg.Name == "idx" {
			return broken
		}
		return good
	}

	if err := svc.BacklogSearch(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 {
		t.Errorf("a failing indexer cost the pass its result: %v", client.added)
	}
}

// The size rule gates the robot on the backlog too, not only on RSS. 700 MB
// cannot be a two-hour 1080p BluRay whatever the name says.
func TestAcq2BacklogDeclinesAnImplausiblySmallRelease(t *testing.T) {
	client := &fakeClient{}
	runt := rel("Test.Movie.2024.1080p.BluRay.x264-RUNT", 40)
	runt.Size = 40 << 20
	svc, db, movieID := autoSetup(t, []ports.Release{runt}, client)
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, movieID)
	item.Runtime = 120
	if err := db.UpdateMediaItemMetadata(ctx, movieID, item); err != nil {
		t.Fatal(err)
	}

	if err := svc.BacklogSearch(ctx); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 0 {
		t.Errorf("grabbed a release too small to hold what it claims: %v", client.added)
	}
}

// "Search on add" for a series is per monitored season, and per monitored copy
// — a copy hunts under its own profile into its own folder, so it needs its own
// search or it is never filled at all.
func TestAcq2AutoSearchItemCoversEverySeasonAndCopy(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	idx := &recordingIndexer{}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return idx }

	// A second season, plus a specials season that must never be searched.
	item, _ := db.GetMediaItemFull(ctx, itemID)
	item.Seasons = append(item.Seasons,
		domain.Season{Number: 0, Monitored: true, Episodes: []domain.Episode{
			{SeasonNumber: 0, EpisodeNumber: 1, Title: "Special", Monitored: true},
		}},
		domain.Season{Number: 2, Monitored: true, Episodes: []domain.Episode{
			{SeasonNumber: 2, EpisodeNumber: 1, Title: "Return", Monitored: true},
		}},
	)
	if err := db.UpdateMediaItemMetadata(ctx, itemID, item); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: itemID, Name: "for dad", QualityProfileID: 2,
		Path: t.TempDir(), Monitored: true,
	}); err != nil {
		t.Fatal(err)
	}
	// An unmonitored copy is not a target, however many seasons it has.
	if _, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: itemID, Name: "paused", QualityProfileID: 1,
		Path: t.TempDir(), Monitored: false,
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.AutoSearchItem(ctx, itemID); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, q := range idx.asked() {
		counts[q]++
	}
	// Two seasons for the primary and two for the monitored copy.
	if counts["Test Show S01"] != 2 || counts["Test Show S02"] != 2 {
		t.Errorf("season searches = %v, want S01 and S02 twice each (primary + copy)", counts)
	}
	if counts["Test Show S00"] != 0 {
		t.Errorf("specials were searched: %v", counts)
	}
}

// An item-wide search with nothing to search with is a distinct answer, not a
// silent success: the UI has to be able to say "configure an indexer first".
func TestAcq2AutoSearchItemReportsMissingIndexersAndItems(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()

	if err := svc.AutoSearchItem(ctx, 987654); err == nil {
		t.Error("auto search accepted an item that does not exist")
	}
	indexers, _ := db.ListIndexers(ctx)
	for _, ix := range indexers {
		if err := db.DeleteIndexer(ctx, ix.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.AutoSearchItem(ctx, movieID); !errors.Is(err, ErrNoIndexers) {
		t.Errorf("err = %v, want ErrNoIndexers", err)
	}
}

// An unmonitored item still has targets — it just must not be searched for
// them. Monitoring is the switch people actually use to pause a show.
func TestAcq2AutoSearchItemSkipsUnmonitoredTargets(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	idx := &recordingIndexer{}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return idx }
	no := false
	if err := db.BulkUpdateItem(ctx, movieID, &no, nil); err != nil {
		t.Fatal(err)
	}

	if err := svc.AutoSearchItem(ctx, movieID); err != nil {
		t.Fatal(err)
	}
	if got := idx.asked(); len(got) != 0 {
		t.Errorf("searched for an unmonitored item: %v", got)
	}
}

// The wanted list is what the Wanted page renders. "Missing" and "wants an
// upgrade" are different rows with different meaning, and the order has to be
// stable or the page reshuffles on every refresh.
func TestAcq2WantedListSeparatesMissingFromUpgradableAndSortsStably(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, movieID)

	// A 720p file under a 1080p profile: present, but below the target.
	have := filepath.Join(item.Path, "Test Movie (2024) [HDTV-720p].mkv")
	if err := os.WriteFile(have, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fid, err := db.UpsertFile(ctx, movieID, 0, have, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQualityFrom(ctx, fid, quality.Quality{Source: quality.SourceHDTV, Resolution: 720},
		mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	// A second, entirely missing film that sorts before it.
	if _, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Alien", SortTitle: "alien", Year: 1979,
		IDs: domain.ExternalIDs{TMDB: 348}, Monitored: true,
	}); err != nil {
		t.Fatal(err)
	}
	svc.InvalidateWanted()

	got, err := svc.WantedList(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("wanted = %+v", got)
	}
	if got[0].Title != "Alien" || !got[0].Missing || got[0].Current != "" {
		t.Errorf("first entry = %+v, want the missing film first", got[0])
	}
	if got[1].Title != "Test Movie" {
		t.Fatalf("second entry = %+v", got[1])
	}
	if got[1].Missing {
		t.Error("a film with a 720p file on disk is reported as missing")
	}
	if got[1].Current == "" || got[1].Detail != "(2024)" {
		t.Errorf("upgrade entry = %+v, want its current quality and year", got[1])
	}
}
