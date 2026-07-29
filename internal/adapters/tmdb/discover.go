package tmdb

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

var _ ports.DiscoverProvider = (*Client)(nil)

// discoverList is a catalogue entry plus the upstream path that serves it.
// The two are kept together so a row cannot be advertised without a way to
// fetch it.
type discoverList struct {
	list  ports.DiscoverList
	path  string
	forTV bool // decode the TV response shape (name/first_air_date)
}

// tmdbLists is the whole catalogue. Ordering is the order rows appear on the
// page: the two "what is happening now" rows first, because a browse surface
// that opens on an all-time top-rated list is a museum.
//
// The blurbs say what each row actually measures. "Popular" and "Trending"
// are TMDB's own words for two different numbers — trending counts what
// people looked up this week, popular is a slower blend that includes votes —
// and a row that does not say so is a ranking nobody can interpret.
var tmdbLists = []discoverList{
	{
		list: ports.DiscoverList{
			ID: "tmdb-trending-movies", Title: "Trending this week",
			Blurb: "What people looked up on TMDB over the last seven days.",
			Kind:  domain.KindMovie, Source: "tmdb",
		},
		path: "/trending/movie/week",
	},
	{
		list: ports.DiscoverList{
			ID: "tmdb-trending-series", Title: "Trending this week",
			Blurb: "What people looked up on TMDB over the last seven days.",
			Kind:  domain.KindSeries, Source: "tmdb",
		},
		path: "/trending/tv/week", forTV: true,
	},
	{
		list: ports.DiscoverList{
			ID: "tmdb-now-playing", Title: "In theaters now",
			Blurb: "Released to cinemas recently — mostly not downloadable yet.",
			Kind:  domain.KindMovie, Source: "tmdb",
		},
		path: "/movie/now_playing",
	},
	{
		list: ports.DiscoverList{
			ID: "tmdb-on-the-air", Title: "On the air",
			Blurb: "Series with an episode airing in the next week.",
			Kind:  domain.KindSeries, Source: "tmdb",
		},
		path: "/tv/on_the_air", forTV: true,
	},
	{
		list: ports.DiscoverList{
			ID: "tmdb-upcoming", Title: "Coming soon",
			Blurb: "Announced cinema releases, soonest first. Add now, monitor, forget.",
			Kind:  domain.KindMovie, Source: "tmdb",
		},
		path: "/movie/upcoming",
	},
	{
		list: ports.DiscoverList{
			ID: "tmdb-popular-movies", Title: "Popular movies",
			Blurb: "TMDB's slower blend of views, votes and recency.",
			Kind:  domain.KindMovie, Source: "tmdb",
		},
		path: "/movie/popular",
	},
	{
		list: ports.DiscoverList{
			ID: "tmdb-popular-series", Title: "Popular shows",
			Blurb: "TMDB's slower blend of views, votes and recency.",
			Kind:  domain.KindSeries, Source: "tmdb",
		},
		path: "/tv/popular", forTV: true,
	},
	{
		list: ports.DiscoverList{
			ID: "tmdb-top-movies", Title: "Top rated movies",
			Blurb: "Highest rated of all time, by TMDB vote average.",
			Kind:  domain.KindMovie, Source: "tmdb",
		},
		path: "/movie/top_rated",
	},
	{
		list: ports.DiscoverList{
			ID: "tmdb-top-series", Title: "Top rated shows",
			Blurb: "Highest rated of all time, by TMDB vote average.",
			Kind:  domain.KindSeries, Source: "tmdb",
		},
		path: "/tv/top_rated", forTV: true,
	},
}

// Name implements ports.DiscoverProvider.
func (c *Client) Name() string { return "tmdb" }

// Lists implements ports.DiscoverProvider.
func (c *Client) Lists() []ports.DiscoverList {
	out := make([]ports.DiscoverList, 0, len(tmdbLists))
	for _, l := range tmdbLists {
		out = append(out, l.list)
	}
	return out
}

// Configured implements ports.DiscoverProvider: TMDB needs the key the whole
// library already needs, so on any working install this is true and Discover
// costs the user no setup.
func (c *Client) Configured(ctx context.Context) bool {
	key, err := c.keyFn(ctx)
	return err == nil && key != ""
}

// Discover implements ports.DiscoverProvider.
func (c *Client) Discover(ctx context.Context, listID string, page int) ([]ports.SearchResult, error) {
	for _, l := range tmdbLists {
		if l.list.ID != listID {
			continue
		}
		if l.forTV {
			return c.seriesList(ctx, l.path, page)
		}
		return c.movieList(ctx, l.path, page)
	}
	return nil, fmt.Errorf("tmdb: %w: %s", ports.ErrUnknownList, listID)
}

// pageParams renders a 1-based page number. TMDB rejects page 0 and caps at
// 500; clamping here rather than erroring means a UI that pages too far gets
// a repeat rather than a broken row.
func pageParams(page int) url.Values {
	if page < 1 {
		page = 1
	}
	if page > 500 {
		page = 500
	}
	return url.Values{"page": {strconv.Itoa(page)}}
}

// movieList fetches any endpoint returning TMDB's movie list shape.
func (c *Client) movieList(ctx context.Context, path string, page int) ([]ports.SearchResult, error) {
	var resp searchMovieResp
	if err := c.get(ctx, path, pageParams(page), &resp); err != nil {
		return nil, err
	}
	out := make([]ports.SearchResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, ports.SearchResult{
			Kind: domain.KindMovie, TMDBID: r.ID, Source: "tmdb", Title: r.Title,
			AltTitles: otherThan(r.Title, r.OriginalTitle),
			Year:      yearOf(r.ReleaseDate), Overview: r.Overview, PosterPath: r.PosterPath,
		})
	}
	return out, nil
}

// seriesList fetches any endpoint returning TMDB's TV list shape.
func (c *Client) seriesList(ctx context.Context, path string, page int) ([]ports.SearchResult, error) {
	var resp searchTVResp
	if err := c.get(ctx, path, pageParams(page), &resp); err != nil {
		return nil, err
	}
	out := make([]ports.SearchResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		out = append(out, ports.SearchResult{
			Kind: domain.KindSeries, TMDBID: r.ID, Source: "tmdb", Title: r.Name,
			AltTitles: otherThan(r.Name, r.OriginalName),
			Year:      yearOf(r.FirstAirDate), Overview: r.Overview, PosterPath: r.PosterPath,
		})
	}
	return out, nil
}

// Summary is the shallow half of GetMovie/GetSeries: title, year, overview
// and artwork for one id, in one request.
//
// It exists because Trakt hands out ids without pictures, and hydrating a
// thirty-item series row through GetSeries would fetch every season of every
// show — a hundred-odd requests for a poster (ADR 0015 §3). Nothing here
// needs seasons, so nothing here asks for them.
func (c *Client) Summary(ctx context.Context, kind domain.MediaKind, tmdbID int64) (ports.SearchResult, error) {
	switch kind {
	case domain.KindMovie:
		var resp movieResp
		if err := c.get(ctx, fmt.Sprintf("/movie/%d", tmdbID), nil, &resp); err != nil {
			return ports.SearchResult{}, err
		}
		return ports.SearchResult{
			Kind: domain.KindMovie, TMDBID: resp.ID, Source: "tmdb", Title: resp.Title,
			Year: yearOf(resp.ReleaseDate), Overview: resp.Overview, PosterPath: resp.PosterPath,
		}, nil
	case domain.KindSeries:
		var resp tvResp
		if err := c.get(ctx, fmt.Sprintf("/tv/%d", tmdbID), nil, &resp); err != nil {
			return ports.SearchResult{}, err
		}
		return ports.SearchResult{
			Kind: domain.KindSeries, TMDBID: resp.ID, Source: "tmdb", Title: resp.Name,
			Year: yearOf(resp.FirstAirDate), Overview: resp.Overview, PosterPath: resp.PosterPath,
		}, nil
	default:
		return ports.SearchResult{}, fmt.Errorf("tmdb: no summary for kind %q", kind)
	}
}
