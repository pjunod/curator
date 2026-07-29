// Package omdb implements ports.RatingsProvider against the OMDb API —
// the practical source for Rotten Tomatoes, IMDb, and Metacritic scores
// (TMDB carries none of them). Free keys at omdbapi.com (1000 req/day),
// so the client caches aggressively and rate-limits politely.
package omdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/pjunod/monarr/internal/adapters/httpx"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// DefaultBaseURL is the OMDb API root.
const DefaultBaseURL = "https://www.omdbapi.com"

// KeyFunc supplies the API key at call time (it lives in settings and can
// change without a restart). Return "" when unconfigured.
type KeyFunc func(ctx context.Context) (string, error)

// Client is a RatingsProvider backed by OMDb.
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
	ratings []domain.Rating
	expires time.Time
}

var _ ports.RatingsProvider = (*Client)(nil)

// New returns a Client. baseURL "" means DefaultBaseURL.
func New(baseURL string, keyFn KeyFunc) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		keyFn:   keyFn,
		http:    httpx.NewClient(15 * time.Second),
		// The free tier is 1000/day; 1 req/s keeps bursts civil.
		limiter: rate.NewLimiter(rate.Limit(1), 3),
		cache:   map[string]cacheEntry{},
		// Ratings drift slowly; a long TTL conserves the daily quota.
		ttl: 12 * time.Hour,
	}
}

// omdbResp is the subset of the payload we consume.
type omdbResp struct {
	Response  string `json:"Response"` // "True" | "False"
	Error     string `json:"Error"`
	IMDBVotes string `json:"imdbVotes"` // "2,412,725"
	Ratings   []struct {
		Source string `json:"Source"` // "Internet Movie Database" | "Rotten Tomatoes" | "Metacritic"
		Value  string `json:"Value"`  // "8.8/10" | "79%" | "67/100"
	} `json:"Ratings"`
}

// Ratings implements ports.RatingsProvider.
func (c *Client) Ratings(ctx context.Context, imdbID string) ([]domain.Rating, error) {
	key, err := c.keyFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("omdb: reading api key: %w", err)
	}
	if key == "" {
		return nil, ports.ErrProviderNotConfigured
	}
	if imdbID == "" {
		return nil, nil
	}

	c.mu.Lock()
	if e, ok := c.cache[imdbID]; ok && time.Now().Before(e.expires) {
		c.mu.Unlock()
		return e.ratings, nil
	}
	c.mu.Unlock()

	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}
	params := url.Values{"apikey": {key}, "i": {imdbID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("omdb: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("omdb: reading response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("omdb: API key rejected (401)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("omdb: unexpected status %d", resp.StatusCode)
	}
	var out omdbResp
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("omdb: bad JSON: %w", err)
	}
	if out.Response != "True" {
		// "Error getting data." / not found — absence, not failure.
		if strings.Contains(strings.ToLower(out.Error), "api key") {
			return nil, fmt.Errorf("omdb: %s", out.Error)
		}
		return nil, nil
	}

	ratings := parseRatings(out)
	c.mu.Lock()
	c.cache[imdbID] = cacheEntry{ratings: ratings, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return ratings, nil
}

func parseRatings(r omdbResp) []domain.Rating {
	votes := parseVotes(r.IMDBVotes)
	var out []domain.Rating
	for _, entry := range r.Ratings {
		switch entry.Source {
		case "Internet Movie Database":
			if v, ok := parseFraction(entry.Value, "/10"); ok {
				out = append(out, domain.Rating{Source: "imdb", Value: v, Votes: votes, Scale: 10})
			}
		case "Rotten Tomatoes":
			if v, ok := parsePercent(entry.Value); ok {
				out = append(out, domain.Rating{Source: "rt", Value: v, Scale: 100})
			}
		case "Metacritic":
			if v, ok := parseFraction(entry.Value, "/100"); ok {
				out = append(out, domain.Rating{Source: "metacritic", Value: v, Scale: 100})
			}
		}
	}
	return out
}

func parseFraction(s, suffix string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), suffix), 64)
	return v, err == nil
}

func parsePercent(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	return v, err == nil
}

func parseVotes(s string) int {
	n, _ := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(s), ",", ""))
	return n
}
