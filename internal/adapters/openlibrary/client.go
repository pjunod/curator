// Package openlibrary implements ports.BookProvider against the Open
// Library API (ADR 0006: Open Library won the metadata bake-off — open
// data, no key, work-level ids, cover images; Google Books rate-limits
// anonymous callers aggressively and Hardcover requires an account).
// Monarr calls openlibrary.org directly; no operated middleman.
package openlibrary

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/monarr-media/monarr/internal/adapters/httpx"
	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// DefaultBaseURL is the Open Library API root.
const DefaultBaseURL = "https://openlibrary.org"

// userAgent identifies us per Open Library's API etiquette.
const userAgent = "Monarr (github.com/monarr-media/monarr)"

// Client is a BookProvider backed by Open Library. No API key needed.
type Client struct {
	baseURL string
	http    *http.Client
	limiter *rate.Limiter

	mu    sync.Mutex
	cache map[string]cacheEntry
	ttl   time.Duration
}

type cacheEntry struct {
	body    []byte
	expires time.Time
}

var _ ports.BookProvider = (*Client)(nil)

// New returns a Client. baseURL "" means DefaultBaseURL.
func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpx.NewClient(15 * time.Second),
		// Open Library asks for courteous use; a few req/s is plenty.
		limiter: rate.NewLimiter(rate.Limit(3), 3),
		cache:   map[string]cacheEntry{},
		ttl:     5 * time.Minute,
	}
}

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	full := c.baseURL + path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}

	c.mu.Lock()
	if e, ok := c.cache[full]; ok && time.Now().Before(e.expires) {
		c.mu.Unlock()
		return json.Unmarshal(e.body, out)
	}
	c.mu.Unlock()

	if err := c.limiter.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("openlibrary: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("openlibrary: reading response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("openlibrary: not found (404): %s", path)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("openlibrary: unexpected status %d for %s", resp.StatusCode, path)
	}

	c.mu.Lock()
	c.cache[full] = cacheEntry{body: body, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()

	return json.Unmarshal(body, out)
}

// ---- wire shapes (only the fields we consume) ----

type searchResp struct {
	Docs []struct {
		Key              string   `json:"key"` // "/works/OL82563W"
		Title            string   `json:"title"`
		AuthorName       []string `json:"author_name"`
		FirstPublishYear int      `json:"first_publish_year"`
		CoverID          int64    `json:"cover_i"`
		ISBN             []string `json:"isbn"`
	} `json:"docs"`
}

type workResp struct {
	Title       string          `json:"title"`
	Description json.RawMessage `json:"description"` // string or {"value": string}
	Covers      []int64         `json:"covers"`
	Subjects    []string        `json:"subjects"`
	FirstPubl   string          `json:"first_publish_date"`
	Authors     []struct {
		Author struct {
			Key string `json:"key"` // "/authors/OL1234A"
		} `json:"author"`
	} `json:"authors"`
}

type authorResp struct {
	Name string `json:"name"`
}

type ratingsResp struct {
	Summary struct {
		Average *float64 `json:"average"` // null when the work has no ratings
		Count   int      `json:"count"`
	} `json:"summary"`
}

type editionsResp struct {
	Entries []struct {
		ISBN13      []string `json:"isbn_13"`
		ISBN10      []string `json:"isbn_10"`
		PublishDate string   `json:"publish_date"`
	} `json:"entries"`
}

// workOLID extracts "OL82563W" from "/works/OL82563W" (idempotent on bare ids).
func workOLID(key string) string {
	return strings.TrimPrefix(strings.TrimPrefix(key, "/works/"), "/")
}

// coverURL renders a full cover image URL; the UI uses PosterPath verbatim
// when it is absolute (TMDB paths are relative, book covers are not).
func coverURL(coverID int64) string {
	if coverID == 0 {
		return ""
	}
	return fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-L.jpg", coverID)
}

func descriptionText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Value
	}
	return ""
}

func yearOf(date string) int {
	// Open Library dates arrive as "2021", "May 4, 2021", "2021-05-04"…
	// take the first plausible 4-digit year anywhere in the string.
	for i := 0; i+4 <= len(date); i++ {
		if (strings.HasPrefix(date[i:], "19") || strings.HasPrefix(date[i:], "20")) &&
			isDigits(date[i:i+4]) &&
			(i+4 == len(date) || !isDigits(date[i+4:i+5])) {
			y := 0
			_, _ = fmt.Sscanf(date[i:i+4], "%d", &y)
			return y
		}
	}
	return 0
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// ---- BookProvider ----

// SearchBooks implements ports.BookProvider.
func (c *Client) SearchBooks(ctx context.Context, query string) ([]ports.SearchResult, error) {
	params := url.Values{
		"q":      {query},
		"limit":  {"20"},
		"fields": {"key,title,author_name,first_publish_year,cover_i,isbn"},
	}
	var resp searchResp
	if err := c.get(ctx, "/search.json", params, &resp); err != nil {
		return nil, err
	}
	out := make([]ports.SearchResult, 0, len(resp.Docs))
	for _, d := range resp.Docs {
		author := ""
		if len(d.AuthorName) > 0 {
			author = d.AuthorName[0]
		}
		out = append(out, ports.SearchResult{
			Kind: domain.KindBook, OLID: workOLID(d.Key), Title: d.Title,
			Author: author, Year: d.FirstPublishYear, PosterPath: coverURL(d.CoverID),
		})
	}
	return out, nil
}

// GetBook implements ports.BookProvider: hydrates the work, its first
// author, and an edition's ISBNs (three calls, rate-limited and cached).
func (c *Client) GetBook(ctx context.Context, olid string) (domain.MediaItem, error) {
	olid = workOLID(olid)
	var work workResp
	if err := c.get(ctx, "/works/"+url.PathEscape(olid)+".json", nil, &work); err != nil {
		return domain.MediaItem{}, err
	}

	item := domain.MediaItem{
		Kind:        domain.KindBook,
		Title:       work.Title,
		SortTitle:   domain.SortTitle(work.Title),
		Year:        yearOf(work.FirstPubl),
		IDs:         domain.ExternalIDs{OLID: olid},
		Overview:    descriptionText(work.Description),
		Status:      "Released",
		ReleaseDate: work.FirstPubl,
	}
	if len(work.Covers) > 0 {
		item.PosterPath = coverURL(work.Covers[0])
	}
	if n := len(work.Subjects); n > 0 {
		if n > 5 {
			n = 5
		}
		item.Genres = work.Subjects[:n]
	}

	if len(work.Authors) > 0 {
		key := work.Authors[0].Author.Key // "/authors/OL1234A"
		var a authorResp
		if err := c.get(ctx, key+".json", nil, &a); err == nil {
			item.Author = a.Name
		}
	}

	// Community rating (0-5 scale), best-effort — absence is not an error.
	var ratings ratingsResp
	if err := c.get(ctx, "/works/"+url.PathEscape(olid)+"/ratings.json", nil, &ratings); err == nil {
		if ratings.Summary.Average != nil && ratings.Summary.Count > 0 {
			item.Rating = *ratings.Summary.Average
			item.RatingVotes = ratings.Summary.Count
			item.Ratings = []domain.Rating{{
				Source: "openlibrary", Value: item.Rating, Votes: item.RatingVotes, Scale: 5,
			}}
		}
	}

	// Best-effort ISBN from the first edition that has one.
	var eds editionsResp
	if err := c.get(ctx, "/works/"+url.PathEscape(olid)+"/editions.json",
		url.Values{"limit": {"10"}}, &eds); err == nil {
		for _, e := range eds.Entries {
			if len(e.ISBN13) > 0 {
				item.IDs.ISBN13 = e.ISBN13[0]
				break
			}
		}
		if item.Year == 0 {
			for _, e := range eds.Entries {
				if y := yearOf(e.PublishDate); y != 0 && (item.Year == 0 || y < item.Year) {
					item.Year = y
				}
			}
		}
	}
	return item, nil
}
