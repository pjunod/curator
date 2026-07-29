package acquisition

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/ports"
)

// The push channel's own state is what the Connections panel reports, and it is
// the only way anybody can tell a live subscription from one that died quietly.
// A stream that is broken while the UI says "connected" is worse than no UI at
// all, so these assert the link state moves with reality.

// deadSubscriber is a client whose stream will not open.
type deadSubscriber struct {
	fakeClient
	err error
}

func (d *deadSubscriber) Subscribe(context.Context) (<-chan ports.ClientEvent, error) {
	return nil, d.err
}

// pushCfg registers an enabled push-mode client and returns its config.
func acq2PushCfg(t *testing.T, svc *Service) ports.ClientConfig {
	t.Helper()
	ctx := context.Background()
	id, err := svc.db.AddDownloadClient(ctx, ports.ClientConfig{
		Type: "nzbd", Name: "nzbd", URL: "http://nzbd:6789",
		Category: "monarr", Enabled: true, Mode: "push",
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := svc.db.GetDownloadClient(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A subscription that is up says so, remembers the last sequence number it saw,
// and goes down when the stream ends. Without the sequence number a stalled
// stream and a quiet one look identical.
func TestAcq2SubscriptionsReportTheLiveLink(t *testing.T) {
	client := newPushClient()
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	cfg := acq2PushCfg(t, svc)
	dl := grabOne(t, svc, db, itemID, "Test.Show.S01E01.1080p.WEB-DL-SUB")

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.subscribeTo(ctx, cfg, client)
	}()

	client.events <- ports.ClientEvent{
		Handle: ports.Handle(dl.Handle), Kind: ports.EventProgress, Seq: 42,
		Status: ports.DownloadStatus{
			Handle: ports.Handle(dl.Handle), State: ports.StateDownloading, Progress: 0.25,
		},
	}
	waitFor(t, func() bool {
		for _, l := range svc.Subscriptions() {
			if l.ClientID == cfg.ID && l.Connected && l.LastSeq == 42 {
				return true
			}
		}
		return false
	})
	links := svc.Subscriptions()
	if len(links) != 1 || links[0].Name != "nzbd" || links[0].Mode != "push" {
		t.Fatalf("links = %+v", links)
	}
	if links[0].Since.IsZero() || links[0].LastEvent.IsZero() {
		t.Errorf("link carries no timestamps: %+v", links[0])
	}
	// An event IS contact for a push client — it is the only kind there is
	// between polls, and without it the contact clock ages out on a client
	// that is working perfectly.
	if c, ok := svc.Contacts()[cfg.ID]; !ok || c.At.IsZero() {
		t.Errorf("an event did not count as contact: %+v", svc.Contacts())
	}

	close(client.events)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("subscribeTo did not return when the stream ended")
	}
	for _, l := range svc.Subscriptions() {
		if l.Connected {
			t.Errorf("the link still claims to be connected after the stream ended: %+v", l)
		}
	}
}

// A stream that will not open must be reported as down WITH the reason. This is
// the difference between "nzbd is refusing us" and a blank panel.
func TestAcq2SubscribeReportsAStreamThatWillNotOpen(t *testing.T) {
	dead := &deadSubscriber{err: errors.New("connection refused")}
	svc, _, _ := setup(t, nil, &dead.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return dead }
	cfg := acq2PushCfg(t, svc)

	svc.subscribeTo(context.Background(), cfg, dead)

	links := svc.Subscriptions()
	if len(links) != 1 {
		t.Fatalf("links = %+v", links)
	}
	if links[0].Connected {
		t.Error("a subscription that never opened is reported as connected")
	}
	if links[0].LastError != "connection refused" {
		t.Errorf("last error = %q, want the client's own words", links[0].LastError)
	}
}

// The supervisor is not a one-shot: clients are re-read so switching one to
// push in the settings UI takes effect without a restart, and cancelling the
// context has to stop every stream it started.
func TestAcq2RunSubscribersStartsAndStopsWithItsContext(t *testing.T) {
	client := newPushClient()
	svc, _, _ := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	acq2PushCfg(t, svc)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.RunSubscribers(ctx)
	}()

	waitFor(t, func() bool { return client.subscriptions() == 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunSubscribers ignored its cancelled context")
	}
}

// Already-running subscriptions are left alone. A supervisor that re-subscribed
// on every pass would open a new stream every 30 seconds and leak the old one.
func TestAcq2SyncSubscribersDoesNotResubscribeWhatIsAlreadyRunning(t *testing.T) {
	client := newPushClient()
	svc, _, _ := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	cfg := acq2PushCfg(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	running := map[int64]context.CancelFunc{}
	svc.syncSubscribers(ctx, running)
	waitFor(t, func() bool { return client.subscriptions() == 1 })
	svc.syncSubscribers(ctx, running)
	svc.syncSubscribers(ctx, running)
	if got := client.subscriptions(); got != 1 {
		t.Errorf("opened %d streams for one client", got)
	}
	if len(running) != 1 || running[cfg.ID] == nil {
		t.Errorf("running = %v", running)
	}
}

// Configured for push against a client that cannot stream: say so once and
// leave it polling. Pretending would leave the download stuck until somebody
// noticed nothing was arriving.
func TestAcq2APushClientThatCannotStreamStaysOnThePoll(t *testing.T) {
	plain := &fakeClient{} // no Subscribe method at all
	svc, _, _ := setup(t, nil, plain)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return plain }
	acq2PushCfg(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	running := map[int64]context.CancelFunc{}
	svc.syncSubscribers(ctx, running)
	if len(running) != 0 {
		t.Errorf("started %d subscription(s) on a client that cannot push", len(running))
	}
}

// Progress is coalesced, not dropped. Once the window has passed the held
// percentage has to actually reach the database, or the bar never moves for a
// download that only ever reports progress.
func TestAcq2CoalescedProgressReachesTheDatabaseAfterTheWindow(t *testing.T) {
	client := newPushClient()
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	dl := grabOne(t, svc, db, itemID, "Test.Show.S01E01.1080p.WEB-DL-FLUSH")
	cfg, _ := db.GetDownloadClient(ctx, dl.ClientID)

	progress := func(p float64) ports.ClientEvent {
		return ports.ClientEvent{
			Handle: ports.Handle(dl.Handle), Kind: ports.EventProgress,
			Status: ports.DownloadStatus{
				Handle: ports.Handle(dl.Handle), State: ports.StateDownloading, Progress: p,
			},
		}
	}
	pending := map[int64]float64{}
	flushed := time.Now()
	svc.handleEvent(ctx, cfg, progress(0.1), pending, &flushed) // moves it to 'downloading'

	// Inside the window: held, not written.
	flushed = time.Now()
	svc.handleEvent(ctx, cfg, progress(0.6), pending, &flushed)
	if got, _ := db.GetDownload(ctx, dl.ID); got.Progress > 0.5 {
		t.Fatalf("progress %v was written inside the flush window", got.Progress)
	}

	// Window elapsed: the newest percentage lands and nothing stays held.
	flushed = time.Now().Add(-2 * progressFlush)
	svc.handleEvent(ctx, cfg, progress(0.75), pending, &flushed)
	got, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Progress < 0.7 {
		t.Errorf("progress = %v, want the coalesced value flushed once the window passed", got.Progress)
	}
	if len(pending) != 0 {
		t.Errorf("%d download(s) still held after a flush", len(pending))
	}
}

// nzbd emits events that name no download (heartbeats, and anything a future
// version adds). They are not errors and must not become one.
func TestAcq2AnEventWithNoHandleIsIgnored(t *testing.T) {
	client := newPushClient()
	svc, db, itemID := setup(t, nil, &client.fakeClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return client }
	ctx := context.Background()
	dl := grabOne(t, svc, db, itemID, "Test.Show.S01E01.1080p.WEB-DL-NOHANDLE")
	cfg, _ := db.GetDownloadClient(ctx, dl.ClientID)

	var flushed time.Time
	svc.handleEvent(ctx, cfg, ports.ClientEvent{Kind: ports.EventCompleted}, map[int64]float64{}, &flushed)

	got, _ := db.GetDownload(ctx, dl.ID)
	if got.State != "grabbed" {
		t.Errorf("an anonymous event moved the download to %q", got.State)
	}
}
