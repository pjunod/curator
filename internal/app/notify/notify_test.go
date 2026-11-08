package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
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
	// An import: both fire, from the rows the import wrote.
	queueImport(t, db, ctx, acquisition.ImportCompleted{
		MediaItemID: 84, Release: "The.Dispossessed.EPUB", Files: 1,
		MediaItemKind: "book", Title: "The Dispossessed",
		BookTitle: "The Dispossessed", BookAuthor: "Ursula K. Le Guin",
		BookMedium:    "ebook",
		BookWorkID:    "curator:openlibrary:OL87320W",
		BookEditionID: "curator:item:84:ebook",
		BookCoverURL:  "https://covers.openlibrary.org/b/id/123-L.jpg",
	})
	d.deliverDue(ctx)

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
	if len(rec["hook"].sent) > 0 {
		book := rec["hook"].sent[0].Import.Book
		if book == nil || book.Medium != "ebook" ||
			book.WorkID != "curator:openlibrary:OL87320W" ||
			book.EditionID != "curator:item:84:ebook" ||
			book.Author != "Ursula K. Le Guin" {
			t.Errorf("book handoff = %+v", book)
		}
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
	// The bus carries the import as a live signal only; nobody is told
	// from it (contract section 4.1).
	b.Publish(importEvent(0))
	time.Sleep(50 * time.Millisecond)
	if rows, _ := db.ListDeliveries(ctx, 1, 10); len(rows) != 0 || target.attempts() != 0 {
		t.Fatalf("the bus event queued %d rows / sent %d — notifications come from the import's own rows", len(rows), target.attempts())
	}
	queueImport(t, db, ctx, importEvent(0))
	if rows, _ := db.ListDeliveries(ctx, 1, 10); len(rows) != 1 || rows[0].Directory != "/media/movies/Heat (1995)" || rows[0].IdempotencyKey == "" {
		t.Fatalf("rows = %+v, want one sub-delivery for the one directory", rows)
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

// A delivery that failed permanently is not gone: once the cause is fixed
// (a key given its scope, a root mounted where Cinema looks), putting it back
// on the queue makes the worker send it again with the schedule starting
// over and a fresh attempt, and Cinema scans afresh.
func TestAFailedDeliveryCanBeRequeuedAndThenDelivered(t *testing.T) {
	target := &sink{err: errors.New("the key is valid but lacks the scan:trigger scope (403)"), permanent: true}
	db, _, d, ctx := queueFixture(t, target)
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

	queueImport(t, db, ctx, importEvent(dl))
	d.deliverDue(ctx)
	rows, _ := db.ListDeliveries(ctx, 1, 10)
	if rows[0].Status != "failed" {
		t.Fatalf("status = %q after a permanent failure, want failed", rows[0].Status)
	}
	exhausted := target.attempts()
	firstAttempt := rows[0].Attempt

	// The key gains its scope; the operator presses Retry.
	target.mu.Lock()
	target.err, target.result = nil, "scanned → plurx item 1201"
	target.mu.Unlock()
	requeued, err := db.RetryFailedDeliveries(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 1 {
		t.Fatalf("requeued = %d, want 1", requeued)
	}
	rows, _ = db.ListDeliveries(ctx, 1, 10)
	if rows[0].Status != "pending" || rows[0].Attempts != 0 {
		t.Fatalf("after retry: status %q attempts %d, want pending with a fresh schedule",
			rows[0].Status, rows[0].Attempts)
	}
	if rows[0].NextAt > time.Now().UnixMilli() {
		t.Error("a retry the operator asked for should be due now, not after a backoff")
	}
	if rows[0].LastError == "" {
		t.Error("the reason it failed must stay readable until the next attempt replaces it")
	}
	if rows[0].Attempt != firstAttempt+1 {
		t.Errorf("attempt = %d after Retry, want %d: the same key, a fresh scan", rows[0].Attempt, firstAttempt+1)
	}

	d.deliverDue(ctx)
	if target.attempts() != exhausted+1 {
		t.Fatalf("attempts = %d, want %d — the worker did not pick the requeued row up",
			target.attempts(), exhausted+1)
	}
	rows, _ = db.ListDeliveries(ctx, 1, 10)
	if rows[0].Status != "ok" || rows[0].Attempts != 1 {
		t.Errorf("after redelivery: status %q attempts %d (%s), want ok on the first attempt of the new schedule",
			rows[0].Status, rows[0].Attempts, rows[0].LastError)
	}
	if rows[0].Result != "scanned → plurx item 1201" || rows[0].LastError != "" {
		t.Errorf("result %q lastError %q — a delivered retry reads like any other delivery",
			rows[0].Result, rows[0].LastError)
	}
	// Nothing left to retry.
	if n, _ := db.RetryFailedDeliveries(ctx, 1); n != 0 {
		t.Errorf("a second sweep requeued %d rows, want 0", n)
	}

	// The download's trace tells the whole story in order: it failed, then
	// it was retried and delivered. Two steps, not one overwritten.
	trace, err := db.GetDownload(ctx, dl)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, h := range trace.Handoff {
		if h.Step == "notify_plurx" {
			steps = append(steps, h.Detail)
		}
	}
	if len(steps) != 2 || !strings.Contains(steps[0], "failed after 1 attempt") ||
		!strings.Contains(steps[1], "scanned → plurx item 1201") {
		t.Errorf("notify_plurx steps = %q, want the failure and then the delivery", steps)
	}
}

// A dead plurx is retried on a schedule that outlives the process and never
// ends for a transient failure (contract section 4.1): 5 s, 30 s, 2 m, 5 m,
// 15 m, 1 h, then hourly. A missed scan is a file that never appears in the
// library, so giving up is not an outcome; the delivery log shows it as a
// warning after half an hour instead.
func TestADeadPlurxIsRetriedWithoutEnd(t *testing.T) {
	target := &sink{err: errors.New("connection refused")}
	db, _, d, ctx := queueFixture(t, target)

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
	queueImport(t, db, ctx, importEvent(dl))

	want := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute,
		15 * time.Minute, time.Hour, time.Hour, time.Hour, time.Hour, time.Hour}
	var rows []sqlite.Delivery
	for i, gap := range want {
		before := time.Now()
		if i > 0 {
			forceDue(t, db, ctx, rows[0].ID)
		}
		d.deliverDue(ctx)
		rows, _ = db.ListDeliveries(ctx, 1, 10)
		if rows[0].Status != "pending" {
			t.Fatalf("try %d: status = %q, want pending — a transient failure never ends a delivery", i+1, rows[0].Status)
		}
		got := time.UnixMilli(rows[0].NextAt).Sub(before)
		if got < gap-time.Second || got > gap+2*time.Second {
			t.Errorf("try %d: next try in %v, want %v", i+1, got, gap)
		}
		if rows[0].LastError == "" {
			t.Error("a pending retry must say what went wrong so far")
		}
	}
	trace, err := db.GetDownload(ctx, dl)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range trace.Handoff {
		if h.Step == "notify_plurx" {
			t.Errorf("a delivery still being retried wrote a final trace step: %q", h.Detail)
		}
	}
	if trace.State != "imported" {
		t.Errorf("state = %q — a late notification must not move the download backwards", trace.State)
	}
}

// A key without the scope will not grow one. Retrying it only postpones the
// moment somebody reads the reason.
func TestAPermanentFailureIsNotRetried(t *testing.T) {
	target := &sink{err: errors.New("lacks the scan:trigger scope"), permanent: true}
	db, _, d, ctx := queueFixture(t, target)
	queueImport(t, db, ctx, importEvent(0))

	d.deliverDue(ctx)
	rows, _ := db.ListDeliveries(ctx, 1, 10)
	if rows[0].Status != "failed" {
		t.Errorf("status = %q, want failed on the first attempt", rows[0].Status)
	}
	if target.attempts() != 1 {
		t.Errorf("attempts = %d — a permanent failure must not be retried", target.attempts())
	}
}

// queueImport writes the outbox rows an import writes, for the notifiers
// configured now — the only way an import reaches a notifier.
func queueImport(t *testing.T, db *sqlite.DB, ctx context.Context, e acquisition.ImportCompleted) {
	t.Helper()
	notifiers, err := db.ListNotifiers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rows, _, err := acquisition.ImportOutbox(notifiers, e, "gen-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnqueueDeliveries(ctx, rows); err != nil {
		t.Fatal(err)
	}
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
		if err := db.SettleDelivery(ctx, r, sqlite.DeliveryTiming{}); err != nil {
			t.Fatal(err)
		}
	}
}
