package library

import (
	"context"
	"errors"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// mutableProvider serves whatever the test loads into it, so a second call
// can simulate the provider learning about new episodes / ratings.
type mutableProvider struct {
	movie  domain.MediaItem
	series domain.MediaItem
}

func (p *mutableProvider) SearchMovies(ctx context.Context, q string) ([]ports.SearchResult, error) {
	return nil, nil
}
func (p *mutableProvider) SearchSeries(ctx context.Context, q string) ([]ports.SearchResult, error) {
	return nil, nil
}
func (p *mutableProvider) GetMovie(ctx context.Context, id int64) (domain.MediaItem, error) {
	return p.movie, nil
}
func (p *mutableProvider) GetSeries(ctx context.Context, id int64) (domain.MediaItem, error) {
	return p.series, nil
}

func seriesV1() domain.MediaItem {
	return domain.MediaItem{
		Kind: domain.KindSeries, Title: "Test Show", SortTitle: "test show",
		Year: 2020, IDs: domain.ExternalIDs{TMDB: 100},
		Status: "Returning Series",
		Seasons: []domain.Season{{
			Number: 1, Monitored: true,
			Episodes: []domain.Episode{
				{SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot", AirDate: "2020-01-01", Monitored: true},
				{SeasonNumber: 1, EpisodeNumber: 2, Title: "Two", AirDate: "2020-01-08", Monitored: true},
			},
		}},
	}
}

func TestRefreshItemUpdatesMetadataAndGrowsTree(t *testing.T) {
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

	prov := &mutableProvider{series: seriesV1()}
	svc := New(db, prov, b, nil)

	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	if item.Rating != 0 || item.RatingVotes != 0 {
		t.Fatalf("expected no rating yet: %+v", item)
	}

	// A human unmonitors episode 2; the refresh must not undo that.
	if _, err := db.W.ExecContext(ctx,
		`UPDATE episodes SET monitored = 0 WHERE media_item_id = ? AND episode_number = 2`, item.ID); err != nil {
		t.Fatal(err)
	}

	// The provider learns things: a rating, a new episode, a whole new
	// season, an IMDb id, and a retitled pilot.
	v2 := seriesV1()
	v2.Rating, v2.RatingVotes = 8.5, 1234
	v2.Status = "Ended"
	v2.Ended = true
	v2.IDs.IMDB = "tt7777777"
	v2.Seasons[0].Episodes[0].Title = "Pilot (extended)"
	v2.Seasons[0].Episodes = append(v2.Seasons[0].Episodes,
		domain.Episode{SeasonNumber: 1, EpisodeNumber: 3, Title: "Three", AirDate: "2020-01-15", Monitored: true})
	v2.Seasons = append(v2.Seasons, domain.Season{
		Number: 2, Monitored: true,
		Episodes: []domain.Episode{
			{SeasonNumber: 2, EpisodeNumber: 1, Title: "Return", AirDate: "2021-01-01", Monitored: true},
		},
	})
	prov.series = v2

	got, err := svc.RefreshItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rating != 8.5 || got.RatingVotes != 1234 {
		t.Errorf("rating = %v/%d", got.Rating, got.RatingVotes)
	}
	if got.Status != "Ended" || !got.Ended {
		t.Errorf("status = %q ended=%v", got.Status, got.Ended)
	}
	if got.IDs.IMDB != "tt7777777" {
		t.Errorf("imdb = %q", got.IDs.IMDB)
	}
	if len(got.Seasons) != 2 {
		t.Fatalf("seasons = %d", len(got.Seasons))
	}
	s1 := got.Seasons[0]
	if len(s1.Episodes) != 3 || s1.Episodes[2].Title != "Three" {
		t.Fatalf("season 1 = %+v", s1.Episodes)
	}
	if s1.Episodes[0].Title != "Pilot (extended)" {
		t.Errorf("episode title not refreshed: %q", s1.Episodes[0].Title)
	}
	if !s1.Episodes[0].Monitored || s1.Episodes[1].Monitored {
		t.Errorf("monitored flags not preserved: ep1=%v ep2=%v",
			s1.Episodes[0].Monitored, s1.Episodes[1].Monitored)
	}
	if !got.Monitored {
		t.Error("item monitoring must survive a refresh")
	}

	// Refresh is idempotent — running it again changes nothing.
	again, err := svc.RefreshItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Seasons) != 2 || len(again.Seasons[0].Episodes) != 3 {
		t.Errorf("second refresh mutated the tree: %+v", again.Seasons)
	}
}

func TestUpdateItemEditsPlacement(t *testing.T) {
	svc, db, _ := newService(t)
	ctx := context.Background()

	rootA, rootB := t.TempDir(), t.TempDir()
	rfA, err := svc.AddRootFolder(ctx, rootA, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	rfB, err := svc.AddRootFolder(ctx, rootB, domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}

	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rfA.ID, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}

	// Move to root B: the folder is recomputed, files are not touched.
	mon := false
	profile := int64(2)
	got, err := svc.UpdateItem(ctx, item.ID, UpdateRequest{
		Monitored: &mon, QualityProfileID: &profile, RootFolderID: &rfB.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Monitored || got.QualityProfileID != 2 || got.RootFolderID != rfB.ID {
		t.Errorf("item = monitored=%v profile=%d root=%d", got.Monitored, got.QualityProfileID, got.RootFolderID)
	}
	if got.Path == item.Path || got.Path == "" {
		t.Errorf("path should move under root B: %q", got.Path)
	}

	// Explicit path override wins.
	p := rootA + "/Custom Folder"
	got, err = svc.UpdateItem(ctx, item.ID, UpdateRequest{Path: &p})
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != p {
		t.Errorf("path = %q, want %q", got.Path, p)
	}

	// Relative paths are rejected.
	bad := "relative/folder"
	if _, err := svc.UpdateItem(ctx, item.ID, UpdateRequest{Path: &bad}); err == nil {
		t.Error("relative path should be rejected")
	}
	_ = db
}

func TestSeasonAndEpisodeMonitoring(t *testing.T) {
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

	prov := &mutableProvider{series: seriesV1()}
	svc := New(db, prov, b, nil)
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}

	// Unmonitor the season: cascades to every episode.
	got, err := svc.SetSeasonMonitored(ctx, item.ID, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	s1 := got.Seasons[0]
	if s1.Monitored || s1.Episodes[0].Monitored || s1.Episodes[1].Monitored {
		t.Fatalf("cascade failed: %+v", s1)
	}

	// Re-monitor a single episode inside the unmonitored season.
	got, err = svc.SetEpisodeMonitored(ctx, item.ID, s1.Episodes[0].ID, true)
	if err != nil {
		t.Fatal(err)
	}
	s1 = got.Seasons[0]
	if !s1.Episodes[0].Monitored || s1.Episodes[1].Monitored || s1.Monitored {
		t.Fatalf("episode flag = %+v", s1)
	}

	// A refresh that brings a NEW episode into the unmonitored season must
	// deliver it unmonitored (season flags are the user's).
	v2 := seriesV1()
	v2.Seasons[0].Episodes = append(v2.Seasons[0].Episodes,
		domain.Episode{SeasonNumber: 1, EpisodeNumber: 3, Title: "Three", AirDate: "2020-01-15", Monitored: true})
	prov.series = v2
	got, err = svc.RefreshItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	s1 = got.Seasons[0]
	if len(s1.Episodes) != 3 {
		t.Fatalf("episodes = %d", len(s1.Episodes))
	}
	if s1.Episodes[2].Monitored {
		t.Error("new episode in an unmonitored season must arrive unmonitored")
	}
	if !s1.Episodes[0].Monitored {
		t.Error("explicitly re-monitored episode must survive the refresh")
	}

	// Unknown season / foreign episode → not found.
	if _, err := svc.SetSeasonMonitored(ctx, item.ID, 99, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("season 99: %v", err)
	}
	if _, err := svc.SetEpisodeMonitored(ctx, item.ID, 999999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("bogus episode: %v", err)
	}
}

func TestAddMonitorPresets(t *testing.T) {
	multi := seriesV1()
	multi.Seasons = append(multi.Seasons, domain.Season{
		Number: 2, Monitored: true,
		Episodes: []domain.Episode{
			{SeasonNumber: 2, EpisodeNumber: 1, Title: "Return", AirDate: "2021-01-01", Monitored: true},
		},
	})

	for _, tc := range []struct {
		preset string
		wantS1 bool
		wantS2 bool
	}{
		{"all", true, true},
		{"", true, true},
		{"latest", false, true},
		{"none", false, false},
	} {
		db, err := sqlite.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		b := bus.New(nil)
		svc := New(db, &mutableProvider{series: multi}, b, nil)

		item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true, Monitor: tc.preset})
		if err != nil {
			t.Fatal(err)
		}
		s1, s2 := item.Seasons[0], item.Seasons[1]
		if s1.Monitored != tc.wantS1 || s2.Monitored != tc.wantS2 {
			t.Errorf("preset %q: s1=%v s2=%v", tc.preset, s1.Monitored, s2.Monitored)
		}
		if s2.Episodes[0].Monitored != tc.wantS2 || s1.Episodes[0].Monitored != tc.wantS1 {
			t.Errorf("preset %q: episode flags s1=%v s2=%v", tc.preset,
				s1.Episodes[0].Monitored, s2.Episodes[0].Monitored)
		}
		b.Close()
		db.Close()
	}
}
