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

	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/ports"
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

// No amber badge with a blank cell beside it.
//
// A client that answers its test but has not been heard from in a while
// degrades on age alone. That branch used to print the last error verbatim —
// and there is no last error, because "stale" means Monarr stopped asking,
// not that asking failed. The panel showed DEGRADED with an empty Detail
// column and no way to find out why. A state the page cannot explain is
// worse than no state at all.
func TestADegradedConnectionAlwaysSaysWhy(t *testing.T) {
	mon := monitorFor(
		[]ports.ClientConfig{{ID: 1, Name: "nzbd_live", Type: "nzbd", Enabled: true}},
		func(ports.ClientConfig) ports.DownloadClient { return probeClient{} },
		// Answering fine; last successful contact 48 minutes ago and no
		// error recorded — exactly the shape that produced a blank.
		map[int64]health.Contact{1: {At: time.Now().Add(-48 * time.Minute)}},
	)
	mon.Check(context.Background())

	for _, c := range connections(t, mon).Connections {
		if c.Name != "nzbd_live" {
			continue
		}
		if string(c.State) != "degraded" {
			t.Fatalf("state = %q, want degraded", c.State)
		}
		if c.Detail == nil || *c.Detail == "" {
			t.Fatal("a degraded row with no detail is unreadable: the user " +
				"sees an amber badge beside an empty cell")
		}
		if !strings.Contains(*c.Detail, "48m") {
			t.Errorf("detail = %q, want it to name how long it has been quiet", *c.Detail)
		}
		return
	}
	t.Fatal("the client was not listed")
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

// The panel used to answer "are these apps talking" in one direction only:
// what Monarr reaches out to. plurx's whole side of the pipeline is inbound,
// so a plurx configured perfectly and calling every few minutes appeared
// nowhere at all — which reads, correctly, as "it is not working".
func TestApplicationsThatCallMonarrAppearOnThePanel(t *testing.T) {
	reg := NewCallerRegistry()
	reg.Note("plurx/0.4.1", "/api/v1/webhooks/plurx")
	srv := &Server{deps: Deps{Callers: reg, Bus: bus.New(nil)}}

	rr := recordJSON(t, srv.GetConnections)
	var out connectionsResponse
	if err := json.Unmarshal(rr, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Connections) != 1 {
		t.Fatalf("connections = %d, want the inbound caller", len(out.Connections))
	}
	c := out.Connections[0]
	if c.Name != "plurx" {
		t.Errorf("name = %q — the product token is what somebody recognizes", c.Name)
	}
	if c.Kind != "inbound" {
		t.Errorf("kind = %q, want inbound", c.Kind)
	}
	if c.State != "calling" {
		t.Errorf("state = %q, want calling", c.State)
	}
	if c.Detail == nil || !contains(*c.Detail, "/api/v1/webhooks/plurx") {
		t.Errorf("detail should name what it called: %v", c.Detail)
	}
}

// A person with a browser open is not a connection, and one row per Chrome
// version would bury the row that matters.
func TestBrowsersAreNotListedAsConnections(t *testing.T) {
	reg := NewCallerRegistry()
	reg.Note("Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/140", "/api/v1/items")
	reg.Note("", "/api/v1/items")
	reg.Note("plurx/0.4.1", "/api/v1/calendar")

	got := reg.List()
	if len(got) != 1 || got[0].Name != "plurx" {
		t.Errorf("callers = %+v, want plurx alone", got)
	}
}

// A caller that has not been heard from in an hour is quiet, not calling.
// The window is generous on purpose: plurx's rail refreshes every 15 minutes
// and its watch pushes are as rare as somebody finishing something.
func TestACallerThatHasGoneAwayReadsAsQuiet(t *testing.T) {
	fresh := inboundDTO(Caller{Name: "plurx", LastSeen: time.Now(), LastPath: "/x"})
	if fresh.State != "calling" {
		t.Errorf("state = %q for a caller heard from just now", fresh.State)
	}
	stale := inboundDTO(Caller{
		Name: "plurx", LastSeen: time.Now().Add(-2 * time.Hour), LastPath: "/x",
	})
	if stale.State != "quiet" {
		t.Errorf("state = %q for a caller last heard two hours ago", stale.State)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
