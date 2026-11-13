package household

import (
	"context"
	"errors"
	"time"

	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func (s *Service) SetAdmission(a ports.WorkAdmission) { s.admission = a }
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) Run(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	s.Wake()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.wake:
		}
		_ = s.Process(ctx)
	}
}

// Process resumes durable target intents; the existing acquisition reducer owns
// every committed transfer and its capability-aware uncertain-submission retry.
func (s *Service) Process(ctx context.Context) error {
	if s.Acquisition == nil {
		return nil
	}
	targets, err := s.DB.HouseholdPendingTargets(ctx)
	if err != nil {
		return err
	}
	for i, target := range targets {
		if i >= 10 {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		work, release, err := ports.TryWork(ctx, s.admission, "household exact request")
		if err != nil {
			return err
		}
		job, cancel := context.WithTimeout(work, 90*time.Second)
		err = s.processTarget(job, target)
		cancel()
		release()
		if err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}
func (s *Service) processTarget(ctx context.Context, target domain.HouseholdTarget) error {
	result := func(state, note string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return s.DB.SetHouseholdTargetResult(ctx, target.ID, target.ApprovalGeneration, state, note)
	}
	if err := s.Library.VerifyPrimaryPresence(ctx, target.MediaItemID); err != nil {
		if target.DownloadID != 0 {
			return result("waiting", "Existing work is waiting for a successful library check.")
		}
		return result("needs_review", "The primary library scope needs a successful check before acquisition.")
	}
	evidence, err := s.DB.HouseholdEvidence(ctx, target.MediaItemID)
	if err != nil {
		return result("needs_review", "The primary library scope could not be verified.")
	}
	_, policy, err := s.DB.HouseholdTargetPolicy(ctx, target.ID)
	if err != nil {
		return err
	}
	present := evidence.PresentMovie
	if policy.Kind == domain.KindSeries {
		present = evidence.PresentEpisodes[target.EpisodeID]
	}
	if present {
		return result("fulfilled", "")
	}
	if target.DownloadID != 0 {
		download, e := s.DB.GetDownload(ctx, target.DownloadID)
		if e != nil || download.GenerationKey != target.DownloadGeneration || download.Superseded || download.State == "failed" || download.State == "imported" {
			return result("needs_review", "Existing work needs owner review before another acquisition can be authorized.")
		}
		return result("waiting", "Approved work is in progress. Fulfillment requires verified primary files.")
	}
	if err = s.DB.CheckHouseholdAdmission(ctx, target.ID, target.ApprovalGeneration); err != nil {
		return result("needs_review", "The approved identity, destination, policy or scope changed. Review it again.")
	}
	request, err := s.DB.HouseholdRequest(ctx, target.RequestID, 0, false)
	if err != nil {
		return err
	}
	if policy.Reacquire {
		if err = s.Acquisition.CancelHouseholdOffloadDeliveries(ctx, target.MediaItemID, request.Scope, policy.OffloadOperations); err != nil {
			return result("waiting", "Waiting to settle the previously approved offload notifications.")
		}
	}
	id, acquireErr := s.Acquisition.AcquireHouseholdTarget(ctx, target)
	// Admission may have committed before a client response or local error. Never
	// reinterpret that as permission to submit again; the reducer owns its token.
	current, e := s.DB.HouseholdTarget(ctx, target.ID)
	if e == nil && current.ApprovalGeneration == target.ApprovalGeneration && current.DownloadID != 0 {
		return result("waiting", "Approved work has been admitted and is being followed.")
	}
	if errors.Is(acquireErr, sqlite.ErrHouseholdPresent) {
		if e = s.Library.VerifyPrimaryPresence(ctx, target.MediaItemID); e == nil {
			return result("fulfilled", "")
		}
		return result("needs_review", "Library facts changed while acquisition was being reviewed.")
	}
	if acquireErr != nil {
		if errors.Is(acquireErr, acquisition.ErrNoIndexers) || errors.Is(acquireErr, context.DeadlineExceeded) {
			return result("waiting", "Waiting for configured sources to return a suitable release.")
		}
		return result("needs_review", "Acquisition needs owner review. Existing transfers remain managed in Activity.")
	}
	if id != 0 {
		// Grab may return existing owner work. Freshly verify before binding, and
		// require its exact generation/leaf coverage without changing its authority.
		if err = s.Library.VerifyPrimaryPresence(ctx, target.MediaItemID); err != nil {
			return result("waiting", "Waiting for the existing work and a current library check.")
		}
		if err = s.DB.BindExistingHouseholdDownload(ctx, target.ID, target.ApprovalGeneration, id); err != nil {
			return result("needs_review", "Existing work could not be bound to the exact requested primary scope.")
		}
		return result("waiting", "Following already authorized work for the requested scope.")
	}
	return result("waiting", "Looking for suitable releases for the approved missing scope.")
}
