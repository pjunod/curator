package tmdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// discoverServer serves the two response shapes Discover reads — TMDB's
// movie list (title/release_date) and its TV list (name/first_air_date) —
// and records the paths and page numbers it was asked for.
func discoverServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"?page="+r.URL.Query().Get("page"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/trending/movie/week", "/movie/now_playing", "/movie/upcoming",
			"/movie/popular", "/movie/top_rated":
			_, _ = w.Write([]byte(`{"results":[
				{"id":601,"title":"The Test Movie","original_title":"Le Test",
				 "release_date":"2024-03-01","overview":"A film.","poster_path":"/m.jpg"}]}`))
		case "/trending/tv/week", "/tv/on_the_air", "/tv/popular", "/tv/top_rated":
			_, _ = w.Write([]byte(`{"results":[
				{"id":700,"name":"The Test Show","original_name":"Le Show",
				 "first_air_date":"2023-09-09","overview":"A show.","poster_path":"/s.jpg"}]}`))
		case "/movie/900":
			_, _ = w.Write([]byte(`{"id":900,"title":"Hydrated","release_date":"2022-01-01",
				"overview":"From TMDB.","poster_path":"/h.jpg"}`))
		case "/tv/901":
			_, _ = w.Write([]byte(`{"id":901,"name":"Hydrated Show","first_air_date":"2021-01-01",
				"overview":"From TMDB.","poster_path":"/hs.jpg","seasons":[{"season_number":1}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// Every advertised list must be fetchable. The catalogue and the fetch table
// are the same literal, so this is really a guard against a row being added
// to one and not the other.
func TestDiscoverServesEveryAdvertisedList(t *testing.T) {
	srv, _ := discoverServer(t)
	c := New(srv.URL, staticKey("k"))

	lists := c.Lists()
	if len(lists) != len(tmdbLists) || len(lists) == 0 {
		t.Fatalf("catalogue is %d entries, want %d", len(lists), len(tmdbLists))
	}
	seen := map[string]bool{}
	for _, l := range lists {
		if seen[l.ID] {
			t.Fatalf("duplicate list id %q", l.ID)
		}
		seen[l.ID] = true
		if l.Title == "" || l.Blurb == "" || l.Source != "tmdb" {
			t.Errorf("list %q is under-described: %+v", l.ID, l)
		}
		if l.Kind != domain.KindMovie && l.Kind != domain.KindSeries {
			t.Errorf("list %q has kind %q, want movie or series", l.ID, l.Kind)
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
		if got[0].PosterPath == "" || got[0].Title == "" || got[0].Year == 0 {
			t.Errorf("%s: thin result %+v", l.ID, got[0])
		}
		if got[0].Source != "tmdb" {
			t.Errorf("%s: source %q", l.ID, got[0].Source)
		}
	}
}

func TestDiscoverDecodesBothShapes(t *testing.T) {
	srv, _ := discoverServer(t)
	c := New(srv.URL, staticKey("k"))

	movies, err := c.Discover(context.Background(), "tmdb-trending-movies", 1)
	if err != nil {
		t.Fatal(err)
	}
	if movies[0].Title != "The Test Movie" || movies[0].Year != 2024 {
		t.Errorf("movie shape decoded as %+v", movies[0])
	}
	// The original-language title rides along free, same as search.
	if len(movies[0].AltTitles) != 1 || movies[0].AltTitles[0] != "Le Test" {
		t.Errorf("alt titles %v", movies[0].AltTitles)
	}

	shows, err := c.Discover(context.Background(), "tmdb-trending-series", 1)
	if err != nil {
		t.Fatal(err)
	}
	if shows[0].Title != "The Test Show" || shows[0].Year != 2023 {
		t.Errorf("tv shape decoded as %+v", shows[0])
	}
}

func TestDiscoverPagesAndClamps(t *testing.T) {
	srv, seen := discoverServer(t)
	c := New(srv.URL, staticKey("k"))

	for _, page := range []int{2, 0, 900} {
		if _, err := c.Discover(context.Background(), "tmdb-popular-movies", page); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"/movie/popular?page=2",
		"/movie/popular?page=1",   // 0 clamps up, TMDB rejects page 0
		"/movie/popular?page=500", // and down, at TMDB's ceiling
	}
	if len(*seen) != len(want) {
		t.Fatalf("requests %v, want %v", *seen, want)
	}
	for i, w := range want {
		if (*seen)[i] != w {
			t.Errorf("request %d was %q, want %q", i, (*seen)[i], w)
		}
	}
}

func TestDiscoverUnknownListIsNotAServerError(t *testing.T) {
	srv, _ := discoverServer(t)
	c := New(srv.URL, staticKey("k"))
	_, err := c.Discover(context.Background(), "tmdb-nope", 1)
	if !errors.Is(err, ports.ErrUnknownList) {
		t.Fatalf("err = %v, want ErrUnknownList", err)
	}
}

func TestConfiguredFollowsTheKey(t *testing.T) {
	srv, _ := discoverServer(t)
	if New(srv.URL, staticKey("")).Configured(context.Background()) {
		t.Error("no key reports configured")
	}
	if !New(srv.URL, staticKey("k")).Configured(context.Background()) {
		t.Error("key present reports unconfigured")
	}
}

// Summary must not walk seasons: that is the whole reason it exists, and a
// regression would only show up as a rate-limit stall in production.
func TestSummaryIsOneRequestAndNeverFetchesSeasons(t *testing.T) {
	srv, seen := discoverServer(t)
	c := New(srv.URL, staticKey("k"))

	movie, err := c.Summary(context.Background(), domain.KindMovie, 900)
	if err != nil {
		t.Fatal(err)
	}
	if movie.PosterPath != "/h.jpg" || movie.Title != "Hydrated" || movie.Year != 2022 {
		t.Errorf("movie summary %+v", movie)
	}

	show, err := c.Summary(context.Background(), domain.KindSeries, 901)
	if err != nil {
		t.Fatal(err)
	}
	if show.PosterPath != "/hs.jpg" || show.Title != "Hydrated Show" {
		t.Errorf("series summary %+v", show)
	}

	want := []string{"/movie/900?page=", "/tv/901?page="}
	if len(*seen) != len(want) {
		t.Fatalf("Summary made %d requests (%v), want %d", len(*seen), *seen, len(want))
	}

	if _, err := c.Summary(context.Background(), domain.KindBook, 1); err == nil {
		t.Error("books have no TMDB summary; want an error")
	}
}

// The import-list vocabulary is stored in import_lists.type, so it has to
// keep working after being re-expressed over the shared fetch path.
func TestDiscoverMoviesStillServesImportLists(t *testing.T) {
	srv, seen := discoverServer(t)
	c := New(srv.URL, staticKey("k"))

	for _, kind := range []string{"popular", "top_rated"} {
		got, err := c.DiscoverMovies(context.Background(), kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if len(got) != 1 || got[0].TMDBID != 601 || got[0].Kind != domain.KindMovie {
			t.Errorf("%s returned %+v", kind, got)
		}
	}
	if (*seen)[0] != "/movie/popular?page=1" || (*seen)[1] != "/movie/top_rated?page=1" {
		t.Errorf("paths %v", *seen)
	}
	if _, err := c.DiscoverMovies(context.Background(), "trending"); err == nil {
		t.Error("unknown import-list kind should still be rejected")
	}
}
