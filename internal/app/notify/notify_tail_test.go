package notify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// flaky records what it was sent and can be told to fail, which is how the
// fan-out tests below tell "did not match" apart from "matched and broke".
type flaky struct {
	mu     sync.Mutex
	events []string
	err    error
	seen   func(ports.Notification)
}

func (f *flaky) Send(_ context.Context, n ports.Notification) error {
	f.mu.Lock()
	f.events = append(f.events, n.Event)
	seen, err := f.seen, f.err
	f.mu.Unlock()
	if seen != nil {
		seen(n)
	}
	return err
}
func (f *flaky) Test(context.Context) error { return nil }
func (f *flaky) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// tailMovie is the library row a download can hang a handoff trace off.
func tailMovie() domain.MediaItem {
	return domain.MediaItem{Kind: domain.KindMovie, Title: "Heat", Year: 1995, Monitored: true}
}

func tailDB(t *testing.T) (*sqlite.DB, context.Context) {
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
	return db, ctx
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Each notifier opts into events one flag at a time, and a notifier that did
// not ask for an event must not be told about it. Getting this wrong is not
// a cosmetic bug: it is a Discord channel filling up with health flaps
// somebody deliberately switched off.
func TestTailEveryEventReachesOnlyItsSubscribers(t *testing.T) {
	db, ctx := tailDB(t)
	b := bus.New(nil)
	t.Cleanup(b.Close)

	targets := map[string]*flaky{}
	for _, spec := range []struct {
		name string
		cfg  ports.NotifierConfig
	}{
		{"grab", ports.NotifierConfig{Type: "webhook", Name: "grab", OnGrab: true, Enabled: true}},
		{"import", ports.NotifierConfig{Type: "webhook", Name: "import", OnImport: true, Enabled: true}},
		{"failed", ports.NotifierConfig{Type: "webhook", Name: "failed", OnFailed: true, Enabled: true}},
		{"health", ports.NotifierConfig{Type: "webhook", Name: "health", OnHealth: true, Enabled: true}},
		// Enabled but subscribed to nothing: the "I set it up and turned
		// everything off" configuration, which must stay silent.
		{"silent", ports.NotifierConfig{Type: "webhook", Name: "silent", Enabled: true}},
		// Every flag on but disabled: the switch has to win over the flags.
		{"off", ports.NotifierConfig{Type: "webhook", Name: "off", OnGrab: true,
			OnImport: true, OnFailed: true, OnHealth: true, Enabled: false}},
	} {
		if _, err := db.AddNotifier(ctx, spec.cfg); err != nil {
			t.Fatal(err)
		}
		targets[spec.name] = &flaky{}
	}

	d := New(db, b, nil, func(cfg ports.NotifierConfig) ports.Notifier { return targets[cfg.Name] })
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond) // let the subscriptions attach

	b.Publish(acquisition.ReleaseGrabbed{Title: "X.2024.1080p", Indexer: "nzbs", Protocol: "usenet"})
	b.Publish(acquisition.ImportCompleted{Release: "X.2024.1080p", Files: 1})
	b.Publish(acquisition.ImportFailed{Release: "X.2024.1080p", Reason: "unpack failed"})
	b.Publish(health.Changed{Overall: "degraded"})

	for _, name := range []string{"grab", "import", "failed", "health"} {
		waitUntil(t, name+" to be notified", func() bool { return len(targets[name].got()) == 1 })
		if got := targets[name].got(); got[0] != name {
			t.Errorf("%s notifier received event %q", name, got[0])
		}
	}
	// Give anything mis-routed a moment to arrive before declaring silence.
	time.Sleep(100 * time.Millisecond)
	if got := targets["silent"].got(); len(got) != 0 {
		t.Errorf("a notifier subscribed to nothing received %v", got)
	}
	if got := targets["off"].got(); len(got) != 0 {
		t.Errorf("a DISABLED notifier received %v — the enable switch is not "+
			"being honoured", got)
	}
}

// One dead notifier must not take the others down with it. The whole reason
// the fan-out is a loop and not a chain is that a webhook pointed at a host
// that no longer exists is the normal steady state of a homelab.
func TestTailOneBrokenNotifierDoesNotSuppressTheRest(t *testing.T) {
	db, ctx := tailDB(t)
	b := bus.New(nil)
	t.Cleanup(b.Close)

	for _, name := range []string{"broken", "healthy"} {
		if _, err := db.AddNotifier(ctx, ports.NotifierConfig{
			Type: "webhook", Name: name, OnImport: true, Enabled: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	targets := map[string]*flaky{
		"broken":  {err: errors.New("dial tcp: connection refused")},
		"healthy": {},
	}
	d := New(db, b, nil, func(cfg ports.NotifierConfig) ports.Notifier { return targets[cfg.Name] })
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	// A transfer id on the event: the failure log is the only place it gets
	// to say WHICH handoff broke, so the notification has to carry it.
	b.Publish(acquisition.ImportCompleted{
		Release: "X.2024.1080p", Files: 1, Transfer: "t-42-a3f9c1",
		Paths: []string{"/media/movies/X (2024)/X.mkv"},
	})

	waitUntil(t, "the healthy notifier to be reached", func() bool {
		return len(targets["healthy"].got()) == 1
	})
	if len(targets["broken"].got()) != 1 {
		t.Error("the broken notifier was never attempted")
	}
}

// The import event carries the same facts twice on purpose: a sentence for
// the notifiers that render text, and the structured half for the ones that
// act on it. A media server told "something changed" pays a full library
// sweep; told "this directory is tmdb 949" it does one folder.
func TestTailAnImportCarriesBothItsSentenceAndItsFacts(t *testing.T) {
	db, ctx := tailDB(t)
	b := bus.New(nil)
	t.Cleanup(b.Close)
	if _, err := db.AddNotifier(ctx, ports.NotifierConfig{
		Type: "webhook", Name: "hook", OnImport: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var got ports.Notification
	target := &flaky{seen: func(n ports.Notification) {
		mu.Lock()
		got = n
		mu.Unlock()
	}}
	d := New(db, b, nil, func(ports.NotifierConfig) ports.Notifier { return target })
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	b.Publish(importEvent(42))

	waitUntil(t, "the import notification", func() bool { return len(target.got()) == 1 })
	mu.Lock()
	defer mu.Unlock()
	if got.Fields["files"] != "1" || got.Fields["upgrade"] != "false" {
		t.Errorf("fields = %v", got.Fields)
	}
	if got.Import == nil {
		t.Fatal("no structured import half — a media server has nothing to act on")
	}
	if got.Import.TmdbID != 949 || got.Import.Kind != "movie" || got.Import.Transfer != "t-42-a3f9c1" {
		t.Errorf("import facts = %+v", got.Import)
	}
	if len(got.Import.Paths) != 1 || len(got.Import.Dirs) != 1 {
		t.Errorf("paths/dirs = %v / %v", got.Import.Paths, got.Import.Dirs)
	}
}

// A media server poke IS the work, not an announcement of it, so it goes
// through the durable queue. Everything else is fired inline — a Discord
// message arriving two minutes late with no way to tell it from a fresh one
// is worse than not arriving.
func TestTailOnlyPlurxGoesThroughTheDurableQueue(t *testing.T) {
	for _, tc := range []struct {
		typ    string
		queued bool
	}{
		{"plurx", true},
		{"plex", false},
		{"jellyfin", false},
		{"webhook", false},
		{"discord", false},
	} {
		if got := queued(ports.NotifierConfig{Type: tc.typ}); got != tc.queued {
			t.Errorf("%s: queued = %v, want %v", tc.typ, got, tc.queued)
		}
	}
	// Media-server types are refresh-only: a grab or a health flap gives
	// them nothing to index.
	for _, typ := range []string{"plex", "jellyfin", "plurx"} {
		if !refreshOnly(ports.NotifierConfig{Type: typ}) {
			t.Errorf("%s should be refresh-only", typ)
		}
	}
	for _, typ := range []string{"webhook", "discord"} {
		if refreshOnly(ports.NotifierConfig{Type: typ}) {
			t.Errorf("%s is a person, not an index — it is not refresh-only", typ)
		}
	}
}

// wants is the flag lookup the fan-out turns on. An event name nothing knows
// about must match NOTHING: defaulting to true would make a new event type
// spam every notifier the moment it was published.
func TestTailAnUnknownEventMatchesNoNotifier(t *testing.T) {
	all := ports.NotifierConfig{OnGrab: true, OnImport: true, OnFailed: true, OnHealth: true}
	for _, event := range []string{"", "test", "rename", "upgrade"} {
		if wants(all, event) {
			t.Errorf("event %q matched a notifier that never opted into it", event)
		}
	}
	for event, want := range map[string]bool{
		"grab": true, "import": true, "failed": true, "health": true,
	} {
		if !wants(all, event) {
			t.Errorf("event %q did not match an all-flags notifier (want %v)", event, want)
		}
		if wants(ports.NotifierConfig{}, event) {
			t.Errorf("event %q matched a notifier with every flag off", event)
		}
	}
}

// "Imported, but plurx has not been told yet" is exactly the handoff state
// that used to be invisible — deducible only from a delivery log somebody
// had to know to open. It has to be ON the in-flight view while the attempt
// is running, and gone once it settles.
func TestTailADeliveryIsVisibleOnTheInFlightViewWhileItRuns(t *testing.T) {
	reg := transfers.New()
	var mu sync.Mutex
	var duringStage string
	var duringDetail string
	target := &sink{seen: func() {
		tr, ok := reg.Stage(42)
		mu.Lock()
		if ok {
			duringStage, duringDetail = tr.Stage, tr.Detail
		}
		mu.Unlock()
	}}
	db, b, d, ctx := queueFixture(t, target)
	d = d.WithRegistry(reg)

	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	b.Publish(importEvent(42))
	waitForQueue(t, db, ctx)

	d.deliverDue(ctx)

	mu.Lock()
	defer mu.Unlock()
	if duringStage != transfers.StageNotifying {
		t.Errorf("stage during delivery = %q, want notifying — the handoff was "+
			"invisible while it was happening", duringStage)
	}
	if !strings.Contains(duringDetail, "plurx") {
		t.Errorf("detail = %q, want it to say what the attempt is", duringDetail)
	}
	if n := reg.Count(""); n != 0 {
		t.Errorf("in flight = %d after the attempt settled, want 0", n)
	}
}

// A retry says which attempt it is and what went wrong last time, because
// "notifying" on its own does not tell anybody whether to wait or to go and
// look at plurx.
func TestTailARetryOnTheInFlightViewSaysWhichAttemptItIs(t *testing.T) {
	reg := transfers.New()
	var mu sync.Mutex
	var details []string
	target := &sink{err: errors.New("connection refused"), seen: func() {
		tr, ok := reg.Stage(42)
		mu.Lock()
		if ok {
			details = append(details, tr.Detail)
		}
		mu.Unlock()
	}}
	db, b, d, ctx := queueFixture(t, target)
	d = d.WithRegistry(reg)

	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	b.Publish(importEvent(42))
	waitForQueue(t, db, ctx)

	d.deliverDue(ctx)
	rows, _ := db.ListDeliveries(ctx, 1, 10)
	forceDue(t, db, ctx, rows[0].ID)
	d.deliverDue(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(details) != 2 {
		t.Fatalf("observed %d attempts, want 2", len(details))
	}
	if details[0] != "telling plurx what landed" {
		t.Errorf("first attempt detail = %q", details[0])
	}
	if !strings.Contains(details[1], "retry 2") ||
		!strings.Contains(details[1], "connection refused") {
		t.Errorf("second attempt detail = %q, want the attempt number and the "+
			"previous error", details[1])
	}
}

// A notifier deleted while its delivery sat in the queue leaves nothing to
// deliver to and nothing anybody can act on. Retrying it three more times
// would just be three more writes into a hole.
func TestTailADeliveryToADeletedNotifierIsSettledNotRetried(t *testing.T) {
	target := &sink{}
	db, _, d, ctx := queueFixture(t, target)

	// Queue against an id that does not exist — the state left behind when
	// somebody removes a notifier between the import and the delivery.
	if _, err := db.EnqueueDelivery(ctx, 9999, 0, "import", `{"event":"import"}`); err != nil {
		t.Fatal(err)
	}
	d.deliverDue(ctx)

	rows, err := db.ListDeliveries(ctx, 9999, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(rows))
	}
	if rows[0].Status != "failed" {
		t.Errorf("status = %q, want failed on the first pass", rows[0].Status)
	}
	if !strings.Contains(rows[0].LastError, "no longer exists") {
		t.Errorf("last error = %q, want it to say the notifier is gone", rows[0].LastError)
	}
	if target.attempts() != 0 {
		t.Error("a delivery with no notifier still tried to send something")
	}
}

// A payload that will not decode cannot be turned into a request, and it
// will not decode on the fourth attempt either. This is what a row written
// by an older build looks like after an upgrade changed the shape.
func TestTailAnUnreadablePayloadIsPermanent(t *testing.T) {
	target := &sink{}
	db, _, d, ctx := queueFixture(t, target)

	if _, err := db.EnqueueDelivery(ctx, 1, 0, "import", `{"event":`); err != nil {
		t.Fatal(err)
	}
	d.deliverDue(ctx)

	rows, _ := db.ListDeliveries(ctx, 1, 10)
	if len(rows) != 1 || rows[0].Status != "failed" {
		t.Fatalf("rows = %+v, want one failed", rows)
	}
	if !strings.Contains(rows[0].LastError, "unreadable payload") {
		t.Errorf("last error = %q", rows[0].LastError)
	}
	if target.attempts() != 0 {
		t.Error("an undecodable payload was still sent somewhere")
	}
}

// The delivery worker runs on a timer because a retry scheduled two minutes
// out has no event to wake it — and because Run is driven by the bus and
// would stall the event loop while a dead server timed out.
func TestTailRunDeliveriesDrainsTheQueueOnItsOwnTimer(t *testing.T) {
	target := &sink{result: "scanned → plurx item 1201"}
	db, _, d, ctx := queueFixture(t, target)

	if _, err := db.EnqueueDelivery(ctx, 1, 0, "import", `{"event":"import"}`); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); d.RunDeliveries(runCtx) }()

	waitUntil(t, "the worker to drain the queue without being told", func() bool {
		rows, err := db.ListDeliveries(ctx, 1, 10)
		return err == nil && len(rows) == 1 && rows[0].Status == "ok"
	})
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunDeliveries did not stop when its context was cancelled")
	}
}

// A cancelled context stops the drain where it is rather than working
// through a backlog during shutdown, when every attempt is about to be
// interrupted anyway.
func TestTailACancelledContextStopsTheDrain(t *testing.T) {
	target := &sink{}
	db, _, d, ctx := queueFixture(t, target)
	for i := 0; i < 3; i++ {
		if _, err := db.EnqueueDelivery(ctx, 1, 0, "import", `{"event":"import"}`); err != nil {
			t.Fatal(err)
		}
	}
	dead, cancel := context.WithCancel(ctx)
	cancel()
	d.deliverDue(dead)

	if target.attempts() != 0 {
		t.Errorf("attempts = %d during shutdown, want 0", target.attempts())
	}
}

// A delivery with no download behind it — a health poke, a manual test —
// has no handoff trace to write onto, and inventing one would attach the
// step to download 0.
func TestTailADeliveryWithNoDownloadWritesNoTrace(t *testing.T) {
	target := &sink{err: errors.New("connection refused"), permanent: true}
	db, _, d, ctx := queueFixture(t, target)

	if _, err := db.EnqueueDelivery(ctx, 1, 0, "import", `{"event":"import"}`); err != nil {
		t.Fatal(err)
	}
	d.deliverDue(ctx) // must not panic or write a step for download 0

	rows, _ := db.ListDeliveries(ctx, 1, 10)
	if len(rows) != 1 || rows[0].Status != "failed" {
		t.Fatalf("rows = %+v, want one failed", rows)
	}
}

// A successful delivery records what plurx made of it on the download's own
// trace — §8 sends people there for "imported, but not in plurx", so the
// entry has to answer that without a second lookup.
func TestTailASuccessfulDeliveryRecordsPlurxsAnswerOnTheTrace(t *testing.T) {
	target := &sink{result: "scanned → plurx item 1201"}
	db, b, d, ctx := queueFixture(t, target)

	item, err := db.CreateMediaItem(ctx, tailMovie())
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownload(ctx, sqlite.Download{
		MediaItemID: item, ReleaseTitle: "Heat.1995.1080p", State: "imported",
	})
	if err != nil {
		t.Fatal(err)
	}
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	b.Publish(importEvent(dl))
	waitForQueue(t, db, ctx)
	d.deliverDue(ctx)

	trace, err := db.GetDownload(ctx, dl)
	if err != nil {
		t.Fatal(err)
	}
	var detail string
	for _, h := range trace.Handoff {
		if h.Step == "notify_plurx" {
			detail = h.Detail
		}
	}
	if !strings.Contains(detail, "scanned → plurx item 1201") {
		t.Errorf("trace detail = %q — plurx's own answer is the point of "+
			"keeping the row", detail)
	}
}

// A notifier that reports no delivery result still gets a trace entry: "we
// told plurx and it did not say what it did" is a different answer from
// "nobody ever told plurx", and only one of them is a problem.
func TestTailASilentSuccessStillWritesATrace(t *testing.T) {
	target := &sink{} // no Delivery() result
	db, b, d, ctx := queueFixture(t, target)

	item, err := db.CreateMediaItem(ctx, tailMovie())
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownload(ctx, sqlite.Download{
		MediaItemID: item, ReleaseTitle: "Heat.1995.1080p", State: "imported",
	})
	if err != nil {
		t.Fatal(err)
	}
	go d.Run(ctx)
	time.Sleep(50 * time.Millisecond)
	b.Publish(importEvent(dl))
	waitForQueue(t, db, ctx)
	d.deliverDue(ctx)

	trace, err := db.GetDownload(ctx, dl)
	if err != nil {
		t.Fatal(err)
	}
	var detail string
	for _, h := range trace.Handoff {
		if h.Step == "notify_plurx" {
			detail = h.Detail
		}
	}
	if detail != "plurx notified" {
		t.Errorf("trace detail = %q, want a plain confirmation", detail)
	}
}

// The store going away is not a reason to take the process with it: a
// dispatcher that panicked on a closed database would turn a shutdown race
// into a crash loop.
func TestTailAnUnreadableStoreIsLoggedNotFatal(t *testing.T) {
	db, ctx := tailDB(t)
	b := bus.New(nil)
	t.Cleanup(b.Close)
	if _, err := db.AddNotifier(ctx, ports.NotifierConfig{
		Type: "webhook", Name: "hook", OnImport: true, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	target := &flaky{}
	d := New(db, b, nil, func(ports.NotifierConfig) ports.Notifier { return target })
	db.Close()

	d.dispatch(ctx, "import", ports.Notification{Event: "import"})
	d.deliverDue(ctx)

	if len(target.got()) != 0 {
		t.Error("notifications were sent from a store that could not be read")
	}
}
