package library

import (
	"context"
	"errors"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/infra/sqlite/sqlitetest"
)

func offloadLib(t *testing.T) (*Service, *sqlite.DB, *mutableProvider) {
	t.Helper()
	db, err := sqlite.Open(sqlitetest.Dir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	prov := &mutableProvider{series: seriesV1()}
	prov.series.Seasons = append(prov.series.Seasons, domain.Season{Number: 2, Episodes: []domain.Episode{
		{SeasonNumber: 2, EpisodeNumber: 1, AirDate: "2999-01-01"},
	}})
	return New(db, prov, nil, nil), db, prov
}

// First season only: the lowest season above 0 is monitored on add; a season
// that appears later arrives unmonitored (it is "Monitor and search", not a
// Re-acquire), and so does one that is the second season on a refresh.
func TestFirstSeasonMonitorMode(t *testing.T) {
	ctx := context.Background()
	svc, _, prov := offloadLib(t)
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true, Monitor: "first_season"})
	if err != nil {
		t.Fatal(err)
	}
	if item.MonitorSeason != 1 {
		t.Fatalf("MonitorSeason = %d, want 1", item.MonitorSeason)
	}
	if !item.Seasons[0].Monitored || item.Seasons[1].Monitored {
		t.Fatalf("seasons: %+v", item.Seasons)
	}
	prov.series.Seasons = append(prov.series.Seasons, domain.Season{Number: 3, Episodes: []domain.Episode{
		{SeasonNumber: 3, EpisodeNumber: 1, AirDate: "2999-02-01"},
	}})
	got, err := svc.RefreshItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got.Seasons {
		if s.Number > 1 && s.Monitored {
			t.Fatalf("season %d arrived monitored under first_season", s.Number)
		}
	}
}

// An offloaded title is not monitored again by a toggle (409 offloaded; the way
// back is Re-acquire), and a season that appears on a whole-show-offloaded
// show arrives unmonitored, not offloaded.
func TestOffloadedRowsRefuseMonitoringAndNewSeasonsArriveUnmonitored(t *testing.T) {
	ctx := context.Background()
	svc, db, prov := offloadLib(t)
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true, Monitor: "new_seasons"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE seasons SET offloaded_at=1 WHERE media_item_id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE episodes SET offloaded_at=1 WHERE media_item_id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	// The whole-show intent is stored (item scope), never inferred.
	if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET offloaded_at=1 WHERE id=?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetSeasonMonitored(ctx, item.ID, 1, true); !errors.Is(err, ErrOffloaded) {
		t.Fatalf("season toggle = %v, want ErrOffloaded", err)
	}
	ep := item.Seasons[0].Episodes[0].ID
	if _, err := svc.SetEpisodeMonitored(ctx, item.ID, ep, true); !errors.Is(err, ErrOffloaded) {
		t.Fatalf("episode toggle = %v, want ErrOffloaded", err)
	}
	// Unmonitoring an offloaded row is harmless.
	if _, err := svc.SetSeasonMonitored(ctx, item.ID, 1, false); err != nil {
		t.Fatalf("unmonitor: %v", err)
	}
	prov.series.Seasons = append(prov.series.Seasons, domain.Season{Number: 3, Episodes: []domain.Episode{
		{SeasonNumber: 3, EpisodeNumber: 1, AirDate: "2999-02-01"},
	}})
	got, err := svc.RefreshItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := got.Seasons[len(got.Seasons)-1]
	if last.Number != 3 || last.Monitored || last.Offloaded() {
		t.Fatalf("new season on an offloaded show: %+v", last)
	}
}

// A movie's monitored flag cannot be switched on while it is offloaded.
func TestOffloadedMovieRefusesMonitoredTrue(t *testing.T) {
	ctx := context.Background()
	svc, db, _ := offloadLib(t)
	mid, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Dune", Year: 2021, IDs: domain.ExternalIDs{TMDB: 7}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET offloaded_at=1 WHERE id=?`, mid); err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := svc.UpdateItem(ctx, mid, UpdateRequest{Monitored: &on}); !errors.Is(err, ErrOffloaded) {
		t.Fatalf("monitored=true = %v, want ErrOffloaded", err)
	}
	// Other edits go through.
	off := false
	if _, err := svc.UpdateItem(ctx, mid, UpdateRequest{Monitored: &off}); err != nil {
		t.Fatalf("monitored=false: %v", err)
	}
}

// Offloading every existing season of a continuing show is not a whole-show
// Offload (that is the item-scope intent): a season that appears later still
// follows the monitor mode.
func TestSeasonOffloadOfAllSeasonsIsNotWholeShow(t *testing.T) {
	ctx := context.Background()
	svc, db, prov := offloadLib(t)
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true, Monitor: "new_seasons"})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE seasons SET offloaded_at=1 WHERE media_item_id=?`, `UPDATE episodes SET offloaded_at=1 WHERE media_item_id=?`} {
		if _, err := db.W.ExecContext(ctx, q, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	prov.series.Seasons = append(prov.series.Seasons, domain.Season{Number: 3, Episodes: []domain.Episode{
		{SeasonNumber: 3, EpisodeNumber: 1, AirDate: "2999-02-01"},
	}})
	got, err := svc.RefreshItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := got.Seasons[len(got.Seasons)-1]
	if last.Number != 3 || !last.Monitored {
		t.Fatalf("season arriving after a season-scope offload must follow the monitor mode: %+v", last)
	}
}

// B1: a re-acquired episode of a still-flagged season can be monitored; only
// an offloaded episode is refused.
func TestEpisodeOfFlaggedSeasonIsMonitorableOnceItsOwnFlagIsClear(t *testing.T) {
	ctx := context.Background()
	svc, db, _ := offloadLib(t)
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true, Monitor: "new_seasons"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE seasons SET offloaded_at=1 WHERE media_item_id=? AND number=1`, item.ID); err != nil {
		t.Fatal(err)
	}
	eps := item.Seasons[0].Episodes
	if _, err := db.W.ExecContext(ctx, `UPDATE episodes SET offloaded_at=1 WHERE id=?`, eps[0].ID); err != nil {
		t.Fatal(err)
	}
	if len(eps) > 1 {
		if _, err := svc.SetEpisodeMonitored(ctx, item.ID, eps[1].ID, true); err != nil {
			t.Fatalf("episode with its own flag clear refused: %v", err)
		}
	}
	if _, err := svc.SetEpisodeMonitored(ctx, item.ID, eps[0].ID, true); !errors.Is(err, ErrOffloaded) {
		t.Fatalf("offloaded episode = %v, want ErrOffloaded", err)
	}
}
