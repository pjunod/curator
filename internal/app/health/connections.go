package health

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

// Contact is when Monarr last got a straight answer out of one client, and
// what it said if it did not. Supplied by whatever does the talking — the
// queue poll and the push supervisor both count.
type Contact struct {
	At    time.Time
	Error string
}

// ConnectionDeps are the pieces the connections group needs: what is
// configured, how to build something that can talk to it, and when each was
// last heard from.
type ConnectionDeps struct {
	Clients     func(context.Context) ([]ports.ClientConfig, error)
	Notifiers   func(context.Context) ([]ports.NotifierConfig, error)
	NewClient   func(ports.ClientConfig) ports.DownloadClient
	NewNotifier func(ports.NotifierConfig) ports.Notifier
	// Contacts reports last-successful-contact per client id. Optional: a
	// nil one just means the age thresholds are not applied.
	Contacts func() map[int64]Contact
}

// How stale a client's last successful contact may get before it is worth
// saying so. The queue poll runs every 30 s, so five minutes is ten missed
// polls — comfortably past a blip — and half an hour is "this has been
// broken since before you sat down".
const (
	contactWarnAfter  = 5 * time.Minute
	contactErrorAfter = 30 * time.Minute
)

// ConnectionState is what one probe learned about one remote application,
// before it is rendered as anything.
//
// It exists because two surfaces need the same facts and must not disagree:
// the health checks (§5.6) and the Connections panel (§5.7). Deriving the
// panel by parsing the health check's English would put a screen one reworded
// sentence away from lying.
type ConnectionState struct {
	ID       int64
	Name     string
	Kind     string // downloadclient | mediaserver
	Type     string
	URL      string
	Probed   bool
	Reach    error
	StaleFor time.Duration
	LastSeen time.Time
	// LastError is what the last failed exchange said, even when the client
	// answers its test now. "Answering, but the stream ended an hour ago"
	// is a different problem from "answering, but quiet", and only this
	// tells them apart.
	LastError string
	Capacity  *ports.Capacity
	Version   string
}

// ConnectionMonitor probes the remote applications once and serves both
// consumers from that one pass — the health registry on its timer, and the
// panel from whatever the last pass found.
type ConnectionMonitor struct {
	deps ConnectionDeps
	mu   sync.Mutex
	last []ConnectionState
	at   time.Time
}

// NewConnectionMonitor returns a monitor over these dependencies.
func NewConnectionMonitor(deps ConnectionDeps) *ConnectionMonitor {
	return &ConnectionMonitor{deps: deps}
}

// Snapshot returns the last probe's findings and when it ran. A zero time
// means nothing has been probed yet, which a caller should say rather than
// render as an all-clear.
func (m *ConnectionMonitor) Snapshot() ([]ConnectionState, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ConnectionState, len(m.last))
	copy(out, m.last)
	return out, m.at
}

// Check implements GroupFunc: probe, remember, and render as health results.
func (m *ConnectionMonitor) Check(ctx context.Context) []CheckResult {
	states, err := m.probe(ctx)
	if err != nil {
		return []CheckResult{{
			Name: "connections", Status: StatusError, Message: oneLine(err.Error()),
		}}
	}
	m.mu.Lock()
	m.last, m.at = states, time.Now()
	m.mu.Unlock()

	var out []CheckResult
	for _, st := range states {
		out = append(out, st.results()...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// results renders one connection as its health lines.
func (st ConnectionState) results() []CheckResult {
	if st.Kind == "mediaserver" {
		name := "mediaserver:" + st.Name
		if !st.Probed {
			return []CheckResult{{Name: name, Status: StatusOK,
				Message: fmt.Sprintf("%s is configured; not probed, because its only "+
					"test is a full library rescan", st.Type)}}
		}
		if st.Reach != nil {
			return []CheckResult{{Name: name, Status: StatusWarning,
				Message: fmt.Sprintf("%s: %s", st.Type, oneLine(st.Reach.Error()))}}
		}
		return []CheckResult{{Name: name, Status: StatusOK}}
	}

	name := "client:" + st.Name
	var out []CheckResult
	switch {
	case st.Reach != nil:
		out = append(out, CheckResult{Name: name, Status: StatusWarning,
			Message: fmt.Sprintf("%s is not answering: %s", st.Type, oneLine(st.Reach.Error()))})
	// Answering is not the same as working. A client that passes its test
	// while nothing has actually been fetched from it for half an hour is
	// the shape of a subscription that died quietly, and the test alone
	// would call that healthy forever.
	case st.StaleFor > contactErrorAfter:
		out = append(out, CheckResult{Name: name, Status: StatusError,
			Message: st.StaleMessage()})
	case st.StaleFor > contactWarnAfter:
		out = append(out, CheckResult{Name: name, Status: StatusWarning,
			Message: st.StaleMessage()})
	default:
		out = append(out, CheckResult{Name: name, Status: StatusOK})
	}

	if st.Capacity != nil {
		capName := name + ":capacity"
		switch {
		case len(st.Capacity.Problems()) > 0:
			out = append(out, CheckResult{Name: capName, Status: StatusWarning,
				Message: fmt.Sprintf("%s is up but not downloading: %s",
					st.Type, strings.Join(st.Capacity.Problems(), "; "))})
		default:
			out = append(out, CheckResult{Name: capName, Status: StatusOK})
		}
	}
	return out
}

// StaleMessage explains a client that answers its test while Monarr has not
// managed to fetch anything from it.
//
// Exported because the Connections panel says the same thing, and this type
// exists precisely so the two surfaces cannot drift. The panel used to print
// LastError here instead, which is empty whenever nothing actually failed —
// an amber badge with a blank cell beside it.
func (st ConnectionState) StaleMessage() string {
	msg := fmt.Sprintf("reachability test answers, but Monarr's queue-status poll has not completed successfully for %s", roughly(st.StaleFor))
	if st.LastError != "" {
		msg += " (last error: " + oneLine(st.LastError) + ")"
	} else {
		msg += "; check whether the queue.refresh task is still running"
	}
	return msg
}

// Connections reports on every other application Monarr depends on, one
// line each (plan §5.6).
//
// One line each, not one aggregate, because the actions differ: a download
// client that stopped answering means grabs are piling up, a client whose
// disk is full means grabs are being accepted and going nowhere, and a
// revoked plurx key means imports are succeeding and never appearing. An
// averaged "2 of 5 connections failing" tells you none of that.
//
// **What it deliberately does not probe.** Chat notifiers have no inert
// test — "testing" a Discord webhook posts a message, and doing that every
// minute is a health check that spams a channel. Plex and Jellyfin are
// worse: their only test IS the action, so probing them would make the
// server rescan its whole library once a minute. plurx is probed because it
// has a public, side-effect-free `GET /api/v1/server`. Where a media server
// offers no such endpoint it is listed as unprobed rather than quietly
// omitted, so an absence is never mistaken for an all-clear.
func Connections(deps ConnectionDeps) GroupFunc {
	return NewConnectionMonitor(deps).Check
}

// probe asks every enabled connection how it is, concurrently.
func (m *ConnectionMonitor) probe(ctx context.Context) ([]ConnectionState, error) {
	deps := m.deps
	var contacts map[int64]Contact
	if deps.Contacts != nil {
		contacts = deps.Contacts()
	}
	clients, err := deps.Clients(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot read download clients: %w", err)
	}
	notifiers, err := deps.Notifiers(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot read notifiers: %w", err)
	}

	var mu sync.Mutex
	var out []ConnectionState
	add := func(st ConnectionState) {
		mu.Lock()
		out = append(out, st)
		mu.Unlock()
	}

	// In parallel: the probe's own timeout should bound the slowest
	// connection, not the sum. One unplugged server must not make every
	// other line report "timed out".
	var wg sync.WaitGroup
	for _, c := range clients {
		if !c.Enabled || deps.NewClient == nil {
			continue
		}
		cfg := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			seen := contacts[cfg.ID]
			st := ConnectionState{
				ID: cfg.ID, Name: cfg.Name, Kind: "downloadclient", Type: cfg.Type,
				URL: ports.RedactURL(cfg.URL), Probed: true, LastSeen: seen.At,
				LastError: seen.Error,
			}
			client := deps.NewClient(cfg)
			st.Reach = client.Test(ctx)
			if st.Reach == nil && !seen.At.IsZero() {
				st.StaleFor = time.Since(seen.At)
			}
			if st.Reach == nil {
				if cap, ok := client.(ports.CapacityReporter); ok {
					if c, err := cap.Capacity(ctx); err == nil {
						st.Capacity, st.Version = &c, c.Version
					}
					// A failed capacity call adds nothing: the
					// reachability result above already reported that
					// outage, and repeating it would double-count one.
				}
			}
			add(st)
		}()
	}
	for _, n := range notifiers {
		if !n.Enabled || !mediaServer(n.Type) || deps.NewNotifier == nil {
			continue
		}
		cfg := n
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := ConnectionState{
				ID: cfg.ID, Name: cfg.Name, Kind: "mediaserver", Type: cfg.Type,
				URL: ports.RedactURL(cfg.Settings["url"]),
			}
			// Only plurx is probed: it has a public, side-effect-free
			// GET /api/v1/server. Plex and Jellyfin have no inert test —
			// their only one IS a full library rescan — so they are listed
			// as unprobed rather than omitted, because an absence reads as
			// an all-clear.
			if cfg.Type == "plurx" {
				st.Probed = true
				st.Reach = deps.NewNotifier(cfg).Test(ctx)
			}
			add(st)
		}()
	}
	wg.Wait()

	// Stable order: a page whose rows shuffle between polls is unreadable,
	// and these were produced by racing goroutines.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func mediaServer(kind string) bool {
	return kind == "plex" || kind == "jellyfin" || kind == "plurx"
}

func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// oneLine flattens an error for a status line, which is one line high.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
