package library

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestMonitoringPoliciesPersistAcrossRefreshAndPause(t *testing.T) {
	for _, mode := range []string{"all", "latest", "future", "new_seasons", "none"} {
		t.Run(mode, func(t *testing.T) {
			db, err := sqlite.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Close() })
			ctx := context.Background()
			if err := db.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			prov := &mutableProvider{series: seriesV1()}
			prov.series.Seasons = append(prov.series.Seasons, domain.Season{Number: 2, Episodes: []domain.Episode{
				{SeasonNumber: 2, EpisodeNumber: 1, AirDate: "2999-01-01"},
			}})
			svc := New(db, prov, nil, nil)
			item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true, Monitor: mode})
			if err != nil {
				t.Fatal(err)
			}
			if item.Monitor != mode {
				t.Fatalf("mode lost: %q", item.Monitor)
			}
			if item.Seasons[0].Episodes[0].Monitored != (mode == "all") {
				t.Fatalf("past episode: %+v", item.Seasons)
			}
			if item.Seasons[1].Monitored != (mode != "none") {
				t.Fatalf("upcoming season: %+v", item.Seasons)
			}
			// A pause must not poison seasons discovered while the show is paused.
			paused := false
			if _, err := svc.UpdateItem(ctx, item.ID, UpdateRequest{Monitored: &paused}); err != nil {
				t.Fatal(err)
			}
			prov.series = seriesV1()
			prov.series.Seasons = append(prov.series.Seasons, domain.Season{Number: 3, Monitored: false, Episodes: []domain.Episode{
				{SeasonNumber: 3, EpisodeNumber: 1, AirDate: "2999-02-01"},
			}})
			got, err := svc.RefreshItem(ctx, item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Monitored || got.Monitor != mode || got.Seasons[2].Monitored != (mode != "none") {
				t.Fatalf("pause/refresh: %+v", got)
			}
			paused = true
			got, err = svc.UpdateItem(ctx, item.ID, UpdateRequest{Monitored: &paused})
			if err != nil || !got.Monitored || got.Seasons[2].Monitored != (mode != "none") {
				t.Fatalf("resume: %+v %v", got, err)
			}
		})
	}
}

func TestEditMonitoringReappliesPolicyButRefreshPreservesOverrides(t *testing.T) {
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	prov := &mutableProvider{series: seriesV1()}
	svc := New(db, prov, nil, nil)
	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindSeries, TMDBID: 100, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	mode := "future"
	got, err := svc.UpdateItem(ctx, item.ID, UpdateRequest{Monitor: &mode})
	if err != nil {
		t.Fatal(err)
	}
	if got.MonitorSince != time.Now().UTC().Format(time.DateOnly) || got.Seasons[0].Episodes[0].Monitored {
		t.Fatalf("policy not applied: %+v", got)
	}
	if _, err := svc.SetEpisodeMonitored(ctx, item.ID, got.Seasons[0].Episodes[0].ID, true); err != nil {
		t.Fatal(err)
	}
	prov.series = seriesV1()
	prov.series.Seasons[0].Episodes = append(prov.series.Seasons[0].Episodes,
		domain.Episode{SeasonNumber: 1, EpisodeNumber: 3, AirDate: "2020-01-15"},
		domain.Episode{SeasonNumber: 1, EpisodeNumber: 4, AirDate: "2999-01-01"})
	got, err = svc.RefreshItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	eps := got.Seasons[0].Episodes
	if !eps[0].Monitored || eps[1].Monitored || eps[2].Monitored || !eps[3].Monitored {
		t.Fatalf("override/new episode flags: %+v", eps)
	}
	bad := "typo"
	if _, err := svc.UpdateItem(ctx, item.ID, UpdateRequest{Monitor: &bad}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid mode: %v", err)
	}
	mode = "all"
	got, err = svc.UpdateItem(ctx, item.ID, UpdateRequest{Monitor: &mode})
	if err != nil || !got.Seasons[0].Episodes[1].Monitored {
		t.Fatalf("reapply all: %+v %v", got, err)
	}
}
