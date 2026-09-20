package tmdb

import (
	"context"
	"github.com/pjunod/monarr/internal/domain"
	"testing"
)

func TestPreviewSeriesIsBoundedAndCached(t *testing.T) {
	srv, hits, _ := newFixtureServer(t)
	client := New(srv.URL, staticKey("key"))
	for range 2 {
		got, err := client.PreviewExternal(context.Background(), domain.KindSeries, domain.ExternalRef{Provider: "tmdb", Value: "100"})
		if err != nil || got.Runtime != 42 || got.Status != "Ended" || len(got.Genres) != 1 || len(got.Seasons) != 0 || got.IDs.TVDB != 424242 {
			t.Fatalf("got %+v, %v", got, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("preview fetched episodes or bypassed cache: %d requests", hits.Load())
	}
}
