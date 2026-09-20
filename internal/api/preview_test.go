package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/domain"
)

type apiPreviewProvider struct{ stubProvider }

func (p apiPreviewProvider) PreviewExternal(ctx context.Context, kind domain.MediaKind, ref domain.ExternalRef) (domain.MediaItem, error) {
	item, err := p.GetMovie(ctx, 550)
	item.Source = "tmdb"
	item.Overview = "First paragraph.\n\nSecond paragraph."
	return item, err
}

func TestPreviewAPIContract(t *testing.T) {
	handler, db := newLibraryServer(t, apiPreviewProvider{stubProvider{configured: true}})
	for _, query := range []string{"kind=movie", "kind=movie&tmdbId=0", "kind=movie&tmdbId=no", "kind=movie&tmdbId=1&tvdbId=2", "kind=movie&tvdbId=2", "kind=book&olid=OL1M", "kind=book&tmdbId=1", "kind=movie&tmdbId=1&tmdbId=2", "kind=person&tmdbId=1"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/metadata/preview?"+query, nil))
		var body map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != 400 || body["code"] != "invalid_external_id" {
			t.Errorf("%s: %d %s", query, w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/metadata/preview?kind=movie&tmdbId=550", nil))
	var got apigen.MetadataPreview
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || got.Title != "Fight Club" || got.RuntimeMinutes == nil || *got.RuntimeMinutes != 139 || got.Ids.Imdb == nil || string(got.Ownership) != "absent" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %+v", w.Code, got)
	}
	rows, err := db.ListMediaItems(context.Background(), domain.KindMovie)
	if err != nil || len(rows) != 0 {
		t.Fatalf("GET wrote library: %+v, %v", rows, err)
	}
}
