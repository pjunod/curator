package health

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/monarr-media/monarr/internal/ports"
)

// ConnectionDeps are the pieces the connections check needs: what is
// configured, and how to build something that can talk to it.
type ConnectionDeps struct {
	Clients     func(context.Context) ([]ports.ClientConfig, error)
	Notifiers   func(context.Context) ([]ports.NotifierConfig, error)
	NewClient   func(ports.ClientConfig) ports.DownloadClient
	NewNotifier func(ports.NotifierConfig) ports.Notifier
}

// Connections reports on the other applications Monarr depends on.
//
// This is the one check that can fail while Monarr itself is perfectly
// healthy, and it is the one worth having: a pipeline is only as good as its
// seams, and every seam here is a URL and a credential in someone else's
// process. A download client that stopped answering means grabs pile up
// invisibly; a plurx key that was revoked means imports quietly stop
// appearing in the library.
//
// **What it deliberately does not probe.** Chat notifiers (webhook, Discord)
// have no inert test — "testing" one posts a message, and doing that every
// minute would be a health check that spams a channel. Plex and Jellyfin are
// worse: their only test IS the action, so probing them would make the
// server rescan its whole library once a minute. plurx is probed because its
// test was built to be inert — it asks for a path that cannot be under any
// library root and treats the rejection as the pass — which is exactly the
// property that makes a thing safe to check on a timer.
func Connections(deps ConnectionDeps) CheckFunc {
	return func(ctx context.Context) Result {
		type probe struct {
			label string
			run   func(context.Context) error
		}
		var probes []probe

		clients, err := deps.Clients(ctx)
		if err != nil {
			return Errorf("cannot read download clients: %v", err)
		}
		for _, c := range clients {
			if !c.Enabled || deps.NewClient == nil {
				continue
			}
			cfg := c
			probes = append(probes, probe{
				label: fmt.Sprintf("%s (%s)", cfg.Name, cfg.Type),
				run:   func(ctx context.Context) error { return deps.NewClient(cfg).Test(ctx) },
			})
		}

		notifiers, err := deps.Notifiers(ctx)
		if err != nil {
			return Errorf("cannot read notifiers: %v", err)
		}
		for _, n := range notifiers {
			if !n.Enabled || n.Type != "plurx" || deps.NewNotifier == nil {
				continue
			}
			cfg := n
			probes = append(probes, probe{
				label: fmt.Sprintf("%s (%s)", cfg.Name, cfg.Type),
				run:   func(ctx context.Context) error { return deps.NewNotifier(cfg).Test(ctx) },
			})
		}

		if len(probes) == 0 {
			return OK()
		}

		// In parallel, so the check's own timeout bounds the slowest
		// connection rather than the sum of all of them. One unplugged
		// server should not make the whole check time out and report every
		// other connection as unknown.
		failures := make([]string, len(probes))
		var wg sync.WaitGroup
		for i, p := range probes {
			wg.Add(1)
			go func(i int, p probe) {
				defer wg.Done()
				if err := p.run(ctx); err != nil {
					failures[i] = fmt.Sprintf("%s: %s", p.label, oneLine(err.Error()))
				}
			}(i, p)
		}
		wg.Wait()

		var bad []string
		for _, f := range failures {
			if f != "" {
				bad = append(bad, f)
			}
		}
		if len(bad) == 0 {
			return OK()
		}
		sort.Strings(bad)
		// A warning, not an error: Monarr keeps working with a download
		// client down — it queues, retries, and catches up. Reporting this
		// at error level would train people to ignore a red badge that
		// clears itself when a container finishes restarting.
		return Warn("%d of %d connections failing — %s",
			len(bad), len(probes), strings.Join(bad, "; "))
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
