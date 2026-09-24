package tvmaze

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

func gapRemote(t *testing.T, err error) *ports.RemoteError {
	t.Helper()
	var remote *ports.RemoteError
	if !errors.As(err, &remote) {
		t.Fatalf("err = %v, want *ports.RemoteError", err)
	}
	return remote
}

// gapShowServer answers /lookup/shows with the given show body regardless of
// the id asked for, which is how identity contradictions are provoked.
func gapShowServer(t *testing.T, showJSON, akasJSON string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/lookup/shows", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/shows/1", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/shows/1", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(showJSON))
	})
	mux.HandleFunc("/shows/1/akas", func(w http.ResponseWriter, _ *http.Request) {
		if akasJSON == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(akasJSON))
	})
	return srv
}

const gapShow = `{"id":1,"name":"Some Show","premiered":"2019-03-04","status":"Running","genres":["Drama"],"averageRuntime":45,` +
	`"summary":"<p>Plain &amp; simple.</p>","image":{"original":"https://img/orig.jpg"},"rating":{"average":null},` +
	`"externals":{"thetvdb":1001,"imdb":"tt1000001"},"network":{"name":"NBC","country":{"code":"us","timezone":"America/New_York"}}}`

func TestGapAdName(t *testing.T) {
	if got := New("").Name(); got != "tvmaze" {
		t.Errorf("Name() = %q", got)
	}
	if got := New("http://example.test/").baseURL; got != "http://example.test" {
		t.Errorf("baseURL should be trimmed: %q", got)
	}
}

func TestGapAdExpectedIDs(t *testing.T) {
	if got := expectedIDs(domain.ExternalRef{Provider: "tvdb", Value: "1001"}); got.TVDB != 1001 || got.IMDB != "" {
		t.Errorf("tvdb = %+v", got)
	}
	if got := expectedIDs(domain.ExternalRef{Provider: "tvdb", Value: "x"}); got != (domain.ExternalIDs{}) {
		t.Errorf("malformed tvdb = %+v", got)
	}
	if got := expectedIDs(domain.ExternalRef{Provider: "imdb", Value: "tt1000001"}); got.IMDB != "tt1000001" || got.TVDB != 0 {
		t.Errorf("imdb = %+v", got)
	}
	if got := expectedIDs(domain.ExternalRef{Provider: "tmdb", Value: "5"}); got != (domain.ExternalIDs{}) {
		t.Errorf("tmdb = %+v", got)
	}
}

func TestGapAdRetryAt(t *testing.T) {
	if !retryAt("").IsZero() || !retryAt("whenever").IsZero() {
		t.Error("empty and unparseable values are the zero time")
	}
	if got := time.Until(retryAt("30")); got < 29*time.Second || got > 30*time.Second {
		t.Errorf("seconds form: %v", got)
	}
	want := time.Date(2026, time.December, 1, 6, 0, 0, 0, time.UTC)
	if got := retryAt(want.Format(http.TimeFormat)); !got.Equal(want) {
		t.Errorf("http-date form: %v", got)
	}
}

func TestGapAdRateLimitedResponsesCarryRetryAt(t *testing.T) {
	var header atomic.Value
	header.Store("")
	var status atomic.Int64
	status.Store(http.StatusTooManyRequests)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if h := header.Load().(string); h != "" {
			w.Header().Set("Retry-After", h)
		}
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL)
	ctx := context.Background()

	_, err := c.SearchSeries(ctx, "one")
	remote := gapRemote(t, err)
	if remote.Category != ports.RemoteRateLimit || remote.HTTPStatus != http.StatusTooManyRequests || !remote.RetryAt.IsZero() {
		t.Errorf("429 without Retry-After: %+v", remote)
	}
	header.Store("20")
	_, err = c.SearchSeries(ctx, "two")
	remote = gapRemote(t, err)
	if until := time.Until(remote.RetryAt); remote.Category != ports.RemoteRateLimit || until < 18*time.Second || until > 21*time.Second {
		t.Errorf("429 with Retry-After: 20 → %+v (in %v)", remote, until)
	}
	deadline := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	header.Store(deadline.Format(http.TimeFormat))
	_, err = c.SearchSeries(ctx, "three")
	if remote = gapRemote(t, err); !remote.RetryAt.Equal(deadline) {
		t.Errorf("429 with HTTP-date: %v, want %v", remote.RetryAt, deadline)
	}
	status.Store(http.StatusBadGateway)
	_, err = c.SearchSeries(ctx, "four")
	if remote = gapRemote(t, err); remote.Category != ports.RemoteInvalidResponse || remote.HTTPStatus != http.StatusBadGateway {
		t.Errorf("502: %+v", remote)
	}
	srv.Close()
	_, err = c.SearchSeries(ctx, "five")
	if remote = gapRemote(t, err); remote.Category != ports.RemoteTransport {
		t.Errorf("closed server: %+v", remote)
	}
}

func TestGapAdPreviewContradictionReportsExpectedIDs(t *testing.T) {
	srv := gapShowServer(t, gapShow, "")
	c := New(srv.URL)
	ctx := context.Background()

	_, err := c.PreviewExternal(ctx, domain.KindSeries, domain.ExternalRef{Provider: "tvdb", Value: "2002"})
	remote := gapRemote(t, err)
	if remote.Category != ports.RemoteIdentityConflict || remote.ExpectedIDs != (domain.ExternalIDs{TVDB: 2002}) || remote.ActualIDs.TVDB != 1001 || remote.ActualIDs.IMDB != "tt1000001" {
		t.Errorf("tvdb contradiction: %+v", remote)
	}
	_, err = c.LookupExternal(ctx, domain.KindSeries, domain.ExternalRef{Provider: "imdb", Value: "tt2000002"})
	remote = gapRemote(t, err)
	if remote.Category != ports.RemoteIdentityConflict || remote.ExpectedIDs != (domain.ExternalIDs{IMDB: "tt2000002"}) || remote.ActualIDs.IMDB != "tt1000001" {
		t.Errorf("imdb contradiction: %+v", remote)
	}
	// Matching refs preview the record; a differently-cased IMDb id matches.
	item, err := c.PreviewExternal(ctx, domain.KindSeries, domain.ExternalRef{Provider: "imdb", Value: "TT1000001"})
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "Some Show" || item.Year != 2019 || item.Overview != "Plain & simple." || item.PosterPath != "https://img/orig.jpg" || item.IDs.TVDB != 1001 || item.Runtime != 45 {
		t.Errorf("preview = %+v", item)
	}
	for _, tc := range []struct {
		kind domain.MediaKind
		ref  domain.ExternalRef
	}{
		{domain.KindMovie, domain.ExternalRef{Provider: "tvdb", Value: "1001"}},
		{domain.KindSeries, domain.ExternalRef{Provider: "tmdb", Value: "1"}},
	} {
		_, err := c.PreviewExternal(ctx, tc.kind, tc.ref)
		if remote := gapRemote(t, err); remote.Category != ports.RemoteUnsupportedQuery {
			t.Errorf("%s/%s: %+v", tc.kind, tc.ref.Provider, remote)
		}
	}
}

func TestGapAdLookupWithoutTVDBIsUnsupportedHydration(t *testing.T) {
	noTVDB := `{"id":1,"name":"Web Only","externals":{"thetvdb":null,"imdb":"tt3000003"}}`
	srv := gapShowServer(t, noTVDB, "")
	c := New(srv.URL)
	ctx := context.Background()

	_, err := c.LookupExternal(ctx, domain.KindSeries, domain.ExternalRef{Provider: "imdb", Value: "tt3000003"})
	remote := gapRemote(t, err)
	if remote.Category != ports.RemoteUnsupportedHydration || remote.ActualIDs.IMDB != "tt3000003" || remote.ActualIDs.TVDB != 0 {
		t.Errorf("lookup: %+v", remote)
	}
	_, err = c.GetSeriesByTVDB(ctx, 1001)
	remote = gapRemote(t, err)
	if remote.Category != ports.RemoteUnsupportedHydration || remote.ExpectedIDs.TVDB != 1001 || remote.ActualIDs.IMDB != "tt3000003" {
		t.Errorf("get by tvdb: %+v", remote)
	}
}

func TestGapAdGetSeriesByTVDBConflictAndEmptyRecord(t *testing.T) {
	srv := gapShowServer(t, gapShow, "")
	c := New(srv.URL)
	_, err := c.GetSeriesByTVDB(context.Background(), 2002)
	remote := gapRemote(t, err)
	if remote.Category != ports.RemoteIdentityConflict || remote.ExpectedIDs.TVDB != 2002 || remote.ActualIDs.TVDB != 1001 || remote.ActualIDs.IMDB != "tt1000001" {
		t.Errorf("conflict: %+v", remote)
	}

	empty := gapShowServer(t, `{}`, "")
	_, err = New(empty.URL).GetSeriesByTVDB(context.Background(), 1001)
	if remote := gapRemote(t, err); remote.Category != ports.RemoteNotFound || remote.HTTPStatus != http.StatusNotFound {
		t.Errorf("empty record: %+v", remote)
	}
}

func TestGapAdIdentityMetadataByIMDbWithNetworkCountry(t *testing.T) {
	akas := `[{"name":"Alguna Serie","country":{"code":"es"}},{"name":"  ","country":null},{"name":"Global Title","country":null}]`
	srv := gapShowServer(t, gapShow, akas)
	c := New(srv.URL)
	ctx := context.Background()

	got, err := c.IdentityMetadata(ctx, domain.KindSeries, domain.ExternalIDs{IMDB: "tt1000001"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Aliases) != 2 || got.Aliases[0].Title != "Alguna Serie" || got.Aliases[0].MarketCountry != "ES" || got.Aliases[0].SourceID != "1" || got.Aliases[1].MarketCountry != "" {
		t.Errorf("aliases = %+v", got.Aliases)
	}
	if len(got.Countries) != 1 || got.Countries[0] != (domain.CountryEvidence{Code: "US", Source: "tvmaze", Basis: "network"}) {
		t.Errorf("countries = %+v", got.Countries)
	}

	_, err = c.IdentityMetadata(ctx, domain.KindMovie, domain.ExternalIDs{TVDB: 1001})
	if remote := gapRemote(t, err); remote.Category != ports.RemoteUnsupportedQuery {
		t.Errorf("movie kind: %+v", remote)
	}
	_, err = c.IdentityMetadata(ctx, domain.KindSeries, domain.ExternalIDs{TMDB: 5})
	if remote := gapRemote(t, err); remote.Category != ports.RemoteUnsupportedQuery {
		t.Errorf("no usable id: %+v", remote)
	}
	_, err = c.IdentityMetadata(ctx, domain.KindSeries, domain.ExternalIDs{TVDB: 1001, IMDB: "tt9999999"})
	remote := gapRemote(t, err)
	if remote.Category != ports.RemoteIdentityConflict || remote.ExpectedIDs.IMDB != "tt9999999" || remote.ActualIDs.TVDB != 1001 {
		t.Errorf("imdb conflict: %+v", remote)
	}
}

func TestGapAdIdentityMetadataAKAFailureIsAnError(t *testing.T) {
	srv := gapShowServer(t, gapShow, "")
	_, err := New(srv.URL).IdentityMetadata(context.Background(), domain.KindSeries, domain.ExternalIDs{TVDB: 1001})
	if remote := gapRemote(t, err); remote.Category != ports.RemoteInvalidResponse || remote.HTTPStatus != http.StatusInternalServerError {
		t.Errorf("aka failure: %+v", remote)
	}
	missing := gapShowServer(t, `{}`, "")
	_, err = New(missing.URL).IdentityMetadata(context.Background(), domain.KindSeries, domain.ExternalIDs{TVDB: 1001})
	if err == nil {
		t.Error("an unknown show cannot publish an alias list")
	}
}
