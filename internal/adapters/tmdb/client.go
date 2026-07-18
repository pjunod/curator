// Package tmdb implements ports.MetadataProvider against The Movie Database
// v3 API — the single metadata provider for both movies and TV
// (blueprint §4.2, one adapter, one key, one rate-limit policy).
package tmdb

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

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// DefaultBaseURL is TMDB's v3 API root.
const DefaultBaseURL = "https://api.themoviedb.org/3"

// KeyFunc supplies the API key at call time (it lives in settings and can
// change without a restart). Return "" when unconfigured.
type KeyFunc func(ctx context.Context) (string, error)

// Client is a MetadataProvider backed by TMDB.
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

var _ ports.MetadataProvider = (*Client)(nil)

// New returns a Client. baseURL "" means DefaultBaseURL.
func New(baseURL string, keyFn KeyFunc) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		keyFn:   keyFn,
		http:    &http.Client{Timeout: 15 * time.Second},
		// TMDB tolerates ~50 req/s; stay well under it.
		limiter: rate.NewLimiter(rate.Limit(10), 10),
		cache:   map[string]cacheEntry{},
		ttl:     5 * time.Minute,
	}
}

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	key, err := c.keyFn(ctx)
	if err != nil {
		return fmt.Errorf("tmdb: reading api key: %w", err)
	}
	if key == "" {
		return ports.ErrProviderNotConfigured
	}

	if params == nil {
		params = url.Values{}
	}
	bearer := strings.HasPrefix(key, "eyJ") // v4 read access tokens are JWTs
	if !bearer {
		params.Set("api_key", key)
	}
	full := c.baseURL + path
	if len(params) > 0 {
		full += "?" + params.Encode()
	}

	cacheKey := full
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
	req.Header.Set("Accept", "application/json")
	if bearer {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tmdb: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("tmdb: reading response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("tmdb: API key rejected (401)")
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("tmdb: not found (404): %s", path)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("tmdb: unexpected status %d for %s", resp.StatusCode, path)
	}

	c.mu.Lock()
	c.cache[cacheKey] = cacheEntry{body: body, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()

	return json.Unmarshal(body, out)
}

// ---- wire shapes (only the fields we consume) ----

type searchMovieResp struct {
	Results []struct {
		ID          int64  `json:"id"`
		Title       string `json:"title"`
		ReleaseDate string `json:"release_date"`
		Overview    string `json:"overview"`
		PosterPath  string `json:"poster_path"`
	} `json:"results"`
}

type searchTVResp struct {
	Results []struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		FirstAirDate string `json:"first_air_date"`
		Overview     string `json:"overview"`
		PosterPath   string `json:"poster_path"`
	} `json:"results"`
}

type genre struct {
	Name string `json:"name"`
}

type movieResp struct {
	ID           int64   `json:"id"`
	Title        string  `json:"title"`
	Overview     string  `json:"overview"`
	ReleaseDate  string  `json:"release_date"`
	Runtime      int     `json:"runtime"`
	Status       string  `json:"status"`
	PosterPath   string  `json:"poster_path"`
	BackdropPath string  `json:"backdrop_path"`
	Genres       []genre `json:"genres"`
	IMDBID       string  `json:"imdb_id"`
	VoteAverage  float64 `json:"vote_average"`
	VoteCount    int     `json:"vote_count"`
}

type tvResp struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Overview       string  `json:"overview"`
	FirstAirDate   string  `json:"first_air_date"`
	Status         string  `json:"status"`
	PosterPath     string  `json:"poster_path"`
	BackdropPath   string  `json:"backdrop_path"`
	Genres         []genre `json:"genres"`
	EpisodeRunTime []int   `json:"episode_run_time"`
	VoteAverage    float64 `json:"vote_average"`
	VoteCount      int     `json:"vote_count"`
	Seasons        []struct {
		SeasonNumber int `json:"season_number"`
	} `json:"seasons"`
	ExternalIDs struct {
		IMDBID string `json:"imdb_id"`
		TVDBID int64  `json:"tvdb_id"`
	} `json:"external_ids"`
}

type seasonResp struct {
	Episodes []struct {
		SeasonNumber  int    `json:"season_number"`
		EpisodeNumber int    `json:"episode_number"`
		Name          string `json:"name"`
		AirDate       string `json:"air_date"`
	} `json:"episodes"`
}

func yearOf(isoDate string) int {
	if len(isoDate) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(isoDate[:4])
	return y
}

// ---- MetadataProvider ----

// SearchMovies implements ports.MetadataProvider.
func (c *Client) SearchMovies(ctx context.Context, query string) ([]ports.SearchResult, error) {
	var resp searchMovieResp
	if err := c.get(ctx, "/search/movie", url.Values{"query": {query}}, &resp); err != nil {
		return nil, err
	}
	out := make([]ports.SearchResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, ports.SearchResult{
			Kind: domain.KindMovie, TMDBID: r.ID, Title: r.Title,
			Year: yearOf(r.ReleaseDate), Overview: r.Overview, PosterPath: r.PosterPath,
		})
	}
	return out, nil
}

// SearchSeries implements ports.MetadataProvider.
func (c *Client) SearchSeries(ctx context.Context, query string) ([]ports.SearchResult, error) {
	var resp searchTVResp
	if err := c.get(ctx, "/search/tv", url.Values{"query": {query}}, &resp); err != nil {
		return nil, err
	}
	out := make([]ports.SearchResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, ports.SearchResult{
			Kind: domain.KindSeries, TMDBID: r.ID, Title: r.Name,
			Year: yearOf(r.FirstAirDate), Overview: r.Overview, PosterPath: r.PosterPath,
		})
	}
	return out, nil
}

// GetMovie implements ports.MetadataProvider.
func (c *Client) GetMovie(ctx context.Context, tmdbID int64) (domain.MediaItem, error) {
	var resp movieResp
	if err := c.get(ctx, fmt.Sprintf("/movie/%d", tmdbID), nil, &resp); err != nil {
		return domain.MediaItem{}, err
	}
	return domain.MediaItem{
		Kind:         domain.KindMovie,
		Title:        resp.Title,
		SortTitle:    domain.SortTitle(resp.Title),
		Year:         yearOf(resp.ReleaseDate),
		IDs:          domain.ExternalIDs{TMDB: resp.ID, IMDB: resp.IMDBID},
		Overview:     resp.Overview,
		PosterPath:   resp.PosterPath,
		BackdropPath: resp.BackdropPath,
		Genres:       genreNames(resp.Genres),
		Status:       resp.Status,
		ReleaseDate:  resp.ReleaseDate,
		Runtime:      resp.Runtime,
		Rating:       resp.VoteAverage,
		RatingVotes:  resp.VoteCount,
		Ratings:      tmdbRatings(resp.VoteAverage, resp.VoteCount),
	}, nil
}

// tmdbRatings renders the vote fields as a labeled rating entry.
func tmdbRatings(avg float64, votes int) []domain.Rating {
	if votes == 0 {
		return nil
	}
	return []domain.Rating{{Source: "tmdb", Value: avg, Votes: votes, Scale: 10}}
}

// GetSeries implements ports.MetadataProvider: hydrates the series plus
// every season's episode list (one call per season, rate-limited).
func (c *Client) GetSeries(ctx context.Context, tmdbID int64) (domain.MediaItem, error) {
	var resp tvResp
	params := url.Values{"append_to_response": {"external_ids"}}
	if err := c.get(ctx, fmt.Sprintf("/tv/%d", tmdbID), params, &resp); err != nil {
		return domain.MediaItem{}, err
	}

	runtime := 0
	if len(resp.EpisodeRunTime) > 0 {
		runtime = resp.EpisodeRunTime[0]
	}
	item := domain.MediaItem{
		Kind:         domain.KindSeries,
		Title:        resp.Name,
		SortTitle:    domain.SortTitle(resp.Name),
		Year:         yearOf(resp.FirstAirDate),
		IDs:          domain.ExternalIDs{TMDB: resp.ID, IMDB: resp.ExternalIDs.IMDBID, TVDB: resp.ExternalIDs.TVDBID},
		Overview:     resp.Overview,
		PosterPath:   resp.PosterPath,
		BackdropPath: resp.BackdropPath,
		Genres:       genreNames(resp.Genres),
		Status:       resp.Status,
		ReleaseDate:  resp.FirstAirDate,
		Runtime:      runtime,
		Rating:       resp.VoteAverage,
		RatingVotes:  resp.VoteCount,
		Ratings:      tmdbRatings(resp.VoteAverage, resp.VoteCount),
		Ended:        strings.EqualFold(resp.Status, "Ended") || strings.EqualFold(resp.Status, "Canceled"),
	}

	// Absolute numbering: TMDB has no first-class absolute numbers, so
	// derive them as the cumulative episode index across regular seasons —
	// correct for the common continuously-numbered anime case (TVDB/AniDB
	// mapping tables are the future refinement).
	absolute := 0
	for _, s := range resp.Seasons {
		var sr seasonResp
		if err := c.get(ctx, fmt.Sprintf("/tv/%d/season/%d", tmdbID, s.SeasonNumber), nil, &sr); err != nil {
			return domain.MediaItem{}, fmt.Errorf("hydrating season %d: %w", s.SeasonNumber, err)
		}
		season := domain.Season{
			Number: s.SeasonNumber,
			// Specials start unmonitored, like upstream.
			Monitored: s.SeasonNumber != 0,
		}
		for _, e := range sr.Episodes {
			ep := domain.Episode{
				SeasonNumber:  e.SeasonNumber,
				EpisodeNumber: e.EpisodeNumber,
				Title:         e.Name,
				AirDate:       e.AirDate,
				Monitored:     s.SeasonNumber != 0,
			}
			if s.SeasonNumber != 0 {
				absolute++
				ep.AbsoluteNum = absolute
			}
			season.Episodes = append(season.Episodes, ep)
		}
		item.Seasons = append(item.Seasons, season)
	}
	return item, nil
}

// DiscoverMovies serves the import lists (Phase 5): kind is "popular" or
// "top_rated"; results carry TMDB ids ready for library adds.
func (c *Client) DiscoverMovies(ctx context.Context, kind string) ([]ports.SearchResult, error) {
	if kind != "popular" && kind != "top_rated" {
		return nil, fmt.Errorf("tmdb: unknown discover kind %q", kind)
	}
	var resp searchMovieResp
	if err := c.get(ctx, "/movie/"+kind, nil, &resp); err != nil {
		return nil, err
	}
	out := make([]ports.SearchResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, ports.SearchResult{
			Kind: domain.KindMovie, TMDBID: r.ID, Title: r.Title,
			Year: yearOf(r.ReleaseDate), Overview: r.Overview, PosterPath: r.PosterPath,
		})
	}
	return out, nil
}

// FindSeriesByTVDB resolves a TVDB id to a fully hydrated series via
// TMDB's /find endpoint — the Sonarr compat personality needs this because
// Sonarr consumers (Jellyseerr) identify series by TVDB id (blueprint §6).
// Not part of ports.MetadataProvider: it is a TMDB-specific capability,
// injected explicitly where needed.
func (c *Client) FindSeriesByTVDB(ctx context.Context, tvdbID int64) (domain.MediaItem, error) {
	var resp struct {
		TVResults []struct {
			ID int64 `json:"id"`
		} `json:"tv_results"`
	}
	path := fmt.Sprintf("/find/%d", tvdbID)
	if err := c.get(ctx, path, url.Values{"external_source": {"tvdb_id"}}, &resp); err != nil {
		return domain.MediaItem{}, err
	}
	if len(resp.TVResults) == 0 {
		return domain.MediaItem{}, fmt.Errorf("tmdb: no series for tvdb id %d", tvdbID)
	}
	return c.GetSeries(ctx, resp.TVResults[0].ID)
}

func genreNames(gs []genre) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}
