package ports

import "context"

// StorageVolumeObservation describes a filesystem as measured by its owner.
type StorageVolumeObservation struct {
	Name       string  `json:"name"`
	Label      string  `json:"label"`
	Path       string  `json:"path"`
	FreeBytes  *uint64 `json:"available_bytes"`
	TotalBytes *uint64 `json:"total_bytes"`
	Current    bool    `json:"current"`
}

// DownloadStorageObserver is optional for download clients. A client may
// report old or unmeasured volumes when its own filesystem probes fail.
type DownloadStorageObserver interface {
	StorageVolumes(context.Context) ([]StorageVolumeObservation, error)
}
