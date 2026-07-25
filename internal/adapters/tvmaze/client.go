// Package tvmaze implements ports.SeriesProvider against the TVmaze API —
// the keyless middle link of the series chain (ADR 0011).
//
// It exists because TMDB groups some programmes under one umbrella title
// that every other database, and the user's folders, treat as separate
// shows. TVmaze splits them the way TheTVDB does and publishes TheTVDB's own
// ids alongside its records, so monarr can key a series on TVDB without
// anyone holding a TVDB subscription.
//
// No API key, no account, no configuration. TVmaze asks for no more than 20
// requests per 10 seconds per IP, which the limiter here respects.
package tvmaze

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/monarr-media/monarr/internal/adapters/httpx"
	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// DefaultBaseURL is the TVmaze API root.
const DefaultBaseURL = "https://api.tvmaze.com"

// Client is a ports.SeriesProvider backed by TVmaze.
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

var _ ports.SeriesProvider = (*Client)(nil)

// New returns a Client. baseURL "" means DefaultBaseURL.
func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpx.NewClient(15 * time.Second),
		// TVmaze publishes "at least 20 calls every 10 seconds per IP".
		// Sit at half of it: adoption runs this over hundreds of folders and
		// a rate-limited provider looks exactly like a broken one.
		limiter: rate.NewLimiter(rate.Limit(1), 5),
		cache:   map[string]cacheEntry{},
		ttl:     5 * time.Minute,
	}
}

// Name implements ports.SeriesProvider.
func (c *Client) Name() string { return "tvmaze" }

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

	// Redirects are load-bearing here: /lookup/shows answers 301 to
	// /shows/{id} rather than returning the show inline, so a client that
	// does not follow them reads an empty body as "no such series".
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("tvmaze: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("tvmaze: reading response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("tvmaze: not found (404): %s", path)
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("tvmaze: rate limited (429)")
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("tvmaze: unexpected status %d for %s", resp.StatusCode, path)
	}

	c.mu.Lock()
	c.cache[full] = cacheEntry{body: body, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()

	return json.Unmarshal(body, out)
}

// ---- wire shapes (only the fields we consume) ----

type show struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	Premiered      string   `json:"premiered"`
	Ended          string   `json:"ended"`
	Status         string   `json:"status"`
	Genres         []string `json:"genres"`
	AverageRuntime int      `json:"averageRuntime"`
	Summary        string   `json:"summary"`
	Image          struct {
		Original string `json:"original"`
		Medium   string `json:"medium"`
	} `json:"image"`
	Rating struct {
		Average *float64 `json:"average"`
	} `json:"rating"`
	Externals struct {
		TheTVDB *int64  `json:"thetvdb"`
		IMDB    *string `json:"imdb"`
	} `json:"externals"`
}

type searchHit struct {
	Show show `json:"show"`
}

type episode struct {
	Name    string `json:"name"`
	Season  int    `json:"season"`
	Number  *int   `json:"number"` // null for specials
	Type    string `json:"type"`
	AirDate string `json:"airdate"`
}

// ---- ports.SeriesProvider ----

// SearchSeries implements ports.SeriesProvider.
//
// Results without a TheTVDB id are dropped rather than returned under a
// TVmaze id: the chain's whole contract is that a series can be named to
// another provider, and a record only this provider can identify cannot be
// reconciled with one from TheTVDB later. Dropping them costs a little
// coverage and keeps identity single-valued.
func (c *Client) SearchSeries(ctx context.Context, query string) ([]ports.SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	var hits []searchHit
	if err := c.get(ctx, "/search/shows", url.Values{"q": {query}}, &hits); err != nil {
		return nil, err
	}
	out := make([]ports.SearchResult, 0, len(hits))
	for _, h := range hits {
		if h.Show.Externals.TheTVDB == nil || *h.Show.Externals.TheTVDB == 0 {
			continue
		}
		out = append(out, ports.SearchResult{
			Kind:       domain.KindSeries,
			TVDBID:     *h.Show.Externals.TheTVDB,
			Source:     "tvmaze",
			Title:      h.Show.Name,
			Year:       yearOf(h.Show.Premiered),
			Overview:   plainText(h.Show.Summary),
			PosterPath: poster(h.Show),
		})
	}
	return out, nil
}

// GetSeriesByTVDB implements ports.SeriesProvider.
func (c *Client) GetSeriesByTVDB(ctx context.Context, tvdbID int64) (domain.MediaItem, error) {
	var s show
	// 301 to /shows/{id}; the client follows it.
	if err := c.get(ctx, "/lookup/shows",
		url.Values{"thetvdb": {strconv.FormatInt(tvdbID, 10)}}, &s); err != nil {
		return domain.MediaItem{}, err
	}
	if s.ID == 0 {
		return domain.MediaItem{}, fmt.Errorf("tvmaze: no series for tvdb id %d", tvdbID)
	}

	var eps []episode
	if err := c.get(ctx, fmt.Sprintf("/shows/%d/episodes", s.ID),
		url.Values{"specials": {"1"}}, &eps); err != nil {
		return domain.MediaItem{}, err
	}

	item := domain.MediaItem{
		Kind:      domain.KindSeries,
		Title:     s.Name,
		SortTitle: domain.SortTitle(s.Name),
		Year:      yearOf(s.Premiered),
		IDs: domain.ExternalIDs{
			TVDB: tvdbID,
			IMDB: deref(s.Externals.IMDB),
		},
		Overview:    plainText(s.Summary),
		PosterPath:  poster(s),
		Genres:      s.Genres,
		Status:      s.Status,
		ReleaseDate: s.Premiered,
		Runtime:     s.AverageRuntime,
		Ended:       strings.EqualFold(s.Status, "Ended") || strings.EqualFold(s.Status, "Canceled"),
	}
	if s.Rating.Average != nil && *s.Rating.Average > 0 {
		item.Rating = *s.Rating.Average
		item.Ratings = []domain.Rating{{Source: "tvmaze", Value: *s.Rating.Average, Scale: 10}}
	}
	item.Seasons = seasonsOf(eps)
	return item, nil
}

// seasonsOf turns TVmaze's flat episode list into monarr's seasons.
//
// Specials are the awkward part: TVmaze numbers them `null` and leaves them
// in the season they aired alongside, while the *arr convention — and the
// naming scene releases use — puts specials in season 0. They are moved
// there and numbered in air order, which is what makes a file called
// `S00E01` link to one.
func seasonsOf(eps []episode) []domain.Season {
	bySeason := map[int][]domain.Episode{}
	specials := 0
	for _, e := range eps {
		season, number := e.Season, 0
		if e.Number != nil {
			number = *e.Number
		} else {
			specials++
			season, number = 0, specials
		}
		bySeason[season] = append(bySeason[season], domain.Episode{
			SeasonNumber:  season,
			EpisodeNumber: number,
			Title:         e.Name,
			AirDate:       e.AirDate,
			Monitored:     season != 0, // specials start unmonitored, like upstream
		})
	}

	numbers := make([]int, 0, len(bySeason))
	for n := range bySeason {
		numbers = append(numbers, n)
	}
	sortInts(numbers)

	// Absolute numbering is the cumulative index across regular seasons —
	// the same derivation the TMDB adapter uses, so the two providers agree
	// on what "episode 37" means.
	absolute := 0
	out := make([]domain.Season, 0, len(numbers))
	for _, n := range numbers {
		list := bySeason[n]
		sortEpisodes(list)
		if n != 0 {
			for i := range list {
				absolute++
				list[i].AbsoluteNum = absolute
			}
		}
		out = append(out, domain.Season{Number: n, Monitored: n != 0, Episodes: list})
	}
	return out
}

func sortInts(v []int) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

func sortEpisodes(v []domain.Episode) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j].EpisodeNumber < v[j-1].EpisodeNumber; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}

// poster prefers TVmaze's medium image over the untouched original.
//
// TVmaze serves absolute URLs, so posterUrl in the web client passes them
// through unresized — which means whatever is stored here is what a library
// grid downloads, once per card. The originals are full-resolution scans:
// 254 KB against 20 KB for medium, which is already 210x295 and larger than
// the w185 TMDB variant the grid asks for.
func poster(s show) string {
	if s.Image.Medium != "" {
		return s.Image.Medium
	}
	return s.Image.Original
}

func yearOf(isoDate string) int {
	if len(isoDate) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(isoDate[:4])
	return y
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

var reTag = regexp.MustCompile(`<[^>]*>`)

// plainText strips the HTML TVmaze puts in summaries. Everything downstream
// — the library grid, the item page, the compat personalities — treats
// Overview as text, and rendering it as markup would be the interesting kind
// of bug.
func plainText(s string) string {
	return strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(s, "")))
}
