package ports

import (
	"context"
	"strings"
)

// PathMapping rewrites a completed-download path prefix the client reports
// into the path Monarr sees the same files at (client on another host or
// container). Sonarr calls this a "remote path mapping".
type PathMapping struct {
	Remote string `json:"remote"`
	Local  string `json:"local"`
}

// MapRemotePath applies the first matching mapping (whole path components
// only — "/data" must not match "/database"). No match returns p unchanged.
func MapRemotePath(maps []PathMapping, p string) string {
	for _, m := range maps {
		remote := strings.TrimRight(m.Remote, "/")
		if remote == "" {
			continue
		}
		local := strings.TrimRight(m.Local, "/")
		if p == remote {
			return local
		}
		if strings.HasPrefix(p, remote+"/") {
			return local + p[len(remote):]
		}
	}
	return p
}

// ClientConfig is a stored download client.
type ClientConfig struct {
	ID           int64
	Type         string // qbittorrent | sabnzbd | transmission | deluge | nzbget
	Name         string
	URL          string
	Username     string
	Password     string // API key for sabnzbd
	Category     string
	Enabled      bool
	PathMappings []PathMapping
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
