package torznab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

const feed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">
<channel>
<item>
  <title>Test.Show.S01E01.1080p.WEB-DL.x264-GRP</title>
  <guid>http://idx/details/1</guid>
  <link>http://idx/dl/1.torrent</link>
  <pubDate>Fri, 17 Jul 2026 10:00:00 +0000</pubDate>
  <enclosure url="http://idx/dl/1.torrent" length="1073741824" type="application/x-bittorrent"/>
  <torznab:attr name="seeders" value="42"/>
  <torznab:attr name="peers" value="7"/>
</item>
<item>
  <title>Test.Show.S01.1080p.BluRay.x264-PACK</title>
  <guid>http://idx/details/2</guid>
  <link>http://idx/dl/2.torrent</link>
  <size>5368709120</size>
  <pubDate>Thu, 16 Jul 2026 10:00:00 +0000</pubDate>
</item>
</channel>
</rss>`

const caps = `<?xml version="1.0"?><caps><server title="test"/></caps>`

func newServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") == "" {
			w.Write([]byte(`<error code="100" description="Invalid API Key"/>`))
			return
		}
		queries = append(queries, r.URL.RawQuery)
		if r.URL.Query().Get("t") == "caps" {
			w.Write([]byte(caps))
			return
		}
		w.Write([]byte(feed))
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

func TestSearchParsesFeed(t *testing.T) {
	srv, queries := newServer(t)
	c := New(ports.IndexerConfig{ID: 1, Name: "idx", URL: srv.URL, APIKey: "k",
		Protocol: "torrent", Categories: []int{5030, 5040}})

	rs, err := c.Search(context.Background(), domain.SearchQuery{Q: "Test Show S01E01", Season: 1, Episode: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("releases = %d", len(rs))
	}
	r := rs[0]
	if r.Title != "Test.Show.S01E01.1080p.WEB-DL.x264-GRP" || r.Seeders != 42 ||
		r.Size != 1073741824 || r.DownloadURL != "http://idx/dl/1.torrent" ||
		r.Protocol != "torrent" || r.Indexer != "idx" {
		t.Errorf("release = %+v", r)
	}
	if rs[1].Size != 5368709120 {
		t.Errorf("size from <size> element: %+v", rs[1])
	}
	// tvsearch with season/ep + categories requested.
	q := (*queries)[0]
	for _, want := range []string{"t=tvsearch", "season=1", "ep=1", "cat=5030%2C5040"} {
		if !contains(q, want) {
			t.Errorf("query %q missing %q", q, want)
		}
	}
}

func TestMovieSearchUsesPlainSearch(t *testing.T) {
	srv, queries := newServer(t)
	c := New(ports.IndexerConfig{Name: "idx", URL: srv.URL, APIKey: "k", Protocol: "torrent"})
	if _, err := c.Search(context.Background(), domain.SearchQuery{Q: "The Matrix 1999"}); err != nil {
		t.Fatal(err)
	}
	if !contains((*queries)[0], "t=search") {
		t.Errorf("movie query should use t=search: %q", (*queries)[0])
	}
}

func TestCapsAndErrors(t *testing.T) {
	srv, _ := newServer(t)
	good := New(ports.IndexerConfig{Name: "idx", URL: srv.URL, APIKey: "k", Protocol: "torrent"})
	if err := good.Test(context.Background()); err != nil {
		t.Errorf("caps test: %v", err)
	}
	bad := New(ports.IndexerConfig{Name: "idx", URL: srv.URL, APIKey: "", Protocol: "torrent"})
	if _, err := bad.Search(context.Background(), domain.SearchQuery{Q: "x"}); err == nil {
		t.Error("newznab error XML should surface as an error")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
