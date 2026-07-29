// Package discover serves the browse surface: curated rows of media from
// metadata providers, cached in memory, added to the library through the
// ordinary library service (ADR 0015).
//
// What this package deliberately does not do: store anything, run on a timer,
// or add anything by itself. Every request here originates in a browser that
// is on the Discover page. Import lists (internal/app/importlist) are the
// feature that adds things unattended; this one is for looking.
package discover

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

const (
	// freshFor is how long a fetched row is served without asking upstream
	// again. Trending data moves daily, so a shorter TTL spends requests
	// re-fetching numbers that have not changed; much longer and the "this
	// week" rows are visibly wrong on the day they roll over.
	freshFor = 30 * time.Minute
	// staleFor is how long a row that can no longer be refreshed keeps being
	// served. A Discover page that goes blank because TMDB had a bad thirty
	// seconds is worse than one showing yesterday's trending list — but past
	// a day the row is not "less fresh", it is wrong, and serving it would
	// hide an outage instead of reporting one.
	staleFor = 24 * time.Hour
	// hydrateWorkers bounds the artwork backfill for providers that hand out
	// ids without pictures. Six is comfortably inside the TMDB limiter's
	// burst of ten while leaving room for whatever else the app is doing.
	hydrateWorkers = 6
)

// Hydrator fills in what a provider could not describe — artwork above all —
// for one title, by TMDB id. It is deliberately not ports.MetadataProvider:
// that interface's GetSeries hydrates every season and every episode, which
// for a thirty-item row is a hundred-odd requests to draw a poster.
type Hydrator func(ctx context.Context, kind domain.MediaKind, tmdbID int64) (ports.SearchResult, error)

// Service assembles the Discover catalogue and serves its rows.
type Service struct {
	log       *slog.Logger
	hydrate   Hydrator
	providers []ports.DiscoverProvider
	// owner maps a list id to the provider that publishes it, built once at
	// construction so a request does not rescan every catalogue.
	owner map[string]ports.DiscoverProvider

	mu    sync.Mutex
	cache map[string]entry
}

type entry struct {
	items   []ports.SearchResult
	fetched time.Time
}

// New wires the service. hydrate may be nil, in which case results keep
// whatever artwork their provider supplied.
//
// Two providers publishing the same list id is a programming error, not a
// runtime condition — the ids are compile-time constants in each adapter —
// so the last one registered wins and the collision is logged rather than
// returned.
func New(log *slog.Logger, hydrate Hydrator, providers ...ports.DiscoverProvider) *Service {
	s := &Service{
		log: log, hydrate: hydrate, providers: providers,
		owner: map[string]ports.DiscoverProvider{},
		cache: map[string]entry{},
	}
	for _, p := range providers {
		for _, l := range p.Lists() {
			if prev, dup := s.owner[l.ID]; dup && log != nil {
				log.Warn("discover: duplicate list id", "id", l.ID, "previous", prev.Name(), "winner", p.Name())
			}
			s.owner[l.ID] = p
		}
	}
	return s
}

// Lists is the catalogue: every row from every provider that is configured,
// in provider order. A provider missing its key contributes nothing rather
// than rows that would always fail.
func (s *Service) Lists(ctx context.Context) []ports.DiscoverList {
	out := []ports.DiscoverList{}
	for _, p := range s.providers {
		if !p.Configured(ctx) {
			continue
		}
		out = append(out, p.Lists()...)
	}
	return out
}

// Configured reports whether any provider can serve anything — the
// difference between "Discover has nothing for you" and "Discover needs a
// key", which are different screens.
func (s *Service) Configured(ctx context.Context) bool {
	for _, p := range s.providers {
		if p.Configured(ctx) {
			return true
		}
	}
	return false
}

// Items returns one page of one list. page is 1-based.
func (s *Service) Items(ctx context.Context, listID string, page int) ([]ports.SearchResult, error) {
	p, ok := s.owner[listID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ports.ErrUnknownList, listID)
	}
	if page < 1 {
		page = 1
	}
	key := fmt.Sprintf("%s#%d", listID, page)

	if items, ok := s.cached(key, freshFor); ok {
		return items, nil
	}

	items, err := p.Discover(ctx, listID, page)
	if err != nil {
		// Serve the last good contents if we have any worth serving. The
		// row is less fresh; the alternative is a blank page, which reads
		// as a bug rather than as an outage.
		if stale, ok := s.cached(key, staleFor); ok {
			s.log.Warn("discover: serving stale row", "list", listID, "page", page, "err", err)
			return stale, nil
		}
		return nil, err
	}
	items = s.hydrateAll(ctx, items)

	s.mu.Lock()
	s.cache[key] = entry{items: items, fetched: time.Now()}
	s.mu.Unlock()
	return copyOf(items), nil
}

// cached returns the entry for key when it is younger than maxAge.
func (s *Service) cached(key string, maxAge time.Duration) ([]ports.SearchResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.cache[key]
	if !ok || time.Since(e.fetched) >= maxAge {
		return nil, false
	}
	return copyOf(e.items), true
}

// copyOf hands out a copy so a caller cannot edit what the next caller reads.
func copyOf(in []ports.SearchResult) []ports.SearchResult {
	return append([]ports.SearchResult(nil), in...)
}

// hydrateAll backfills artwork for results that arrived without any. A
// hydration failure costs that card its picture, never the row: an upstream
// that is rate-limiting us should degrade the page, not empty it.
func (s *Service) hydrateAll(ctx context.Context, items []ports.SearchResult) []ports.SearchResult {
	if s.hydrate == nil {
		return items
	}
	todo := make([]int, 0, len(items))
	for i, r := range items {
		if r.PosterPath == "" && r.TMDBID != 0 {
			todo = append(todo, i)
		}
	}
	if len(todo) == 0 {
		return items
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, hydrateWorkers)
	for _, i := range todo {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			full, err := s.hydrate(ctx, items[i].Kind, items[i].TMDBID)
			if err != nil {
				s.log.Debug("discover: hydrate failed", "tmdb", items[i].TMDBID, "err", err)
				return
			}
			items[i].PosterPath = full.PosterPath
			if items[i].Overview == "" {
				items[i].Overview = full.Overview
			}
			if items[i].Year == 0 {
				items[i].Year = full.Year
			}
		}(i)
	}
	wg.Wait()
	return items
}
