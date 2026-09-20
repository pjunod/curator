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

	"github.com/pjunod/monarr/internal/adapters/httpx"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
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

var (
	_ ports.MetadataProvider         = (*Client)(nil)
	_ ports.AltTitleProvider         = (*Client)(nil)
	_ ports.ExternalLookupProvider   = (*Client)(nil)
	_ ports.IdentityMetadataProvider = (*Client)(nil)
)

// New returns a Client. baseURL "" means DefaultBaseURL.
func New(baseURL string, keyFn KeyFunc) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		keyFn:   keyFn,
		http:    httpx.NewClient(15 * time.Second),
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
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &ports.RemoteError{Category: ports.RemoteTransport, Cause: fmt.Errorf("tmdb request failed")}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("tmdb: reading response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return &ports.RemoteError{Category: ports.RemoteAuth, HTTPStatus: resp.StatusCode}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &ports.RemoteError{Category: ports.RemoteRateLimit, HTTPStatus: resp.StatusCode, RetryAt: tmdbRetryAt(resp.Header.Get("Retry-After"))}
	case resp.StatusCode == http.StatusNotFound:
		return &ports.RemoteError{Category: ports.RemoteNotFound, HTTPStatus: resp.StatusCode}
	case resp.StatusCode != http.StatusOK:
		return &ports.RemoteError{Category: ports.RemoteInvalidResponse, HTTPStatus: resp.StatusCode}
	}

	c.mu.Lock()
	c.cache[cacheKey] = cacheEntry{body: body, expires: time.Now().Add(c.ttl)}
	c.mu.Unlock()

	if err := json.Unmarshal(body, out); err != nil {
		return &ports.RemoteError{Category: ports.RemoteInvalidResponse, HTTPStatus: resp.StatusCode, Cause: err}
	}
	return nil
}

func tmdbRetryAt(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Now().Add(time.Duration(seconds) * time.Second)
	}
	parsed, _ := http.ParseTime(value)
	return parsed
}

// ---- wire shapes (only the fields we consume) ----

type searchMovieResp struct {
	Results []struct {
		ID            int64  `json:"id"`
		Title         string `json:"title"`
		OriginalTitle string `json:"original_title"`
		ReleaseDate   string `json:"release_date"`
		Overview      string `json:"overview"`
		PosterPath    string `json:"poster_path"`
	} `json:"results"`
}

type searchTVResp struct {
	Results []struct {
		ID           int64  `json:"id"`
		Name         string `json:"name"`
		OriginalName string `json:"original_name"`
		FirstAirDate string `json:"first_air_date"`
		Overview     string `json:"overview"`
		PosterPath   string `json:"poster_path"`
	} `json:"results"`
}

// altTitlesResp covers both alternative-titles endpoints: movies return the
// list under "titles", TV under "results", and nothing else differs.
type altTitlesResp struct {
	Titles []struct {
		Title   string `json:"title"`
		Country string `json:"iso_3166_1"`
		Type    string `json:"type"`
	} `json:"titles"`
	Results []struct {
		Title   string `json:"title"`
		Country string `json:"iso_3166_1"`
		Type    string `json:"type"`
	} `json:"results"`
}

type genre struct {
	Name string `json:"name"`
}

type movieResp struct {
	ID            int64    `json:"id"`
	Title         string   `json:"title"`
	OriginalTitle string   `json:"original_title"`
	OriginCountry []string `json:"origin_country"`
	Overview      string   `json:"overview"`
	ReleaseDate   string   `json:"release_date"`
	Runtime       int      `json:"runtime"`
	Status        string   `json:"status"`
	PosterPath    string   `json:"poster_path"`
	BackdropPath  string   `json:"backdrop_path"`
	Genres        []genre  `json:"genres"`
	IMDBID        string   `json:"imdb_id"`
	VoteAverage   float64  `json:"vote_average"`
	VoteCount     int      `json:"vote_count"`
}

type tvResp struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	OriginalName   string   `json:"original_name"`
	OriginCountry  []string `json:"origin_country"`
	Overview       string   `json:"overview"`
	FirstAirDate   string   `json:"first_air_date"`
	Status         string   `json:"status"`
	PosterPath     string   `json:"poster_path"`
	BackdropPath   string   `json:"backdrop_path"`
	Genres         []genre  `json:"genres"`
	EpisodeRunTime []int    `json:"episode_run_time"`
	VoteAverage    float64  `json:"vote_average"`
	VoteCount      int      `json:"vote_count"`
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
			Kind: domain.KindMovie, TMDBID: r.ID, Title: r.Title, Source: "tmdb", HydrationSource: "tmdb",
			AltTitles: otherThan(r.Title, r.OriginalTitle),
			Year:      yearOf(r.ReleaseDate), Overview: r.Overview, PosterPath: r.PosterPath,
		})
	}
	return out, nil
}

// otherThan returns alt as a one-element slice when it says something the
// primary title does not. The original-language title rides along in every
// search response, so it is the one alternate name that costs nothing.
func otherThan(primary, alt string) []string {
	if alt == "" || alt == primary {
		return nil
	}
	return []string{alt}
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
			Kind: domain.KindSeries, TMDBID: r.ID, Title: r.Name, Source: "tmdb", HydrationSource: "tmdb",
			AltTitles: otherThan(r.Name, r.OriginalName),
			Year:      yearOf(r.FirstAirDate), Overview: r.Overview, PosterPath: r.PosterPath,
		})
	}
	return out, nil
}

// AlternativeTitles implements ports.AltTitleProvider.
//
// TMDB keeps these on a separate endpoint, one request per title, which is
// why adoption asks for them only after a plain title comparison has already
// failed.
func (c *Client) AlternativeTitles(ctx context.Context, kind domain.MediaKind, tmdbID int64) ([]string, error) {
	var path string
	switch kind {
	case domain.KindMovie:
		path = fmt.Sprintf("/movie/%d/alternative_titles", tmdbID)
	case domain.KindSeries:
		path = fmt.Sprintf("/tv/%d/alternative_titles", tmdbID)
	default:
		return nil, fmt.Errorf("tmdb: no alternative titles for kind %q", kind)
	}
	var resp altTitlesResp
	if err := c.get(ctx, path, nil, &resp); err != nil {
		return nil, err
	}
	rows := resp.Titles
	if len(rows) == 0 {
		rows = resp.Results
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Title != "" {
			out = append(out, r.Title)
		}
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
		Source:       "tmdb",
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
		Source:       "tmdb",
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

// LookupExternal implements exact TMDB/TVDB/IMDb resolution. Result
// collections are filtered by the requested kind; episode/person results are
// never promoted to works.
func (c *Client) LookupExternal(ctx context.Context, kind domain.MediaKind, ref domain.ExternalRef) ([]ports.SearchResult, error) {
	item, err := c.PreviewExternal(ctx, kind, ref)
	if err != nil {
		return nil, err
	}
	return []ports.SearchResult{searchResultOf(item, "tmdb")}, nil
}

// PreviewExternal shares exact-ID validation with search, without episode I/O.
func (c *Client) PreviewExternal(ctx context.Context, kind domain.MediaKind, ref domain.ExternalRef) (domain.MediaItem, error) {
	if kind != domain.KindMovie && kind != domain.KindSeries {
		return domain.MediaItem{}, &ports.RemoteError{Category: ports.RemoteUnsupportedQuery}
	}
	if ref.Provider == "tmdb" {
		id, err := strconv.ParseInt(ref.Value, 10, 64)
		if err != nil || id <= 0 {
			return domain.MediaItem{}, &ports.RemoteError{Category: ports.RemoteUnsupportedQuery, Cause: err}
		}
		if kind == domain.KindMovie {
			item, err := c.GetMovie(ctx, id)
			if err != nil {
				return domain.MediaItem{}, err
			}
			if item.IDs.TMDB != id {
				return domain.MediaItem{}, &ports.RemoteError{Category: ports.RemoteIdentityConflict, ExpectedIDs: domain.ExternalIDs{TMDB: id}, ActualIDs: item.IDs}
			}
			item.Source = "tmdb"
			return item, nil
		}
		item, err := c.getSeriesRecord(ctx, id)
		if err != nil {
			return domain.MediaItem{}, err
		}
		if item.IDs.TMDB != id {
			return domain.MediaItem{}, &ports.RemoteError{Category: ports.RemoteIdentityConflict, ExpectedIDs: domain.ExternalIDs{TMDB: id}, ActualIDs: item.IDs}
		}
		item.Source = "tmdb"
		return item, nil
	}
	if ref.Provider != "tvdb" && ref.Provider != "imdb" {
		return domain.MediaItem{}, &ports.RemoteError{Category: ports.RemoteUnsupportedQuery}
	}
	if ref.Provider == "tvdb" && kind != domain.KindSeries {
		return domain.MediaItem{}, &ports.RemoteError{Category: ports.RemoteUnsupportedQuery}
	}
	var resp struct {
		MovieResults []struct {
			ID int64 `json:"id"`
		} `json:"movie_results"`
		TVResults []struct {
			ID int64 `json:"id"`
		} `json:"tv_results"`
	}
	externalSource := ref.Provider + "_id"
	if err := c.get(ctx, "/find/"+ref.Value, url.Values{"external_source": {externalSource}}, &resp); err != nil {
		return domain.MediaItem{}, err
	}
	var ids []int64
	if kind == domain.KindMovie {
		for _, r := range resp.MovieResults {
			ids = append(ids, r.ID)
		}
	} else {
		for _, r := range resp.TVResults {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return domain.MediaItem{}, &ports.RemoteError{Category: ports.RemoteNotFound, HTTPStatus: http.StatusNotFound}
	}
	if len(ids) > 1 {
		return domain.MediaItem{}, &ports.RemoteError{Category: ports.RemoteIdentityConflict, ExpectedIDs: expectedTMDBIDs(ref)}
	}
	var item domain.MediaItem
	var err error
	if kind == domain.KindMovie {
		item, err = c.GetMovie(ctx, ids[0])
	} else {
		item, err = c.getSeriesRecord(ctx, ids[0])
	}
	if err != nil {
		return domain.MediaItem{}, err
	}
	if tmdbExternalConflict(ref, item.IDs) {
		return domain.MediaItem{}, &ports.RemoteError{Category: ports.RemoteIdentityConflict, ExpectedIDs: expectedTMDBIDs(ref), ActualIDs: item.IDs}
	}
	item.Source = "tmdb"
	return item, nil
}

func (c *Client) getSeriesRecord(ctx context.Context, id int64) (domain.MediaItem, error) {
	var resp tvResp
	if err := c.get(ctx, fmt.Sprintf("/tv/%d", id), url.Values{"append_to_response": {"external_ids"}}, &resp); err != nil {
		return domain.MediaItem{}, err
	}
	runtime := 0
	if len(resp.EpisodeRunTime) > 0 {
		runtime = resp.EpisodeRunTime[0]
	}
	return domain.MediaItem{Kind: domain.KindSeries, Title: resp.Name, SortTitle: domain.SortTitle(resp.Name), Year: yearOf(resp.FirstAirDate), IDs: domain.ExternalIDs{TMDB: resp.ID, IMDB: resp.ExternalIDs.IMDBID, TVDB: resp.ExternalIDs.TVDBID}, Overview: resp.Overview, PosterPath: resp.PosterPath, Source: "tmdb", Genres: genreNames(resp.Genres), Status: resp.Status, Runtime: runtime}, nil
}

func searchResultOf(item domain.MediaItem, source string) ports.SearchResult {
	return ports.SearchResult{Kind: item.Kind, TMDBID: item.IDs.TMDB, TVDBID: item.IDs.TVDB, IMDBID: item.IDs.IMDB, Source: source, HydrationSource: "tmdb", Title: item.Title, Year: item.Year, Overview: item.Overview, PosterPath: item.PosterPath}
}
func expectedTMDBIDs(ref domain.ExternalRef) domain.ExternalIDs {
	if ref.Provider == "imdb" {
		return domain.ExternalIDs{IMDB: ref.Value}
	}
	if ref.Provider == "tvdb" {
		v, _ := strconv.ParseInt(ref.Value, 10, 64)
		return domain.ExternalIDs{TVDB: v}
	}
	return domain.ExternalIDs{}
}
func tmdbExternalConflict(ref domain.ExternalRef, ids domain.ExternalIDs) bool {
	switch ref.Provider {
	case "imdb":
		return ids.IMDB != "" && !strings.EqualFold(ids.IMDB, ref.Value)
	case "tvdb":
		return ids.TVDB != 0 && strconv.FormatInt(ids.TVDB, 10) != ref.Value
	}
	return false
}

// IdentityMetadata returns the canonical/original title and alternative-title
// endpoint as one complete provider snapshot.
func (c *Client) IdentityMetadata(ctx context.Context, kind domain.MediaKind, ids domain.ExternalIDs) (ports.IdentityMetadata, error) {
	if ids.TMDB == 0 {
		return ports.IdentityMetadata{}, &ports.RemoteError{Category: ports.RemoteUnsupportedQuery}
	}
	var canonical, original string
	var origins []string
	switch kind {
	case domain.KindMovie:
		var r movieResp
		if err := c.get(ctx, fmt.Sprintf("/movie/%d", ids.TMDB), nil, &r); err != nil {
			return ports.IdentityMetadata{}, err
		}
		canonical, original, origins = r.Title, r.OriginalTitle, r.OriginCountry
		actual := domain.ExternalIDs{TMDB: r.ID, IMDB: r.IMDBID}
		if identityIDsConflict(ids, actual) {
			return ports.IdentityMetadata{}, &ports.RemoteError{Category: ports.RemoteIdentityConflict, ExpectedIDs: ids, ActualIDs: actual}
		}
	case domain.KindSeries:
		var r tvResp
		if err := c.get(ctx, fmt.Sprintf("/tv/%d", ids.TMDB), url.Values{"append_to_response": {"external_ids"}}, &r); err != nil {
			return ports.IdentityMetadata{}, err
		}
		canonical, original, origins = r.Name, r.OriginalName, r.OriginCountry
		actual := domain.ExternalIDs{TMDB: r.ID, IMDB: r.ExternalIDs.IMDBID, TVDB: r.ExternalIDs.TVDBID}
		if identityIDsConflict(ids, actual) {
			return ports.IdentityMetadata{}, &ports.RemoteError{Category: ports.RemoteIdentityConflict, ExpectedIDs: ids, ActualIDs: actual}
		}
	default:
		return ports.IdentityMetadata{}, &ports.RemoteError{Category: ports.RemoteUnsupportedQuery}
	}
	var resp altTitlesResp
	path := fmt.Sprintf("/%s/%d/alternative_titles", map[domain.MediaKind]string{domain.KindMovie: "movie", domain.KindSeries: "tv"}[kind], ids.TMDB)
	if err := c.get(ctx, path, nil, &resp); err != nil {
		return ports.IdentityMetadata{}, err
	}
	out := ports.IdentityMetadata{}
	if original != "" && original != canonical {
		out.Aliases = append(out.Aliases, domain.TitleAlias{Title: original, Source: "tmdb", SourceID: strconv.FormatInt(ids.TMDB, 10), Scope: "work", Role: "original", Searchable: true})
	}
	rows := resp.Titles
	if len(rows) == 0 {
		rows = resp.Results
	}
	for _, r := range rows {
		if strings.TrimSpace(r.Title) == "" {
			continue
		}
		scope := "work"
		if kind == domain.KindSeries && ids.TMDB == 79063 && strings.EqualFold(strings.TrimSpace(r.Title), "Cunk on Earth") {
			scope = "unsupported_numbering"
		}
		out.Aliases = append(out.Aliases, domain.TitleAlias{Title: r.Title, Source: "tmdb", SourceID: strconv.FormatInt(ids.TMDB, 10), MarketCountry: strings.ToUpper(r.Country), Scope: scope, Role: "alternate"})
	}
	for _, code := range origins {
		if code != "" {
			out.Countries = append(out.Countries, domain.CountryEvidence{Code: strings.ToUpper(code), Source: "tmdb", Basis: "origin"})
		}
	}
	return out, nil
}

func identityIDsConflict(expected, actual domain.ExternalIDs) bool {
	return expected.TMDB != 0 && actual.TMDB != 0 && expected.TMDB != actual.TMDB || expected.TVDB != 0 && actual.TVDB != 0 && expected.TVDB != actual.TVDB || expected.IMDB != "" && actual.IMDB != "" && !strings.EqualFold(expected.IMDB, actual.IMDB)
}

// DiscoverMovies serves the import lists (Phase 5): kind is "popular" or
// "top_rated"; results carry TMDB ids ready for library adds.
//
// The Discover browse surface (ADR 0015) reaches the same endpoints through
// ports.DiscoverProvider, so this shares its fetch path — but keeps its own
// signature, because import lists are a different feature with a different
// vocabulary and changing it would move a stored `import_lists.type` value.
func (c *Client) DiscoverMovies(ctx context.Context, kind string) ([]ports.SearchResult, error) {
	if kind != "popular" && kind != "top_rated" {
		return nil, fmt.Errorf("tmdb: unknown discover kind %q", kind)
	}
	return c.movieList(ctx, "/movie/"+kind, 1)
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
