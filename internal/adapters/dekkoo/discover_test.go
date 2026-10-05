package dekkoo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

func TestTitleExtractionFromPublisherFormats(t *testing.T) {
	cases := []struct {
		title, content string
		categories     []string
		want           []candidate
	}{
		{"SCORCH MARKS Is Now Streaming Exclusively on Dekkoo", `<a href="https://watch.dekkoo.com/scorch-marks">WATCH SCORCH MARKS NOW</a>`, []string{"Gay Movies"}, []candidate{{"SCORCH MARKS", domain.KindMovie, 0}}},
		{"New on Dekkoo: Desire Takes Hold in A King, Gazing at the Sea", `<a href="https://watch.dekkoo.com/a-king-gazing-at-the-sea">WATCH A KING, GAZING AT THE SEA ON DEKKOO →</a>`, []string{"Gay Short Films"}, []candidate{{"A KING, GAZING AT THE SEA", domain.KindMovie, 0}}},
		{"Sauna Is Now Streaming on Dekkoo — A Bold Gay Romance From Denmark", `<a href="https://watch.dekkoo.com/sauna">Watch</a> <a href="https://watch.dekkoo.com/sauna"><em>Sauna</em></a> <a href="https://watch.dekkoo.com/sauna">now on Dekkoo</a>`, []string{"Gay Romance"}, []candidate{{"Sauna", domain.KindMovie, 0}}},
		{"Woke Season 2 Is Now Streaming on Dekkoo", `<a href="https://watch.dekkoo.com/woke/season:2">Watch Woke Season 2 on Dekkoo</a><a href="https://watch.dekkoo.com/woke/season:1">Season 1</a>`, []string{"Gay Series"}, []candidate{{"Woke", domain.KindSeries, 0}}},
		{"9 Gay Horror Movies and Queer Thrillers to Stream This Halloween", `<h2>Scorch Marks</h2><h2>There’s a Zombie Outside</h2><h2>3. Birder (2023)</h2>`, []string{"Gay Movies"}, []candidate{{"Scorch Marks", domain.KindMovie, 0}, {"There’s a Zombie Outside", domain.KindMovie, 0}, {"Birder", domain.KindMovie, 2023}}},
		{"Oceania: A Queer Story About Memory", `<h2>A Film About Remembering</h2><a href="https://watch.dekkoo.com/oceania">Watch Oceania now on Dekkoo →</a>`, []string{"Gay Movies"}, []candidate{{"Oceania", domain.KindMovie, 0}}},
		{"Catalog navigation is not a title", `<a href="https://watch.dekkoo.com/movies">Movies</a><a href="https://watch.dekkoo.com/dekkoo-selects-out-for-blood">Dekkoo Selects: Out For Blood</a><a href="https://watch.dekkoo.com/videos/rent-free-trailer">Rent Free Trailer</a><a href="https://evil.test/sauna">Sauna</a><a href="https://watch.dekkoo.com.evil.test/sauna">Sauna</a>`, []string{"Gay Movies"}, []candidate{}},
	}
	for _, tc := range cases {
		t.Run(tc.title, func(t *testing.T) {
			got := titles(feedArticle{Title: tc.title, Content: tc.content, Categories: tc.categories})
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("titles = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestResolveRejectsAmbiguousAndWrongIdentities(t *testing.T) {
	movie := ports.SearchResult{Kind: domain.KindMovie, TMDBID: 12, Title: "Sauna", Year: 2025, PosterPath: "/sauna.jpg"}
	namesake := movie
	namesake.TMDBID = 13
	namesake.Year = 2008
	cases := []struct {
		name    string
		title   candidate
		results []ports.SearchResult
		want    int64
	}{
		{"unique", candidate{"SAUNA", domain.KindMovie, 0}, []ports.SearchResult{movie}, 12},
		{"namesakes", candidate{"Sauna", domain.KindMovie, 0}, []ports.SearchResult{movie, namesake}, 0},
		{"explicit year", candidate{"Sauna", domain.KindMovie, 2025}, []ports.SearchResult{movie, namesake}, 12},
		{"wrong year", candidate{"Sauna", domain.KindMovie, 2026}, []ports.SearchResult{movie}, 0},
		{"wrong kind", candidate{"Sauna", domain.KindSeries, 0}, []ports.SearchResult{movie}, 0},
		{"fuzzy title", candidate{"Sauna nights", domain.KindMovie, 0}, []ports.SearchResult{movie}, 0},
		{"same identity twice", candidate{"Sauna", domain.KindMovie, 0}, []ports.SearchResult{movie, movie}, 12},
		{"original title", candidate{"Réveil", domain.KindMovie, 0}, []ports.SearchResult{{Kind: domain.KindMovie, TMDBID: 14, Title: "Awakening", AltTitles: []string{"Réveil"}}}, 14},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.title, tc.results); got.TMDBID != tc.want {
				t.Fatalf("resolve = %+v", got)
			}
		})
	}
}

type fakeMetadata struct {
	mu    sync.Mutex
	calls map[string]int
	fail  bool
}

func (*fakeMetadata) Configured(context.Context) bool { return true }
func (f *fakeMetadata) search(kind domain.MediaKind, title string) ([]ports.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[title]++
	if f.fail {
		return nil, errors.New("lookup unavailable")
	}
	id := int64(10)
	if kind == domain.KindSeries {
		id = 20
	}
	return []ports.SearchResult{{Kind: kind, TMDBID: id, Title: title, Year: 2024, PosterPath: "/poster.jpg", Overview: "Verified TMDB summary"}}, nil
}
func (f *fakeMetadata) SearchMovies(_ context.Context, title string) ([]ports.SearchResult, error) {
	return f.search(domain.KindMovie, title)
}
func (f *fakeMetadata) SearchSeries(_ context.Context, title string) ([]ports.SearchResult, error) {
	return f.search(domain.KindSeries, title)
}

func TestDiscoverCombinesCategoriesAsIdentifiedMedia(t *testing.T) {
	items := ""
	for i, category := range categories {
		title := "Movie"
		if category == "Gay Series" {
			title = "Show"
		}
		items += fmt.Sprintf(`<item><title>New on Dekkoo</title><link>https://dekkoo.blog/post-%d/</link><category>%s</category><content:encoded><![CDATA[<a href="https://watch.dekkoo.com/%s">%s</a>]]></content:encoded></item>`, i, category, normalized(title), title)
	}
	// A valid title in an unrelated category must never reach metadata search.
	items += `<item><title>News</title><link>https://dekkoo.blog/news/</link><category>News</category><content:encoded><![CDATA[<a href="https://watch.dekkoo.com/unrelated">Unrelated</a>]]></content:encoded></item>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(rss(items))) }))
	defer server.Close()
	meta := &fakeMetadata{calls: map[string]int{}}
	c := New(server.URL, meta)
	for _, kind := range []string{"movies", "series"} {
		got, err := c.Discover(context.Background(), "dekkoo-"+kind, 1)
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: %+v %v", kind, got, err)
		}
		if got[0].TMDBID == 0 || got[0].Source != "dekkoo" || got[0].PosterPath != "/poster.jpg" || got[0].Overview != "Verified TMDB summary" {
			t.Fatalf("not normal media: %+v", got)
		}
	}
	if !reflect.DeepEqual(meta.calls, map[string]int{"Movie": 1, "Show": 1}) {
		t.Fatalf("searches: %v", meta.calls)
	}
	if _, err := c.Discover(context.Background(), "unknown", 1); !errors.Is(err, ports.ErrUnknownList) {
		t.Fatal(err)
	}
	if got, err := c.Discover(context.Background(), "dekkoo-movies", 2); err != nil || len(got) != 0 {
		t.Fatalf("page 2: %v %v", got, err)
	}
	meta.fail = true
	if _, err := c.Discover(context.Background(), "dekkoo-movies", 1); err == nil {
		t.Fatal("lookup outage cached as empty row")
	}
	if _, err := New(server.URL, nil).Discover(context.Background(), "dekkoo-movies", 1); !errors.Is(err, ports.ErrProviderNotConfigured) {
		t.Fatal(err)
	}
}
