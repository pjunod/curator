package acquisition

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/app/pipeline"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// The replacement grab worker (contract section 3.3): a failed terminal
// writes a replacement_grabs row in its own transaction, and this worker
// executes it idempotently. A crash between "recorded failed" and "grabbed
// the next one" leaves the row unsettled, and the next tick runs it; a
// settled row is never run twice.

const (
	replacementGrabTick     = 5 * time.Second
	replacementGrabBatch    = 8
	replacementGrabAttempts = 3
)

// replacementGrabOwnerKey marks a search the worker runs for one failed row,
// so that row's own pending grab does not count as "in flight" against it.
type replacementGrabOwnerKey struct{}

func replacementGrabOwner(ctx context.Context) int64 {
	id, _ := ctx.Value(replacementGrabOwnerKey{}).(int64)
	return id
}

// kickReplacementGrabs wakes the worker now rather than at its next tick.
func (s *Service) kickReplacementGrabs() {
	if !s.grabWorker.Load() {
		// No worker (tests, tools): run the grab now, as the worker would.
		s.RunReplacementGrabs(context.WithoutCancel(s.grabCtx()))
		return
	}
	s.grabKickOnce.Do(func() { s.grabKick = make(chan struct{}, 1) })
	select {
	case s.grabKick <- struct{}{}:
	default:
	}
}

// startReplacementGrabWorker runs the worker until ctx ends.
func (s *Service) startReplacementGrabWorker(ctx context.Context) {
	s.grabKickOnce.Do(func() { s.grabKick = make(chan struct{}, 1) })
	s.grabWorker.Store(true)
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		ticker := time.NewTicker(replacementGrabTick)
		defer ticker.Stop()
		for {
			s.RunReplacementGrabs(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-s.grabKick:
			}
		}
	}()
}

func (s *Service) grabCtx() context.Context {
	if s.importCtx != nil {
		return s.importCtx
	}
	return context.Background()
}

// RunReplacementGrabs executes every due replacement grab once.
func (s *Service) RunReplacementGrabs(ctx context.Context) {
	ctx, release, err := ports.EnterWork(ctx, s.admission, "replacement grabs")
	if err != nil {
		return
	}
	defer release()
	due, err := s.db.DueReplacementGrabs(ctx, time.Now(), replacementGrabBatch)
	if err != nil {
		s.log.Warn("replacement grabs could not be read", "err", err)
		return
	}
	for _, g := range due {
		if ctx.Err() != nil {
			return
		}
		s.runReplacementGrab(ctx, g)
	}
}

// runReplacementGrab performs one row's next grab, per kind: a movie or book
// is searched; an episode is searched for a single-episode release, never a
// pack; an episode with no single release goes to the season planner, which
// is the fallback, not the path.
func (s *Service) runReplacementGrab(ctx context.Context, g sqlite.ReplacementGrab) {
	claimed, err := s.db.ClaimReplacementGrab(ctx, g.DownloadID, time.Now())
	if err != nil || !claimed {
		return
	}
	dl, err := s.db.GetDownload(ctx, g.DownloadID)
	if err != nil {
		_ = s.db.SettleReplacementGrab(ctx, g.DownloadID, "skipped: download row is gone", time.Time{})
		return
	}
	enabled, err := s.enabledIndexers(ctx)
	if err != nil || len(enabled) == 0 || s.newIndexer == nil {
		s.settleGrabRetry(ctx, g, fmt.Errorf("no enabled indexer"))
		return
	}
	key := pipeline.Key(dl.Transfer, dl.ID)
	ctx = context.WithValue(ctx, replacementGrabOwnerKey{}, dl.ID)
	var grabbed, planned []string
	var lastErr error
	plannerSeasons := map[[3]int64]bool{}
	for _, idStr := range g.WantableIDs {
		w, err := s.wantableFromID(ctx, idStr)
		if err != nil {
			continue
		}
		var targets []domain.Wantable
		switch t := w.(type) {
		case domain.SeasonWantable:
			eps, err := s.seasonEpisodeWantables(ctx, t)
			if err != nil {
				lastErr = err
				continue
			}
			targets = eps
		default:
			targets = []domain.Wantable{w}
		}
		for _, target := range targets {
			if !s.stillWanted(ctx, target) {
				continue
			}
			if capped, spent := s.regrabCapped(ctx, target); capped {
				_ = s.db.AddHistory(ctx, HistoryRegrabCapped, dl.MediaItemID, dl.ReleaseTitle,
					map[string]any{"wantable": string(target.ID()), "failed": spent, "window_hours": int(reGrabWindow.Hours())})
				continue
			}
			var allowed candidatePredicate
			ep, episode := target.(domain.EpisodeWantable)
			if episode {
				allowed = exactWantedCandidate // a replacement for an episode is never a pack
			}
			// The clock starts at the first search actually attempted (M0
			// semantics: first start wins; nothing to search records nothing).
			s.timings.Start(ctx, pipeline.FailureToNextGrab, key, dl.ID)
			tally, err := s.searchAndGrabBestScoped(ctx, target, enabled, allowed, nil)
			if err != nil {
				lastErr = err
				continue
			}
			if tally.Grabbed != "" {
				grabbed = append(grabbed, string(target.ID()))
				continue
			}
			if episode && tally.Incomplete == nil {
				plannerSeasons[[3]int64{ep.Item, int64(ep.Season), ep.Copy}] = true
				planned = append(planned, string(target.ID()))
			}
		}
	}
	for season := range plannerSeasons {
		status, err := s.enqueueSeasonStatus(ctx, season[0], season[2], int(season[1]), seasonTriggerFailedAcquisition, true)
		if err != nil {
			lastErr = err
			continue
		}
		if status == seasonQueued {
			grabbed = append(grabbed, fmt.Sprintf("season:%d:%d (planner)", season[0], season[1]))
		}
	}
	switch {
	case len(grabbed) > 0:
		s.timings.End(ctx, pipeline.FailureToNextGrab, key)
		_ = s.db.SettleReplacementGrab(ctx, g.DownloadID, "grabbed: "+strings.Join(grabbed, ", "), time.Time{})
	case lastErr != nil:
		s.settleGrabRetry(ctx, g, lastErr)
	default:
		// Nothing better to grab now. The want stays wanted; the wanted pass
		// and the planner own it from here.
		s.timings.Discard(ctx, pipeline.FailureToNextGrab, key)
		result := "nothing to grab"
		if len(planned) > 0 {
			result = "handed to the season planner: " + strings.Join(planned, ", ")
		}
		_ = s.db.SettleReplacementGrab(ctx, g.DownloadID, result, time.Time{})
	}
	s.InvalidateWanted()
}

// settleGrabRetry re-queues a grab whose search failed on the environment,
// until it has had its attempts; then the want is left to the wanted pass.
// Not a retry ladder: the worker's own tick is the schedule, and a budget
// deferral carries its provider's deadline.
func (s *Service) settleGrabRetry(ctx context.Context, g sqlite.ReplacementGrab, cause error) {
	if g.Attempts+1 >= replacementGrabAttempts {
		s.timings.Discard(ctx, pipeline.FailureToNextGrab, s.grabTimingKey(ctx, g.DownloadID))
		_ = s.db.SettleReplacementGrab(ctx, g.DownloadID, "deferred: "+cause.Error(), time.Time{})
		return
	}
	next := time.Now().Add(time.Minute)
	if deadline := submissionRetryDeadline(cause); deadline.After(next) {
		next = deadline
	}
	var budget interface{ RetryDeadline() time.Time }
	if errors.As(cause, &budget) && budget.RetryDeadline().After(next) {
		next = budget.RetryDeadline()
	}
	_ = s.db.SettleReplacementGrab(ctx, g.DownloadID, "", next)
}

func (s *Service) grabTimingKey(ctx context.Context, downloadID int64) string {
	if dl, err := s.db.GetDownload(ctx, downloadID); err == nil {
		return pipeline.Key(dl.Transfer, dl.ID)
	}
	return pipeline.Key("", downloadID)
}

// stillWanted is the search's own precondition, asked first so a target
// that is not wanted is neither searched nor handed to the planner.
func (s *Service) stillWanted(ctx context.Context, w domain.Wantable) bool {
	fresh, err := s.wantableFromID(ctx, string(w.ID()))
	if err != nil || !fresh.Monitored() {
		return false
	}
	profile, err := s.db.GetProfile(ctx, fresh.ProfileID())
	if err != nil {
		return false
	}
	return wants(profile, fresh) && len(s.notInFlight(ctx, []domain.Wantable{fresh})) > 0
}

// seasonEpisodeWantables expands a failed season pack into its episodes.
func (s *Service) seasonEpisodeWantables(ctx context.Context, t domain.SeasonWantable) ([]domain.Wantable, error) {
	item, err := s.db.GetMediaItemFull(ctx, t.Item)
	if err != nil {
		return nil, err
	}
	var cp *domain.MediaCopy
	if t.Copy != 0 {
		mc, err := s.db.GetMediaCopy(ctx, t.Item, t.Copy)
		if err != nil {
			return nil, err
		}
		cp = &mc
	}
	var out []domain.Wantable
	for _, season := range item.Seasons {
		if season.Number != t.Season {
			continue
		}
		for _, ep := range season.Episodes {
			w, err := s.targetCopy(ctx, item, season.Number, ep.EpisodeNumber, cp)
			if err != nil {
				continue
			}
			out = append(out, w)
		}
	}
	return out, nil
}
