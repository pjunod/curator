// Package trakt reads Trakt's public data: the curated lists behind Phase 5
// import lists, and the trending/anticipated/box-office rows behind the
// Discover surface (ADR 0015).
//
// Trakt requires a (free) API app client id even for public data. There are
// two places it can come from, for a reason: Discover reads a single client
// id from settings, while an import list carries its own in its config
// because lists predate the setting and one that already works must not stop
// working because a global field is empty.
package trakt

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

// DefaultBaseURL is the Trakt API root.
const DefaultBaseURL = "https://api.trakt.tv"

// KeyFunc supplies the client id at call time (it lives in settings and can
// change without a restart). Return "" when unconfigured.
type KeyFunc func(ctx context.Context) (string, error)

// Client reads Trakt's public endpoints.
type Client struct {
	baseURL string
	keyFn   KeyFunc
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

// New returns a Client reading its client id from keyFn. baseURL "" means
// DefaultBaseURL.
func New(baseURL string, keyFn KeyFunc) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		keyFn:   keyFn,
		http:    httpx.NewClient(15 * time.Second),
		// Trakt's documented ceiling for unauthenticated GETs is 1000 calls
		// per 5 minutes (~3.3/s). Stay well under it.
		limiter: rate.NewLimiter(rate.Limit(2), 4),
		cache:   map[string]cacheEntry{},
		// Short, like TMDB's: the Discover service does the long caching
		// (ADR 0015 §4). This one only collapses bursts.
		ttl: 5 * time.Minute,
	}
}

// NewStatic returns a Client with a fixed client id — the import-list path,
// where the id comes from the list's own config rather than from settings.
func NewStatic(baseURL, clientID string) *Client {
	return New(baseURL, func(context.Context) (string, error) { return clientID, nil })
}

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	id, err := c.keyFn(ctx)
	if err != nil {
		return fmt.Errorf("trakt: reading client id: %w", err)
	}
	if id == "" {
		return ports.ErrProviderNotConfigured
	}

	full := c.baseURL + path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}

	// The client id is a header, not a query param, so unlike TMDB the URL
	// alone is the whole cache key only as long as one Client has one id.
	// It does — a Client's KeyFunc reads one setting — but a changed id must
	// not be served stale answers fetched under the old one, so it is mixed
	// in explicitly.
	cacheKey := id + "\x00" + full
	c.mu.Lock()
	if e, ok := c.cache[cacheKey]; ok && time.Now().Before(e.expires) {
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
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("trakt-api-version", "2")
	req.Header.Set("trakt-api-key", id)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("trakt: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("trakt: reading response: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("trakt: client id rejected (%d)", resp.StatusCode)
	case http.StatusNotFound:
		return fmt.Errorf("trakt: not found (404): %s", path)
	default:
		return fmt.Errorf("trakt: unexpected status %d for %s", resp.StatusCode, path)
	}

	c.mu.Lock()
	c.cache[cacheKey] = cacheEntry{body: body, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("trakt: bad response: %w", err)
	}
	return nil
}

// entity is Trakt's movie/show object, the same shape wherever it appears.
type entity struct {
	Title    string `json:"title"`
	Year     int    `json:"year"`
	Overview string `json:"overview"`
	IDs      struct {
		TMDB int64 `json:"tmdb"`
		TVDB int64 `json:"tvdb"`
	} `json:"ids"`
}

// envelope covers every list-shaped Trakt response we read. Trending wraps
// the entity next to a `watchers` count, anticipated next to `list_count`,
// box office next to `revenue`, and list items next to a `type` — the
// counters differ, the entity does not, so one struct decodes all of them.
type envelope struct {
	Type  string  `json:"type"`
	Movie *entity `json:"movie"`
	Show  *entity `json:"show"`
}

// results maps envelopes to search results, keeping only entries with a TMDB
// id. Everything downstream — the library, the add flow, artwork hydration —
// is keyed on TMDB, so an entry Trakt cannot tie to one is not something
// Monarr can act on.
func results(items []envelope) []ports.SearchResult {
	out := make([]ports.SearchResult, 0, len(items))
	for _, it := range items {
		switch {
		case it.Movie != nil && it.Movie.IDs.TMDB != 0:
			out = append(out, result(domain.KindMovie, it.Movie))
		case it.Show != nil && it.Show.IDs.TMDB != 0:
			out = append(out, result(domain.KindSeries, it.Show))
		}
	}
	return out
}

func result(kind domain.MediaKind, e *entity) ports.SearchResult {
	r := ports.SearchResult{
		Kind: kind, TMDBID: e.IDs.TMDB, Source: "trakt",
		Title: e.Title, Year: e.Year, Overview: e.Overview,
	}
	if kind == domain.KindSeries {
		r.TVDBID = e.IDs.TVDB
	}
	return r
}

// ListItems fetches users/{user}/lists/{slug}/items and maps entries with
// TMDB ids to search results (movies and shows; other types skipped).
func (c *Client) ListItems(ctx context.Context, user, slug string) ([]ports.SearchResult, error) {
	var items []envelope
	path := fmt.Sprintf("/users/%s/lists/%s/items", url.PathEscape(user), url.PathEscape(slug))
	if err := c.get(ctx, path, nil, &items); err != nil {
		return nil, err
	}
	return results(items), nil
}
