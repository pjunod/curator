package health

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/ports"
)

type fakeClient struct {
	err     error
	delay   time.Duration
	probes  *int32
	handles []ports.DownloadStatus
}

func (f *fakeClient) Add(context.Context, string, string) (ports.Handle, error) {
	return "", nil
}
func (f *fakeClient) Statuses(context.Context) ([]ports.DownloadStatus, error) {
	return f.handles, nil
}
func (f *fakeClient) Remove(context.Context, ports.Handle, bool) error { return nil }
func (f *fakeClient) Test(ctx context.Context) error {
	if f.probes != nil {
		atomic.AddInt32(f.probes, 1)
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.err
}

type fakeNotifier struct {
	err    error
	probes *int32
}

func (f *fakeNotifier) Send(context.Context, ports.Notification) error { return nil }
func (f *fakeNotifier) Test(context.Context) error {
	if f.probes != nil {
		atomic.AddInt32(f.probes, 1)
	}
	return f.err
}

func deps(clients []ports.ClientConfig, notifiers []ports.NotifierConfig,
	c func(ports.ClientConfig) ports.DownloadClient,
	n func(ports.NotifierConfig) ports.Notifier) ConnectionDeps {
	return ConnectionDeps{
		Clients:     func(context.Context) ([]ports.ClientConfig, error) { return clients, nil },
		Notifiers:   func(context.Context) ([]ports.NotifierConfig, error) { return notifiers, nil },
		NewClient:   c,
		NewNotifier: n,
	}
}

func TestConnectionsReportsWhichOneIsDownAndStaysAWarning(t *testing.T) {
	check := Connections(deps(
		[]ports.ClientConfig{
			{Name: "qb", Type: "qbittorrent", Enabled: true},
			{Name: "nzbd", Type: "nzbd", Enabled: true},
		},
		[]ports.NotifierConfig{{Name: "plurx", Type: "plurx", Enabled: true}},
		func(cfg ports.ClientConfig) ports.DownloadClient {
			if cfg.Name == "nzbd" {
				return &fakeClient{err: errors.New("connection refused")}
			}
			return &fakeClient{}
		},
		func(ports.NotifierConfig) ports.Notifier { return &fakeNotifier{} },
	))

	res := check(context.Background())
	// A warning, not an error: Monarr queues and catches up when a client is
	// down, and a red badge that clears itself teaches people to ignore it.
	if res.Status != StatusWarning {
		t.Errorf("status = %s, want warning", res.Status)
	}
	if !strings.Contains(res.Message, "nzbd") {
		t.Errorf("the message must name the failing connection: %q", res.Message)
	}
	if !strings.Contains(res.Message, "connection refused") {
		t.Errorf("the message must carry the reason through: %q", res.Message)
	}
	if strings.Contains(res.Message, "qb:") {
		t.Errorf("a healthy connection must not be listed as failing: %q", res.Message)
	}
	if !strings.Contains(res.Message, "1 of 3") {
		t.Errorf("the message must say how much is broken: %q", res.Message)
	}
}

func TestConnectionsIsQuietWhenEverythingAnswers(t *testing.T) {
	check := Connections(deps(
		[]ports.ClientConfig{{Name: "qb", Type: "qbittorrent", Enabled: true}},
		nil,
		func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} },
		nil,
	))
	if res := check(context.Background()); res.Status != StatusOK {
		t.Errorf("status = %s (%s), want ok", res.Status, res.Message)
	}
}

// The check runs on a one-minute timer, so what it probes has to be inert.
// Testing a Discord webhook posts a message; testing a Plex notifier IS a
// full library rescan. Neither belongs on a timer.
func TestConnectionsNeverProbesSomethingWithSideEffects(t *testing.T) {
	var probes int32
	check := Connections(deps(
		nil,
		[]ports.NotifierConfig{
			{Name: "discord", Type: "discord", Enabled: true},
			{Name: "hook", Type: "webhook", Enabled: true},
			{Name: "plex", Type: "plex", Enabled: true},
			{Name: "jellyfin", Type: "jellyfin", Enabled: true},
		},
		nil,
		func(ports.NotifierConfig) ports.Notifier { return &fakeNotifier{probes: &probes} },
	))
	if res := check(context.Background()); res.Status != StatusOK {
		t.Errorf("status = %s (%s)", res.Status, res.Message)
	}
	if probes != 0 {
		t.Errorf("%d notifier(s) were probed — a health check must not post to a "+
			"channel or start a library rescan every minute", probes)
	}
}

func TestConnectionsSkipsWhatIsDisabled(t *testing.T) {
	var clientProbes, notifierProbes int32
	check := Connections(deps(
		[]ports.ClientConfig{{Name: "qb", Type: "qbittorrent", Enabled: false}},
		[]ports.NotifierConfig{{Name: "plurx", Type: "plurx", Enabled: false}},
		func(ports.ClientConfig) ports.DownloadClient {
			return &fakeClient{probes: &clientProbes, err: errors.New("down")}
		},
		func(ports.NotifierConfig) ports.Notifier {
			return &fakeNotifier{probes: &notifierProbes, err: errors.New("down")}
		},
	))
	if res := check(context.Background()); res.Status != StatusOK {
		t.Errorf("a disabled connection must not be reported: %s (%s)", res.Status, res.Message)
	}
	if clientProbes != 0 || notifierProbes != 0 {
		t.Errorf("probed disabled connections: %d client, %d notifier", clientProbes, notifierProbes)
	}
}

// One unresponsive server must not hide the state of every other one. The
// probes run together so the check's own timeout bounds the slowest, not the
// sum.
func TestConnectionsProbesInParallelSoOneHangDoesNotHideTheRest(t *testing.T) {
	check := Connections(deps(
		[]ports.ClientConfig{
			{Name: "slow", Type: "qbittorrent", Enabled: true},
			{Name: "alsoslow", Type: "sabnzbd", Enabled: true},
			{Name: "broken", Type: "nzbd", Enabled: true},
		},
		nil,
		func(cfg ports.ClientConfig) ports.DownloadClient {
			if cfg.Name == "broken" {
				return &fakeClient{err: errors.New("no route to host")}
			}
			return &fakeClient{delay: 300 * time.Millisecond}
		},
		nil,
	))

	start := time.Now()
	res := check(context.Background())
	elapsed := time.Since(start)
	if elapsed > 600*time.Millisecond {
		t.Errorf("took %s — the probes ran one after another", elapsed)
	}
	if !strings.Contains(res.Message, "broken") {
		t.Errorf("the broken connection was not reported: %q", res.Message)
	}
}
