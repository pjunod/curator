package ports

import (
	"context"
	"fmt"
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
	// RemoveCompleted deletes the payload from the client once monarr has
	// imported it.
	//
	// Off means every grab leaves a second full copy behind for as long as the
	// client keeps it, and when the client's directory and the library are on
	// different filesystems — the normal arrangement — that copy is real bytes
	// rather than a hardlink. It adds up faster than anyone expects.
	//
	// Not defaulted the same way for everything. Usenet has no obligation once
	// the download is done, so those default on. A torrent is still seeding,
	// and monarr cannot yet tell "finished seeding" from "seeding happily", so
	// those default off and stay a deliberate choice.
	RemoveCompleted bool
}

// ProtocolOfClient reports whether a client type speaks usenet or torrent.
// It lives here rather than in the acquisition service because the answer
// changes defaults in the API layer too — a usenet client has no obligation
// after a download and a torrent is still seeding, and that difference decides
// what RemoveCompleted starts as.
func ProtocolOfClient(clientType string) string {
	switch clientType {
	case "sabnzbd", "nzbget", "nzbd":
		return "usenet"
	}
	return "torrent"
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
	// Blameless marks a StateFailed the RELEASE is not responsible for, so
	// it must not be blocklisted.
	//
	// The distinction is not academic. A failed download normally means a bad
	// release, and blocklisting it is how Monarr avoids spending the night
	// re-grabbing the same broken copy. But "someone clicked delete in the
	// download client" arrives on exactly the same channel, and treating that
	// as the release's fault burns a good one and sends Monarr after a worse
	// copy — a punishment for an operator action, applied silently and
	// permanently.
	Blameless bool
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

// Capacity is a download client's own account of whether it can keep
// working — the queue-hold reasons, in its words.
//
// Every field here is a state where the client is up, answering, and
// downloading nothing. That combination is the one worth surfacing:
// "unreachable" is obvious from the outside and gets noticed, while "fine,
// but the destination disk is full" looks identical to "idle" until
// somebody wonders where last night's episodes went.
type Capacity struct {
	Version        string
	DiskLow        bool
	QuotaReached   bool
	BlockedServers int
	Paused         bool
	// HealthAbort is a POLICY, not a problem: nzbd sets it whenever
	// `[post] health_action` is park or delete, which is a deliberate and
	// sensible operator choice — it stops wasting bandwidth on a download
	// that cannot be repaired. It is reported as context, never as a
	// fault. See Problems().
	HealthAbort bool
}

// Problems renders the capacity as plain sentences, or nothing when there
// is nothing wrong. Plain words on purpose: a health page that says
// "quota_reached: true" has made the reader translate.
//
// HealthAbort is deliberately absent. Plan §5.6 maps it to an error, but
// the field does not mean what that reading assumed: nzbd derives it from
// `[post] health_action`, so it is true on any server configured to park or
// delete unrepairable downloads — a default-good setting, on permanently.
// Reporting it as an error produced a red badge that could never clear,
// which is the exact failure this file is careful about everywhere else.
func (c Capacity) Problems() []string {
	var out []string
	if c.DiskLow {
		out = append(out, "the destination disk is low on space")
	}
	if c.QuotaReached {
		out = append(out, "the download quota is used up")
	}
	if c.BlockedServers > 0 {
		out = append(out, fmt.Sprintf("%d news server(s) are blocked", c.BlockedServers))
	}
	if c.Paused {
		out = append(out, "the queue is paused")
	}
	return out
}

// CapacityReporter is an OPTIONAL capability: a client that can say why it
// is not downloading, beyond the queue itself.
//
// Optional because most clients have no such notion, and because a client
// that cannot answer must not thereby look unhealthy. Asserted for, in the
// same shape as TaggedAdder and Subscriber.
type CapacityReporter interface {
	Capacity(ctx context.Context) (Capacity, error)
}
