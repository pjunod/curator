package notify

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/app/acquisition"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

type recorder struct {
	mu   sync.Mutex
	sent []ports.Notification
}

func (r *recorder) Send(_ context.Context, n ports.Notification) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, n)
	return nil
}
func (r *recorder) Test(context.Context) error { return nil }

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

func TestDispatcherRoutesByFlags(t *testing.T) {
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	b := bus.New(nil)
	t.Cleanup(b.Close)

	// One notifier that wants imports only; one media-server poke.
	if _, err := db.AddNotifier(ctx, ports.NotifierConfig{
		Type: "webhook", Name: "hook", OnImport: true, OnGrab: false, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddNotifier(ctx, ports.NotifierConfig{
		Type: "plex", Name: "plex", OnImport: true, OnGrab: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	rec := map[string]*recorder{"hook": {}, "plex": {}}
	d := New(db, b, nil, func(cfg ports.NotifierConfig) ports.Notifier { return rec[cfg.Name] })
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond) // let subscriptions attach

	// A grab: hook has OnGrab=false; plex is refresh-only (import events only).
	b.Publish(acquisition.ReleaseGrabbed{Title: "X.2024.1080p"})
	// An import: both fire.
	b.Publish(acquisition.ImportCompleted{Release: "X.2024.1080p", Files: 1})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec["hook"].count() == 1 && rec["plex"].count() == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := rec["hook"].count(); got != 1 {
		t.Errorf("hook notifications = %d, want 1 (import only)", got)
	}
	if got := rec["plex"].count(); got != 1 {
		t.Errorf("plex refreshes = %d, want 1 (imports only, no grab chatter)", got)
	}
	if len(rec["hook"].sent) > 0 && rec["hook"].sent[0].Event != "import" {
		t.Errorf("hook got event %q", rec["hook"].sent[0].Event)
	}
}
