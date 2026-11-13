package api

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pjunod/monarr/internal/app/discover"
	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/ports"
	"net/http"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

func TestAvailabilityAPIRejectsMalformedAndKeepsUnknown(t *testing.T) {
	h, db := newLibraryServer(t, stubProvider{configured: true})
	ctx := context.Background()
	id, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Tracked", IDs: domain.ExternalIDs{TMDB: 88}})
	if err != nil {
		t.Fatal(err)
	}
	rr := edgeDo(t, h, http.MethodPost, "/api/v1/metadata/availability", `[{"kind":"movie","tmdbId":88},{"kind":"series","tmdbId":88}]`, nil)
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	var values []struct {
		Catalog, Presence string
		LibraryItemID     int64 `json:"libraryItemId"`
	}
	if err = json.Unmarshal(rr.Body.Bytes(), &values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Catalog != "present" || values[0].Presence != "unknown" || values[0].LibraryItemID != id || values[1].Catalog != "absent" {
		t.Fatal(rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("local facts can be cached")
	}
	for _, body := range []string{`null`, `{}`, `[{"kind":"bad","tmdbId":88}]`, `[{"kind":"movie","tmdbId":-1}]`, `[{"kind":"movie","path":"private"}]`, `[] []`} {
		rr = edgeDo(t, h, http.MethodPost, "/api/v1/metadata/availability", body, nil)
		if rr.Code != 400 {
			t.Fatalf("%s => %d", body, rr.Code)
		}
	}
}

type changingLibraryProvider struct{ during func() }

func (p changingLibraryProvider) Related(context.Context, domain.MediaKind, int64) ([]ports.SearchResult, error) {
	p.during()
	return []ports.SearchResult{{Kind: domain.KindMovie, TMDBID: 99, Title: "Related"}}, nil
}
func (p changingLibraryProvider) MovieCollection(context.Context, int64) (ports.MovieCollection, error) {
	return ports.MovieCollection{}, nil
}
func TestLibraryShelfRechecksIdentityAfterProviderIO(t *testing.T) {
	_, db := newLibraryServer(t, stubProvider{configured: true})
	ctx := context.Background()
	id, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Seed", IDs: domain.ExternalIDs{TMDB: 88, IMDB: "tt88"}})
	if err != nil {
		t.Fatal(err)
	}
	source := changingLibraryProvider{during: func() {
		if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET imdb_id='tt-changed' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}}
	h := New(Deps{Store: db, Log: edgeQuietLog(), Health: health.NewRegistry(nil), LibraryDiscovery: discover.NewLibrary(db, source)}).Handler()
	rr := edgeDo(t, h, http.MethodGet, fmt.Sprintf("/api/v1/discover/library/%d/items?kind=movie&tmdbId=88&shelf=related", id), "", nil)
	if rr.Code != 409 {
		t.Fatalf("stale seed returned: %d %s", rr.Code, rr.Body.String())
	}
}

func TestLibraryShelfRejectsNewCrossIDConflictDuringProviderIO(t *testing.T) {
	_, db := newLibraryServer(t, stubProvider{configured: true})
	ctx := context.Background()
	id, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Seed", IDs: domain.ExternalIDs{TMDB: 88}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Other", IDs: domain.ExternalIDs{TMDB: 89, IMDB: "tt-shared"}}); err != nil {
		t.Fatal(err)
	}
	source := changingLibraryProvider{during: func() {
		if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET imdb_id='tt-shared' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}}
	h := New(Deps{Store: db, Log: edgeQuietLog(), Health: health.NewRegistry(nil), LibraryDiscovery: discover.NewLibrary(db, source)}).Handler()
	rr := edgeDo(t, h, http.MethodGet, fmt.Sprintf("/api/v1/discover/library/%d/items?kind=movie&tmdbId=88&shelf=related", id), "", nil)
	if rr.Code != 409 {
		t.Fatalf("new conflict returned: %d %s", rr.Code, rr.Body.String())
	}
}
