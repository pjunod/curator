package ports

import "context"

// NotifierConfig is a stored notification target (Phase 3). Settings is
// type-specific: webhook/discord need {"url"}, plex needs {"url","token"},
// jellyfin needs {"url","apiKey"}.
type NotifierConfig struct {
	ID       int64
	Type     string // webhook | discord | plex | jellyfin
	Name     string
	Settings map[string]string
	OnGrab   bool
	OnImport bool
	OnFailed bool
	OnHealth bool
	Enabled  bool
}

// Notification is one event rendered for the outside world.
type Notification struct {
	Event string // grab | import | failed | health
	Title string
	Body  string
	// Fields carries structured extras (release, quality, item title…).
	Fields map[string]string
}

// Notifier delivers notifications. Library-refresh notifiers (Plex,
// Jellyfin) ignore the payload and poke the media server instead.
type Notifier interface {
	Send(ctx context.Context, n Notification) error
	Test(ctx context.Context) error
}
