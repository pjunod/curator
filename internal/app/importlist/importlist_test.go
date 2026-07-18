package importlist

import (
	"context"
	"testing"

	"github.com/monarr-media/monarr/internal/app/library"
	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

type fakeMeta struct{}

func (fakeMeta) SearchMovies(ctx context.Context, q string) ([]ports.SearchResult, error) {
	return nil, nil
}
func (fakeMeta) SearchSeries(ctx context.Context, q string) ([]ports.SearchResult, error) {
	return nil, nil
}
func (fakeMeta) GetMovie(ctx context.Context, id int64) (domain.MediaItem, error) {
	return domain.MediaItem{Kind: domain.KindMovie, Title: "Listed Movie",
		SortTitle: "listed movie", Year: 2024, IDs: domain.ExternalIDs{TMDB: id}}, nil
}
func (fakeMeta) GetSeries(ctx context.Context, id int64) (domain.MediaItem, error) {
	return domain.MediaItem{Kind: domain.KindSeries, Title: "Listed Show",
		SortTitle: "listed show", IDs: domain.ExternalIDs{TMDB: id}}, nil
}

func TestSyncAddsMissingOnly(t *testing.T) {
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
	lib := library.New(db, fakeMeta{}, b, nil)

	discovered := []ports.SearchResult{
		{Kind: domain.KindMovie, TMDBID: 901, Title: "Listed Movie"},
		{Kind: domain.KindMovie, TMDBID: 902, Title: "Listed Movie 2"},
	}
	svc := New(db, lib, Sources{
		TMDBDiscover: func(ctx context.Context, kind string) ([]ports.SearchResult, error) {
			return discovered, nil
		},
	}, nil)

	if _, err := db.AddImportList(ctx, sqlite.ImportList{
		Name: "Popular", Type: "tmdb-popular", Kind: "movie",
		QualityProfileID: 2, Monitored: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	if err := svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	items, _ := db.ListMediaItems(ctx, domain.KindMovie)
	if len(items) != 2 {
		t.Fatalf("items after first sync = %d", len(items))
	}
	if items[0].QualityProfileID != 2 || !items[0].Monitored {
		t.Errorf("list policy not applied: %+v", items[0])
	}

	// Second sync is a no-op (everything already in the library).
	if err := svc.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	items, _ = db.ListMediaItems(ctx, domain.KindMovie)
	if len(items) != 2 {
		t.Fatalf("items after second sync = %d (duplicated)", len(items))
	}
}
