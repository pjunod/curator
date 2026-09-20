package acquisition

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/decision"
	"github.com/pjunod/monarr/internal/domain/format"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
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
func (s *Service) autoGrab(ctx context.Context, w domain.Wantable, r ports.Release, evidence domain.MatchEvidence) error {
	if _, book := w.(domain.BookWantable); !book {
		index, err := s.allIdentity(ctx, time.Now())
		if err != nil {
			return err
		}
		if work, ok := index.Works[w.MediaItemID()]; ok {
			w = wantableWithIdentity(w, work.Identity)
		}
		parsed := parser.Parse(r.Title)
		match := releaseMatch(r, parsed, w, index)
		if !match.Matched {
			return fmt.Errorf("identity changed before grab: %s", match.Reason)
		}
		evidence = matchEvidence(parsed, w, r, match)
	}
	season, episode := wantableGrabTarget(w)
	_, err := s.Grab(ctx, GrabRequest{
		MediaItemID: w.MediaItemID(), CopyID: domain.WantableCopy(w),
		Season: season, Episode: episode,
		Title: r.Title, DownloadURL: r.DownloadURL, Indexer: r.Indexer,
		Protocol: r.Protocol, Size: r.Size,
		MatchEvidence: &evidence,
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
	identityIndex, err := s.allIdentity(ctx, time.Now())
	if err != nil {
		return err
	}
	enabled, err := s.enabledIndexers(ctx)
	if err != nil || len(enabled) == 0 {
		return err
	}

	grabbed := map[string]bool{} // wantable id → grabbed this run
	runtimes := runtimeMemo{}
	for _, cfg := range enabled {
		indexer := s.newIndexer(cfg)
		if provider, ok := indexer.(ports.IndexerCapabilitiesProvider); ok {
			caps, capsErr := provider.Capabilities(ctx)
			if capsErr != nil {
				s.log.Warn("rss: capabilities unavailable", "indexer", cfg.Name, "err", capsErr)
				continue
			}
			if caps.Generic.Known && !caps.Generic.Available {
				s.log.Info("rss: skipped because generic search is disabled", "indexer", cfg.Name)
				continue
			}
		}
		cctx, cancel := context.WithTimeout(ctx, s.searchTimeout)
		releases, err := indexer.FetchRSS(cctx)
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
			for _, w := range wanted {
				match := releaseMatch(r, p, w, identityIndex)
				if !match.Matched {
					continue
				}
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
				unlock, err := s.reservations.acquire(ctx, w)
				if err != nil {
					return err
				}
				fresh, err := s.wantableFromID(ctx, string(w.ID()))
				if err != nil || !fresh.Monitored() || len(s.notInFlight(ctx, []domain.Wantable{fresh})) == 0 {
					unlock()
					continue
				}
				freshProfile, err := s.db.GetProfile(ctx, fresh.ProfileID())
				if err != nil || !wants(freshProfile, fresh) || !decision.Decide(p.Quality, fresh, freshProfile).Accepted {
					unlock()
					continue
				}
				evidence := matchEvidence(p, fresh, r, match)
				if err := s.autoGrab(ctx, fresh, r, evidence); err != nil {
					unlock()
					s.log.Warn("rss: grab failed", "release", r.Title, "err", err)
					continue
				}
				unlock()
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
		if _, err := s.searchAndGrabBest(ctx, w, enabled); err != nil {
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

// searchTally counts what one wantable's search saw, so a caller can say
// something truer than "nothing happened".
//
// The funnel is the diagnosis: 0 seen means the indexers returned nothing for
// the query, seen>0 with matched 0 means the releases were for something else
// (or the title does not normalize the same way), and matched>0 with accepted
// 0 means the profile turned every one of them down. Those are three
// completely different problems and they used to look identical from outside.
type searchTally struct {
	Seen     int    // releases returned, after de-duplication
	Matched  int    // releases the matcher tied to this wantable
	Accepted int    // of those, ones the profile would take
	Grabbed  string // release title, "" if nothing was grabbed
}

// searchAndGrabBest runs the wantable's planned queries against the given
// indexers and grabs the single best accepted release, if any.
//
// The indexer fan-out is concurrent, as it is in interactive search. It was
// serial, which was tolerable when only the nightly backlog called this and
// is not now that a person can be sitting in front of it: with the 30 s
// per-call bound, four indexers meant a two-minute worst case.
func (s *Service) searchAndGrabBest(ctx context.Context, w domain.Wantable,
	enabled []ports.IndexerConfig) (searchTally, error) {
	return s.searchAndGrabBestWhere(ctx, w, enabled, nil)
}

// candidatePredicate narrows an automatic search without changing the
// decision engine. Explicit Wanted searches use it to reject packs and
// multi-episode releases both while planning queries and before the grab.
type candidatePredicate func(parser.Parsed) bool

func (s *Service) searchAndGrabBestWhere(ctx context.Context, w domain.Wantable,
	enabled []ports.IndexerConfig, allowed candidatePredicate) (searchTally, error) {
	return s.searchAndGrabBestScoped(ctx, w, enabled, allowed, nil)
}

func (s *Service) searchAndGrabBestScoped(ctx context.Context, w domain.Wantable,
	enabled []ports.IndexerConfig, allowed candidatePredicate, beforeGrab func() error) (searchTally, error) {
	release, err := s.reservations.acquire(ctx, w)
	if err != nil {
		return searchTally{}, err
	}
	defer release()
	if beforeGrab != nil {
		if err := beforeGrab(); err != nil {
			return searchTally{}, err
		}
	}
	// A caller may have waited behind a season/episode operation for this
	// item-copy. Re-resolve after acquiring, because that predecessor may have
	// satisfied the target or put it in flight while this caller waited.
	fresh, err := s.wantableFromID(ctx, string(w.ID()))
	if err != nil || !fresh.Monitored() {
		return searchTally{}, nil
	}
	profile, err := s.db.GetProfile(ctx, fresh.ProfileID())
	if err != nil {
		return searchTally{}, err
	}
	if !wants(profile, fresh) || len(s.notInFlight(ctx, []domain.Wantable{fresh})) == 0 {
		return searchTally{}, nil
	}
	w = fresh
	return s.searchAndGrabBestReserved(ctx, w, enabled, allowed, beforeGrab)
}

func (s *Service) searchAndGrabBestReserved(ctx context.Context, w domain.Wantable,
	enabled []ports.IndexerConfig, allowed candidatePredicate, beforeGrab func() error) (searchTally, error) {
	var tally searchTally
	profile, err := s.db.GetProfile(ctx, w.ProfileID())
	if err != nil {
		return tally, err
	}

	formats, _ := s.db.ListCustomFormats(ctx)
	runtime := s.db.ItemRuntime(ctx, w.MediaItemID())
	identityIndex, err := s.allIdentity(ctx, time.Now())
	if err != nil {
		return tally, err
	}

	var (
		mu       sync.Mutex
		releases []ports.Release
		wg       sync.WaitGroup
	)
	for _, cfg := range enabled {
		wg.Add(1)
		go func(cfg ports.IndexerConfig) {
			defer wg.Done()
			indexer := s.newIndexer(cfg)
			eligible := func(rs []ports.Release) bool {
				eligible := false
				for _, release := range rs {
					if s.isBlocklisted(ctx, release.Title, release.Indexer) {
						continue
					}
					parsed := parser.Parse(release.Title)
					if allowed != nil && !allowed(parsed) {
						continue
					}
					if !releaseMatch(release, parsed, w, identityIndex).Matched {
						continue
					}
					if !decision.Decide(parsed.Quality, w, profile).Accepted {
						continue
					}
					if _, bad := sizeImplausible(parsed.Quality, release, runtime); bad {
						continue
					}
					eligible = true
					break
				}
				return eligible
			}
			rs, reasons := s.executeIndexerSearch(ctx, indexer, w, false, eligible)
			for _, reason := range reasons {
				s.log.Warn("auto search: indexer incomplete", "indexer", cfg.Name, "reason", reason)
			}
			mu.Lock()
			releases = append(releases, rs...)
			mu.Unlock()
		}(cfg)
	}
	wg.Wait()

	type scored struct {
		r        ports.Release
		q        quality.Quality
		score    int
		evidence domain.MatchEvidence
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
	for _, r := range deduplicateReleases(releases) {
		tally.Seen++

		if s.isBlocklisted(ctx, r.Title, r.Indexer) {
			continue
		}
		p := parser.Parse(r.Title)
		if allowed != nil && !allowed(p) {
			continue
		}
		match := releaseMatch(r, p, w, identityIndex)
		if !match.Matched {
			continue
		}
		tally.Matched++
		if d := decision.Decide(p.Quality, w, profile); !d.Accepted {
			continue
		}
		if why, bad := sizeImplausible(p.Quality, r, runtime); bad {
			s.log.Info("auto search: declined on size", "release", r.Title, "why", why.Reason)
			continue
		}
		tally.Accepted++
		cand := &scored{r: r, q: p.Quality, score: format.Score(r.Title, formats), evidence: matchEvidence(p, w, r, match)}
		if best == nil || better(cand, best) {
			best = cand
		}
	}
	if best == nil {
		return tally, nil
	}
	if capped, _ := s.regrabCapped(ctx, w); capped {
		return tally, nil
	}
	if len(s.notInFlight(ctx, []domain.Wantable{w})) == 0 {
		return tally, nil
	}
	if beforeGrab != nil {
		if err := beforeGrab(); err != nil {
			return tally, err
		}
	}
	if err := s.autoGrab(ctx, w, best.r, best.evidence); err != nil {
		return tally, fmt.Errorf("grab %q: %w", best.r.Title, err)
	}
	tally.Grabbed = best.r.Title
	s.log.Info("auto search: grabbed", "release", best.r.Title, "wantable", w.ID())
	return tally, nil
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
	Reason      string `json:"reason"`  // missing | upgrade
	Kind        string `json:"kind"`    // movie | series | book
	CopyID      int64  `json:"copyId"`  // 0 = primary
	Season      *int   `json:"season,omitempty"`
	Episode     *int   `json:"episode,omitempty"`
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
			Copy: domain.WantableCopyName(w), CopyID: domain.WantableCopy(w), Reason: "missing",
		}
		if q, ok := w.CurrentQuality(); ok {
			ws.Missing, ws.Current, ws.Reason = false, q.Display(), "upgrade"
		}
		switch t := w.(type) {
		case domain.MovieWantable:
			ws.Title, ws.Detail, ws.Kind = t.Title, fmt.Sprintf("(%d)", t.Year), "movie"
		case domain.EpisodeWantable:
			season, episode := t.Season, t.Episode
			ws.Title, ws.Detail, ws.Kind = t.Title, fmt.Sprintf("S%02dE%02d", t.Season, t.Episode), "series"
			ws.Season, ws.Episode = &season, &episode
		case domain.BookWantable:
			ws.Title, ws.Kind = t.Title, "book"
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

// Skip reasons reported by AutoSearchItem for a target it did not search.
// These are the answers to "I pressed the button and nothing happened".
const (
	SkipUnmonitored = "unmonitored"
	SkipInFlight    = "downloading"
)

// AutoSearchTarget is what happened to one wantable in an auto search.
type AutoSearchTarget struct {
	WantableID string `json:"wantableId"`
	Label      string `json:"label"`             // "Blade Runner (1982)", "Show season 2"
	Skipped    string `json:"skipped,omitempty"` // set = not searched, and why
	Seen       int    `json:"seen"`              // releases the indexers returned
	Matched    int    `json:"matched"`           // releases that were for this
	Accepted   int    `json:"accepted"`          // of those, ones the profile takes
	Grabbed    string `json:"grabbed,omitempty"` // release title, if one was grabbed
	Error      string `json:"error,omitempty"`
}

// AutoSearchOutcome is what an auto search actually did.
//
// This exists because the endpoint used to return 202 and nothing else. Every
// outcome — grabbed a 2160p remux, found four releases and declined all four,
// skipped the item entirely because a download row from March was still
// counted as in flight — produced the same "Searching in the background"
// banner. A button whose only feedback is identical whether it worked or not
// is a button you cannot trust, and the notInFlight bug hid behind it for as
// long as it did precisely because of that.
type AutoSearchOutcome struct {
	Targets []AutoSearchTarget `json:"targets"`
	Grabbed int                `json:"grabbed"`
}

// AutoSearchItem searches for everything one item still wants and grabs
// the best accepted release per wantable — Sonarr's "search on add" /
// "automatic search" semantics: no candidate list, the decision engine
// picks. Series search season packs (missing episodes fall to the RSS and
// backlog loops if no pack exists).
func (s *Service) AutoSearchItem(ctx context.Context, itemID int64) (AutoSearchOutcome, error) {
	var out AutoSearchOutcome
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return out, err
	}
	enabled, err := s.enabledIndexers(ctx)
	if err != nil {
		return out, err
	}
	if len(enabled) == 0 {
		s.log.Info("auto search: no enabled indexers yet", "item", item.Title)
		return out, ErrNoIndexers
	}

	var targets []domain.Wantable
	switch item.Kind {
	case domain.KindMovie, domain.KindBook:
		w, err := s.target(ctx, item, 0, 0)
		if err != nil {
			return out, err
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
	// Each monitored copy is its own automation target, including the other
	// first-class edition of a book.
	for i := range item.Copies {
		cp := item.Copies[i]
		if !cp.Monitored {
			continue
		}
		switch item.Kind {
		case domain.KindMovie, domain.KindBook:
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

	// Report on every target, including the ones that are not going to be
	// searched. A skipped target is the single most useful thing this can
	// say, and it is the one thing the old fire-and-forget version could not.
	searchable := map[string]bool{}
	for _, w := range s.notInFlight(ctx, targets) {
		searchable[string(w.ID())] = true
	}
	out.Targets = make([]AutoSearchTarget, 0, len(targets))
	for _, w := range targets {
		t := AutoSearchTarget{WantableID: string(w.ID()), Label: describeTarget(w)}
		switch {
		case !w.Monitored():
			t.Skipped = SkipUnmonitored
		case !searchable[string(w.ID())]:
			t.Skipped = SkipInFlight
		default:
			tally, err := s.searchAndGrabBest(ctx, w, enabled)
			t.Seen, t.Matched, t.Accepted = tally.Seen, tally.Matched, tally.Accepted
			t.Grabbed = tally.Grabbed
			if err != nil {
				t.Error = err.Error()
				s.log.Warn("auto search: failed", "wantable", w.ID(), "err", err)
			}
			if t.Grabbed != "" {
				out.Grabbed++
			}
		}
		out.Targets = append(out.Targets, t)
	}
	s.log.Info("auto search: done", "item", item.Title,
		"targets", len(out.Targets), "grabbed", out.Grabbed)
	s.InvalidateWanted()
	return out, nil
}
