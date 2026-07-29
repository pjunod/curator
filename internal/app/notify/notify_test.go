package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// sink is a notifier whose behaviour a test can change between attempts.
type sink struct {
	mu        sync.Mutex
	sent      []ports.Notification
	err       error
	result    string
	permanent bool
	// seen runs inside Send, so a test can look at the world as it is DURING
	// the attempt — which is the only moment the in-flight view has anything
	// to say about this delivery.
	seen func()
}

func (s *sink) Send(_ context.Context, n ports.Notification) error {
	s.mu.Lock()
	s.sent = append(s.sent, n)
	hook := s.seen
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		return nil
	}
	if s.permanent {
		return fmt.Errorf("%w: %s", ports.ErrNotifyPermanent, s.err)
	}
	return s.err
}
func (s *sink) Test(context.Context) error { return nil }
func (s *sink) Delivery() string           { return s.result }
func (s *sink) attempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func queueFixture(t *testing.T, target ports.Notifier) (*sqlite.DB, *bus.Bus, *Dispatcher, context.Context) {
	t.Helper()
	db, err := sqlite.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
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
	d := New(db, b, nil, func(ports.NotifierConfig) ports.Notifier { return target })
	return db, b, d, ctx
}

func importEvent(downloadID int64) acquisition.ImportCompleted {
	return acquisition.ImportCompleted{
		MediaItemID: 1, DownloadID: downloadID, Release: "Heat.1995.1080p",
		Files: 1, MediaItemKind: "movie", Title: "Heat",
		Paths:    []string{"/media/movies/Heat (1995)/Heat (1995).mkv"},
		Dirs:     []string{"/media/movies/Heat (1995)"},
		TmdbID:   949,
		Transfer: "t-42-a3f9c1",
	}
}

// A plurx notification is queued, not fired and forgotten. The row is what
// survives the restart that loses an in-memory retry — which is the common
// failure, not an exotic one: the host reboots, both apps come back, and
// monarr's import finishes a few seconds before plurx is listening.
func TestAPlurxNotificationIsQueuedAndThenDelivered(t *testing.T) {
	target := &sink{result: "scanned → plurx item 1201"}
	db, b, d, ctx := queueFixture(t, target)

	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	b.Publish(importEvent(0))

	// It is a row before it is an attempt.
	var queuedRow sqlite.Delivery
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := db.ListDeliveries(ctx, 1, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 {
			queuedRow = rows[0]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if queuedRow.ID == 0 {
		t.Fatal("the import was never queued — nothing would survive a restart")
	}
	if target.attempts() != 0 {
		t.Error("the dispatcher sent inline; the queue is meant to own delivery")
	}

	d.deliverDue(ctx)
	if target.attempts() != 1 {
		t.Fatalf("attempts = %d, want 1", target.attempts())
	}
	rows, err := db.ListDeliveries(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Status != "ok" {
		t.Errorf("status = %q (%s)", rows[0].Status, rows[0].LastError)
	}
	if rows[0].Result != "scanned → plurx item 1201" {
		t.Errorf("result = %q — plurx's answer is the point of keeping the row", rows[0].Result)
	}
	got := rows[0].Payload
	if !strings.Contains(got, "t-42-a3f9c1") || !strings.Contains(got, "Heat (1995)") {
		t.Errorf("the stored payload cannot rebuild the request: %s", got)
	}
}

// A dead plurx is retried on a schedule that outlives the process, and
// eventually gives up and says so on the download's own trace — which is
// where §8 sends somebody asking "imported, but not in plurx".
func TestADeadPlurxIsRetriedThenRecordedOnTheTrace(t *testing.T) {
	target := &sink{err: errors.New("connection refused")}
	db, b, d, ctx := queueFixture(t, target)

	item, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Heat", Year: 1995, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownload(ctx, sqlite.Download{
		MediaItemID: item, ReleaseTitle: "Heat.1995.1080p", State: "imported",
		Transfer: "t-42-a3f9c1",
	})
	if err != nil {
		t.Fatal(err)
	}

	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	b.Publish(importEvent(dl))
	waitForQueue(t, db, ctx)

	// Attempt 1 fails and schedules a retry rather than giving up.
	d.deliverDue(ctx)
	rows, _ := db.ListDeliveries(ctx, 1, 10)
	if rows[0].Status != "pending" {
		t.Fatalf("status after one failure = %q, want pending", rows[0].Status)
	}
	if rows[0].NextAt <= time.Now().UnixMilli() {
		t.Error("the retry is due immediately — the backoff is not being applied")
	}
	if rows[0].LastError == "" {
		t.Error("a pending retry must say what went wrong so far")
	}

	// Run the schedule out. Each pass is forced due, standing in for the
	// wall-clock wait the worker would do.
	for i := 0; i < len(deliveryBackoff); i++ {
		forceDue(t, db, ctx, rows[0].ID)
		d.deliverDue(ctx)
		rows, _ = db.ListDeliveries(ctx, 1, 10)
	}
	if rows[0].Status != "failed" {
		t.Fatalf("status = %q after exhausting the schedule, want failed", rows[0].Status)
	}
	if want := len(deliveryBackoff) + 1; int(rows[0].Attempts) != want {
		t.Errorf("attempts = %d, want %d (the first try plus %d retries)",
			rows[0].Attempts, want, len(deliveryBackoff))
	}

	trace, err := db.GetDownload(ctx, dl)
	if err != nil {
		t.Fatal(err)
	}
	var step sqlite.HandoffEntry
	for _, h := range trace.Handoff {
		if h.Step == "notify_plurx" {
			step = h
		}
	}
	if step.Step == "" {
		t.Fatal("no notify_plurx step — the trace is where §8 sends people for exactly this")
	}
	if !strings.Contains(step.Detail, "connection refused") {
		t.Errorf("the trace must say why, got %q", step.Detail)
	}
	if trace.State != "imported" {
		t.Errorf("state = %q — a late notification must not move the download backwards", trace.State)
	}
}

// A key without the scope will not grow one. Retrying it only postpones the
// moment somebody reads the reason.
func TestAPermanentFailureIsNotRetried(t *testing.T) {
	target := &sink{err: errors.New("lacks the scan:trigger scope"), permanent: true}
	db, b, d, ctx := queueFixture(t, target)

	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	b.Publish(importEvent(0))
	waitForQueue(t, db, ctx)

	d.deliverDue(ctx)
	rows, _ := db.ListDeliveries(ctx, 1, 10)
	if rows[0].Status != "failed" {
		t.Errorf("status = %q, want failed on the first attempt", rows[0].Status)
	}
	if target.attempts() != 1 {
		t.Errorf("attempts = %d — a permanent failure must not be retried", target.attempts())
	}
}

func waitForQueue(t *testing.T, db *sqlite.DB, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := db.ListDeliveries(ctx, 1, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("nothing was queued")
}

// forceDue brings a scheduled retry forward, so a test exercises the
// schedule's shape without waiting out its wall clock.
func forceDue(t *testing.T, db *sqlite.DB, ctx context.Context, id int64) {
	t.Helper()
	rows, err := db.ListDeliveries(ctx, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID != id {
			continue
		}
		r.NextAt = time.Now().Add(-time.Second).UnixMilli()
		if err := db.SettleDelivery(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
}
