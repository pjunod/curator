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
)

// FeedURL deliberately excludes the unfiltered blog feed and broader categories.
const FeedURL = "https://dekkoo.blog/feed/?category_name=gay-movies,gay-series,gay-romance,gay-comedy,gay-short-films"
const maxBytes = 2 << 20

var categories = []string{"Gay Movies", "Gay Series", "Gay Romance", "Gay Comedy", "Gay Short Films"}
var tags = regexp.MustCompile(`<[^>]*>`)

type feedArticle struct {
	Title, URL, Content string
	Categories          []string
	PublishedAt         *time.Time
}
type feedSnapshot struct {
	Items     []feedArticle
	FetchedAt time.Time
}

// Client keeps one bounded RSS snapshot, fetched only when a row is opened.
type Client struct {
	endpoint string
	http     *http.Client
	mu       sync.Mutex
	cached   feedSnapshot
	now      func() time.Time
	metadata Metadata
}

// New accepts an operator-supplied endpoint override for integration fixtures.
// Browsers cannot choose the upstream URL.
func New(endpoint string, metadata Metadata) *Client {
	if endpoint == "" {
		endpoint = FeedURL
	}
	return &Client{endpoint: endpoint, http: &http.Client{Timeout: 15 * time.Second}, now: time.Now, metadata: metadata}
}

// feed shares one fresh RSS snapshot across both rows. The Discover service
// owns stale media rows, so an RSS outage cannot reset their expiry.
func (c *Client) feed(ctx context.Context) (feedSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	age := now.Sub(c.cached.FetchedAt)
	if !c.cached.FetchedAt.IsZero() && age < 30*time.Minute {
		return clone(c.cached), nil
	}
	items, err := c.fetch(ctx)
	if err != nil {
		return feedSnapshot{}, err
	}
	c.cached = feedSnapshot{Items: items, FetchedAt: c.now()}
	return clone(c.cached), nil
}

func clone(in feedSnapshot) feedSnapshot {
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

func (c *Client) fetch(ctx context.Context) ([]feedArticle, error) {
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

func parse(body []byte) ([]feedArticle, error) {
	var rss struct {
		XMLName xml.Name `xml:"rss"`
		Channel *struct {
			Items []struct {
				Title      string   `xml:"title"`
				Link       string   `xml:"link"`
				Content    string   `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
				Date       string   `xml:"pubDate"`
				Categories []string `xml:"category"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(body, &rss); err != nil {
		return nil, fmt.Errorf("invalid Dekkoo RSS: %w", err)
	}
	if rss.Channel == nil {
		return nil, fmt.Errorf("dekkoo RSS has no channel")
	}
	out := []feedArticle{}
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
		article := feedArticle{Title: title, URL: link, Content: item.Content, Categories: matched}
		for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822} {
			if date, err := time.Parse(layout, strings.TrimSpace(item.Date)); err == nil {
				article.PublishedAt = &date
				break
			}
		}
		out = append(out, article)
	}
	slices.SortStableFunc(out, func(a, b feedArticle) int {
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
