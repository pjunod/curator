package omdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

const payload = `{
  "Title": "Fight Club", "imdbID": "tt0137523", "imdbVotes": "2,412,725",
  "Ratings": [
    {"Source": "Internet Movie Database", "Value": "8.8/10"},
    {"Source": "Rotten Tomatoes", "Value": "79%"},
    {"Source": "Metacritic", "Value": "67/100"}
  ],
  "Response": "True"
}`

func staticKey(k string) KeyFunc {
	return func(context.Context) (string, error) { return k, nil }
}

func TestRatingsParsesAllSources(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("apikey") != "k" || r.URL.Query().Get("i") != "tt0137523" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, staticKey("k"))
	rs, err := c.Ratings(context.Background(), "tt0137523")
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("ratings = %+v", rs)
	}
	imdb, rt, mc := rs[0], rs[1], rs[2]
	if imdb.Source != "imdb" || imdb.Value != 8.8 || imdb.Scale != 10 || imdb.Votes != 2412725 {
		t.Errorf("imdb = %+v", imdb)
	}
	if rt.Source != "rt" || rt.Value != 79 || rt.Scale != 100 {
		t.Errorf("rt = %+v", rt)
	}
	if mc.Source != "metacritic" || mc.Value != 67 || mc.Scale != 100 {
		t.Errorf("metacritic = %+v", mc)
	}

	// Second call is served from cache — the daily quota is precious.
	if _, err := c.Ratings(context.Background(), "tt0137523"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("expected 1 upstream call, got %d", calls)
	}
}

func TestRatingsUnconfiguredAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Response":"False","Error":"Invalid API key!"}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := New(srv.URL, staticKey("")).Ratings(context.Background(), "tt1"); !errors.Is(err, ports.ErrProviderNotConfigured) {
		t.Errorf("no key: %v", err)
	}
	if _, err := New(srv.URL, staticKey("bad")).Ratings(context.Background(), "tt1"); err == nil {
		t.Error("bad key should surface as an error")
	}

	// Not-found is absence, not failure.
	nf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Response":"False","Error":"Error getting data."}`))
	}))
	t.Cleanup(nf.Close)
	rs, err := New(nf.URL, staticKey("k")).Ratings(context.Background(), "tt2")
	if err != nil || rs != nil {
		t.Errorf("not found: rs=%v err=%v", rs, err)
	}
}
