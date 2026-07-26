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
	// ManualApproval holds a completed download at 'awaiting_import' until
	// the user approves it, instead of importing automatically.
	ManualApproval bool
	// Mode is how Monarr learns this client's state: "poll" (ask every
	// 30 s) or "push" (hold the client's event stream open). Push still
	// polls underneath — it is an optimization, never a dependency — so
	// the worst case for a client wrongly set to push is the behavior it
	// had before. Empty means poll.
	Mode string
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

// TaggedAdder is an OPTIONAL capability: a client that can carry Monarr's
// transfer id onto the download itself, so the same id is visible in the
// client's UI, in whatever it reports back, and in Monarr's handoff trace.
// Grepping one id across both applications is the point.
//
// Optional rather than part of DownloadClient because most clients have
// nowhere to put it — qBittorrent tags and SABnzbd's nzo metadata are not
// the same thing and pretending otherwise would make every adapter lie
// about a capability only one of them has. Callers type-assert; a client
// that does not implement this gets a plain Add and Monarr keeps the id
// on its own row.
type TaggedAdder interface {
	AddTagged(ctx context.Context, downloadURL, category, transfer string) (Handle, error)
}

// ClientEventKind is what a pushed event says happened.
type ClientEventKind string

// Event kinds. Progress is advisory; the other three are decisions.
const (
	// EventProgress is a rate/percentage update. High frequency, no
	// decision attached — consumers throttle it.
	EventProgress ClientEventKind = "progress"
	// EventStage is post-processing moving between stages (verify,
	// repair, unpack). This is what turns "stuck" into "repairing, since
	// four minutes ago".
	EventStage ClientEventKind = "stage"
	// EventCompleted means the payload is finished AND ready to import —
	// past post-processing, with a path attached.
	EventCompleted ClientEventKind = "completed"
	// EventFailed means the client gave up on this download.
	EventFailed ClientEventKind = "failed"
	// EventReset means the stream could not be resumed and anything may
	// have been missed. The consumer must reconcile by polling before it
	// trusts the stream again. It is the difference between a consumer
	// that knows it is behind and one that silently is.
	EventReset ClientEventKind = "reset"
)

// ClientEvent is one pushed observation about one download.
type ClientEvent struct {
	Handle Handle
	Kind   ClientEventKind
	// Stage is the post-processing stage name when Kind is EventStage.
	Stage string
	// Status is populated for progress/completed/failed, in exactly the
	// shape the poller produces — so both channels feed one reconciler
	// rather than two that can disagree.
	Status DownloadStatus
	// Seq is the client's own event cursor, for logs and for saying which
	// channel delivered a step. Zero when the client does not number.
	Seq uint64
}

// Subscriber is an OPTIONAL capability: a client that can push state
// changes instead of waiting to be asked.
//
// Optional because most download clients have no such stream, and a port
// every adapter must implement by returning "unsupported" is a port that
// tells you nothing. Push is also never load-bearing — the poll keeps
// running underneath — so a client that cannot stream loses latency and
// nothing else.
//
// The returned channel is closed when ctx is cancelled or the stream ends
// unrecoverably. Implementations reconnect internally and report a gap
// they could not bridge as an EventReset rather than by closing.
type Subscriber interface {
	Subscribe(ctx context.Context) (<-chan ClientEvent, error)
}
