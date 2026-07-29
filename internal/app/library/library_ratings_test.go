package library

import (
	"context"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// fakeRatings hands back canned OMDb-style extras.
type fakeRatings struct{ calls int }

func (f *fakeRatings) Ratings(ctx context.Context, imdbID string) ([]domain.Rating, error) {
	f.calls++
	if imdbID != "tt0137523" {
		return nil, nil
	}
	return []domain.Rating{
		{Source: "imdb", Value: 8.8, Votes: 2412725, Scale: 10},
		{Source: "rt", Value: 79, Scale: 100},
	}, nil
}

func TestAddEnrichesRatingsFromProvider(t *testing.T) {
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

	prov := &mutableProvider{movie: domain.MediaItem{
		Kind: domain.KindMovie, Title: "Fight Club", SortTitle: "fight club",
		Year: 1999, IDs: domain.ExternalIDs{TMDB: 550, IMDB: "tt0137523"},
		Rating: 8.4, RatingVotes: 26280,
		Ratings: []domain.Rating{{Source: "tmdb", Value: 8.4, Votes: 26280, Scale: 10}},
	}}
	fr := &fakeRatings{}
	svc := New(db, prov, b, nil).WithRatings(fr)

	item, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	bySource := map[string]domain.Rating{}
	for _, r := range item.Ratings {
		bySource[r.Source] = r
	}
	if len(item.Ratings) != 3 {
		t.Fatalf("ratings = %+v", item.Ratings)
	}
	if bySource["tmdb"].Value != 8.4 || bySource["imdb"].Value != 8.8 || bySource["rt"].Value != 79 {
		t.Errorf("merged wrong: %+v", item.Ratings)
	}
	if bySource["rt"].Scale != 100 {
		t.Errorf("rt scale = %d", bySource["rt"].Scale)
	}

	// Refresh re-enriches and stays deduped.
	got, err := svc.RefreshItem(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Ratings) != 3 {
		t.Errorf("after refresh: %+v", got.Ratings)
	}
	if fr.calls != 2 {
		t.Errorf("provider calls = %d", fr.calls)
	}
}

// A provider with no key must never fail an add.
type unconfiguredRatings struct{}

func (unconfiguredRatings) Ratings(ctx context.Context, imdbID string) ([]domain.Rating, error) {
	return nil, ports.ErrProviderNotConfigured
}

func TestAddToleratesUnconfiguredRatingsProvider(t *testing.T) {
	svc, _, _ := newService(t)
	svc.WithRatings(unconfiguredRatings{})
	if _, err := svc.Add(context.Background(), AddRequest{Kind: domain.KindMovie, TMDBID: 550, Monitored: true}); err != nil {
		t.Fatalf("add should succeed without a ratings key: %v", err)
	}
}
