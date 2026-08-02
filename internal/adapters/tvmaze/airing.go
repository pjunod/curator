package tvmaze

import (
	"context"
	"net/url"
	"strconv"
	"strings"

	"github.com/pjunod/monarr/internal/ports"
)

var _ ports.AiringProvider = (*Client)(nil)

// Airing implements ports.AiringProvider (ADR 0016): the show-level
// broadcast slot, which the calendar composes with each episode's air date
// into a UTC instant at read time.
//
// It reuses the same request path as the rest of the adapter, so it inherits
// the rate limiter and the URL-keyed response cache — a refresh that just
// hydrated a series through GetSeriesByTVDB pays nothing for this.
//
// Three shapes come back from TVmaze and all three are correct answers:
//
//   - a linear broadcaster: network name plus its country's IANA timezone,
//     and a schedule time — a real instant is composable;
//   - a streaming show: webChannel name, no country, and almost always an
//     empty schedule time — the network is worth showing, the time does not
//     exist, and the entry stays date-only;
//   - a show with neither — the zero value, and the calendar says nothing.
//
// Errors are the caller's to ignore; enrichment is best-effort by contract.
func (c *Client) Airing(ctx context.Context, tvdbID int64, imdbID string) (ports.Airing, error) {
	var s show
	switch {
	case tvdbID != 0:
		if err := c.get(ctx, "/lookup/shows",
			url.Values{"thetvdb": {strconv.FormatInt(tvdbID, 10)}}, &s); err != nil {
			return ports.Airing{}, err
		}
	case strings.TrimSpace(imdbID) != "":
		if err := c.get(ctx, "/lookup/shows",
			url.Values{"imdb": {strings.TrimSpace(imdbID)}}, &s); err != nil {
			return ports.Airing{}, err
		}
	default:
		// Neither id known: there is nothing to look up, and a query with an
		// empty key would be a request that can only 404.
		return ports.Airing{}, nil
	}
	if s.ID == 0 {
		return ports.Airing{}, nil
	}

	out := ports.Airing{Time: strings.TrimSpace(s.Schedule.Time)}
	// A network's country is what carries the timezone; a webChannel usually
	// has none, and inventing one would turn "streams whenever" into a
	// confident evening slot in some arbitrary city.
	switch {
	case s.Network != nil:
		out.Network = strings.TrimSpace(s.Network.Name)
		if s.Network.Country != nil {
			out.Timezone = strings.TrimSpace(s.Network.Country.Timezone)
		}
	case s.WebChannel != nil:
		out.Network = strings.TrimSpace(s.WebChannel.Name)
		if s.WebChannel.Country != nil {
			out.Timezone = strings.TrimSpace(s.WebChannel.Country.Timezone)
		}
	}
	// A time with no timezone is not a time — it is a number that would be
	// read in whatever zone the reader happens to sit in. Drop it rather
	// than let it be composed against the wrong clock.
	if out.Timezone == "" {
		out.Time = ""
	}
	return out, nil
}
