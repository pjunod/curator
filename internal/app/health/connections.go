package health

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/monarr-media/monarr/internal/ports"
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
	return func(ctx context.Context) []CheckResult {
		var contacts map[int64]Contact
		if deps.Contacts != nil {
			contacts = deps.Contacts()
		}

		clients, err := deps.Clients(ctx)
		if err != nil {
			return []CheckResult{{
				Name: "connections", Status: StatusError,
				Message: fmt.Sprintf("cannot read download clients: %v", err),
			}}
		}
		notifiers, err := deps.Notifiers(ctx)
		if err != nil {
			return []CheckResult{{
				Name: "connections", Status: StatusError,
				Message: fmt.Sprintf("cannot read notifiers: %v", err),
			}}
		}

		var mu sync.Mutex
		var out []CheckResult
		add := func(r CheckResult) {
			mu.Lock()
			out = append(out, r)
			mu.Unlock()
		}

		// In parallel: the check's own timeout should bound the slowest
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
				client := deps.NewClient(cfg)
				add(clientResult(ctx, cfg, client, contacts[cfg.ID]))
				if cap, ok := client.(ports.CapacityReporter); ok {
					if r, reported := capacityResult(ctx, cfg, cap); reported {
						add(r)
					}
				}
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
				add(mediaServerResult(ctx, cfg, deps.NewNotifier))
			}()
		}
		wg.Wait()

		// Stable order: a health page whose rows shuffle between polls is
		// unreadable, and these were produced by racing goroutines.
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
}

func mediaServer(kind string) bool {
	return kind == "plex" || kind == "jellyfin" || kind == "plurx"
}

// clientResult probes one download client and folds in how long it has been
// since anything actually worked.
func clientResult(ctx context.Context, cfg ports.ClientConfig, client ports.DownloadClient, seen Contact) CheckResult {
	name := fmt.Sprintf("client:%s", cfg.Name)
	if err := client.Test(ctx); err != nil {
		return CheckResult{Name: name, Status: StatusWarning,
			Message: fmt.Sprintf("%s is not answering: %s", cfg.Type, oneLine(err.Error()))}
	}
	// Answering is not the same as working. A client that passes its test
	// while nothing has actually been fetched from it for half an hour is
	// the shape of a subscription that died quietly, and the test alone
	// would call that healthy forever.
	if !seen.At.IsZero() {
		if age := time.Since(seen.At); age > contactErrorAfter {
			return CheckResult{Name: name, Status: StatusError,
				Message: fmt.Sprintf("answering, but nothing has come through it for %s%s",
					roughly(age), because(seen.Error))}
		} else if age > contactWarnAfter {
			return CheckResult{Name: name, Status: StatusWarning,
				Message: fmt.Sprintf("answering, but nothing has come through it for %s%s",
					roughly(age), because(seen.Error))}
		}
	}
	return CheckResult{Name: name, Status: StatusOK}
}

// capacityResult reports a client that is up, answering, and unable to
// download — the state that looks exactly like "idle" from outside.
func capacityResult(ctx context.Context, cfg ports.ClientConfig, cap ports.CapacityReporter) (CheckResult, bool) {
	name := fmt.Sprintf("client:%s:capacity", cfg.Name)
	c, err := cap.Capacity(ctx)
	if err != nil {
		// The reachability line above already says the client is unwell.
		// Repeating it here would double-count one outage.
		return CheckResult{}, false
	}
	// A health abort is armed destruction: nzbd is set to park or delete on
	// critical health, so this one outranks a warning.
	if c.HealthAbort {
		return CheckResult{Name: name, Status: StatusError,
			Message: fmt.Sprintf("%s reports its critical-health abort is armed — "+
				"failing downloads will be parked or deleted", cfg.Type)}, true
	}
	problems := c.Problems()
	if len(problems) == 0 {
		return CheckResult{Name: name, Status: StatusOK}, true
	}
	return CheckResult{Name: name, Status: StatusWarning,
		Message: fmt.Sprintf("%s is up but not downloading: %s",
			cfg.Type, strings.Join(problems, "; "))}, true
}

// mediaServerResult probes a media server, where probing it is safe.
func mediaServerResult(ctx context.Context, cfg ports.NotifierConfig, factory func(ports.NotifierConfig) ports.Notifier) CheckResult {
	name := fmt.Sprintf("mediaserver:%s", cfg.Name)
	if cfg.Type != "plurx" {
		return CheckResult{Name: name, Status: StatusOK,
			Message: fmt.Sprintf("%s is configured; not probed, because its only "+
				"test is a full library rescan", cfg.Type)}
	}
	if err := factory(cfg).Test(ctx); err != nil {
		return CheckResult{Name: name, Status: StatusWarning,
			Message: fmt.Sprintf("plurx: %s", oneLine(err.Error()))}
	}
	return CheckResult{Name: name, Status: StatusOK}
}

func because(msg string) string {
	if msg == "" {
		return ""
	}
	return " (last error: " + oneLine(msg) + ")"
}

// roughly renders a duration the way somebody reads it off a status page.
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
