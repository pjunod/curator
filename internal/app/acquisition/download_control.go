package acquisition

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// storeControl keeps the newest Runner control for display (pipeline contract
// v2, section 3.2). It is not a gate: the refusal matrix that compared
// revisions and instances is gone, because the transfer token now identifies
// the job and a terminal ends a hold unconditionally. A held control still
// reads as queued, so the row stays out of the import path while it lasts.
func (s *Service) storeControl(ctx context.Context, dl *sqlite.Download, st *ports.DownloadStatus) bool {
	next := st.Control
	if next == nil {
		return true
	}
	if dl.State == "failed" || dl.State == "imported" {
		return false
	}
	if next.Held() {
		st.State = ports.StateQueued
		st.Stage = next.Stage
		st.Message = next.Message
		dl.Error = next.Message
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return false
	}
	dl.RunnerControl = string(raw)
	dl.Progress = st.Progress
	if err := s.db.UpdateDownloadControl(ctx, *dl); err != nil {
		s.log.Warn("control persistence failed", "download", dl.ID, "err", err)
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
	if json.Unmarshal([]byte(dl.RunnerControl), &control) != nil || !control.Held() {
		return fmt.Errorf("this transfer is not held")
	}
	// Whether a hold can be resumed is Runner's to say: it answers 409 when a
	// resume is refused, and that answer reaches the caller unchanged.
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
