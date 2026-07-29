package health

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

type fakeClient struct {
	err      error
	delay    time.Duration
	probes   *int32
	capacity ports.Capacity
	capErr   error
}

func (f *fakeClient) Add(context.Context, string, string) (ports.Handle, error) {
	return "", nil
}
func (f *fakeClient) Statuses(context.Context) ([]ports.DownloadStatus, error) { return nil, nil }
func (f *fakeClient) Remove(context.Context, ports.Handle, bool) error         { return nil }
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

// capable is a client that also reports capacity — the optional half.
type capable struct{ *fakeClient }

func (c capable) Capacity(context.Context) (ports.Capacity, error) {
	return c.capacity, c.capErr
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

func byName(results []CheckResult) map[string]CheckResult {
	out := map[string]CheckResult{}
	for _, r := range results {
		out[r.Name] = r
	}
	return out
}

// One line per connection, not one average. The actions differ completely:
// "not answering" means grabs are piling up, and an averaged "1 of 3
// failing" says which of those is happening to nobody.
func TestConnectionsReportsEachOneSeparately(t *testing.T) {
	group := Connections(deps(
		[]ports.ClientConfig{
			{ID: 1, Name: "qb", Type: "qbittorrent", Enabled: true},
			{ID: 2, Name: "nzbd", Type: "nzbd", Enabled: true},
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

	got := byName(group(context.Background()))
	if got["client:qb"].Status != StatusOK {
		t.Errorf("qb = %s (%s)", got["client:qb"].Status, got["client:qb"].Message)
	}
	broken := got["client:nzbd"]
	if broken.Status != StatusWarning {
		t.Errorf("nzbd = %s, want warning", broken.Status)
	}
	if !strings.Contains(broken.Message, "connection refused") {
		t.Errorf("the reason must come through: %q", broken.Message)
	}
	if got["mediaserver:plurx"].Status != StatusOK {
		t.Errorf("plurx = %s (%s)", got["mediaserver:plurx"].Status, got["mediaserver:plurx"].Message)
	}
}

// A client that is up, answering, and unable to download looks exactly like
// an idle one from outside. Each condition has to say what it is, in words
// somebody can act on.
func TestCapacityNamesTheReasonNothingIsDownloading(t *testing.T) {
	cases := []struct {
		name   string
		cap    ports.Capacity
		status Status
		says   string
	}{
		{"disk low", ports.Capacity{DiskLow: true}, StatusWarning, "disk is low on space"},
		{"quota", ports.Capacity{QuotaReached: true}, StatusWarning, "quota is used up"},
		{"blocked servers", ports.Capacity{BlockedServers: 2}, StatusWarning, "2 news server(s) are blocked"},
		{"paused", ports.Capacity{Paused: true}, StatusWarning, "queue is paused"},
		// NOT a problem. nzbd sets health_abort from `[post] health_action`,
		// so it is true on any server configured to park or delete
		// unrepairable downloads — a good default, on permanently. Plan
		// §5.6 maps it to an error; that reading produced a red badge that
		// could never clear, which is the failure this file is careful
		// about everywhere else.
		{"health abort is policy, not a fault", ports.Capacity{HealthAbort: true}, StatusOK, ""},
		{"nothing wrong", ports.Capacity{}, StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			group := Connections(deps(
				[]ports.ClientConfig{{ID: 1, Name: "nzbd", Type: "nzbd", Enabled: true}},
				nil,
				func(ports.ClientConfig) ports.DownloadClient {
					return capable{&fakeClient{capacity: tc.cap}}
				},
				nil,
			))
			got := byName(group(context.Background()))
			res, ok := got["client:nzbd:capacity"]
			if !ok {
				t.Fatalf("no capacity line; got %v", got)
			}
			if res.Status != tc.status {
				t.Errorf("status = %s, want %s (%s)", res.Status, tc.status, res.Message)
			}
			if tc.says != "" && !strings.Contains(res.Message, tc.says) {
				t.Errorf("message = %q, want it to say %q in plain words", res.Message, tc.says)
			}
		})
	}
}

// A client with no capacity notion must not thereby look unhealthy, and one
// whose capacity call fails must not double-count an outage the
// reachability line already reported.
func TestCapacityIsOptionalAndNeverDoubleCountsAnOutage(t *testing.T) {
	group := Connections(deps(
		[]ports.ClientConfig{
			{ID: 1, Name: "qb", Type: "qbittorrent", Enabled: true},
			{ID: 2, Name: "flaky", Type: "nzbd", Enabled: true},
		},
		nil,
		func(cfg ports.ClientConfig) ports.DownloadClient {
			if cfg.Name == "flaky" {
				return capable{&fakeClient{capErr: errors.New("boom")}}
			}
			return &fakeClient{}
		},
		nil,
	))
	got := byName(group(context.Background()))
	if _, ok := got["client:qb:capacity"]; ok {
		t.Error("a client with no capacity notion produced a capacity line")
	}
	if _, ok := got["client:flaky:capacity"]; ok {
		t.Error("a failed capacity call must not add a second line for one outage")
	}
}

// Answering is not the same as working. A dead subscription passes Test
// forever, and reachability alone would call that healthy.
func TestAClientThatAnswersButDeliversNothingIsReported(t *testing.T) {
	build := func(age time.Duration) map[string]CheckResult {
		d := deps(
			[]ports.ClientConfig{{ID: 1, Name: "nzbd", Type: "nzbd", Enabled: true}},
			nil,
			func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} },
			nil,
		)
		d.Contacts = func() map[int64]Contact {
			return map[int64]Contact{1: {At: time.Now().Add(-age), Error: "stream ended"}}
		}
		return byName(Connections(d)(context.Background()))
	}

	if got := build(time.Minute)["client:nzbd"]; got.Status != StatusOK {
		t.Errorf("a fresh contact must be quiet: %s (%s)", got.Status, got.Message)
	}
	warn := build(10 * time.Minute)["client:nzbd"]
	if warn.Status != StatusWarning {
		t.Errorf("10m stale = %s, want warning", warn.Status)
	}
	if !strings.Contains(warn.Message, "stream ended") {
		t.Errorf("the last error belongs in the message: %q", warn.Message)
	}
	if got := build(time.Hour)["client:nzbd"].Status; got != StatusError {
		t.Errorf("an hour stale = %s, want error", got)
	}
}

// The group runs on a one-minute timer, so what it probes has to be inert.
// Testing a Discord webhook posts a message; testing a Plex notifier IS a
// full library rescan. Both are listed, neither is touched.
func TestNothingWithSideEffectsIsEverProbed(t *testing.T) {
	var probes int32
	group := Connections(deps(
		nil,
		[]ports.NotifierConfig{
			{Name: "discord", Type: "discord", Enabled: true},
			{Name: "plex", Type: "plex", Enabled: true},
			{Name: "jellyfin", Type: "jellyfin", Enabled: true},
		},
		nil,
		func(ports.NotifierConfig) ports.Notifier { return &fakeNotifier{probes: &probes} },
	))
	got := byName(group(context.Background()))
	if probes != 0 {
		t.Errorf("%d notifier(s) probed — this must not post to a channel or "+
			"start a library rescan every minute", probes)
	}
	if _, ok := got["mediaserver:discord"]; ok {
		t.Error("a chat notifier is not a media server")
	}
	// Listed but unprobed, and saying so: an absence must not read as an
	// all-clear.
	plex := got["mediaserver:plex"]
	if plex.Status != StatusOK || !strings.Contains(plex.Message, "not probed") {
		t.Errorf("plex = %s %q, want ok and an explanation", plex.Status, plex.Message)
	}
}

func TestDisabledConnectionsAreSkipped(t *testing.T) {
	var probes int32
	group := Connections(deps(
		[]ports.ClientConfig{{ID: 1, Name: "qb", Type: "qbittorrent", Enabled: false}},
		[]ports.NotifierConfig{{Name: "plurx", Type: "plurx", Enabled: false}},
		func(ports.ClientConfig) ports.DownloadClient {
			return &fakeClient{probes: &probes, err: errors.New("down")}
		},
		func(ports.NotifierConfig) ports.Notifier { return &fakeNotifier{probes: &probes} },
	))
	if got := group(context.Background()); len(got) != 0 {
		t.Errorf("disabled connections reported: %v", got)
	}
	if probes != 0 {
		t.Errorf("probed %d disabled connections", probes)
	}
}

// One unresponsive server must not hide the state of every other one.
func TestProbesRunTogetherSoOneHangDoesNotHideTheRest(t *testing.T) {
	group := Connections(deps(
		[]ports.ClientConfig{
			{ID: 1, Name: "slow", Type: "qbittorrent", Enabled: true},
			{ID: 2, Name: "alsoslow", Type: "sabnzbd", Enabled: true},
			{ID: 3, Name: "broken", Type: "nzbd", Enabled: true},
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
	got := byName(group(context.Background()))
	if elapsed := time.Since(start); elapsed > 600*time.Millisecond {
		t.Errorf("took %s — the probes ran one after another", elapsed)
	}
	if got["client:broken"].Status != StatusWarning {
		t.Errorf("the broken client was not reported: %v", got["client:broken"])
	}
}
