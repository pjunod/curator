package ports

import "context"

// ClientConfig is a stored download client.
type ClientConfig struct {
	ID       int64
	Type     string // qbittorrent | sabnzbd
	Name     string
	URL      string
	Username string
	Password string // API key for sabnzbd
	Category string
	Enabled  bool
}

// Handle identifies an item inside a download client (torrent hash, nzo id).
type Handle string

// DownloadState is the client-side lifecycle.
type DownloadState string

// Client-side states, reconciled onto the downloads table.
const (
	StateQueued      DownloadState = "queued"
	StateDownloading DownloadState = "downloading"
	StateCompleted   DownloadState = "completed"
	StateFailed      DownloadState = "failed"
)

// DownloadStatus is one item's live state in a client.
type DownloadStatus struct {
	Handle   Handle
	Name     string
	State    DownloadState
	Progress float64 // 0..1
	SavePath string  // where the payload lands when completed
	Message  string
}

// DownloadClient adds and tracks downloads (blueprint §5 ports).
type DownloadClient interface {
	Add(ctx context.Context, downloadURL, category string) (Handle, error)
	Statuses(ctx context.Context) ([]DownloadStatus, error)
	Remove(ctx context.Context, h Handle, deleteData bool) error
	Test(ctx context.Context) error
}
