package dekkoo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func article(title, link, category, date string) string {
	return fmt.Sprintf(`<item><title>%s</title><link>%s</link><category>%s</category><pubDate>%s</pubDate><description><![CDATA[<p>A gay &amp; romantic story.</p>]]></description></item>`, title, link, category, date)
}
func rss(items string) string { return `<rss version="2.0"><channel>` + items + `</channel></rss>` }

func TestSelectedCategoriesOnlyDeduplicatedNewestFirst(t *testing.T) {
	old := "Mon, 28 Sep 2026 12:00:00 GMT"
	recent := "Mon, 05 Oct 2026 12:00:00 +0000"
	items := ""
	for i, category := range categories {
		items += article(category, fmt.Sprintf("https://dekkoo.blog/article-%d/", i), category, old)
	}
	items += article("Latest", "https://dekkoo.blog/latest/", "gay series", recent)
	items += article("Duplicate", "https://dekkoo.blog/latest/#comment", "Gay Romance", recent)
	for _, category := range []string{"News", "Drama", "Horror", "LGBTQ+ Cinema", "Queer Cinema"} {
		items += article("Excluded", "https://dekkoo.blog/other-"+category, category, recent)
	}
	for _, link := range []string{"javascript:alert(1)", "https://evil.test/", "https://dekkoo.blog.evil.test/", "http://dekkoo.blog/", "https://user@dekkoo.blog/", "https://dekkoo.blog:8080/"} {
		items += article("Unsafe", link, "Gay Movies", recent)
	}
	got, err := parse([]byte(rss(items)))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d articles: %+v", len(got), got)
	}
	if got[0].PublishedAt == nil {
		t.Fatal("RSS numeric timezone date was lost")
	}
	if got[0].Title != "Latest" || got[0].URL != "https://dekkoo.blog/latest/" {
		t.Fatalf("latest = %+v", got[0])
	}
	if got[0].Summary != "A gay & romantic story." {
		t.Fatalf("summary = %q", got[0].Summary)
	}
	if got[0].Categories[0] != "Gay Series" {
		t.Fatalf("categories = %v", got[0].Categories)
	}
}

func TestFeedCacheStaleExpiryAndIsolation(t *testing.T) {
	var calls, status atomic.Int32
	status.Store(200)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Accept") == "" {
			t.Error("missing RSS Accept header")
		}
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(rss(article("One", "https://dekkoo.blog/one/", "Gay Movies", "Mon, 05 Oct 2026 12:00:00 GMT"))))
	}))
	defer server.Close()
	c := New(server.URL)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	first, err := c.Feed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first.Items[0].Title = "mutated"
	first.Items[0].Categories[0] = "mutated"
	*first.Items[0].PublishedAt = time.Time{}
	first.Categories[0] = "mutated"
	second, err := c.Feed(context.Background())
	if err != nil || calls.Load() != 1 || second.Items[0].Title != "One" || second.Items[0].Categories[0] != "Gay Movies" || second.Categories[0] != "Gay Movies" || second.Items[0].PublishedAt.IsZero() {
		t.Fatalf("cache: %+v %v calls=%d", second, err, calls.Load())
	}
	status.Store(503)
	now = now.Add(31 * time.Minute)
	stale, err := c.Feed(context.Background())
	if err != nil || !stale.Stale || !stale.FetchedAt.Equal(second.FetchedAt) {
		t.Fatalf("stale: %+v %v", stale, err)
	}
	_, _ = c.Feed(context.Background())
	if calls.Load() != 2 {
		t.Fatal("failure backoff ignored")
	}
	now = now.Add(time.Minute)
	status.Store(200)
	fresh, err := c.Feed(context.Background())
	if err != nil || fresh.Stale || !fresh.FetchedAt.Equal(now) {
		t.Fatalf("refresh: %+v %v", fresh, err)
	}
	status.Store(503)
	now = now.Add(24 * time.Hour)
	if _, err = c.Feed(context.Background()); err == nil {
		t.Fatal("expired snapshot served")
	}
}

func TestInvalidAndBoundedFeed(t *testing.T) {
	for _, body := range []string{`<html/>`, `<rss/>`, `<rss><channel>`, strings.Repeat("x", maxBytes+1)} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			if _, err := New(server.URL).Feed(context.Background()); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
	got, err := parse([]byte(rss("")))
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty feed: %v %v", got, err)
	}
	items := ""
	for i := range 60 {
		items += article(strings.Repeat("é", 350), fmt.Sprintf("https://dekkoo.blog/%d", i), "Gay Movies", "invalid-date")
	}
	got, err = parse([]byte(rss(items)))
	if err != nil || len(got) != 50 || got[0].PublishedAt != nil || len([]rune(got[0].Title)) != 301 {
		t.Fatalf("limits: len=%d err=%v", len(got), err)
	}
}

func TestCancelledFetch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New("http://127.0.0.1:1").Feed(ctx); err == nil {
		t.Fatal("cancelled request succeeded")
	}
}
