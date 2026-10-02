package acquisition

import (
	"context"
	"encoding/json"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// observeTorrentStall counts fresh successful operational observations, never
// tracker scrape counts or wall time across an outage/pause.
func (s *Service) observeTorrentStall(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, st ports.DownloadStatus) {
	if dl.PlanID == 0 || dl.Protocol != "torrent" || dl.Superseded {
		return
	}
	var ob executionObservation
	_ = json.Unmarshal([]byte(dl.Observation), &ob)
	now := time.Now()
	eligible := st.State == ports.StateDownloading && (st.OperationalState == "downloading" || st.OperationalState == "stalledDL" || st.OperationalState == "forcedDL") && !persistedDownloadHeld(dl.RunnerControl) && st.BytesCompleted != nil
	if !eligible {
		ob.LastStallObservation = time.Time{}
		_ = s.db.UpdateDownloadObservation(ctx, dl.ID, ob)
		return
	}
	bytes := *st.BytesCompleted
	if !ob.BytesKnown || bytes > ob.LastBytes {
		ob.LastBytes = bytes
		ob.BytesKnown = true
		ob.LastProgress = now
		ob.ShortStall = 0
		ob.LongStall = 0
	} else if !ob.LastStallObservation.IsZero() && now.Sub(ob.LastStallObservation) <= 5*time.Minute && bytes == ob.LastBytes {
		delta := now.Sub(ob.LastStallObservation)
		ob.LongStall += delta
		poor := (st.ConnectedSeeds != nil && *st.ConnectedSeeds == 0) || (st.Availability != nil && *st.Availability < 1)
		if poor {
			ob.ShortStall += delta
		} else {
			ob.ShortStall = 0
		}
	}
	ob.LastStallObservation = now
	_ = s.db.UpdateDownloadObservation(ctx, dl.ID, ob)
	if ob.ShortStall < 48*time.Hour && ob.LongStall < 7*24*time.Hour {
		return
	}
	// Preserve partial custody unless safe ownership is proved by the cleanup
	// coordinator. Removal must be confirmed before replacement can start.
	client := s.newClient(cfg)
	fresh, err := client.Statuses(ctx)
	if err != nil {
		return
	}
	current, ok := matchStatus(dl, fresh)
	if !ok || current.BytesCompleted == nil || *current.BytesCompleted != bytes || current.State != ports.StateDownloading || (current.OperationalState != "downloading" && current.OperationalState != "stalledDL" && current.OperationalState != "forcedDL") || current.Control.Held() || (ob.LongStall < 7*24*time.Hour && !((current.ConnectedSeeds != nil && *current.ConnectedSeeds == 0) || (current.Availability != nil && *current.Availability < 1))) {
		return
	}
	if err = client.Remove(ctx, ports.Handle(dl.Handle), false); err != nil {
		_ = s.db.ParkDownload(ctx, dl.ID, "stalled torrent removal uncertain; episode remains reserved")
		return
	}
	after, err := client.Statuses(ctx)
	if err != nil {
		_ = s.db.ParkDownload(ctx, dl.ID, "stalled torrent retirement awaiting confirmation")
		return
	}
	if _, ok = matchStatus(dl, after); ok {
		_ = s.db.ParkDownload(ctx, dl.ID, "stalled torrent still reported by client")
		return
	}
	_, _ = s.db.W.ExecContext(ctx, `UPDATE downloads SET cleanup_pending=1,save_path=?,import_path=?,parked_at=0,parked_reason='partial payload cleanup pending' WHERE id=?`, st.SavePath, ports.MapRemotePath(cfg.PathMappings, st.SavePath), dl.ID)
	s.handleFailure(ctx, dl, st.Progress, "torrent retired after observed no-progress threshold; partial payload cleanup pending", false)
}

func (s *Service) resetStallObservation(ctx context.Context, dl sqlite.Download) {
	if dl.PlanID == 0 || dl.Protocol != "torrent" {
		return
	}
	unlock := s.lockDownload(dl.ID)
	defer unlock()
	fresh, err := s.db.GetDownload(ctx, dl.ID)
	if err != nil {
		return
	}
	var ob executionObservation
	_ = json.Unmarshal([]byte(fresh.Observation), &ob)
	ob.LastStallObservation = time.Time{}
	_ = s.db.UpdateDownloadObservation(ctx, dl.ID, ob)
}
