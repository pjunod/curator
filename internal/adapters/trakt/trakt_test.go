package trakt

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// traktServer answers every endpoint the adapter reads, in Trakt's real
// envelope shape: the entity wrapped next to a counter that differs per
// endpoint and that we do not read.
func traktServer(t *testing.T) (*httptest.Server, *atomic.Int64, *[]string) {
	t.Helper()
	var hits atomic.Int64
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		seen = append(seen, r.URL.String())
		if r.Header.Get("trakt-api-key") == "" || r.Header.Get("trakt-api-version") != "2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/movies/trending":
			_, _ = w.Write([]byte(`[{"watchers":42,"movie":{"title":"Trending Film","year":2025,
				"overview":"Being watched.","ids":{"tmdb":601,"imdb":"tt1"}}}]`))
		case "/movies/anticipated":
			_, _ = w.Write([]byte(`[{"list_count":9,"movie":{"title":"Soon","year":2026,
				"ids":{"tmdb":602}}}]`))
		case "/movies/boxoffice":
			_, _ = w.Write([]byte(`[{"revenue":1000,"movie":{"title":"Grossing","year":2025,
				"ids":{"tmdb":603}}}]`))
		case "/shows/trending":
			_, _ = w.Write([]byte(`[{"watchers":7,"show":{"title":"Trending Show","year":2024,
				"overview":"Also watched.","ids":{"tmdb":700,"tvdb":12345}}}]`))
		case "/shows/anticipated":
			_, _ = w.Write([]byte(`[{"list_count":3,"show":{"title":"Show Soon","year":2026,
				"ids":{"tmdb":701}}}]`))
		case "/users/someone/lists/my-list/items":
			// One movie, one show, one entry with no TMDB id, one episode.
			_, _ = w.Write([]byte(`[
				{"type":"movie","movie":{"title":"Listed","year":2020,"ids":{"tmdb":800}}},
				{"type":"show","show":{"title":"Listed Show","year":2019,"ids":{"tmdb":801,"tvdb":99}}},
				{"type":"movie","movie":{"title":"Untraceable","year":2001,"ids":{"tmdb":0}}},
				{"type":"episode","episode":{"title":"An episode"}}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &seen
}

func staticKey(k string) KeyFunc {
	return func(context.Context) (string, error) { return k, nil }
}

func TestDiscoverServesEveryAdvertisedList(t *testing.T) {
	srv, _, _ := traktServer(t)
	c := New(srv.URL, staticKey("client-id"))

	lists := c.Lists()
	if len(lists) != len(traktLists) || len(lists) == 0 {
		t.Fatalf("catalogue is %d entries, want %d", len(lists), len(traktLists))
	}
	for _, l := range lists {
		if l.Title == "" || l.Blurb == "" || l.Source != "trakt" {
			t.Errorf("list %q is under-described: %+v", l.ID, l)
		}
		got, err := c.Discover(context.Background(), l.ID, 1)
		if err != nil {
			t.Fatalf("%s: %v", l.ID, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: %d results, want 1", l.ID, len(got))
		}
		if got[0].Kind != l.Kind {
			t.Errorf("%s: result kind %q, list kind %q", l.ID, got[0].Kind, l.Kind)
		}
		if got[0].TMDBID == 0 || got[0].Source != "trakt" {
			t.Errorf("%s: %+v", l.ID, got[0])
		}
		// Trakt has no artwork in the free tier — the Discover service
		// hydrates it. If this ever stops being true the hydrator can go.
		if got[0].PosterPath != "" {
			t.Errorf("%s: unexpected poster %q", l.ID, got[0].PosterPath)
		}
	}
}

func TestDiscoverAsksForOverviewAndAPageWorthOfResults(t *testing.T) {
	srv, _, seen := traktServer(t)
	c := New(srv.URL, staticKey("client-id"))

	if _, err := c.Discover(context.Background(), "trakt-trending-movies", 2); err != nil {
		t.Fatal(err)
	}
	got := (*seen)[0]
	want := "/movies/trending?extended=full&limit=30&page=2"
	if got != want {
		t.Errorf("request %q, want %q", got, want)
	}
}

// Box office is a fixed top ten: Trakt ignores page, so asking for page 2
// would repeat page 1. An ended row is honest; a repeated one is not.
func TestUnpagedListEndsRatherThanRepeating(t *testing.T) {
	srv, hits, _ := traktServer(t)
	c := New(srv.URL, staticKey("client-id"))

	first, err := c.Discover(context.Background(), "trakt-boxoffice", 1)
	if err != nil || len(first) != 1 {
		t.Fatalf("page 1: %v, %d results", err, len(first))
	}
	before := hits.Load()
	second, err := c.Discover(context.Background(), "trakt-boxoffice", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Errorf("page 2 returned %d results, want none", len(second))
	}
	if hits.Load() != before {
		t.Error("page 2 hit the network; it should not have")
	}
}

func TestSeriesResultsCarryTheTVDBID(t *testing.T) {
	srv, _, _ := traktServer(t)
	c := New(srv.URL, staticKey("client-id"))
	got, err := c.Discover(context.Background(), "trakt-trending-series", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].TVDBID != 12345 {
		t.Errorf("tvdb id %d, want 12345", got[0].TVDBID)
	}
	if got[0].Overview == "" {
		t.Error("extended=full should have carried an overview")
	}
}

func TestUnknownListAndMissingClientID(t *testing.T) {
	srv, _, _ := traktServer(t)

	_, err := New(srv.URL, staticKey("client-id")).Discover(context.Background(), "trakt-nope", 1)
	if !errors.Is(err, ports.ErrUnknownList) {
		t.Errorf("err = %v, want ErrUnknownList", err)
	}

	unset := New(srv.URL, staticKey(""))
	if unset.Configured(context.Background()) {
		t.Error("no client id reports configured")
	}
	if _, err := unset.Discover(context.Background(), "trakt-boxoffice", 1); !errors.Is(err, ports.ErrProviderNotConfigured) {
		t.Errorf("err = %v, want ErrProviderNotConfigured", err)
	}
}

func TestResponsesAreCachedPerClientID(t *testing.T) {
	srv, hits, _ := traktServer(t)
	c := New(srv.URL, staticKey("client-id"))

	for range 3 {
		if _, err := c.Discover(context.Background(), "trakt-trending-movies", 1); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("%d upstream requests for 3 identical calls, want 1", hits.Load())
	}

	// A changed client id must not be served an answer fetched under the old
	// one — the id is a header, so it is not in the URL that keys the cache.
	var id atomic.Value
	id.Store("first")
	rotating := New(srv.URL, func(context.Context) (string, error) { return id.Load().(string), nil })
	if _, err := rotating.Discover(context.Background(), "trakt-trending-movies", 1); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	id.Store("second")
	if _, err := rotating.Discover(context.Background(), "trakt-trending-movies", 1); err != nil {
		t.Fatal(err)
	}
	if hits.Load() == before {
		t.Error("a new client id was served the previous id's cached response")
	}
}

// Import lists are the older caller and must be unchanged by the rewrite.
func TestListItemsKeepsOnlyActionableEntries(t *testing.T) {
	srv, _, _ := traktServer(t)
	c := NewStatic(srv.URL, "client-id")

	got, err := c.ListItems(context.Background(), "someone", "my-list")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d results, want 2 (a movie and a show; no tmdb id and episodes are dropped)", len(got))
	}
	if got[0].Kind != domain.KindMovie || got[0].TMDBID != 800 {
		t.Errorf("first result %+v", got[0])
	}
	if got[1].Kind != domain.KindSeries || got[1].TMDBID != 801 {
		t.Errorf("second result %+v", got[1])
	}
}

func TestRejectedClientIDIsNamedAsSuch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	_, err := New(srv.URL, staticKey("bad")).Discover(context.Background(), "trakt-boxoffice", 1)
	if err == nil || !strings.Contains(err.Error(), "client id rejected") {
		t.Errorf("err = %v, want a client-id-rejected message", err)
	}
}
