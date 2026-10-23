package acquisition

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func submissionRetryDeadline(err error) time.Time {
	var provider ports.RetryDeadline
	if errors.As(err, &provider) {
		return provider.RetryDeadline()
	}
	return time.Time{}
}

func permanentSubmissionRefusal(err error) bool {
	if errors.Is(err, ports.ErrAdmissionRejected) {
		return true
	}
	var status interface{ HTTPStatusCode() int }
	if !errors.As(err, &status) {
		return false
	}
	return status.HTTPStatusCode() == 409 || status.HTTPStatusCode() == 422
}

const runnerSubmissionBlockedWarning = 30 * time.Minute

var errSubmissionRetryNotDue = errors.New("submission retry deadline has not arrived")

// startAutomaticAdmissionWorker resumes exact request-token submissions off
// the queue polling/control path. It is safe to replay only rows for which the
// Runner's durable token idempotency capability was negotiated at admission.
func (s *Service) startAutomaticAdmissionWorker(ctx context.Context) {
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		s.processRunnerSubmissionRetries(ctx)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.processRunnerSubmissionRetries(ctx)
			}
		}
	}()
}

func (s *Service) processRunnerSubmissionRetries(ctx context.Context) {
	now := time.Now()
	retries, err := s.db.ListRunnerSubmissionRetries(ctx, now, 16)
	if err != nil {
		s.log.Warn("Runner submission retry scan failed", "err", err)
		return
	}
	for _, retry := range retries {
		if ctx.Err() != nil {
			return
		}
		unlock := s.lockDownload(retry.DownloadID)
		dl, err := s.db.GetDownload(ctx, retry.DownloadID)
		if err != nil || dl.GenerationKey != retry.GenerationKey || !automaticSubmissionRetryable(dl) {
			unlock()
			continue
		}
		cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID)
		if err == nil {
			err = s.retryAutomaticSubmission(ctx, dl, cfg)
		}
		if err == nil {
			unlock()
			continue
		}
		if errors.Is(err, errSubmissionRetryNotDue) {
			unlock()
			continue
		}
		if permanentSubmissionRefusal(err) {
			if rejectErr := s.db.RejectDownloadSubmission(context.WithoutCancel(ctx), dl.ID, dl.GenerationKey, dl.Transfer, "client permanently rejected the exact submission: "+err.Error()); rejectErr != nil {
				s.log.Warn("permanent Runner refusal could not be persisted", "download", dl.ID, "err", rejectErr)
			}
			unlock()
			continue
		}
		updated, scheduleErr := s.db.ScheduleRunnerSubmissionRetry(context.WithoutCancel(ctx), dl.ID, dl.GenerationKey, time.Now(), err.Error(), submissionRetryDeadline(err))
		if scheduleErr != nil {
			s.log.Warn("Runner submission retry could not be scheduled", "download", dl.ID, "err", scheduleErr)
			unlock()
			continue
		}
		if !updated.BlockedAt.IsZero() && time.Since(updated.BlockedAt) >= runnerSubmissionBlockedWarning {
			warning := fmt.Sprintf("Automatic download submission has been uncertain for at least 30 minutes; the same Runner request will keep retrying. Last error: %s", err)
			_ = s.db.UpdateRunnerSubmissionWarning(context.WithoutCancel(ctx), dl.ID, dl.GenerationKey, warning)
		}
		unlock()
		s.log.Warn("automatic Runner submission replay deferred", "download", dl.ID, "err", err)
	}
}

func automaticSubmissionRetryable(dl sqlite.Download) bool {
	return dl.RequestTokenIdempotent && automaticSubmissionPending(dl)
}

func automaticSubmissionPending(dl sqlite.Download) bool {
	return dl.Intent != GrabIntentManualAdditional &&
		dl.SubmissionPhase != "rejected" && (dl.SubmissionPhase == "submitting" || dl.SubmissionPhase == "uncertain") &&
		dl.ParkedAt.UnixMilli() == 0 && dl.ParkedReason == "" && !dl.Superseded &&
		dl.State != "failed" && dl.State != "imported" && dl.State != "superseded"
}
