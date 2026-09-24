package tmdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// gapServer serves canned JSON per path; unknown paths are 404.
func gapServer(t *testing.T, routes map[string]string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func gapRemote(t *testing.T, err error) *ports.RemoteError {
	t.Helper()
	var remote *ports.RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("err = %v, want *ports.RemoteError", err)
	}
	return remote
}

func TestGapAdExpectedTMDBIDs(t *testing.T) {
	if got := expectedTMDBIDs(domain.ExternalRef{Provider: "imdb", Value: "tt0137523"}); got.IMDB != "tt0137523" || got.TVDB != 0 || got.TMDB != 0 {
		t.Errorf("imdb ref = %+v", got)
	}
	if got := expectedTMDBIDs(domain.ExternalRef{Provider: "tvdb", Value: "424242"}); got.TVDB != 424242 || got.IMDB != "" {
		t.Errorf("tvdb ref = %+v", got)
	}
	if got := expectedTMDBIDs(domain.ExternalRef{Provider: "tvdb", Value: "not-a-number"}); got != (domain.ExternalIDs{}) {
		t.Errorf("malformed tvdb ref = %+v", got)
	}
	if got := expectedTMDBIDs(domain.ExternalRef{Provider: "tmdb", Value: "550"}); got != (domain.ExternalIDs{}) {
		t.Errorf("tmdb ref carries no expectation = %+v", got)
	}
}

func TestGapAdAmbiguousFindReportsTheExpectedIDs(t *testing.T) {
	srv, _ := gapServer(t, map[string]string{
		"/find/tt0137523": `{"movie_results":[{"id":550},{"id":551}],"tv_results":[{"id":100},{"id":101}]}`,
		"/find/424242":    `{"movie_results":[],"tv_results":[{"id":100},{"id":101}]}`,
	})
	c := New(srv.URL, staticKey("v3key"))
	ctx := context.Background()

	_, err := c.PreviewExternal(ctx, domain.KindMovie, domain.ExternalRef{Provider: "imdb", Value: "tt0137523"})
	remote := gapRemote(t, err)
	if remote.Category != ports.RemoteIdentityConflict || remote.ExpectedIDs.IMDB != "tt0137523" || remote.ActualIDs != (domain.ExternalIDs{}) {
		t.Errorf("ambiguous imdb movie: %+v", remote)
	}
	_, err = c.LookupExternal(ctx, domain.KindSeries, domain.ExternalRef{Provider: "tvdb", Value: "424242"})
	remote = gapRemote(t, err)
	if remote.Category != ports.RemoteIdentityConflict || remote.ExpectedIDs.TVDB != 424242 {
		t.Errorf("ambiguous tvdb series: %+v", remote)
	}
}

func TestGapAdFindResultThatContradictsTheRefIsAConflict(t *testing.T) {
	srv, _ := gapServer(t, map[string]string{
		"/find/tt0000001": `{"movie_results":[{"id":550}],"tv_results":[]}`,
		"/movie/550":      `{"id":550,"title":"Fight Club","imdb_id":"tt0137523","release_date":"1999-10-15"}`,
		"/find/777":       `{"movie_results":[],"tv_results":[{"id":100}]}`,
		"/tv/100":         `{"id":100,"name":"Test Show","first_air_date":"2020-01-01","external_ids":{"imdb_id":"tt9999999","tvdb_id":424242}}`,
	})
	c := New(srv.URL, staticKey("v3key"))
	ctx := context.Background()

	_, err := c.PreviewExternal(ctx, domain.KindMovie, domain.ExternalRef{Provider: "imdb", Value: "tt0000001"})
	remote := gapRemote(t, err)
	if remote.Category != ports.RemoteIdentityConflict || remote.ExpectedIDs.IMDB != "tt0000001" || remote.ActualIDs.IMDB != "tt0137523" || remote.ActualIDs.TMDB != 550 {
		t.Errorf("imdb contradiction: %+v", remote)
	}
	_, err = c.PreviewExternal(ctx, domain.KindSeries, domain.ExternalRef{Provider: "tvdb", Value: "777"})
	remote = gapRemote(t, err)
	if remote.Category != ports.RemoteIdentityConflict || remote.ExpectedIDs.TVDB != 777 || remote.ActualIDs.TVDB != 424242 {
		t.Errorf("tvdb contradiction: %+v", remote)
	}
}

func TestGapAdIdentityMetadataForMovies(t *testing.T) {
	srv, hits := gapServer(t, map[string]string{
		"/movie/550":                    `{"id":550,"title":"Fight Club","original_title":"Fight Club","origin_country":["US",""],"imdb_id":"tt0137523"}`,
		"/movie/550/alternative_titles": `{"titles":[{"title":"El club de la lucha","iso_3166_1":"es"},{"title":"   ","iso_3166_1":"XX"}]}`,
		"/movie/551":                    `{"id":551,"title":"Le Fabuleux Destin d'Amélie Poulain (EN)","original_title":"Le Fabuleux Destin d'Amélie Poulain","origin_country":["FR"],"imdb_id":"tt0211915"}`,
		"/movie/551/alternative_titles": `{"results":[{"title":"Amelie","iso_3166_1":"gb"}]}`,
	})
	c := New(srv.URL, staticKey("v3key"))
	ctx := context.Background()

	got, err := c.IdentityMetadata(ctx, domain.KindMovie, domain.ExternalIDs{TMDB: 550, IMDB: "TT0137523"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Aliases) != 1 || got.Aliases[0].Title != "El club de la lucha" || got.Aliases[0].MarketCountry != "ES" || got.Aliases[0].Role != "alternate" || got.Aliases[0].SourceID != "550" {
		t.Errorf("movie aliases = %+v", got.Aliases)
	}
	if len(got.Countries) != 1 || got.Countries[0].Code != "US" || got.Countries[0].Basis != "origin" || got.Countries[0].Source != "tmdb" {
		t.Errorf("movie countries = %+v", got.Countries)
	}
	if hits.Load() != 2 {
		t.Errorf("movie identity should cost two requests, got %d", hits.Load())
	}

	// An original title that differs from the canonical one is a searchable
	// "original" alias; the TV-style "results" envelope is read too.
	got, err = c.IdentityMetadata(ctx, domain.KindMovie, domain.ExternalIDs{TMDB: 551})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Aliases) != 2 || got.Aliases[0].Role != "original" || !got.Aliases[0].Searchable || got.Aliases[0].Title != "Le Fabuleux Destin d'Amélie Poulain" || got.Aliases[1].Title != "Amelie" || got.Aliases[1].MarketCountry != "GB" {
		t.Errorf("aliases = %+v", got.Aliases)
	}
	if len(got.Countries) != 1 || got.Countries[0].Code != "FR" {
		t.Errorf("countries = %+v", got.Countries)
	}
}

func TestGapAdIdentityMetadataRejectsConflictsAndUnsupportedInput(t *testing.T) {
	srv, _ := gapServer(t, map[string]string{
		"/movie/550": `{"id":550,"title":"Fight Club","imdb_id":"tt0137523"}`,
		"/tv/100":    `{"id":100,"name":"Test Show","external_ids":{"imdb_id":"tt9999999","tvdb_id":424242}}`,
		"/tv/200":    `{"id":200,"name":"No Akas"}`,
	})
	c := New(srv.URL, staticKey("v3key"))
	ctx := context.Background()

	_, err := c.IdentityMetadata(ctx, domain.KindMovie, domain.ExternalIDs{IMDB: "tt0137523"})
	if remote := gapRemote(t, err); remote.Category != ports.RemoteUnsupportedQuery {
		t.Errorf("no TMDB id: %+v", remote)
	}
	_, err = c.IdentityMetadata(ctx, domain.KindBook, domain.ExternalIDs{TMDB: 550})
	if remote := gapRemote(t, err); remote.Category != ports.RemoteUnsupportedQuery {
		t.Errorf("book kind: %+v", remote)
	}
	_, err = c.IdentityMetadata(ctx, domain.KindMovie, domain.ExternalIDs{TMDB: 550, IMDB: "tt0000001"})
	remote := gapRemote(t, err)
	if remote.Category != ports.RemoteIdentityConflict || remote.ExpectedIDs.IMDB != "tt0000001" || remote.ActualIDs.IMDB != "tt0137523" {
		t.Errorf("movie imdb conflict: %+v", remote)
	}
	_, err = c.IdentityMetadata(ctx, domain.KindSeries, domain.ExternalIDs{TMDB: 100, TVDB: 1})
	remote = gapRemote(t, err)
	if remote.Category != ports.RemoteIdentityConflict || remote.ExpectedIDs.TVDB != 1 || remote.ActualIDs.TVDB != 424242 {
		t.Errorf("series tvdb conflict: %+v", remote)
	}
	// Record fetch failures and alternative-title failures both surface.
	_, err = c.IdentityMetadata(ctx, domain.KindMovie, domain.ExternalIDs{TMDB: 999})
	if remote := gapRemote(t, err); remote.Category != ports.RemoteNotFound {
		t.Errorf("missing movie: %+v", remote)
	}
	_, err = c.IdentityMetadata(ctx, domain.KindSeries, domain.ExternalIDs{TMDB: 999})
	if remote := gapRemote(t, err); remote.Category != ports.RemoteNotFound {
		t.Errorf("missing series: %+v", remote)
	}
	_, err = c.IdentityMetadata(ctx, domain.KindSeries, domain.ExternalIDs{TMDB: 200})
	if remote := gapRemote(t, err); remote.Category != ports.RemoteNotFound {
		t.Errorf("missing alternative titles: %+v", remote)
	}
}

func TestGapAdIdentityMetadataMarksCunkOnEarthNumbering(t *testing.T) {
	srv, _ := gapServer(t, map[string]string{
		"/tv/79063":                    `{"id":79063,"name":"Cunk on Britain","original_name":"Cunk on Britain","origin_country":["GB"],"external_ids":{"imdb_id":"tt7797170"}}`,
		"/tv/79063/alternative_titles": `{"results":[{"title":" cunk on earth ","iso_3166_1":"US"},{"title":"Cunk sur Terre","iso_3166_1":"FR"}]}`,
	})
	c := New(srv.URL, staticKey("v3key"))
	got, err := c.IdentityMetadata(context.Background(), domain.KindSeries, domain.ExternalIDs{TMDB: 79063})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Aliases) != 2 {
		t.Fatalf("aliases = %+v", got.Aliases)
	}
	if got.Aliases[0].Scope != "unsupported_numbering" || got.Aliases[0].Title != " cunk on earth " {
		t.Errorf("Cunk on Earth alias = %+v", got.Aliases[0])
	}
	if got.Aliases[1].Scope != "work" || got.Aliases[1].MarketCountry != "FR" {
		t.Errorf("other alias = %+v", got.Aliases[1])
	}
	if len(got.Countries) != 1 || got.Countries[0].Code != "GB" {
		t.Errorf("countries = %+v", got.Countries)
	}
}

func TestGapAdRateLimitRetryAtFollowsTheHeader(t *testing.T) {
	var header atomic.Value
	header.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if h := header.Load().(string); h != "" {
			w.Header().Set("Retry-After", h)
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, staticKey("v3key"))
	ctx := context.Background()

	_, err := c.GetMovie(ctx, 1)
	remote := gapRemote(t, err)
	if remote.Category != ports.RemoteRateLimit || remote.HTTPStatus != http.StatusTooManyRequests || !remote.RetryAt.IsZero() {
		t.Errorf("429 without Retry-After: %+v", remote)
	}

	header.Store("45")
	_, err = c.GetMovie(ctx, 2)
	remote = gapRemote(t, err)
	if until := time.Until(remote.RetryAt); remote.Category != ports.RemoteRateLimit || until < 43*time.Second || until > 46*time.Second {
		t.Errorf("429 with Retry-After: 45 → %+v (in %v)", remote, until)
	}

	deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	header.Store(deadline.Format(http.TimeFormat))
	_, err = c.GetMovie(ctx, 3)
	remote = gapRemote(t, err)
	if !remote.RetryAt.Equal(deadline) {
		t.Errorf("429 with HTTP-date Retry-After: %v, want %v", remote.RetryAt, deadline)
	}

	header.Store("later")
	_, err = c.GetMovie(ctx, 4)
	if remote = gapRemote(t, err); !remote.RetryAt.IsZero() {
		t.Errorf("unparseable Retry-After should leave RetryAt zero: %v", remote.RetryAt)
	}
}

func TestGapAdTMDBRetryAt(t *testing.T) {
	if !tmdbRetryAt("").IsZero() || !tmdbRetryAt("garbage").IsZero() {
		t.Error("empty and unparseable values are the zero time")
	}
	if got := time.Until(tmdbRetryAt("10")); got < 9*time.Second || got > 10*time.Second {
		t.Errorf("seconds form: %v", got)
	}
	want := time.Date(2026, time.November, 5, 8, 0, 0, 0, time.UTC)
	if got := tmdbRetryAt(want.Format(http.TimeFormat)); !got.Equal(want) {
		t.Errorf("http-date form: %v", got)
	}
}
