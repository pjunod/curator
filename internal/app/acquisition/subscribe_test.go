package acquisition

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
)

// pushClient is a fake that can stream. Tests drive its channel directly,
// which is the only way to assert what an event DOES without waiting on a
// real network.
type pushClient struct {
	fakeClient
	events chan ports.ClientEvent
	subs   int
	mu     sync.Mutex
}

func newPushClient() *pushClient {
	return &pushClient{events: make(chan ports.ClientEvent, 16)}
}

func (c *pushClient) Subscribe(ctx context.Context) (<-chan ports.ClientEvent, error) {
	c.mu.Lock()
	c.subs++
	c.mu.Unlock()
	return c.events, nil
}

func (c *pushClient) subscriptions() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.subs
}

// grabOne puts a real download row in flight and returns it.
func grabOne(t *testing.T, svc *Service, db *sqlite.DB, itemID int64, title string) sqlite.Download {
	t.Helper()
	ctx := context.Background()
	id, err := svc.Grab(ctx, GrabRequest{
		MediaItemID: itemID, Season: 1, Episode: 1, Title: title,
		DownloadURL: "https://idx/" + title, Indexer: "idx", Protocol: "torrent", Size: 1,
	})
	if err != nil {
		t.Fatalf("Grab: %v", err)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return dl
}

// The reason push exists: a completion event moves the download without
// anyone waiting for the next poll.
func TestACompletionEventImportsWithoutWaitingForAPoll(t *testing.T) {
	client := newPushClient()
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	dl := grabOne(t, svc, db, itemID, "Test.Show.S01E01.1080p.WEB-DL-P")

	cfg, err := db.GetDownloadClient(ctx, dl.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	var flushed time.Time
	svc.handleEvent(ctx, cfg, ports.ClientEvent{
		Handle: ports.Handle(dl.Handle), Kind: ports.EventCompleted, Seq: 913,
		Status: ports.DownloadStatus{
			Handle: ports.Handle(dl.Handle), State: ports.StateCompleted,
			Progress: 1, SavePath: t.TempDir(),
		},
	}, map[int64]float64{}, &flushed)

	got, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State == "grabbed" || got.State == "downloading" {
		t.Fatalf("state = %q — the event did not move the download", got.State)
	}
	if got.SavePath == "" {
		t.Error("the path from the event was not recorded")
	}
	// Which channel delivered a step is the difference between knowing
	// push works and assuming it does.
	var sawEvent bool
	for _, h := range got.Handoff {
		if h.Step == stepDownloaded && contains(h.Detail, "event 913") {
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Errorf("trace does not say the event delivered it: %+v", got.Handoff)
	}
}

// A stage event updates the trace but must never be mistaken for
// completion — importing mid-unpack finds a half-extracted folder.
func TestAStageEventDoesNotImport(t *testing.T) {
	client := newPushClient()
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	dl := grabOne(t, svc, db, itemID, "Test.Show.S01E01.1080p.WEB-DL-Q")
	cfg, _ := db.GetDownloadClient(ctx, dl.ClientID)

	var flushed time.Time
	svc.handleEvent(ctx, cfg, ports.ClientEvent{
		Handle: ports.Handle(dl.Handle), Kind: ports.EventStage, Stage: "unpack",
		Status: ports.DownloadStatus{
			Handle: ports.Handle(dl.Handle), State: ports.StateDownloading,
			Message: "post-processing: unpack",
		},
	}, map[int64]float64{}, &flushed)

	got, _ := db.GetDownload(ctx, dl.ID)
	if got.State != "downloading" {
		t.Fatalf("state = %q, want still downloading during post-processing", got.State)
	}
	var saidStage bool
	for _, h := range got.Handoff {
		if contains(h.Detail, "unpack") {
			saidStage = true
		}
	}
	if !saidStage {
		t.Errorf("the stage is not in the trace: %+v — 'stuck' should read as 'unpacking since…'", got.Handoff)
	}
}

// nzbd ticks at 1 Hz. Writing every tick through to SQLite would be tens
// of thousands of writes a day per download to move a progress bar.
func TestProgressIsCoalescedButStateChangesAreNot(t *testing.T) {
	client := newPushClient()
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	dl := grabOne(t, svc, db, itemID, "Test.Show.S01E01.1080p.WEB-DL-R")
	cfg, _ := db.GetDownloadClient(ctx, dl.ClientID)

	// Get it into 'downloading' first; only then is progress coalesced.
	var flushed time.Time
	progressEv := func(p float64) ports.ClientEvent {
		return ports.ClientEvent{
			Handle: ports.Handle(dl.Handle), Kind: ports.EventProgress,
			Status: ports.DownloadStatus{
				Handle: ports.Handle(dl.Handle), State: ports.StateDownloading, Progress: p,
			},
		}
	}
	pending := map[int64]float64{}
	svc.handleEvent(ctx, cfg, progressEv(0.1), pending, &flushed)
	flushed = time.Now() // the first event flushed; start the window

	for _, p := range []float64{0.2, 0.3, 0.4} {
		svc.handleEvent(ctx, cfg, progressEv(p), pending, &flushed)
	}
	if pending[dl.ID] != 0.4 {
		t.Errorf("held progress = %v, want the newest value coalesced", pending[dl.ID])
	}
	got, _ := db.GetDownload(ctx, dl.ID)
	if got.Progress > 0.15 {
		t.Errorf("progress %v reached the database inside the flush window", got.Progress)
	}

	// A completion is a decision and must not wait behind a progress bar.
	svc.handleEvent(ctx, cfg, ports.ClientEvent{
		Handle: ports.Handle(dl.Handle), Kind: ports.EventCompleted,
		Status: ports.DownloadStatus{
			Handle: ports.Handle(dl.Handle), State: ports.StateCompleted,
			Progress: 1, SavePath: t.TempDir(),
		},
	}, pending, &flushed)
	if _, held := pending[dl.ID]; held {
		t.Error("a decision left stale progress held for this download")
	}
	got, _ = db.GetDownload(ctx, dl.ID)
	if got.State == "downloading" {
		t.Error("the completion waited behind coalesced progress")
	}
}

// A gap means anything could have been missed, so the poll — which IS the
// reconcile — has to run rather than waiting out its interval.
func TestAResetTriggersAReconcilePoll(t *testing.T) {
	client := newPushClient()
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	dl := grabOne(t, svc, db, itemID, "Test.Show.S01E01.1080p.WEB-DL-S")
	cfg, _ := db.GetDownloadClient(ctx, dl.ClientID)

	// The client's poll view says the download finished — which the
	// stream never told us, because the stream had a gap.
	client.statuses = []ports.DownloadStatus{{
		Handle: ports.Handle(dl.Handle), State: ports.StateCompleted,
		Progress: 1, SavePath: t.TempDir(),
	}}
	var flushed time.Time
	svc.handleEvent(ctx, cfg, ports.ClientEvent{Kind: ports.EventReset}, map[int64]float64{}, &flushed)

	got, _ := db.GetDownload(ctx, dl.ID)
	if got.State == "grabbed" {
		t.Fatal("a reset did not reconcile — a gap in the stream must fall back to the poll")
	}
}

// An event about something Monarr never grabbed is not an error, and must
// not become one: nzbd is allowed to have other downloads in it.
func TestEventsForUnknownDownloadsAreIgnored(t *testing.T) {
	client := newPushClient()
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	_ = grabOne(t, svc, db, itemID, "Test.Show.S01E01.1080p.WEB-DL-T")
	cfg, _ := db.GetDownloadClient(ctx, 1)

	var flushed time.Time
	svc.handleEvent(ctx, cfg, ports.ClientEvent{
		Handle: "99999", Kind: ports.EventCompleted,
		Status: ports.DownloadStatus{Handle: "99999", State: ports.StateCompleted},
	}, map[int64]float64{}, &flushed)
	// No panic, no state change anywhere: nothing to assert but the
	// absence of damage.
	rows, _ := db.ListRecentDownloads(ctx)
	for _, r := range rows {
		if r.State != "grabbed" {
			t.Errorf("an unrelated event moved download %d to %q", r.ID, r.State)
		}
	}
}

// The poll and an event genuinely race — an event can arrive in the same
// instant the sweep reads the same client. Without serialization both see
// 'downloaded' and both import, which places the files twice and writes a
// trace nobody can read.
func TestPollAndEventCannotBothImport(t *testing.T) {
	client := newPushClient()
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	dl := grabOne(t, svc, db, itemID, "Test.Show.S01E01.1080p.WEB-DL-U")
	cfg, _ := db.GetDownloadClient(ctx, dl.ClientID)

	done := ports.DownloadStatus{
		Handle: ports.Handle(dl.Handle), State: ports.StateCompleted,
		Progress: 1, SavePath: t.TempDir(),
	}
	client.statuses = []ports.DownloadStatus{done}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_ = svc.RefreshQueue(ctx)
				return
			}
			var flushed time.Time
			svc.handleEvent(ctx, cfg, ports.ClientEvent{
				Handle: ports.Handle(dl.Handle), Kind: ports.EventCompleted, Status: done,
			}, map[int64]float64{}, &flushed)
		}(i)
	}
	wg.Wait()

	got, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	var downloaded, imported int
	for _, h := range got.Handoff {
		switch h.Step {
		case stepDownloaded:
			downloaded++
		case stepImported:
			imported++
		}
	}
	if downloaded > 1 {
		t.Errorf("the handoff records %d 'downloaded' steps; the race was not serialized:\n%+v",
			downloaded, got.Handoff)
	}
	if imported > 1 {
		t.Errorf("the handoff records %d 'imported' steps — the import ran more than once:\n%+v",
			imported, got.Handoff)
	}
}

// The supervisor starts a subscription for a push client, and only for a
// push client — a poll-mode client must not hold a stream open.
func TestSupervisorSubscribesOnlyToPushClients(t *testing.T) {
	client := newPushClient()
	svc, db, _ := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	running := map[int64]context.CancelFunc{}
	svc.syncSubscribers(ctx, running) // the seeded client is poll mode
	if len(running) != 0 {
		t.Fatalf("subscribed to %d poll-mode client(s)", len(running))
	}

	id, err := db.AddDownloadClient(ctx, ports.ClientConfig{
		Type: "nzbd", Name: "nzbd", URL: "http://nzbd:6789",
		Category: "monarr", Enabled: true, Mode: "push",
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.syncSubscribers(ctx, running)
	if _, ok := running[id]; !ok {
		t.Fatalf("no subscription started for the push client: %v", running)
	}
	waitFor(t, func() bool { return client.subscriptions() == 1 })

	// Switching it back to poll must stop the stream, or the setting is
	// only half a setting.
	cfg, _ := db.GetDownloadClient(ctx, id)
	cfg.Mode = "poll"
	if err := db.UpdateDownloadClient(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	svc.syncSubscribers(ctx, running)
	if _, ok := running[id]; ok {
		t.Error("the subscription survived being switched back to poll")
	}
}

// Mode defaults to poll — for a fresh client, and for one stored before
// the column existed. Anything else would silently change behavior on
// upgrade.
func TestModeDefaultsToPoll(t *testing.T) {
	_, db, _ := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	id, err := db.AddDownloadClient(ctx, ports.ClientConfig{
		Type: "nzbd", Name: "n", URL: "http://x", Category: "monarr", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetDownloadClient(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "poll" {
		t.Errorf("mode = %q, want poll", got.Mode)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && stringsContains(s, sub) }

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never became true")
}
