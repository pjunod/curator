package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
	"math/big"
)

// acceptDownloadControl runs under the same storage/row locks as acquisition
// decisions. Absence is legacy, never evidence that a persisted hold resolved.
func (s *Service) acceptDownloadControl(ctx context.Context, dl *sqlite.Download, st *ports.DownloadStatus) bool {
	var prior *ports.DownloadControl
	if dl.RunnerControl != "" {
		if err := json.Unmarshal([]byte(dl.RunnerControl), &prior); err != nil {
			return false
		}
	}
	next := st.Control
	if next == nil {
		return !prior.Held()
	}
	// No blanket resurrection of historical terminal rows.
	if dl.State == "failed" || dl.State == "imported" {
		return false
	}
	revision, ok := new(big.Int).SetString(next.Revision, 10)
	if !ok || revision.Sign() <= 0 || len(next.Revision) > 20 || next.Instance == "" {
		return false
	}
	if prior != nil {
		if prior.Instance != next.Instance {
			// Continuity loss requires explicit reconciliation; retain custody.
			return false
		}
		previous, valid := new(big.Int).SetString(prior.Revision, 10)
		if !valid || revision.Cmp(previous) <= 0 {
			return false
		}
		if prior.Held() && !next.Held() {
			if next.Version != 1 || next.Lifecycle != "running" ||
				(prior.Cause != "capacity" && prior.Cause != "quota") ||
				next.Cause != prior.Cause || next.Stage != prior.Stage {
				return false
			}
		}
	}
	if next.Held() {
		st.State = ports.StateQueued
		st.Stage = next.Stage
		st.Message = next.Message
		dl.State = "downloading" // remains in every existing active-row query
		dl.Error = next.Message
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return false
	}
	dl.RunnerControl = string(raw)
	dl.Progress = st.Progress
	if err := s.db.UpdateDownloadControl(ctx, *dl); err != nil {
		s.log.Warn("control persistence failed; keeping acquisition suppressed", "download", dl.ID, "err", err)
		return false
	}
	return true
}

func heldWantOverlap(row sqlite.Download, itemID, copyID int64, wants []string) bool {
	if row.RunnerControl == "" || row.MediaItemID != itemID || row.CopyID != copyID {
		return false
	}
	var control *ports.DownloadControl
	if json.Unmarshal([]byte(row.RunnerControl), &control) != nil || !control.Held() {
		return false
	}
	for _, want := range wants {
		if wantableOnRow(row, want) {
			return true
		}
		for _, existing := range row.WantableIDs {
			candidate := sqlite.Download{WantableIDs: []string{want}}
			if wantableOnRow(candidate, existing) {
				return true
			}
		}
	}
	return false
}

func (s *Service) ResumeHeldDownload(ctx context.Context, id int64) error {
	unlock := s.lockDownload(id)
	defer unlock()
	dl, err := s.db.GetDownload(ctx, id)
	if err != nil {
		return err
	}
	var control *ports.DownloadControl
	if json.Unmarshal([]byte(dl.RunnerControl), &control) != nil || !control.Held() || control.RetryPolicy != "resume_same_job" {
		return fmt.Errorf("this transfer requires custody review")
	}
	cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID)
	if err != nil {
		return err
	}
	resumer, ok := s.newClient(cfg).(ports.SameJobResumer)
	if !ok {
		return fmt.Errorf("download client cannot resume this job")
	}
	// The hold remains persisted until a newer authoritative control snapshot.
	return resumer.Resume(ctx, ports.Handle(dl.Handle))
}

func persistedDownloadHeld(raw string) bool {
	if raw == "" {
		return false
	}
	var control ports.DownloadControl
	return json.Unmarshal([]byte(raw), &control) != nil || control.Held()
}
