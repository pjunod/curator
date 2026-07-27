package notify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/app/acquisition"
	"github.com/monarr-media/monarr/internal/domain"
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

type failer struct{ recorder }

func (f *failer) Send(ctx context.Context, n ports.Notification) error {
	_ = f.recorder.Send(ctx, n)
	return errors.New("plurx unreachable")
}

// The structured half of an import has to reach the notifier that acts on
// it, and the delivery has to be recorded where somebody would look. A
// notification that failed is otherwise visible only in the server log,
// which is the last place anyone checks and the first place this matters.
func TestDispatcherCarriesImportDetailAndRecordsTheDelivery(t *testing.T) {
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

	if _, err := db.AddNotifier(ctx, ports.NotifierConfig{
		Type: "plurx", Name: "plurx", OnImport: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	item, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Heat", Year: 1995, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	sink := &failer{}
	d := New(db, b, nil, func(ports.NotifierConfig) ports.Notifier { return sink })
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	b.Publish(acquisition.ImportCompleted{
		MediaItemID: item, Release: "Heat.1995.1080p", Files: 1,
		Paths:    []string{"/media/movies/Heat (1995)/Heat (1995).mkv"},
		TMDBID:   949,
		Transfer: "t-42-a3f9c1",
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sink.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if sink.count() != 1 {
		t.Fatalf("plurx notifier called %d times", sink.count())
	}
	got := sink.sent[0]
	if got.Import == nil {
		t.Fatal("the import detail did not reach the notifier — it would have nothing to index")
	}
	if len(got.Import.Paths) != 1 || got.Import.TMDBID != 949 {
		t.Errorf("import detail = %+v", got.Import)
	}
	if got.Import.Transfer != "t-42-a3f9c1" {
		t.Errorf("transfer = %q", got.Import.Transfer)
	}

	// The failure landed on the item's own history, next to the import.
	var recorded bool
	for time.Now().Before(deadline) && !recorded {
		hist, err := db.ListHistory(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hist {
			if h.Type == "notify_failed" && h.MediaItemID == item {
				recorded = true
			}
		}
		if !recorded {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !recorded {
		t.Error("a failed delivery left no trace anywhere a user would look")
	}
}
