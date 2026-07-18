package openlibrary

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
)

// fixtureServer replays recorded Open Library responses — the adapter's
// contract test runs with no network, like the TMDB one.
func fixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	serve := func(w http.ResponseWriter, file string) {
		b, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("no User-Agent header (Open Library etiquette)")
		}
		switch r.URL.Path {
		case "/search.json":
			if q := r.URL.Query().Get("q"); !strings.Contains(q, "Hail Mary") {
				t.Errorf("unexpected query %q", q)
			}
			serve(w, "search.json")
		case "/works/OL17091839W.json":
			serve(w, "work.json")
		case "/works/OL17091839W/editions.json":
			serve(w, "editions.json")
		case "/works/OL17091839W/ratings.json":
			serve(w, "ratings.json")
		case "/authors/OL7115219A.json":
			serve(w, "author.json")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSearchBooks(t *testing.T) {
	c := New(fixtureServer(t).URL)
	got, err := c.SearchBooks(context.Background(), "Project Hail Mary")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("results = %d, want 2", len(got))
	}
	r := got[0]
	if r.Kind != domain.KindBook || r.OLID != "OL17091839W" || r.Author != "Andy Weir" ||
		r.Title != "Project Hail Mary" || r.Year != 2021 {
		t.Errorf("result = %+v", r)
	}
	if !strings.Contains(r.PosterPath, "covers.openlibrary.org") ||
		!strings.HasPrefix(r.PosterPath, "https://") {
		t.Errorf("cover url = %q", r.PosterPath)
	}
}

func TestGetBook(t *testing.T) {
	c := New(fixtureServer(t).URL)
	item, err := c.GetBook(context.Background(), "OL17091839W")
	if err != nil {
		t.Fatal(err)
	}
	if item.Kind != domain.KindBook || item.Title != "Project Hail Mary" {
		t.Fatalf("item = %+v", item)
	}
	if item.Author != "Andy Weir" {
		t.Errorf("author = %q", item.Author)
	}
	if item.Year != 2021 {
		t.Errorf("year = %d", item.Year)
	}
	if item.IDs.OLID != "OL17091839W" || item.IDs.ISBN13 != "9780593135204" {
		t.Errorf("ids = %+v", item.IDs)
	}
	if !strings.Contains(item.Overview, "astronaut") {
		t.Errorf("overview = %q", item.Overview)
	}
	if len(item.Genres) != 5 { // capped at 5 subjects
		t.Errorf("genres = %v", item.Genres)
	}
	if item.SortTitle != "project hail mary" {
		t.Errorf("sort title = %q", item.SortTitle)
	}
	if item.Rating < 4.28 || item.Rating > 4.29 || item.RatingVotes != 206 {
		t.Errorf("rating = %v (%d votes)", item.Rating, item.RatingVotes)
	}
	if len(item.Ratings) != 1 || item.Ratings[0].Source != "openlibrary" || item.Ratings[0].Scale != 5 {
		t.Errorf("labeled ratings = %+v", item.Ratings)
	}

	// The "/works/" prefix form works too (search hands back bare ids, but
	// callers may pass raw keys).
	again, err := c.GetBook(context.Background(), "/works/OL17091839W")
	if err != nil || again.IDs.OLID != "OL17091839W" {
		t.Errorf("prefixed olid: %+v err %v", again.IDs, err)
	}
}

func TestYearOf(t *testing.T) {
	for in, want := range map[string]int{
		"May 4, 2021": 2021, "2021": 2021, "2021-05-04": 2021,
		"1968": 1968, "": 0, "n.d.": 0, "20211": 0,
	} {
		if got := yearOf(in); got != want {
			t.Errorf("yearOf(%q) = %d, want %d", in, got, want)
		}
	}
}
