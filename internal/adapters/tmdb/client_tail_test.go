package tmdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// jsonServer answers a fixed path table and 404s everything else, matching
// the shape of the fixture server the other tests in this package use but
// letting a case declare its own bodies inline.
func jsonServer(t *testing.T, routes map[string]string) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// SearchSeries is the TV half of search and decodes a different shape from
// the movie half — name/first_air_date rather than title/release_date. A
// mapping mistake here shows up as a library full of untitled year-0 shows.
func TestTailSearchSeriesDecodesTheTVShape(t *testing.T) {
	srv, _ := jsonServer(t, map[string]string{
		"/search/tv": `{"results":[
			{"id":700,"name":"The Test Show","original_name":"Le Show",
			 "first_air_date":"2023-09-09","overview":"A show.","poster_path":"/s.jpg"},
			{"id":701,"name":"Unaired","original_name":"Unaired","first_air_date":""}]}`,
	})
	c := New(srv.URL, staticKey("k"))

	got, err := c.SearchSeries(context.Background(), "test show")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("results = %d, want 2", len(got))
	}
	if got[0].Kind != domain.KindSeries || got[0].TMDBID != 700 ||
		got[0].Title != "The Test Show" || got[0].Year != 2023 {
		t.Errorf("first = %+v", got[0])
	}
	// The original-language title rides along free in every search response,
	// so it is the one alternate name adoption gets without a second call.
	if len(got[0].AltTitles) != 1 || got[0].AltTitles[0] != "Le Show" {
		t.Errorf("alt titles = %v, want [Le Show]", got[0].AltTitles)
	}
	// A title that says nothing new is not an alternate.
	if got[1].AltTitles != nil {
		t.Errorf("alt titles = %v for a show whose original name matches", got[1].AltTitles)
	}
	if got[1].Year != 0 {
		t.Errorf("an unaired show got year %d, want 0", got[1].Year)
	}
}

// Alternative titles cost one request per title, which is why adoption only
// asks after a plain comparison has failed. The two endpoints return the
// same rows under different keys — movies under "titles", TV under
// "results" — and reading only one of them would make half of adoption's
// fallback silently return nothing.
func TestTailAlternativeTitlesReadsBothEnvelopes(t *testing.T) {
	srv, _ := jsonServer(t, map[string]string{
		"/movie/550/alternative_titles": `{"titles":[
			{"title":"Fight Club"},{"title":"El club de la lucha"},{"title":""}]}`,
		"/tv/100/alternative_titles": `{"results":[{"title":"Le Show"}]}`,
	})
	c := New(srv.URL, staticKey("k"))
	ctx := context.Background()

	movie, err := c.AlternativeTitles(ctx, domain.KindMovie, 550)
	if err != nil {
		t.Fatal(err)
	}
	// The empty row is dropped: an empty alternate matches everything.
	if len(movie) != 2 || movie[1] != "El club de la lucha" {
		t.Errorf("movie alt titles = %v", movie)
	}

	series, err := c.AlternativeTitles(ctx, domain.KindSeries, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0] != "Le Show" {
		t.Errorf("series alt titles = %v", series)
	}

	// Books are not a TMDB kind at all; asking is a programming error and
	// gets an error rather than an empty list that reads as "no alternates".
	if _, err := c.AlternativeTitles(ctx, domain.KindBook, 1); err == nil {
		t.Error("a book got alternative titles from TMDB")
	}
}

// Sonarr consumers (Jellyseerr) identify series by TVDB id, so the compat
// personality cannot work without this hop — and a TVDB id TMDB has never
// heard of has to say so rather than hydrating series 0.
func TestTailFindSeriesByTVDBResolvesThenHydrates(t *testing.T) {
	srv, seen := jsonServer(t, map[string]string{
		"/find/424242": `{"tv_results":[{"id":100}]}`,
		"/find/999":    `{"tv_results":[]}`,
		"/tv/100": `{"id":100,"name":"The Test Show","first_air_date":"2020-01-01",
			"status":"Ended","external_ids":{"tvdb_id":424242,"imdb_id":"tt9999999"},
			"seasons":[{"season_number":1}]}`,
		"/tv/100/season/1": `{"episodes":[
			{"season_number":1,"episode_number":1,"name":"Pilot","air_date":"2020-01-01"}]}`,
	})
	c := New(srv.URL, staticKey("k"))

	got, err := c.FindSeriesByTVDB(context.Background(), 424242)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "The Test Show" || got.IDs.TVDB != 424242 || !got.Ended {
		t.Errorf("series = %+v", got)
	}
	if len(got.Seasons) != 1 || len(got.Seasons[0].Episodes) != 1 {
		t.Errorf("the resolved series was not hydrated: %+v", got.Seasons)
	}
	if len(*seen) != 3 {
		t.Errorf("requests = %v, want find + tv + season", *seen)
	}

	_, err = c.FindSeriesByTVDB(context.Background(), 999)
	if err == nil {
		t.Fatal("an unmatched TVDB id resolved to a series")
	}
	if !strings.Contains(err.Error(), "999") {
		t.Errorf("err = %v, want the id in it so the log says which one", err)
	}
}

// Absolute numbering is derived, because TMDB has no first-class absolute
// numbers: a cumulative index across regular seasons, with specials skipped.
// Counting specials would shift every anime episode number by however many
// extras a show has.
func TestTailAbsoluteNumberingSkipsSpecialsAndRunsAcrossSeasons(t *testing.T) {
	srv, _ := jsonServer(t, map[string]string{
		"/tv/100": `{"id":100,"name":"Anime","first_air_date":"2020-01-01",
			"seasons":[{"season_number":0},{"season_number":1},{"season_number":2}]}`,
		"/tv/100/season/0": `{"episodes":[
			{"season_number":0,"episode_number":1,"name":"OVA"}]}`,
		"/tv/100/season/1": `{"episodes":[
			{"season_number":1,"episode_number":1,"name":"One"},
			{"season_number":1,"episode_number":2,"name":"Two"}]}`,
		"/tv/100/season/2": `{"episodes":[
			{"season_number":2,"episode_number":1,"name":"Three"}]}`,
	})
	c := New(srv.URL, staticKey("k"))

	got, err := c.GetSeries(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Seasons) != 3 {
		t.Fatalf("seasons = %d, want 3", len(got.Seasons))
	}
	if n := got.Seasons[0].Episodes[0].AbsoluteNum; n != 0 {
		t.Errorf("a special got absolute number %d — specials must not consume one", n)
	}
	if got.Seasons[0].Episodes[0].Monitored || got.Seasons[0].Monitored {
		t.Error("specials should start unmonitored, like upstream")
	}
	// Season 2 episode 1 is absolute 3: the count continues across the
	// season boundary, which is the whole point of the number.
	if n := got.Seasons[2].Episodes[0].AbsoluteNum; n != 3 {
		t.Errorf("season 2 episode 1 is absolute %d, want 3", n)
	}
	// A show TMDB has no status for is not Ended — guessing would stop
	// monitoring a running series.
	if got.Ended {
		t.Error("a series with no status was marked ended")
	}
}

// A season that will not load fails the whole hydration rather than storing
// a series with a silently missing season — which would read as "these
// episodes do not exist" and un-want every one of them.
func TestTailAFailedSeasonFailsTheWholeSeries(t *testing.T) {
	srv, _ := jsonServer(t, map[string]string{
		"/tv/100": `{"id":100,"name":"Half","seasons":[{"season_number":1},{"season_number":2}]}`,
		"/tv/100/season/1": `{"episodes":[
			{"season_number":1,"episode_number":1,"name":"One"}]}`,
		// season 2 is absent from the table and 404s.
	})
	c := New(srv.URL, staticKey("k"))

	_, err := c.GetSeries(context.Background(), 100)
	if err == nil {
		t.Fatal("a series with an unloadable season was returned as complete")
	}
	if !strings.Contains(err.Error(), "season 2") {
		t.Errorf("err = %v, want it to name the season that failed", err)
	}
}

// An unrated title has no rating to render, and a labeled entry reading
// "0/10 from 0 votes" is worse than no entry at all.
func TestTailAnUnratedTitleGetsNoLabeledRating(t *testing.T) {
	srv, _ := jsonServer(t, map[string]string{
		"/movie/550": `{"id":550,"title":"Obscure","release_date":"1999-10-15",
			"vote_average":0,"vote_count":0}`,
	})
	c := New(srv.URL, staticKey("k"))

	got, err := c.GetMovie(context.Background(), 550)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ratings != nil {
		t.Errorf("ratings = %+v, want none for a title with no votes", got.Ratings)
	}
	if got.RatingVotes != 0 {
		t.Errorf("votes = %d, want 0", got.RatingVotes)
	}
}

// The key lives in settings and is read at call time, so reading it can
// fail. That is not "not configured" — it is a broken settings store, and
// conflating the two would tell the user to paste a key they already have.
func TestTailAnUnreadableKeyIsNotTheSameAsAnAbsentOne(t *testing.T) {
	srv, _ := jsonServer(t, nil)
	boom := errors.New("settings database is locked")
	c := New(srv.URL, func(context.Context) (string, error) { return "", boom })

	_, err := c.SearchMovies(context.Background(), "x")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the underlying settings error", err)
	}
	if errors.Is(err, ports.ErrProviderNotConfigured) {
		t.Error("a broken settings store was reported as an unconfigured provider")
	}
	// Configured() reads the same key and must not claim TMDB is ready.
	if c.Configured(context.Background()) {
		t.Error("Configured reported true while the key could not be read")
	}
}

// 401 has exactly one cause — the key — and saying so is the difference
// between a user fixing it and a user filing a bug.
func TestTailARejectedKeyIsNamed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New(srv.URL, staticKey("badkey"))
	_, err := c.SearchMovies(context.Background(), "x")
	if err == nil {
		t.Fatal("a 401 was treated as an empty result set")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "key") {
		t.Errorf("err = %v, want it to name the key and the status", err)
	}
}

// Anything else non-200 — a 429 from the rate limiter, a 503 from TMDB
// itself — carries its status, because the right response to each differs.
func TestTailAnUnexpectedStatusCarriesItsCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(srv.URL, staticKey("k"))
	_, err := c.SearchMovies(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("err = %v, want the 429 in it", err)
	}
}

// A 200 whose body is not JSON must not decode as an empty result set: an
// empty search looks like "TMDB has never heard of this film", and that is a
// conclusion a truncated response should never be allowed to reach.
func TestTailAMalformedBodyIsNotAnEmptyResultSet(t *testing.T) {
	srv, _ := jsonServer(t, map[string]string{"/search/movie": `{"results":[`})
	c := New(srv.URL, staticKey("k"))

	got, err := c.SearchMovies(context.Background(), "x")
	if err == nil {
		t.Fatal("a truncated body decoded successfully")
	}
	if got != nil {
		t.Errorf("results = %+v, want none alongside the error", got)
	}
}

// Only successful responses are cached. A cached failure would keep
// answering with the outage long after it ended, and the TTL is five
// minutes — long enough for a user to fix a key and be told it is still bad.
func TestTailFailuresAreNotCached(t *testing.T) {
	var fail = true
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"id":1,"title":"Back","release_date":"2020-01-01"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, staticKey("k"))
	if _, err := c.SearchMovies(context.Background(), "x"); err == nil {
		t.Fatal("the 503 was not reported")
	}
	fail = false
	got, err := c.SearchMovies(context.Background(), "x")
	if err != nil {
		t.Fatalf("the recovered provider still failed: %v", err)
	}
	if len(got) != 1 || got[0].Title != "Back" {
		t.Errorf("results = %+v, want the recovered response", got)
	}
	if hits != 2 {
		t.Errorf("upstream hits = %d, want 2 — the failure was served from cache", hits)
	}
}

// The provider name is what a Discover row is attributed to in the UI and
// what a stored list id is matched against.
func TestTailProviderNameIsStable(t *testing.T) {
	if got := New("", staticKey("k")).Name(); got != "tmdb" {
		t.Errorf("Name() = %q, want tmdb", got)
	}
}

// An empty base URL means the real API, and a trailing slash on a
// self-hosted proxy must not produce "//search/movie".
func TestTailBaseURLDefaultsAndIsTrimmed(t *testing.T) {
	if got := New("", staticKey("k")).baseURL; got != DefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", got, DefaultBaseURL)
	}
	srv, seen := jsonServer(t, map[string]string{"/search/movie": `{"results":[]}`})
	c := New(srv.URL+"/", staticKey("k"))
	if _, err := c.SearchMovies(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 || (*seen)[0] != "/search/movie" {
		t.Errorf("paths = %v, want /search/movie with no doubled slash", *seen)
	}
}
