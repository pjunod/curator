package tvmaze

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// newFixtureServer serves responses recorded from the real API, including
// the 301 that /lookup/shows answers with — see TestLookupFollowsTheRedirect
// for why that redirect is worth reproducing rather than flattening.
func newFixtureServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/search/shows", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "search_shows.json"))
	})
	mux.HandleFunc("/lookup/shows", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Query().Get("thetvdb") != "414217" && r.URL.Query().Get("imdb") != "tt16867040" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, srv.URL+"/shows/63900", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/shows/63900", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "show_63900.json"))
	})
	mux.HandleFunc("/shows/63900/episodes", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "episodes_63900.json"))
	})
	mux.HandleFunc("/shows/63900/akas", func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "akas_63900.json"))
	})
	return srv, &hits
}

func TestSearchSeriesKeysOnTheTVDBID(t *testing.T) {
	srv, _ := newFixtureServer(t)
	res, err := New(srv.URL).SearchSeries(context.Background(), "Cunk on Earth")
	if err != nil {
		t.Fatal(err)
	}
	// Two hits in the fixture; the one with no TheTVDB id is dropped, because
	// a record only this provider can name cannot be reconciled with one from
	// TheTVDB later.
	if len(res) != 1 {
		t.Fatalf("want 1 usable result, got %d", len(res))
	}
	got := res[0]
	if got.TVDBID != 414217 {
		t.Errorf("tvdb id = %d, want 414217", got.TVDBID)
	}
	if got.TMDBID != 0 {
		t.Errorf("a TVmaze result must not claim a TMDB id, got %d", got.TMDBID)
	}
	if got.Kind != domain.KindSeries || got.Title != "Cunk on Earth" || got.Year != 2022 {
		t.Errorf("unexpected result: %+v", got)
	}
	if got.Source != "tvmaze" {
		t.Errorf("source = %q, want tvmaze", got.Source)
	}
	if strings.Contains(got.Overview, "<") {
		t.Errorf("summaries are HTML upstream and must arrive as text: %q", got.Overview)
	}
	// The grid downloads whatever is stored here once per card, and TVmaze's
	// "original" is a full-resolution scan an order of magnitude larger.
	if !strings.Contains(got.PosterPath, "medium") {
		t.Errorf("want the medium image for the grid, got %q", got.PosterPath)
	}
}

// /lookup/shows answers 301 to /shows/{id} rather than returning the show.
// A client that does not follow that reads an empty body and reports "no
// such series" — which is what a hand-check with curl looks like, and is why
// the fixture server redirects too.
func TestLookupFollowsTheRedirect(t *testing.T) {
	srv, hits := newFixtureServer(t)
	item, err := New(srv.URL).GetSeriesByTVDB(context.Background(), 414217)
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "Cunk on Earth" {
		t.Fatalf("title = %q", item.Title)
	}
	if hits.Load() < 3 {
		t.Errorf("want lookup + show + episodes, got %d requests", hits.Load())
	}
}

func TestGetSeriesByTVDBHydratesEpisodes(t *testing.T) {
	srv, _ := newFixtureServer(t)
	item, err := New(srv.URL).GetSeriesByTVDB(context.Background(), 414217)
	if err != nil {
		t.Fatal(err)
	}

	if item.IDs.TVDB != 414217 || item.IDs.IMDB != "tt16867040" {
		t.Errorf("ids = %+v", item.IDs)
	}
	if item.Year != 2022 || !item.Ended {
		t.Errorf("year = %d ended = %t", item.Year, item.Ended)
	}
	if len(item.Ratings) != 1 || item.Ratings[0].Source != "tvmaze" || item.Ratings[0].Scale != 10 {
		t.Errorf("ratings = %+v", item.Ratings)
	}

	// The shape Plex shows and the folder implies: one season of five, in
	// order, with absolute numbers.
	var regular *domain.Season
	var specials *domain.Season
	for i := range item.Seasons {
		switch item.Seasons[i].Number {
		case 0:
			specials = &item.Seasons[i]
		case 1:
			regular = &item.Seasons[i]
		}
	}
	if regular == nil || len(regular.Episodes) != 5 {
		t.Fatalf("want season 1 with 5 episodes, got %+v", item.Seasons)
	}
	for i, e := range regular.Episodes {
		if e.EpisodeNumber != i+1 || e.AbsoluteNum != i+1 || !e.Monitored {
			t.Errorf("episode %d = %+v", i, e)
		}
	}

	// TVmaze numbers specials null and leaves them in the season they aired
	// beside; the *arr convention — and the naming releases use — is season
	// zero, unmonitored.
	if specials == nil || len(specials.Episodes) != 1 {
		t.Fatalf("want one special in season 0, got %+v", item.Seasons)
	}
	sp := specials.Episodes[0]
	if sp.EpisodeNumber != 1 || sp.Monitored || sp.AbsoluteNum != 0 {
		t.Errorf("special = %+v", sp)
	}
	if sp.Title != "Cunk on Life" {
		t.Errorf("special title = %q", sp.Title)
	}
}

func TestUnknownTVDBIDIsAnError(t *testing.T) {
	srv, _ := newFixtureServer(t)
	if _, err := New(srv.URL).GetSeriesByTVDB(context.Background(), 1); err == nil {
		t.Fatal("want an error for a series the provider does not have")
	}
}

func TestExactIMDbLookupAndIdentitySnapshot(t *testing.T) {
	srv, _ := newFixtureServer(t)
	client := New(srv.URL)
	results, err := client.LookupExternal(context.Background(), domain.KindSeries, domain.ExternalRef{Provider: "imdb", Value: "tt16867040"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].TVDBID != 414217 || results[0].HydrationSource != "tvmaze" {
		t.Fatalf("results = %+v", results)
	}
	metadata, err := client.IdentityMetadata(context.Background(), domain.KindSeries, domain.ExternalIDs{TVDB: 414217, IMDB: "tt16867040"})
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.Aliases) != 2 || metadata.Aliases[1].MarketCountry != "DE" || metadata.Aliases[0].Scope != "work" {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestEmptyQueryCostsNoRequest(t *testing.T) {
	srv, hits := newFixtureServer(t)
	res, err := New(srv.URL).SearchSeries(context.Background(), "   ")
	if err != nil || len(res) != 0 {
		t.Fatalf("res = %v err = %v", res, err)
	}
	if hits.Load() != 0 {
		t.Errorf("a blank query should not reach the network, got %d requests", hits.Load())
	}
}

func TestPlainText(t *testing.T) {
	got := plainText("<p>Philomena &amp; friends<br>ask <b>questions</b>.</p>")
	if want := "Philomena & friends ask questions."; !strings.Contains(got, "&") ||
		strings.Contains(got, "<") {
		t.Errorf("plainText(...) = %q, want something like %q", got, want)
	}
}
