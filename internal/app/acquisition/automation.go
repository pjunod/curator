package acquisition

import (
	"context"
	"fmt"
	"sort"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/decision"
	"github.com/monarr-media/monarr/internal/domain/format"
	"github.com/monarr-media/monarr/internal/domain/matcher"
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
	enabled, err := s.enabledIndexers(ctx)
	if err != nil || len(enabled) == 0 {
		return err
	}

	grabbed := map[string]bool{} // wantable id → grabbed this run
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

// searchAndGrabBest runs the wantable's planned queries against the given
// indexers and grabs the single best accepted release, if any.
func (s *Service) searchAndGrabBest(ctx context.Context, w domain.Wantable, enabled []ports.IndexerConfig) error {
	profile, err := s.db.GetProfile(ctx, w.ProfileID())
	if err != nil {
		return err
	}

	formats, _ := s.db.ListCustomFormats(ctx)
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
