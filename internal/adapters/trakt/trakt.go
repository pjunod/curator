// Package trakt reads public Trakt lists for the Phase 5 import lists.
// Trakt requires a (free) API app client id even for public data; the user
// supplies it in the list config.
package trakt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// DefaultBaseURL is the Trakt API root.
const DefaultBaseURL = "https://api.trakt.tv"

// Client reads public lists.
type Client struct {
	baseURL  string
	clientID string
	http     *http.Client
}

// New returns a Client. baseURL "" means DefaultBaseURL.
func New(baseURL, clientID string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"), clientID: clientID,
		http: &http.Client{Timeout: 15 * time.Second},
	}
}

type listItem struct {
	Type  string `json:"type"`
	Movie *struct {
		Title string `json:"title"`
		Year  int    `json:"year"`
		IDs   struct {
			TMDB int64 `json:"tmdb"`
		} `json:"ids"`
	} `json:"movie"`
	Show *struct {
		Title string `json:"title"`
		Year  int    `json:"year"`
		IDs   struct {
			TMDB int64 `json:"tmdb"`
		} `json:"ids"`
	} `json:"show"`
}

// ListItems fetches user/{user}/lists/{slug}/items and maps entries with
// TMDB ids to search results (movies and shows; other types skipped).
func (c *Client) ListItems(ctx context.Context, user, slug string) ([]ports.SearchResult, error) {
	if c.clientID == "" {
		return nil, ports.ErrProviderNotConfigured
	}
	u := fmt.Sprintf("%s/users/%s/lists/%s/items", c.baseURL, user, slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("trakt-api-version", "2")
	req.Header.Set("trakt-api-key", c.clientID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("trakt: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("trakt: client id rejected (%d)", resp.StatusCode)
	case http.StatusNotFound:
		return nil, fmt.Errorf("trakt: list %s/%s not found", user, slug)
	default:
		return nil, fmt.Errorf("trakt: unexpected status %d", resp.StatusCode)
	}

	var items []listItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("trakt: bad response: %w", err)
	}
	out := make([]ports.SearchResult, 0, len(items))
	for _, it := range items {
		switch {
		case it.Movie != nil && it.Movie.IDs.TMDB != 0:
			out = append(out, ports.SearchResult{
				Kind: domain.KindMovie, TMDBID: it.Movie.IDs.TMDB,
				Title: it.Movie.Title, Year: it.Movie.Year,
			})
		case it.Show != nil && it.Show.IDs.TMDB != 0:
			out = append(out, ports.SearchResult{
				Kind: domain.KindSeries, TMDBID: it.Show.IDs.TMDB,
				Title: it.Show.Title, Year: it.Show.Year,
			})
		}
	}
	return out, nil
}
