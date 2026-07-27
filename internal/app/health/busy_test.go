package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/ports"
)

type busyClient struct{}

func (busyClient) Add(context.Context, string, string) (ports.Handle, error) {
	return "", errors.New("not used")
}
func (busyClient) Statuses(context.Context) ([]ports.DownloadStatus, error) { return nil, nil }
func (busyClient) Remove(context.Context, ports.Handle, bool) error         { return nil }
func (busyClient) Test(context.Context) error                               { return nil }

func stateFor(t *testing.T, c Contact) ConnectionState {
	t.Helper()
	mon := NewConnectionMonitor(ConnectionDeps{
		Clients: func(context.Context) ([]ports.ClientConfig, error) {
			return []ports.ClientConfig{{ID: 1, Name: "nzbd", Type: "nzbd", Enabled: true}}, nil
		},
		Notifiers: func(context.Context) ([]ports.NotifierConfig, error) { return nil, nil },
		NewClient: func(ports.ClientConfig) ports.DownloadClient { return busyClient{} },
		Contacts:  func() map[int64]Contact { return map[int64]Contact{1: c} },
	})
	mon.Check(context.Background())
	states, _ := mon.Snapshot()
	if len(states) != 1 {
		t.Fatalf("states = %d, want 1", len(states))
	}
	return states[0]
}

// A client Monarr is mid-import from is the opposite of a quiet one.
//
// The poll and the import share a goroutine: the sweep calls the client,
// records the contact, then imports whatever finished. A 20 GB import across a
// network mount holds that sweep open for minutes, and the contact clock ages
// for the whole of it — so the panel degraded the client that was actively
// feeding it, while the per-row Test beside it passed, because the probe is a
// separate call. A panel that contradicts the button next to it is worse than
// one that says nothing.
func TestASweepInFlightIsNotSilence(t *testing.T) {
	long := Contact{At: time.Now().Add(-45 * time.Minute), Busy: true}
	st := stateFor(t, long)
	if st.StaleFor != 0 {
		t.Errorf("StaleFor = %s while a sweep is in flight, want 0", st.StaleFor)
	}
	for _, r := range st.results() {
		if r.Name == "client:nzbd" && r.Status != StatusOK {
			t.Errorf("client:nzbd = %s (%q) during an import — Monarr is busy "+
				"BECAUSE of this client", r.Status, r.Message)
		}
	}
}

// And the check still does its job when nothing is running: a stream that
// died quietly must still be reported, or the fix above would have removed
// the only thing that notices it.
func TestAQuietClientIsStillReported(t *testing.T) {
	st := stateFor(t, Contact{At: time.Now().Add(-45 * time.Minute)})
	if st.StaleFor < 40*time.Minute {
		t.Fatalf("StaleFor = %s, want ~45m", st.StaleFor)
	}
	var found bool
	for _, r := range st.results() {
		if r.Name == "client:nzbd" {
			found = true
			if r.Status != StatusError {
				t.Errorf("status = %s, want error after 45 minutes of silence", r.Status)
			}
			if r.Message == "" {
				t.Error("an error with no message is unactionable")
			}
		}
	}
	if !found {
		t.Fatal("no client:nzbd check was produced")
	}
}
