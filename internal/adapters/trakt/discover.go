package trakt

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

var _ ports.DiscoverProvider = (*Client)(nil)

// pageSize is how many entries a Discover row asks for. Trakt defaults to 10,
// which is half a screen of posters.
const pageSize = 30

type discoverList struct {
	list ports.DiscoverList
	path string
	// paged is false for endpoints Trakt serves as a fixed top-N. Asking
	// those for page 2 returns page 1 again, so we return nothing instead —
	// a row that ends is better than one that silently repeats.
	paged bool
}

// traktLists is the whole Trakt catalogue. These five rows exist because TMDB
// cannot answer them: Trakt's trending is scrobbles — what is actually being
// played right now — where TMDB's is lookups, and neither "anticipated" nor
// "box office" has a TMDB equivalent at all (ADR 0015 §2).
//
// Every endpoint here wraps its entity in an envelope with a counter
// (`watchers`, `list_count`, `revenue`). /movies/popular does not — it
// returns bare movie objects — which is why it is absent rather than
// forgotten.
var traktLists = []discoverList{
	{
		list: ports.DiscoverList{
			ID: "trakt-trending-movies", Title: "Being watched right now",
			Blurb: "Ranked by how many Trakt users have this playing this minute.",
			Kind:  domain.KindMovie, Source: "trakt",
		},
		path: "/movies/trending", paged: true,
	},
	{
		list: ports.DiscoverList{
			ID: "trakt-trending-series", Title: "Being watched right now",
			Blurb: "Ranked by how many Trakt users have this playing this minute.",
			Kind:  domain.KindSeries, Source: "trakt",
		},
		path: "/shows/trending", paged: true,
	},
	{
		list: ports.DiscoverList{
			ID: "trakt-anticipated-movies", Title: "Most anticipated",
			Blurb: "Unreleased, ranked by how many watchlists they sit on.",
			Kind:  domain.KindMovie, Source: "trakt",
		},
		path: "/movies/anticipated", paged: true,
	},
	{
		list: ports.DiscoverList{
			ID: "trakt-anticipated-series", Title: "Most anticipated",
			Blurb: "Unreleased, ranked by how many watchlists they sit on.",
			Kind:  domain.KindSeries, Source: "trakt",
		},
		path: "/shows/anticipated", paged: true,
	},
	{
		list: ports.DiscoverList{
			ID: "trakt-boxoffice", Title: "Box office",
			Blurb: "Last weekend's top ten US grosses.",
			Kind:  domain.KindMovie, Source: "trakt",
		},
		path: "/movies/boxoffice",
	},
}

// Name implements ports.DiscoverProvider.
func (c *Client) Name() string { return "trakt" }

// Lists implements ports.DiscoverProvider.
func (c *Client) Lists() []ports.DiscoverList {
	out := make([]ports.DiscoverList, 0, len(traktLists))
	for _, l := range traktLists {
		out = append(out, l.list)
	}
	return out
}

// Configured implements ports.DiscoverProvider. Trakt is optional: with no
// client id these five rows are absent from the catalogue and the page never
// mentions them.
func (c *Client) Configured(ctx context.Context) bool {
	id, err := c.keyFn(ctx)
	return err == nil && id != ""
}

// Discover implements ports.DiscoverProvider.
func (c *Client) Discover(ctx context.Context, listID string, page int) ([]ports.SearchResult, error) {
	for _, l := range traktLists {
		if l.list.ID != listID {
			continue
		}
		if page < 1 {
			page = 1
		}
		if !l.paged && page > 1 {
			return nil, nil
		}
		params := url.Values{
			"limit": {strconv.Itoa(pageSize)},
			// extended=full is what carries the overview. Without it a card
			// has a title and a year and nothing to read.
			"extended": {"full"},
		}
		if l.paged {
			params.Set("page", strconv.Itoa(page))
		}
		var items []envelope
		if err := c.get(ctx, l.path, params, &items); err != nil {
			return nil, err
		}
		return results(items), nil
	}
	return nil, fmt.Errorf("trakt: %w: %s", ports.ErrUnknownList, listID)
}
