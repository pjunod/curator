package tvmaze

import (
	"context"
	"github.com/pjunod/monarr/internal/domain"
	"testing"
)

func TestPreviewSeriesIsBoundedAndCached(t *testing.T) {
	srv, hits := newFixtureServer(t)
	client := New(srv.URL)
	for range 2 {
		got, err := client.PreviewExternal(context.Background(), domain.KindSeries, domain.ExternalRef{Provider: "tvdb", Value: "414217"})
		if err != nil || got.Runtime <= 0 || got.Status == "" || len(got.Seasons) != 0 || got.IDs.TVDB != 414217 {
			t.Fatalf("got %+v, %v", got, err)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("lookup + redirect should be cached, no episodes: %d requests", hits.Load())
	}
}
func TestPreviewPlainTextPreservesParagraphs(t *testing.T) {
	got := plainText("<p>One &amp; two.<br />Next line.</p><p><b>Last</b> paragraph.</p>")
	if got != "One & two.\nNext line.\n\nLast paragraph." {
		t.Fatalf("got %q", got)
	}
}
