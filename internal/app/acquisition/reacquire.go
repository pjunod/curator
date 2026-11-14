package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Re-acquire (ADR 0028) is the way back from an Offload: it clears the
// offloaded state inside a scope, monitors it, cancels the Cinema
// sub-deliveries of an Offload that Cinema has not confirmed for that scope,
// and runs a search at the current profile(s); a season is searched as a pack
// first, as season acquisition does. The item page's button and Cinema's Get
// it back (the reacquire webhook) are this one service.

// Re-acquire states, as the webhook answers them (contract 4.6).
const (
	ReacquireSearching    = "searching"
	ReacquireDownloading  = "downloading"
	ReacquireImporting    = "importing"
	ReacquireOnDisk       = "on_disk"
	ReacquireNotOffloaded = "not_offloaded"
	// ReacquireOffloading: an Offload of that title is running (deleting or
	// stopping), so nothing can come back yet.
	ReacquireOffloading = "offloading"
	// ReacquireOffloaded: the scope is still offloaded and a Re-acquire has
	// not started (it was only read, or it could not take the title in time).
	ReacquireOffloaded = "offloaded"
)

// reacquireReserveWait bounds how long a Re-acquire waits for the title's
// reservation (an automatic grab holds it for a moment): Cinema's call is
// synchronous, so it is answered rather than left hanging.
var reacquireReserveWait = 2 * time.Second

// ReacquireRequest names the scope: item (the movie, or every offloaded
// season and episode of a show), season (Season) or episode (EpisodeID).
type ReacquireRequest struct {
	ItemID    int64  `json:"-"`
	Scope     string `json:"scope"`
	Season    int    `json:"season,omitempty"`
	EpisodeID int64  `json:"episodeId,omitempty"`
}

// ReacquireResult is what Re-acquire (or the status read) answers.
type ReacquireResult struct {
	MediaItemID int64  `json:"mediaItemId"`
	State       string `json:"state"`
	// Cleared says this call cleared the offloaded state (false: it was not
	// offloaded, so the answer is the title's state as it is).
	Cleared bool   `json:"cleared"`
	Message string `json:"message,omitempty"`
}

// ErrOffloadBusy is a Re-acquire while the Offload of that title is still
// deleting files.
var ErrOffloadBusy = fmt.Errorf("%w: the offload is still deleting files; wait for it or Stop it first", ErrInvalidOffload)

type reacquireScope struct {
	item     domain.MediaItem
	clear    string // movie | whole | season | episode
	season   int
	episode  int64
	episodes map[int64]bool // episodes the scope covers (series)
}

func (s *Service) reacquireScope(ctx context.Context, req ReacquireRequest) (reacquireScope, error) {
	item, err := s.db.GetMediaItemFull(ctx, req.ItemID)
	if err != nil {
		return reacquireScope{}, err
	}
	sc := reacquireScope{item: item, episodes: map[int64]bool{}}
	switch item.Kind {
	case domain.KindMovie:
		sc.clear = "movie"
	case domain.KindSeries:
		switch req.Scope {
		case "item", "":
			sc.clear = "whole"
		case "season":
			sc.clear, sc.season = "season", req.Season
		case "episode":
			sc.clear, sc.episode = "episode", req.EpisodeID
		default:
			return sc, fmt.Errorf("%w: scope must be item, season or episode", ErrInvalidOffload)
		}
		found := sc.clear == "whole"
		for _, sn := range item.Seasons {
			for _, e := range sn.Episodes {
				switch {
				case sc.clear == "whole", sc.clear == "season" && sn.Number == sc.season, sc.clear == "episode" && e.ID == sc.episode:
					sc.episodes[e.ID] = true
					found = true
				}
			}
			if sc.clear == "season" && sn.Number == sc.season {
				found = true
			}
		}
		if !found {
			return sc, fmt.Errorf("%w: %s has no such season or episode", ErrNotFound, item.Title)
		}
	default:
		return sc, fmt.Errorf("%w: only a movie or a show can be re-acquired", ErrInvalidOffload)
	}
	return sc, nil
}

// offloadedIn reports whether anything inside the scope is offloaded.
func (sc reacquireScope) offloadedIn() bool {
	if sc.clear == "movie" {
		return sc.item.Offloaded()
	}
	for _, sn := range sc.item.Seasons {
		if (sc.clear == "whole" || (sc.clear == "season" && sn.Number == sc.season)) && sn.Offloaded() {
			return true
		}
		for _, e := range sn.Episodes {
			if sc.episodes[e.ID] && e.Offloaded() {
				return true
			}
		}
	}
	return false
}

// Reacquire clears Offload inside the scope, monitors it and searches. It
// answers at once with state searching; the search runs in the background,
// admitted as work. Asking again is harmless: a scope that is no longer
// offloaded answers the title's current state.
func (s *Service) Reacquire(ctx context.Context, req ReacquireRequest) (ReacquireResult, error) {
	sc, err := s.reacquireScope(ctx, req)
	if err != nil {
		return ReacquireResult{}, err
	}
	if open, err := s.db.OpenOffloadForItem(ctx, req.ItemID); err != nil {
		return ReacquireResult{}, err
	} else if open != "" && sc.offloadedIn() {
		return ReacquireResult{MediaItemID: req.ItemID, State: ReacquireOffloading, Message: ErrOffloadBusy.Error()}, ErrOffloadBusy
	}
	// Serialised with Offload, and with an automatic grab, on the item's
	// per-copy reservations. Bounded: a held title is answered, not awaited.
	rctx, stop := context.WithTimeout(ctx, reacquireReserveWait)
	unlock, err := s.reserveItem(rctx, req.ItemID)
	stop()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			res, serr := s.reacquireState(ctx, sc)
			if serr == nil && res.State == ReacquireOffloaded {
				res.Message = "the title is busy; Re-acquire was not started, ask again"
			}
			return res, serr
		}
		return ReacquireResult{}, err
	}
	sc, err = s.reacquireScope(ctx, req)
	if err != nil {
		unlock()
		return ReacquireResult{}, err
	}
	// An Offload can start between the first check and the reservation: look
	// again now that nothing else can start one.
	if open, err := s.db.OpenOffloadForItem(ctx, req.ItemID); err != nil {
		unlock()
		return ReacquireResult{}, err
	} else if open != "" && sc.offloadedIn() {
		unlock()
		return ReacquireResult{MediaItemID: req.ItemID, State: ReacquireOffloading, Message: ErrOffloadBusy.Error()}, ErrOffloadBusy
	}
	if !sc.offloadedIn() {
		unlock()
		return s.reacquireState(ctx, sc)
	}
	cleared, err := s.db.ClearOffloaded(ctx, req.ItemID, sc.clear, sc.season, sc.episode)
	if err != nil {
		unlock()
		return ReacquireResult{}, err
	}
	s.cancelOffloadDeliveries(ctx, sc)
	_ = s.db.AddHistory(ctx, HistoryReacquired, req.ItemID, "", map[string]any{"scope": req.Scope, "season": req.Season,
		"episodes": len(cleared.Episodes), "movie": cleared.Movie})
	unlock()
	s.InvalidateWanted()
	s.log.Info("re-acquire", "item", sc.item.Title, "scope", sc.clear, "season", sc.season, "episode", sc.episode)
	s.startReacquireSearch(sc)
	return ReacquireResult{MediaItemID: req.ItemID, State: ReacquireSearching, Cleared: true}, nil
}

// HistoryReacquired records a Re-acquire: the scope that was brought back.
const HistoryReacquired = "reacquired"

// cancelOffloadDeliveries narrows the unconfirmed Cinema requests of Offloads
// that overlap the scope: Cinema must not be told a path left that is coming
// back, but is still told about every path that stays gone. A request whose
// paths are all re-acquired is cancelled; one that has others is rewritten
// without the re-acquired paths (as Stop rewrites it).
func (s *Service) cancelOffloadDeliveries(ctx context.Context, sc reacquireScope) {
	if err := s.cancelOffloadDeliveriesRestricted(ctx, sc, nil); err != nil {
		s.log.Warn("re-acquire: offload notification narrowing failed", "err", err)
	}
}
func (s *Service) cancelOffloadDeliveriesRestricted(ctx context.Context, sc reacquireScope, only []string) error {
	s.offloadNarrowMu.Lock()
	defer s.offloadNarrowMu.Unlock()
	const why = "cancelled: re-acquired before cinema confirmed it"
	ops, err := s.db.PendingOffloadOperationsForItem(ctx, sc.item.ID)
	if err != nil {
		s.log.Warn("re-acquire: pending offload requests unreadable", "err", err)
		return err
	}
	stillOffloaded := map[int64]bool{}
	for _, sn := range sc.item.Seasons {
		for _, e := range sn.Episodes {
			stillOffloaded[e.ID] = e.Offloaded()
		}
	}
	for _, op := range ops {
		if only != nil && !slices.Contains(only, op.ID) {
			continue
		}
		var stored offloadStored
		if err = json.Unmarshal(op.Plan, &stored); err != nil {
			return err
		}
		files, err := s.db.ListOffloadFiles(ctx, op.ID)
		if err != nil {
			return err
		}
		overlap := sc.clear == "movie" || stored.Movie
		for _, id := range stored.Episodes {
			if sc.episodes[id] {
				overlap = true
			}
		}
		if !overlap {
			continue
		}
		excluded, err := s.db.HouseholdOffloadExclusions(ctx, op.ID)
		if err != nil {
			return err
		}
		var keepPaths, removePaths []string
		for _, f := range files {
			if f.Kind != "media" || f.State != "deleted" {
				continue
			}
			back := excluded[f.Path]
			if only == nil { // Ordinary owner re-acquire retains its existing all-copy scope.
				back = back || sc.clear == "movie" || stored.Movie
				for _, id := range stored.FileEpisodes[f.Path] {
					back = back || sc.episodes[id] || !stillOffloaded[id]
				}
			} else if _, known := stored.FileCopies[f.Path]; !known {
				return fmt.Errorf("%w: historical offload file has no copy attribution", sqlite.ErrRequestChanged)
			} else if stored.FileCopies[f.Path] == 0 {
				back = back || sc.clear == "movie"
				for _, id := range stored.FileEpisodes[f.Path] {
					back = back || sc.episodes[id]
				}
			}
			if back {
				removePaths = append(removePaths, f.Path)
			} else {
				keepPaths = append(keepPaths, f.Path)
			}
		}
		if err = s.db.ExcludeHouseholdOffloadPaths(ctx, op.ID, removePaths); err != nil {
			return err
		}

		var keep []sqlite.OutboxWrite
		if len(keepPaths) > 0 {
			if keep, err = s.offloadOutbox(ctx, op.ID, sc.item, keepPaths, false); err != nil {
				s.log.Warn("re-acquire: cannot narrow the offload requests", "err", err)
				return err
			}
		}
		if err := s.db.RewriteOffloadOutbox(ctx, op.ID, why, keep); err != nil {
			s.log.Warn("re-acquire: could not cancel offload requests", "err", err)
			return err
		}
	}

	return nil
}

// startReacquireSearch runs the search in the background, admitted as work so
// a deployment drain waits for it.
func (s *Service) startReacquireSearch(sc reacquireScope) {
	workCtx, release, err := ports.TryWork(s.rematchBase(), s.admission, "reacquire")
	if err != nil {
		s.log.Info("re-acquire: search deferred for a drain; the wanted passes find it", "item", sc.item.Title)
		return
	}
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		defer release()
		ctx := withSearchTrigger(WithManualSearch(workCtx), "reacquire")
		if err := s.searchReacquired(ctx, sc); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Warn("re-acquire: search failed; the wanted passes retry it", "item", sc.item.Title, "err", err)
		}
		s.InvalidateWanted()
	}()
}

func (s *Service) searchReacquired(ctx context.Context, sc reacquireScope) error {
	item, err := s.db.GetMediaItemFull(ctx, sc.item.ID)
	if err != nil {
		return err
	}
	switch sc.clear {
	case "movie", "whole":
		_, err := s.AutoSearchItem(ctx, item.ID)
		return err
	}
	copies := []*domain.MediaCopy{nil}
	for i := range item.Copies {
		if item.Copies[i].Monitored {
			copies = append(copies, &item.Copies[i])
		}
	}
	enabled, err := s.enabledIndexers(ctx)
	if err != nil {
		return err
	}
	if len(enabled) == 0 {
		return ErrNoIndexers
	}
	for _, cp := range copies {
		var copyID int64
		if cp != nil {
			copyID = cp.ID
		}
		if sc.clear == "season" {
			// Pack first, as season acquisition does.
			if _, err := s.enqueueSeasonStatus(ctx, item.ID, copyID, sc.season, "reacquire", true); err != nil {
				return err
			}
			continue
		}
		for _, sn := range item.Seasons {
			for _, e := range sn.Episodes {
				if e.ID != sc.episode {
					continue
				}
				w, err := s.targetCopy(ctx, item, sn.Number, e.EpisodeNumber, cp)
				if err != nil {
					return err
				}
				if _, err := s.searchAndGrabBestScoped(ctx, w, enabled, exactWantedCandidate, nil); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// ReacquireStatus reads the title's state for the scope without changing
// anything (contract 4.7).
func (s *Service) ReacquireStatus(ctx context.Context, req ReacquireRequest) (ReacquireResult, error) {
	sc, err := s.reacquireScope(ctx, req)
	if err != nil {
		return ReacquireResult{}, err
	}
	return s.reacquireState(ctx, sc)
}

func (s *Service) reacquireState(ctx context.Context, sc reacquireScope) (ReacquireResult, error) {
	out := ReacquireResult{MediaItemID: sc.item.ID, State: ReacquireNotOffloaded}
	if sc.offloadedIn() {
		out.State, out.Message = ReacquireOffloaded, "still offloaded"
		if open, err := s.db.OpenOffloadForItem(ctx, sc.item.ID); err != nil {
			return out, err
		} else if open != "" {
			out.State, out.Message = ReacquireOffloading, "an offload of this title is running"
		}
		return out, nil
	}
	downloads, err := s.db.ListDownloadReservations(ctx)
	if err != nil {
		return out, err
	}
	best := ""
	rank := map[string]int{ReacquireDownloading: 2, ReacquireImporting: 3}
	for _, dl := range downloads {
		if dl.MediaItemID != sc.item.ID || !inFlight(dl.State) {
			continue
		}
		if sc.item.Kind == domain.KindSeries && sc.clear != "whole" && len(dl.ReservedEpisodes) > 0 {
			hit := false
			for _, id := range dl.ReservedEpisodes {
				hit = hit || sc.episodes[id]
			}
			if !hit {
				continue
			}
		}
		state := ReacquireDownloading
		if slices.Contains([]string{"downloaded", "awaiting_import", "importing"}, dl.State) {
			state = ReacquireImporting
		}
		if rank[state] > rank[best] {
			best = state
		}
	}
	if best != "" {
		out.State = best
		return out, nil
	}
	// On disk: every episode of the scope has a file (a movie: any file).
	if sc.item.Kind == domain.KindMovie {
		if len(sc.item.Files) > 0 {
			out.State = ReacquireOnDisk
			return out, nil
		}
	} else {
		all, any := true, false
		for _, sn := range sc.item.Seasons {
			for _, e := range sn.Episodes {
				if sc.episodes[e.ID] {
					any = true
					all = all && e.HasFile
				}
			}
		}
		if any && all {
			out.State = ReacquireOnDisk
			return out, nil
		}
	}
	// Wanted again and waiting for a release: searching.
	if sc.item.Monitored && s.scopeMonitored(sc) {
		out.State = ReacquireSearching
	}
	return out, nil
}

func (s *Service) scopeMonitored(sc reacquireScope) bool {
	if sc.clear == "movie" {
		return true
	}
	for _, sn := range sc.item.Seasons {
		for _, e := range sn.Episodes {
			if sc.episodes[e.ID] && sn.Monitored && e.Monitored {
				return true
			}
		}
	}
	return false
}
