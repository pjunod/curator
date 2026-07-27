package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/app/health"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/ports"
)

type probeClient struct {
	err error
	cap *ports.Capacity
}

func (p probeClient) Add(context.Context, string, string) (ports.Handle, error) { return "", nil }
func (p probeClient) Statuses(context.Context) ([]ports.DownloadStatus, error)  { return nil, nil }
func (p probeClient) Remove(context.Context, ports.Handle, bool) error          { return nil }
func (p probeClient) Test(context.Context) error                                { return p.err }
func (p probeClient) Capacity(context.Context) (ports.Capacity, error) {
	if p.cap == nil {
		return ports.Capacity{}, errors.New("no capacity")
	}
	return *p.cap, nil
}

func monitorFor(clients []ports.ClientConfig, build func(ports.ClientConfig) ports.DownloadClient,
	contacts map[int64]health.Contact) *health.ConnectionMonitor {
	return health.NewConnectionMonitor(health.ConnectionDeps{
		Clients:   func(context.Context) ([]ports.ClientConfig, error) { return clients, nil },
		Notifiers: func(context.Context) ([]ports.NotifierConfig, error) { return nil, nil },
		NewClient: build,
		Contacts:  func() map[int64]health.Contact { return contacts },
	})
}

func connections(t *testing.T, mon *health.ConnectionMonitor) connectionsResponse {
	t.Helper()
	srv := &Server{deps: Deps{Connections: mon, Bus: bus.New(nil)}}
	rr := recordJSON(t, srv.GetConnections)
	var out connectionsResponse
	if err := json.Unmarshal(rr, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The panel answers "are they talking right now", so the words have to mean
// distinct things. `polling` in particular is NOT a lesser `live` — it is
// the fallback working exactly as designed, and colouring it as a fault
// would train people to ignore the one that is.
func TestConnectionStateSaysWhichKindOfWorkingItIs(t *testing.T) {
	full := ports.Capacity{DiskLow: true}
	mon := monitorFor(
		[]ports.ClientConfig{
			{ID: 1, Name: "qb", Type: "qbittorrent", Enabled: true, URL: "http://qb:8080"},
			{ID: 2, Name: "dead", Type: "sabnzbd", Enabled: true},
			{ID: 3, Name: "stuffed", Type: "nzbd", Enabled: true},
		},
		func(cfg ports.ClientConfig) ports.DownloadClient {
			switch cfg.Name {
			case "dead":
				return probeClient{err: errors.New("connection refused")}
			case "stuffed":
				return probeClient{cap: &full}
			}
			return probeClient{}
		},
		map[int64]health.Contact{1: {At: time.Now()}, 3: {At: time.Now()}},
	)
	mon.Check(context.Background())

	got := map[string]string{}
	detail := map[string]string{}
	for _, c := range connections(t, mon).Connections {
		got[c.Name] = string(c.State)
		if c.Detail != nil {
			detail[c.Name] = *c.Detail
		}
	}
	if got["qb"] != "polling" {
		t.Errorf("qb = %q, want polling", got["qb"])
	}
	if got["dead"] != "unreachable" {
		t.Errorf("dead = %q, want unreachable", got["dead"])
	}
	if got["stuffed"] != "degraded" {
		t.Errorf("stuffed = %q, want degraded — it answers but cannot download", got["stuffed"])
	}
	if detail["stuffed"] == "" {
		t.Error("a degraded connection must say what is wrong with it")
	}
}

// A status page is made to be screenshotted and pasted into an issue.
// Credentials in a client URL ride along unnoticed (§10.10).
func TestConnectionURLsNeverCarryAPassword(t *testing.T) {
	mon := monitorFor(
		[]ports.ClientConfig{{
			ID: 1, Name: "qb", Type: "qbittorrent", Enabled: true,
			URL: "http://admin:hunter2@qb:8080",
		}},
		func(ports.ClientConfig) ports.DownloadClient { return probeClient{} },
		nil,
	)
	mon.Check(context.Background())

	for _, c := range connections(t, mon).Connections {
		if c.Url == nil {
			continue
		}
		if strings.Contains(*c.Url, "hunter2") {
			t.Errorf("the password reached the panel: %q", *c.Url)
		}
		if !strings.Contains(*c.Url, "admin") {
			t.Errorf("the username is useful and should survive: %q", *c.Url)
		}
	}
}

// Nothing probed yet must read as "not probed yet", not as an all-clear.
func TestAnUnprobedPanelSaysSoRatherThanLookingHealthy(t *testing.T) {
	mon := monitorFor(nil, nil, nil)
	out := connections(t, mon)
	if out.CheckedAt != nil {
		t.Errorf("checkedAt = %v before any probe", *out.CheckedAt)
	}
	if len(out.Connections) != 0 {
		t.Errorf("connections = %v before any probe", out.Connections)
	}
}

func recordJSON(t *testing.T, h func(http.ResponseWriter, *http.Request)) []byte {
	t.Helper()
	rr := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/system/connections", nil)
	h(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	return rr.Body.Bytes()
}
