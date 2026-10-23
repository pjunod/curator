package acquisition

import (
	"context"
	"fmt"
	"time"
)

const manualAdmissionBlockedWarning = 30 * time.Minute

// startManualAdmissionWorker resumes reserved manual requests independently
// of Activity or a browser retry. The database row already contains the exact
// URL, category, priority, copy, generation, and transfer token needed to
// dispatch the same action.
func (s *Service) startManualAdmissionWorker(ctx context.Context) {
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		process := func() {
			now := time.Now()
			retries, err := s.db.ListManualAdmissionRetries(ctx, now, 32)
			if err != nil {
				s.log.Warn("manual admission retry scan failed", "err", err)
				return
			}
			for _, retry := range retries {
				if ctx.Err() != nil {
					return
				}
				if _, err := s.resumeManualRequest(ctx, retry.DownloadID); err != nil {
					if _, loadErr := s.db.GetDownload(ctx, retry.DownloadID); loadErr == nil {
						_ = s.db.ScheduleManualAdmissionRetry(context.WithoutCancel(ctx), retry.DownloadID, time.Now(), err.Error())
						blockedSince := retry.BlockedAt
						if blockedSince.IsZero() {
							blockedSince = now
						}
						if now.Sub(blockedSince) >= manualAdmissionBlockedWarning {
							warning := fmt.Sprintf("Manual download submission has been blocked for at least 30 minutes; retry remains queued. Last error: %s", err)
							_ = s.db.SetManualAdmissionWarning(context.WithoutCancel(ctx), retry.DownloadID, warning)
						}
					}
					s.log.Warn("manual admission recovery deferred", "download", retry.DownloadID, "err", err)
				} else if !retry.BlockedAt.IsZero() && now.Sub(retry.BlockedAt) >= manualAdmissionBlockedWarning {
					if dl, loadErr := s.db.GetDownload(ctx, retry.DownloadID); loadErr == nil && dl.SubmissionPhase == "submitted" && dl.Handle == "" && dl.ExactTransferCorrelation {
						warning := "Manual request was accepted but the client has not exposed the exact transfer token for 30 minutes; review the client before retrying."
						_ = s.db.SetManualAdmissionWarning(context.WithoutCancel(ctx), retry.DownloadID, warning)
					}
				}
			}
		}
		process()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				process()
			}
		}
	}()
}
