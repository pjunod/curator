package ports

import "context"

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
	// Paths are absolute as MONARR sees them. A consumer on another host
	// may need a path mapping — the same caveat download clients carry.
	Paths []string `json:"paths,omitempty"`
	// Episode distinguishes "these are episodes of a series" from a movie
	// or a book, which decides what TMDBID identifies.
	Episode bool `json:"episode,omitempty"`
	// TMDBID and IMDBID identify the item: for a series, the SHOW.
	TMDBID int64  `json:"tmdbId,omitempty"`
	IMDBID string `json:"imdbId,omitempty"`
	// Transfer names this transfer end to end, across applications.
	Transfer string `json:"transfer,omitempty"`
}

// Notifier delivers notifications. Library-refresh notifiers (Plex,
// Jellyfin) ignore the payload and poke the media server instead.
type Notifier interface {
	Send(ctx context.Context, n Notification) error
	Test(ctx context.Context) error
}
