package library

import (
	"context"
	"errors"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// ScanStatus describes the durable scan job, not the timer that enqueued it.
// Adoption and file probing have separate jobs and do not delay completion.
type ScanStatus struct {
	RequestedAt *time.Time `json:"requestedAt,omitempty"`
	ID          int64      `json:"id,omitempty"`
	State       string     `json:"state"`
	Attempts    int64      `json:"attempts,omitempty"`
	Error       string     `json:"error,omitempty"`
}

func (s *Service) ScanStatus(ctx context.Context) (ScanStatus, error) {
	j, err := s.db.LatestJob(ctx, JobScan)
	if errors.Is(err, sqlite.ErrNotFound) {
		// Deployments without a queue still run scans inline.
		report, ok, err := s.LastScanReport(ctx)
		if ok {
			return ScanStatus{State: "completed", RequestedAt: &report.ScannedAt}, err
		}
		return ScanStatus{State: "idle"}, err
	}
	if err != nil {
		return ScanStatus{}, err
	}
	state := map[domain.JobState]string{
		domain.JobQueued: "queued", domain.JobLeased: "running",
		domain.JobDone: "completed", domain.JobFailed: "failed",
	}[j.State]
	return ScanStatus{ID: j.ID, State: state, Attempts: j.Attempts, Error: j.LastError, RequestedAt: &j.CreatedAt}, nil
}
