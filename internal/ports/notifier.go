package ports

import (
	"context"
	"errors"
)

// ErrNotifyPermanent marks a delivery failure that retrying cannot fix: a
// path outside every library root, a key without the scope, a revoked
// credential. Wrap it and the delivery queue stops immediately instead of
// spending its whole backoff schedule postponing the moment somebody reads
// the reason.
var ErrNotifyPermanent = errors.New("permanent delivery failure")

// NotifierConfig is a stored notification target (Phase 3). Settings is
// type-specific: webhook/discord need {"url"}, plex needs {"url","token"},
// jellyfin and plurx need {"url","apiKey"}.
type NotifierConfig struct {
	ID       int64
	Type     string // webhook | discord | plex | jellyfin | plurx
	Name     string
	Settings map[string]string
	OnGrab   bool
	OnImport bool
	OnFailed bool
	OnHealth bool
	Enabled  bool
}

// Notification is one event rendered for the outside world. The JSON shape
// is the webhook wire format — lowercase, stable.
type Notification struct {
	Event string `json:"event"` // grab | import | failed | health | test
	Title string `json:"title"`
	Body  string `json:"body"`
	// Fields carries structured extras (release, quality, item title…).
	Fields map[string]string `json:"fields,omitempty"`
	// Import is the machine-readable half of an import event: what landed,
	// where, and what it is. Present only on `import`.
	//
	// Chat notifiers have no use for it and ignore it; the webhook passes it
	// through, which is the point of a webhook. A media server that can be
	// told "index exactly this path, it is tmdb 949" needs all of it — the
	// difference between one folder indexed correctly and a whole library
	// swept and then matched by guessing at a filename.
	Import *ImportInfo `json:"import,omitempty"`
}

// ImportInfo is the structured detail behind an import notification.
type ImportInfo struct {
	MediaItemID int64 `json:"mediaItemId,omitempty"`
	DownloadID  int64 `json:"downloadId,omitempty"`
	// Paths are every placed file; Dirs are their unique parent
	// directories — what a media server is actually asked to index. Both
	// are absolute as MONARR sees them: a consumer on another host may
	// need a path mapping, the same caveat download clients carry.
	Paths []string `json:"paths,omitempty"`
	Dirs  []string `json:"dirs,omitempty"`
	// Kind is movie | series | book. The plurx notifier uses it to keep
	// movie ids, series ids, and path-identified Books imports on their
	// honest request shapes.
	Kind  string `json:"kind,omitempty"`
	Title string `json:"title,omitempty"`
	// TmdbID and ImdbID identify the item: for a series, the SHOW.
	TmdbID int64  `json:"tmdbId,omitempty"`
	ImdbID string `json:"imdbId,omitempty"`
	// Transfer names this transfer end to end, across applications.
	Transfer string `json:"transfer,omitempty"`
}

// Notifier delivers notifications. Library-refresh notifiers (Plex,
// Jellyfin) ignore the payload and poke the media server instead.
type Notifier interface {
	Send(ctx context.Context, n Notification) error
	Test(ctx context.Context) error
}

// DeliveryReporter is an optional capability: a notifier whose far side
// answered with something worth keeping.
//
// Optional because most have nothing to say — a webhook returning 204 tells
// you it was accepted and no more. plurx answers a scan with what it made of
// the files, and that belongs in the transfer's trace: it is the only place
// the chain "grabbed → downloaded → imported → indexed as item 1201" is ever
// joined up. Asserted for after Send, in the same shape as the download
// clients' TaggedAdder and Subscriber.
type DeliveryReporter interface {
	// Delivery summarizes the last Send in one line, or "" if there is
	// nothing to add.
	Delivery() string
}
