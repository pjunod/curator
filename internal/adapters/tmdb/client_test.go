package tmdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newFixtureServer serves the recorded TMDB responses and counts hits.
func newFixtureServer(t *testing.T) (*httptest.Server, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	var hits atomic.Int64
	var sawBearer atomic.Bool
	routes := map[string]string{
		"/search/movie":    "search_movie.json",
		"/search/tv":       "search_tv.json",
		"/movie/550":       "movie_550.json",
		"/tv/100":          "tv_100.json",
		"/tv/100/season/0": "tv_100_season_0.json",
		"/tv/100/season/1": "tv_100_season_1.json",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			sawBearer.Store(true)
		}
		if r.Header.Get("Authorization") == "" && r.URL.Query().Get("api_key") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		file, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, file))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &sawBearer
}

func staticKey(k string) KeyFunc {
	return func(ctx context.Context) (string, error) { return k, nil }
}

func TestUnconfiguredKey(t *testing.T) {
	srv, _, _ := newFixtureServer(t)
	c := New(srv.URL, staticKey(""))
	if _, err := c.SearchMovies(context.Background(), "x"); !errors.Is(err, ports.ErrProviderNotConfigured) {
		t.Errorf("err = %v, want ErrProviderNotConfigured", err)
	}
}

func TestSearchMovies(t *testing.T) {
	srv, _, _ := newFixtureServer(t)
	c := New(srv.URL, staticKey("v3key"))

	got, err := c.SearchMovies(context.Background(), "fight club")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("results = %d", len(got))
	}
	first := got[0]
	if first.Kind != domain.KindMovie || first.TMDBID != 550 ||
		first.Title != "Fight Club" || first.Year != 1999 {
		t.Errorf("first = %+v", first)
	}
	if got[1].Year != 0 {
		t.Errorf("empty release date should give year 0, got %d", got[1].Year)
	}
}

func TestGetMovieMapsFields(t *testing.T) {
	srv, _, _ := newFixtureServer(t)
	c := New(srv.URL, staticKey("v3key"))

	m, err := c.GetMovie(context.Background(), 550)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != domain.KindMovie || m.Title != "Fight Club" ||
		m.SortTitle != "fight club" || m.Year != 1999 || m.Runtime != 139 {
		t.Errorf("movie = %+v", m)
	}
	if m.IDs.TMDB != 550 || m.IDs.IMDB != "tt0137523" {
		t.Errorf("ids = %+v", m.IDs)
	}
	if len(m.Genres) != 2 || m.Genres[0] != "Drama" {
		t.Errorf("genres = %v", m.Genres)
	}
	if m.Rating != 8.438 || m.RatingVotes != 26280 {
		t.Errorf("rating = %v (%d votes)", m.Rating, m.RatingVotes)
	}
	if len(m.Ratings) != 1 || m.Ratings[0].Source != "tmdb" || m.Ratings[0].Scale != 10 {
		t.Errorf("labeled ratings = %+v", m.Ratings)
	}
}

func TestGetSeriesHydratesSeasons(t *testing.T) {
	srv, _, _ := newFixtureServer(t)
	c := New(srv.URL, staticKey("v3key"))

	s, err := c.GetSeries(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != domain.KindSeries || s.Title != "The Test Show" ||
		s.SortTitle != "test show" || !s.Ended || s.Runtime != 42 {
		t.Errorf("series = %+v", s)
	}
	if s.IDs.TVDB != 424242 || s.IDs.IMDB != "tt9999999" {
		t.Errorf("ids = %+v", s.IDs)
	}
	if s.Rating != 8.1 || s.RatingVotes != 1234 {
		t.Errorf("rating = %v (%d votes)", s.Rating, s.RatingVotes)
	}
	if len(s.Seasons) != 2 {
		t.Fatalf("seasons = %d", len(s.Seasons))
	}
	specials, s1 := s.Seasons[0], s.Seasons[1]
	if specials.Number != 0 || specials.Monitored {
		t.Errorf("specials should be unmonitored: %+v", specials)
	}
	if s1.Number != 1 || !s1.Monitored || len(s1.Episodes) != 2 {
		t.Errorf("season 1 = %+v", s1)
	}
	if s1.Episodes[0].Title != "Pilot" || s1.Episodes[0].AirDate != "2020-01-01" {
		t.Errorf("episode = %+v", s1.Episodes[0])
	}
}

func TestCaching(t *testing.T) {
	srv, hits, _ := newFixtureServer(t)
	c := New(srv.URL, staticKey("v3key"))
	ctx := context.Background()

	if _, err := c.GetMovie(ctx, 550); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	if _, err := c.GetMovie(ctx, 550); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != before {
		t.Errorf("second identical request should be served from cache (hits %d → %d)", before, hits.Load())
	}
}

func TestBearerAuthForV4Tokens(t *testing.T) {
	srv, _, sawBearer := newFixtureServer(t)
	c := New(srv.URL, staticKey("eyJhbGciOiJIUzI1NiJ9.fake.v4token"))
	if _, err := c.SearchMovies(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if !sawBearer.Load() {
		t.Error("v4 JWT key should be sent as an Authorization bearer header")
	}
}

func TestNotFoundAndBadKey(t *testing.T) {
	srv, _, _ := newFixtureServer(t)
	c := New(srv.URL, staticKey("v3key"))
	if _, err := c.GetMovie(context.Background(), 999); err == nil {
		t.Error("404 should surface as an error")
	}
}
