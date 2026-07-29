package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// seriesSetup builds a series whose episodes have air dates, because an episode
// with no air date has not happened yet and must never be hunted.
func seriesSetup(t *testing.T, client *fakeClient) (*Service, *sqlite.DB, int64) {
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
		func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} },
		func(ports.ClientConfig) ports.DownloadClient { return client },
	)
	id, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindSeries, Title: "Test Show", SortTitle: "test show", Year: 2020,
		IDs: domain.ExternalIDs{TMDB: 100}, Monitored: true, Path: t.TempDir(),
		QualityProfileID: 1,
		Seasons: []domain.Season{
			{Number: 1, Monitored: true, Episodes: []domain.Episode{
				{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot", Monitored: true, AirDate: "2020-01-01"},
				{SeasonNumber: 1, EpisodeNumber: 2, Title: "Second", Monitored: false, AirDate: "2020-01-08"},
				{SeasonNumber: 1, EpisodeNumber: 3, Title: "Unaired", Monitored: true},
			}},
			{Number: 2, Monitored: false, Episodes: []domain.Episode{
				{SeasonNumber: 2, EpisodeNumber: 1, Title: "Later", Monitored: true, AirDate: "2021-01-01"},
			}},
		},
	})
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
	return svc, db, id
}

// The wanted index is the input to every unattended loop, so what it leaves out
// matters as much as what it contains: an unmonitored season, an unmonitored
// episode and an episode that has not aired are each a different reason not to
// go looking, and any of them leaking in is a nightly search for something
// nobody can have.
func TestAcq2WantedSkipsUnmonitoredAndUnairedEpisodes(t *testing.T) {
	svc, _, itemID := seriesSetup(t, &fakeClient{})
	ctx := context.Background()

	wanted, err := svc.Wanted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, w := range wanted {
		ids = append(ids, string(w.ID()))
	}
	if len(ids) != 1 || ids[0] != "episode:"+itoa(itemID)+":1:1" {
		t.Fatalf("wanted = %v, want only the aired, monitored episode", ids)
	}

	// The second call is served from the cache; invalidating has to make the
	// next one rebuild, or an import never changes what is hunted.
	again, err := svc.Wanted(ctx)
	if err != nil || len(again) != 1 {
		t.Fatalf("cached wanted = %v err %v", again, err)
	}
}

// Each monitored copy hunts its own file set under its own profile — the 720p
// copy keeps looking even when the 4K primary is done — and a season pack in
// flight for ONE copy must not silence the other's episodes.
func TestAcq2CopiesWantTheirOwnEpisodesAndPacksOnlyCoverTheirOwn(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := seriesSetup(t, client)
	ctx := context.Background()

	copyID, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: itemID, Name: "for dad", QualityProfileID: 2,
		Path: t.TempDir(), Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// An unmonitored copy contributes nothing at all.
	if _, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: itemID, Name: "paused", QualityProfileID: 1,
		Path: t.TempDir(), Monitored: false,
	}); err != nil {
		t.Fatal(err)
	}
	svc.InvalidateWanted()

	wanted, err := svc.Wanted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, w := range wanted {
		ids[string(w.ID())] = true
	}
	primary := "episode:" + itoa(itemID) + ":1:1"
	copyWant := primary + ":c" + itoa(copyID)
	if !ids[primary] || !ids[copyWant] || len(ids) != 2 {
		t.Fatalf("wanted = %v, want the primary's episode and the copy's", ids)
	}

	// A season pack in flight for the PRIMARY covers the primary's episode…
	if _, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 0,
		Title: "Test.Show.S01.1080p.WEB-DL-PACK", DownloadURL: "m",
		Indexer: "idx", Protocol: "torrent",
	}); err != nil {
		t.Fatal(err)
	}
	svc.InvalidateWanted()
	wanted, _ = svc.Wanted(ctx)
	left := svc.notInFlight(ctx, wanted)
	if len(left) != 1 || string(left[0].ID()) != copyWant {
		t.Fatalf("after the primary's pack, still wanted = %v, want only the copy's episode",
			wantableIDs(left))
	}

	// …and a pack for the COPY covers the copy's.
	if _, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, CopyID: copyID, Season: 1, Episode: 0,
		Title: "Test.Show.S01.720p.WEB-DL-PACK", DownloadURL: "m2",
		Indexer: "idx", Protocol: "torrent",
	}); err != nil {
		t.Fatal(err)
	}
	svc.InvalidateWanted()
	wanted, _ = svc.Wanted(ctx)
	if left := svc.notInFlight(ctx, wanted); len(left) != 0 {
		t.Errorf("still wanted with both packs in flight: %v", wantableIDs(left))
	}
}

func wantableIDs(ws []domain.Wantable) []string {
	var out []string
	for _, w := range ws {
		out = append(out, string(w.ID()))
	}
	return out
}

// An item — or a copy — pointing at a profile that no longer exists cannot be
// judged, so it is left out rather than hunted under a zero-valued profile that
// would accept literally anything.
func TestAcq2WantedSkipsItemsAndCopiesWithNoProfile(t *testing.T) {
	svc, db, itemID := seriesSetup(t, &fakeClient{})
	ctx := context.Background()

	if _, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: itemID, Name: "orphan", QualityProfileID: 4242,
		Path: t.TempDir(), Monitored: true,
	}); err != nil {
		t.Fatal(err)
	}
	svc.InvalidateWanted()
	wanted, err := svc.Wanted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range wanted {
		if strings.Contains(string(w.ID()), ":c") {
			t.Errorf("a copy with no profile is being hunted: %s", w.ID())
		}
	}

	// And the same for the item itself.
	gone := int64(4242)
	if err := db.BulkUpdateItem(ctx, itemID, nil, &gone); err != nil {
		t.Fatal(err)
	}
	svc.InvalidateWanted()
	wanted, err = svc.Wanted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wanted) != 0 {
		t.Errorf("wanted = %v for an item whose profile is gone", wantableIDs(wanted))
	}
}

// A season wantable is searched as a pack (no episode number); a movie has
// neither. Getting this wrong sends a pack query out as an episode query and
// finds nothing.
func TestAcq2WantableGrabTargetsMapToSearchCoordinates(t *testing.T) {
	if s, e := wantableGrabTarget(domain.SeasonWantable{Season: 3}); s != 3 || e != 0 {
		t.Errorf("season target = %d/%d, want 3/0", s, e)
	}
	if s, e := wantableGrabTarget(domain.EpisodeWantable{Season: 2, Episode: 5}); s != 2 || e != 5 {
		t.Errorf("episode target = %d/%d", s, e)
	}
	if s, e := wantableGrabTarget(domain.MovieWantable{}); s != -1 || e != 0 {
		t.Errorf("movie target = %d/%d, want -1/0", s, e)
	}
}

// A file far shorter than the feature it claims to be is not the feature. The
// item goes back to being hunted, and the history entry is the only record of
// why something that looked finished last week is being searched for again.
func TestAcq2AFileTooShortForItsRuntimeIsNotBelieved(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, _ := db.GetMediaItemFull(ctx, movieID)
	item.Runtime = 600 // ten hours: nothing real is going to match this
	if err := db.UpdateMediaItemMetadata(ctx, movieID, item); err != nil {
		t.Fatal(err)
	}

	release := "Test.Movie.2024.720p.WEB-DL-SHORT"
	payload := t.TempDir()
	if err := os.WriteFile(filepath.Join(payload, release+".mkv"),
		corpusFile(t, wholeCorpus720), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.importDownload(ctx, sqlite.Download{
		MediaItemID: movieID, ReleaseTitle: release, Indexer: "idx",
	}, payload, false); err != nil {
		t.Fatal(err)
	}

	recs, err := db.FileQualityRecords(ctx, movieID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records = %+v err %v", recs, err)
	}
	if recs[0].SourceVerified() {
		t.Errorf("a file monarr does not believe is still counted as verified: %+v", recs[0])
	}

	events, _ := db.ListHistory(ctx)
	var told bool
	for _, e := range events {
		if e.Type == HistoryImplausibleFile && e.ReleaseTitle == release {
			told = true
		}
	}
	if !told {
		t.Errorf("no %s entry: nothing explains why the item is wanted again", HistoryImplausibleFile)
	}

	// And the item is hunted again, which is the point of not believing it.
	svc.InvalidateWanted()
	wanted, err := svc.Wanted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(wanted) != 1 {
		t.Errorf("wanted = %v, want the film still being hunted", wantableIDs(wanted))
	}
}
