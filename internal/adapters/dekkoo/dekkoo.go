// Package dekkoo reads the union of Dekkoo's five explicitly gay categories.
package dekkoo

import (
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

// FeedURL deliberately excludes the unfiltered blog feed and broader categories.
const FeedURL = "https://dekkoo.blog/feed/?category_name=gay-movies,gay-series,gay-romance,gay-comedy,gay-short-films"
const maxBytes = 2 << 20

var categories = []string{"Gay Movies", "Gay Series", "Gay Romance", "Gay Comedy", "Gay Short Films"}
var tags = regexp.MustCompile(`<[^>]*>`)

// Client keeps one bounded snapshot; it fetches only when the feed is opened.
type Client struct {
	endpoint   string
	http       *http.Client
	mu         sync.Mutex
	cached     ports.EditorialFeed
	now        func() time.Time
	retryAfter time.Time
}

// New accepts an operator-supplied endpoint override for integration fixtures.
// Browsers cannot choose the upstream URL.
func New(endpoint string) *Client {
	if endpoint == "" {
		endpoint = FeedURL
	}
	return &Client{endpoint: endpoint, http: &http.Client{Timeout: 15 * time.Second}, now: time.Now}
}

// Feed caches successes for 30 minutes and reports stale data for at most a day.
func (c *Client) Feed(ctx context.Context) (ports.EditorialFeed, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	age := now.Sub(c.cached.FetchedAt)
	if !c.cached.FetchedAt.IsZero() && age < 30*time.Minute {
		return clone(c.cached), nil
	}
	if !c.cached.FetchedAt.IsZero() && age < 24*time.Hour && now.Before(c.retryAfter) {
		stale := clone(c.cached)
		stale.Stale = true
		return stale, nil
	}
	items, err := c.fetch(ctx)
	if err != nil {
		if !c.cached.FetchedAt.IsZero() && age < 24*time.Hour && ctx.Err() == nil {
			c.retryAfter = now.Add(time.Minute)
			stale := clone(c.cached)
			stale.Stale = true
			return stale, nil
		}
		return ports.EditorialFeed{}, err
	}
	c.cached = ports.EditorialFeed{URL: FeedURL, Categories: slices.Clone(categories), Items: items, FetchedAt: c.now()}
	c.retryAfter = time.Time{}
	return clone(c.cached), nil
}

func clone(in ports.EditorialFeed) ports.EditorialFeed {
	in.Categories = slices.Clone(in.Categories)
	in.Items = slices.Clone(in.Items)
	for i := range in.Items {
		in.Items[i].Categories = slices.Clone(in.Items[i].Categories)
		if in.Items[i].PublishedAt != nil {
			t := *in.Items[i].PublishedAt
			in.Items[i].PublishedAt = &t
		}
	}
	return in
}

func (c *Client) fetch(ctx context.Context) ([]ports.EditorialArticle, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/rss+xml, application/xml")
	req.Header.Set("User-Agent", "Curator/DekkooFeed")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dekkoo returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes {
		return nil, fmt.Errorf("dekkoo feed exceeds size limit")
	}
	return parse(body)
}

func parse(body []byte) ([]ports.EditorialArticle, error) {
	var rss struct {
		XMLName xml.Name `xml:"rss"`
		Channel *struct {
			Items []struct {
				Title       string   `xml:"title"`
				Link        string   `xml:"link"`
				Description string   `xml:"description"`
				Date        string   `xml:"pubDate"`
				Categories  []string `xml:"category"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(body, &rss); err != nil {
		return nil, fmt.Errorf("invalid Dekkoo RSS: %w", err)
	}
	if rss.Channel == nil {
		return nil, fmt.Errorf("dekkoo RSS has no channel")
	}
	out := []ports.EditorialArticle{}
	seen := map[string]bool{}
	for _, item := range rss.Channel.Items {
		matched := []string{}
		for _, allowed := range categories {
			for _, cat := range item.Categories {
				if strings.EqualFold(strings.TrimSpace(cat), allowed) {
					matched = append(matched, allowed)
					break
				}
			}
		}
		if len(matched) == 0 {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(item.Link))
		if err != nil || u.Scheme != "https" || u.Host != "dekkoo.blog" || u.User != nil {
			continue
		}
		u.Fragment = ""
		link := u.String()
		title := plain(item.Title, 300)
		if seen[link] || title == "" {
			continue
		}
		seen[link] = true
		article := ports.EditorialArticle{Title: title, URL: link, Summary: plain(item.Description, 450), Categories: matched}
		for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822} {
			if date, err := time.Parse(layout, strings.TrimSpace(item.Date)); err == nil {
				article.PublishedAt = &date
				break
			}
		}
		out = append(out, article)
	}
	slices.SortStableFunc(out, func(a, b ports.EditorialArticle) int {
		if a.PublishedAt == nil {
			if b.PublishedAt == nil {
				return 0
			}
			return 1
		}
		if b.PublishedAt == nil {
			return -1
		}
		return b.PublishedAt.Compare(*a.PublishedAt)
	})
	if len(out) > 50 {
		out = out[:50]
	}
	return out, nil
}

// Text is always rendered as text by clients, never as publisher HTML.
func plain(s string, limit int) string {
	s = strings.Join(strings.Fields(html.UnescapeString(tags.ReplaceAllString(s, " "))), " ")
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit]) + "…"
	}
	return s
}
