package acquisition

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/decision"
	"github.com/monarr-media/monarr/internal/domain/format"
	"github.com/monarr-media/monarr/internal/domain/matcher"
	"github.com/monarr-media/monarr/internal/domain/mediainfo"
	"github.com/monarr-media/monarr/internal/domain/parser"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/ports"
)

// backlogPerRun bounds how many wantables one backlog pass searches, so a
// large library doesn't hammer indexers in a single run.
const backlogPerRun = 20

func (s *Service) enabledIndexers(ctx context.Context) ([]ports.IndexerConfig, error) {
	indexers, err := s.db.ListIndexers(ctx)
	if err != nil {
		return nil, err
	}
	var enabled []ports.IndexerConfig
	for _, ic := range indexers {
		if ic.Enabled {
			enabled = append(enabled, ic)
		}
	}
	return enabled, nil
}

// sizeImplausible reports whether a release's advertised size can hold what
// its name claims, and why not.
//
// Automation gets the veto; interactive search only gets the warning. Somebody
// looking at a list can see "500 MB" next to "Remux 2160p" and decide for
// themselves — maybe the tracker's size field is wrong, maybe they know
// something monarr does not. An unattended loop at 3am cannot, and the cost of
// it guessing wrong is a fake file that marks the item satisfied and ends the
// search. That asymmetry runs through the whole decision engine already: gate
// the robot, never the person.
func sizeImplausible(claimed quality.Quality, r ports.Release, runtimeMin int) (decision.Rejection, bool) {
	bad, ok := mediainfo.SizeImplausible(claimed, r.Size, runtimeMin)
	if !ok {
		return decision.Rejection{}, false
	}
	return decision.Rejection{Code: decision.CodeSizeImplausible, Reason: bad.Reason}, true
}

// runtimeMemo caches item runtimes for one automation pass. An RSS sweep walks
// every release from every indexer against every wantable, so without this the
// same handful of items get their runtime read hundreds of times per run.
type runtimeMemo map[int64]int

func (m runtimeMemo) of(ctx context.Context, s *Service, w domain.Wantable) int {
	id := w.MediaItemID()
	if v, ok := m[id]; ok {
		return v
	}
	v := s.db.ItemRuntime(ctx, id)
	m[id] = v
	return v
}

// autoGrab sends an accepted release to a client on behalf of a wantable —
// the unattended twin of the interactive grab.
func (s *Service) autoGrab(ctx context.Context, w domain.Wantable, r ports.Release) error {
	season, episode := wantableGrabTarget(w)
	_, err := s.Grab(ctx, GrabRequest{
		MediaItemID: w.MediaItemID(), CopyID: domain.WantableCopy(w),
		Season: season, Episode: episode,
		Title: r.Title, DownloadURL: r.DownloadURL, Indexer: r.Indexer,
		Protocol: r.Protocol, Size: r.Size,
	})
	return err
}

// SyncRSS is the Phase 3 automation heart: pull each enabled indexer's
// recent releases, match them against the wanted index, and grab everything
// the decision engine accepts (blueprint §5.1 "RSS sync loop").
func (s *Service) SyncRSS(ctx context.Context) error {
	wanted, err := s.Wanted(ctx)
	if err != nil {
		return err
	}
	wanted = s.notInFlight(ctx, wanted)
	if len(wanted) == 0 {
		return nil
	}
	wanted = s.watchedFirst(ctx, wanted)
	enabled, err := s.enabledIndexers(ctx)
	if err != nil || len(enabled) == 0 {
		return err
	}

	grabbed := map[string]bool{} // wantable id → grabbed this run
	runtimes := runtimeMemo{}
	for _, cfg := range enabled {
		cctx, cancel := context.WithTimeout(ctx, s.searchTimeout)
		releases, err := s.newIndexer(cfg).FetchRSS(cctx)
		cancel()
		if err != nil {
			s.log.Warn("rss: fetch failed", "indexer", cfg.Name, "err", err)
			continue
		}
		for _, r := range releases {
			if s.isBlocklisted(ctx, r.Title, r.Indexer) {
				continue
			}
			p := parser.Parse(r.Title)
			for _, m := range matcher.Match(p, wanted) {
				w := m.Wantable
				if grabbed[string(w.ID())] {
					continue
				}
				profile, err := s.db.GetProfile(ctx, w.ProfileID())
				if err != nil {
					continue
				}
				if d := decision.Decide(p.Quality, w, profile); !d.Accepted {
					continue
				}
				if why, bad := sizeImplausible(p.Quality, r, runtimes.of(ctx, s, w)); bad {
					s.log.Info("rss: declined on size", "release", r.Title, "why", why.Reason)
					continue
				}
				if err := s.autoGrab(ctx, w, r); err != nil {
					s.log.Warn("rss: grab failed", "release", r.Title, "err", err)
					continue
				}
				grabbed[string(w.ID())] = true
				s.log.Info("rss: grabbed", "release", r.Title, "wantable", w.ID())
			}
		}
	}
	if len(grabbed) > 0 {
		s.InvalidateWanted()
	}
	return nil
}

// BacklogSearch actively searches indexers for wanted items — the catch-up
// pass for things RSS already scrolled past (blueprint §5.1).
func (s *Service) BacklogSearch(ctx context.Context) error {
	wanted, err := s.Wanted(ctx)
	if err != nil {
		return err
	}
	wanted = s.notInFlight(ctx, wanted)
	if len(wanted) == 0 {
		return nil
	}
	wanted = s.watchedFirst(ctx, wanted)
	enabled, err := s.enabledIndexers(ctx)
	if err != nil || len(enabled) == 0 {
		return err
	}

	searched := 0
	for _, w := range wanted {
		if searched >= backlogPerRun {
			s.log.Info("backlog: per-run cap reached", "cap", backlogPerRun)
			break
		}
		searched++
		if err := s.searchAndGrabBest(ctx, w, enabled); err != nil {
			s.log.Warn("backlog: search failed", "wantable", w.ID(), "err", err)
		}
	}
	if searched > 0 {
		s.InvalidateWanted()
	}
	return nil
}

// activeWindow is how recently something must have been watched to count as
// "being watched". A month covers the gap between seasons of a show somebody
// is following without letting a series they finished last spring outrank
// one they started on Tuesday.
const activeWindow = 30 * 24 * time.Hour

// watchedFirst reorders the backlog so things somebody is actually watching
// are searched first (master plan §11.1).
//
// This matters because of the per-run cap: what gets searched first is, on a
// large backlog, what gets searched at all this run. Being three episodes
// into a series and waiting a week for the upgrade — while a film nobody has
// opened in two years is retried nightly — is the case this exists for.
//
// A stable partition rather than a score. The signal is "somebody watched
// this recently", and turning a handful of timestamps into a numeric
// intensity would be precision nobody asked for and nobody could check.
// Without plurx paired the map is empty and the order is exactly what it was.
func (s *Service) watchedFirst(ctx context.Context, wanted []domain.Wantable) []domain.Wantable {
	active, err := s.db.ActivelyWatched(ctx, time.Now().Add(-activeWindow))
	if err != nil {
		// Ordering is an optimization; failing to read it must not stop the
		// backlog from running at all.
		s.log.Debug("backlog: cannot read watch signals", "err", err)
		return wanted
	}
	if len(active) == 0 {
		return wanted
	}
	out := make([]domain.Wantable, 0, len(wanted))
	var rest []domain.Wantable
	for _, w := range wanted {
		if _, ok := active[w.MediaItemID()]; ok {
			out = append(out, w)
		} else {
			rest = append(rest, w)
		}
	}
	if len(out) > 0 {
		s.log.Info("backlog: prioritizing what is being watched",
			"active", len(out), "total", len(wanted))
	}
	return append(out, rest...)
}

// searchAndGrabBest runs the wantable's planned queries against the given
// indexers and grabs the single best accepted release, if any.
func (s *Service) searchAndGrabBest(ctx context.Context, w domain.Wantable, enabled []ports.IndexerConfig) error {
	profile, err := s.db.GetProfile(ctx, w.ProfileID())
	if err != nil {
		return err
	}

	formats, _ := s.db.ListCustomFormats(ctx)
	runtime := s.db.ItemRuntime(ctx, w.MediaItemID())
	type scored struct {
		r     ports.Release
		q     quality.Quality
		score int
	}
	better := func(a, b *scored) bool { // is a strictly better than b
		if quality.Rank(a.q) != quality.Rank(b.q) {
			return quality.Rank(a.q) > quality.Rank(b.q)
		}
		if a.score != b.score {
			return a.score > b.score
		}
		return a.r.Seeders > b.r.Seeders
	}
	var best *scored
	for _, cfg := range enabled {
		for _, q := range domain.PlanSearch(w) {
			cctx, cancel := context.WithTimeout(ctx, s.searchTimeout)
			releases, err := s.newIndexer(cfg).Search(cctx, q)
			cancel()
			if err != nil {
				s.log.Warn("backlog: indexer failed", "indexer", cfg.Name, "err", err)
				continue
			}
			for _, r := range releases {
				if s.isBlocklisted(ctx, r.Title, r.Indexer) {
					continue
				}
				p := parser.Parse(r.Title)
				if len(matcher.Match(p, []domain.Wantable{w})) == 0 {
					continue
				}
				if d := decision.Decide(p.Quality, w, profile); !d.Accepted {
					continue
				}
				if why, bad := sizeImplausible(p.Quality, r, runtime); bad {
					s.log.Info("backlog: declined on size", "release", r.Title, "why", why.Reason)
					continue
				}
				cand := &scored{r: r, q: p.Quality, score: format.Score(r.Title, formats)}
				if best == nil || better(cand, best) {
					best = cand
				}
			}
		}
	}
	if best == nil {
		return nil
	}
	if err := s.autoGrab(ctx, w, best.r); err != nil {
		return fmt.Errorf("grab %q: %w", best.r.Title, err)
	}
	s.log.Info("backlog: grabbed", "release", best.r.Title, "wantable", w.ID())
	return nil
}

// WantedSummary is one wanted entry for the API/UI.
type WantedSummary struct {
	WantableID  string `json:"wantableId"`
	MediaItemID int64  `json:"mediaItemId"`
	Title       string `json:"title"`
	Detail      string `json:"detail"`  // "S01E03", "by Author", "(2024)"
	Missing     bool   `json:"missing"` // false = cutoff unmet (upgrade wanted)
	Current     string `json:"current"` // current quality display, "" if missing
	Copy        string `json:"copy"`    // media-copy label; "" = the primary
}

// WantedList renders the wanted index for the API, stably ordered.
func (s *Service) WantedList(ctx context.Context) ([]WantedSummary, error) {
	wanted, err := s.Wanted(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]WantedSummary, 0, len(wanted))
	for _, w := range wanted {
		ws := WantedSummary{
			WantableID: string(w.ID()), MediaItemID: w.MediaItemID(), Missing: true,
			Copy: domain.WantableCopyName(w),
		}
		if q, ok := w.CurrentQuality(); ok {
			ws.Missing, ws.Current = false, q.Display()
		}
		switch t := w.(type) {
		case domain.MovieWantable:
			ws.Title, ws.Detail = t.Title, fmt.Sprintf("(%d)", t.Year)
		case domain.EpisodeWantable:
			ws.Title, ws.Detail = t.Title, fmt.Sprintf("S%02dE%02d", t.Season, t.Episode)
		case domain.BookWantable:
			ws.Title = t.Title
			if t.Author != "" {
				ws.Detail = "by " + t.Author
			}
		}
		out = append(out, ws)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Title != out[j].Title {
			return out[i].Title < out[j].Title
		}
		return out[i].WantableID < out[j].WantableID
	})
	return out, nil
}

// isBlocklisted reports whether this (release, indexer) pair failed before.
func (s *Service) isBlocklisted(ctx context.Context, title, indexer string) bool {
	blocked, err := s.db.IsBlocklisted(ctx, title, indexer)
	if err != nil {
		return false
	}
	return blocked
}

// AutoSearchItem searches for everything one item still wants and grabs
// the best accepted release per wantable — Sonarr's "search on add" /
// "automatic search" semantics: no candidate list, the decision engine
// picks. Series search season packs (missing episodes fall to the RSS and
// backlog loops if no pack exists).
func (s *Service) AutoSearchItem(ctx context.Context, itemID int64) error {
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return err
	}
	enabled, err := s.enabledIndexers(ctx)
	if err != nil {
		return err
	}
	if len(enabled) == 0 {
		s.log.Info("auto search: no enabled indexers yet", "item", item.Title)
		return ErrNoIndexers
	}

	var targets []domain.Wantable
	switch item.Kind {
	case domain.KindMovie, domain.KindBook:
		w, err := s.target(ctx, item, 0, 0)
		if err != nil {
			return err
		}
		targets = append(targets, w)
	case domain.KindSeries:
		for _, season := range item.Seasons {
			if season.Number == 0 || !season.Monitored || len(season.Episodes) == 0 {
				continue
			}
			w, err := s.target(ctx, item, season.Number, 0)
			if err != nil {
				continue
			}
			targets = append(targets, w)
		}
	}
	// Each monitored copy is its own automation target.
	for i := range item.Copies {
		cp := item.Copies[i]
		if !cp.Monitored || item.Kind == domain.KindBook {
			continue
		}
		switch item.Kind {
		case domain.KindMovie:
			if w, err := s.targetCopy(ctx, item, 0, 0, &cp); err == nil {
				targets = append(targets, w)
			}
		case domain.KindSeries:
			for _, season := range item.Seasons {
				if season.Number == 0 || !season.Monitored || len(season.Episodes) == 0 {
					continue
				}
				if w, err := s.targetCopy(ctx, item, season.Number, 0, &cp); err == nil {
					targets = append(targets, w)
				}
			}
		}
	}

	for _, w := range s.notInFlight(ctx, targets) {
		if !w.Monitored() {
			continue
		}
		if err := s.searchAndGrabBest(ctx, w, enabled); err != nil {
			s.log.Warn("auto search: failed", "wantable", w.ID(), "err", err)
		}
	}
	s.InvalidateWanted()
	return nil
}
