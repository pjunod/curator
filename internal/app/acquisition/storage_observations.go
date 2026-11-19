package acquisition

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

const runnerStorageObservationsKey = "runner_storage_observations"

type StorageObservation struct {
	Owner                   string     `json:"owner"`
	Role                    string     `json:"role"`
	Name                    string     `json:"name,omitempty"`
	Path                    string     `json:"path"`
	Status                  string     `json:"status"`
	ObservedAt              *time.Time `json:"observedAt,omitempty"`
	FreeBytes               *uint64    `json:"freeBytes,omitempty"`
	TotalBytes              *uint64    `json:"totalBytes,omitempty"`
	Error                   string     `json:"error,omitempty"`
	StorageIdentity         string     `json:"storageIdentity,omitempty"`
	ObservedStorageIdentity string     `json:"observedStorageIdentity,omitempty"`
}

type CleanupObservation struct {
	LogicalBytesRemoved *int64     `json:"logicalBytesRemoved,omitempty"`
	FilesRemoved        *int64     `json:"filesRemoved,omitempty"`
	LastRemovalAt       *time.Time `json:"lastRemovalAt,omitempty"`
}

type StorageObservations struct {
	Roots   []StorageObservation `json:"roots"`
	Cleanup CleanupObservation   `json:"cleanup"`
}

// ScanStorageObservations refreshes the scheduled download inventory and reads
// how its native Runner peers measured their own volumes.
func (s *Service) ScanStorageObservations(ctx context.Context) error {
	runnerErr := s.refreshRunnerStorage(ctx)
	return errors.Join(s.ScanCompleted(ctx), runnerErr)
}

func (s *Service) refreshRunnerStorage(ctx context.Context) error {
	clients, err := s.db.ListDownloadClients(ctx)
	if err != nil {
		return err
	}
	previous, err := s.readRunnerStorage(ctx)
	if err != nil {
		return err
	}
	next := make(map[int64][]StorageObservation)
	for _, client := range clients {
		if !client.Enabled || !runnerStorageClient(client) {
			continue
		}
		observer, ok := s.newClient(client).(ports.DownloadStorageObserver)
		if !ok {
			continue
		}
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		volumes, err := observer.StorageVolumes(probe)
		cancel()
		if err != nil {
			stale := previous[client.ID]
			if len(stale) == 0 {
				stale = []StorageObservation{{Owner: "Runner", Role: "downloads", Name: client.Name}}
			}
			for i := range stale {
				stale[i].Status = "unavailable"
				stale[i].FreeBytes, stale[i].TotalBytes = nil, nil
				stale[i].Error = "Could not read storage from this Runner connection."
			}
			next[client.ID] = stale
			continue
		}
		reported := time.Now().UTC()
		for _, volume := range volumes {
			observation := StorageObservation{
				Owner: "Runner", Role: volume.Label, Name: client.Name, Path: volume.Path,
				Status: "unknown", ObservedAt: &reported,
			}
			if observation.Role == "" {
				observation.Role = volume.Name
			}
			if volume.Current && volume.FreeBytes != nil {
				observation.Status = "available"
				observation.FreeBytes, observation.TotalBytes = volume.FreeBytes, volume.TotalBytes
			} else if !volume.Current {
				observation.Status = "unavailable"
				observation.Error = "Runner did not have a current measurement for this volume."
			}
			next[client.ID] = append(next[client.ID], observation)
		}
	}
	if len(next) == 0 && len(previous) == 0 {
		return nil
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	return s.db.SetMeta(ctx, runnerStorageObservationsKey, string(raw))
}

func runnerStorageClient(client ports.ClientConfig) bool {
	return client.Type == "nzbd" || client.Type == "runner-torrent"
}

func (s *Service) readRunnerStorage(ctx context.Context) (map[int64][]StorageObservation, error) {
	raw, err := s.db.GetMeta(ctx, runnerStorageObservationsKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cached map[int64][]StorageObservation
	if err := json.Unmarshal([]byte(raw), &cached); err != nil {
		return nil, err
	}
	return cached, nil
}

// StorageObservations reads only persisted observations and ledger totals.
// SQLite projects the root metadata so inventory file rows never reach the
// Activity or Storage client.
func (s *Service) StorageObservations(ctx context.Context) (StorageObservations, error) {
	out := StorageObservations{Roots: []StorageObservation{}}
	local, err := s.observedCompletedRoots(ctx)
	if err != nil {
		return out, err
	}
	out.Roots = append(out.Roots, local...)
	remote, err := s.readRunnerStorage(ctx)
	if err != nil {
		return out, err
	}
	clients, err := s.db.ListDownloadClients(ctx)
	if err != nil {
		return out, err
	}
	for _, client := range clients {
		if !client.Enabled || !runnerStorageClient(client) {
			continue
		}
		if len(remote[client.ID]) == 0 {
			out.Roots = append(out.Roots, StorageObservation{
				Owner: "Runner", Role: "downloads", Name: client.Name, Status: "unknown",
			})
			continue
		}
		out.Roots = append(out.Roots, remote[client.ID]...)
	}
	out.Cleanup, err = s.recordedCleanup(ctx)
	return out, err
}

func (s *Service) observedCompletedRoots(ctx context.Context) ([]StorageObservation, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT root, json_extract(report,'$.checkedAt'),
		json_extract(report,'$.freeBytes'),json_extract(report,'$.totalBytes'),
		COALESCE(json_extract(report,'$.error'),''), COALESCE(json_extract(report,'$.storageIdentity'),''),
		COALESCE(json_extract(report,'$.observedStorageIdentity'),'')
		FROM completed_scans ORDER BY root`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []StorageObservation
	for rows.Next() {
		root := StorageObservation{Owner: "Curator", Role: "downloads", Status: "unknown"}
		var checked sql.NullString
		if err := rows.Scan(&root.Path, &checked, &root.FreeBytes, &root.TotalBytes, &root.Error, &root.StorageIdentity, &root.ObservedStorageIdentity); err != nil {
			return nil, err
		}
		if at, err := time.Parse(time.RFC3339Nano, checked.String); err == nil && !at.IsZero() {
			root.ObservedAt = &at
		}
		if root.Error != "" {
			root.Status = "unavailable"
			root.FreeBytes, root.TotalBytes = nil, nil
		} else if root.FreeBytes != nil {
			root.Status = "available"
		}
		out = append(out, root)
	}
	return out, rows.Err()
}

func (s *Service) recordedCleanup(ctx context.Context) (CleanupObservation, error) {
	var out CleanupObservation
	var last sql.NullInt64
	err := s.db.R.QueryRowContext(ctx, `SELECT SUM(json_extract(data,'$.bytes')), SUM(json_extract(data,'$.files')), MAX(ts)
		FROM history_events WHERE type=? AND json_valid(data) AND json_extract(data,'$.by')='per-file cleanup'
		AND json_type(data,'$.bytes')='integer' AND json_type(data,'$.files')='integer'
		AND json_extract(data,'$.bytes')>=0 AND json_extract(data,'$.files')>0`, HistoryPayloadRemoved).
		Scan(&out.LogicalBytesRemoved, &out.FilesRemoved, &last)
	if last.Valid {
		at := time.UnixMilli(last.Int64).UTC()
		out.LastRemovalAt = &at
	}
	return out, err
}
