// Package notify is the notification dispatcher. Chat-shaped events (grab,
// failed, health) come off the bus and go out inline, once. Imports come off
// the durable outbox (notifier_deliveries), written in the import's own
// transaction: one row per chat notifier, and one sub-delivery per directory
// for Cinema, which is retried without end and read back (pipeline contract
// v2 section 4). Adapters are injected as a factory, like indexers and
// clients.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Factory builds a Notifier from stored config (real adapters in main,
// fakes in tests).
type Factory func(ports.NotifierConfig) ports.Notifier

// Dispatcher routes bus events to notifiers.
type Dispatcher struct {
	db  *sqlite.DB
	bus *bus.Bus
	log *slog.Logger
	new Factory
	// The in-flight view, shared with acquisition. Optional: without one
	// every report is a no-op. A notify is a seam like any other and belongs
	// on the same surface as the import that triggered it — "the file landed
	// but plurx has not been told yet" is a state a person should be able to
	// SEE, not deduce from a delivery log they had to know to open.
	inflight *transfers.Registry
	// now and sendTimeout are the clock and each attempt's deadline;
	// tests shorten both.
	now         func() time.Time
	sendTimeout time.Duration

	// The delivery lanes: rows in flight, and how many per notifier.
	mu      sync.Mutex
	running map[int64]bool
	lanes   map[int64]int
	wg      sync.WaitGroup
	// featuresRead marks notifiers whose features this session re-read.
	featuresRead map[int64]bool
}

// WithRegistry reports deliveries on the in-flight data-plane view.
func (d *Dispatcher) WithRegistry(r *transfers.Registry) *Dispatcher {
	d.inflight = r
	return d
}

// New returns a Dispatcher.
func New(db *sqlite.DB, b *bus.Bus, log *slog.Logger, f Factory) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{db: db, bus: b, log: log, new: f, now: time.Now, sendTimeout: sendTimeout,
		running: map[int64]bool{}, lanes: map[int64]int{}}
}

// Run subscribes and dispatches until ctx ends. Call in a goroutine.
func (d *Dispatcher) Run(ctx context.Context) {
	grabs, cancelG := bus.Subscribe[acquisition.ReleaseGrabbed](d.bus, 32)
	defer cancelG()
	fails, cancelF := bus.Subscribe[acquisition.ImportFailed](d.bus, 32)
	defer cancelF()
	healths, cancelH := bus.Subscribe[health.Changed](d.bus, 32)
	defer cancelH()

	for {
		select {
		case <-ctx.Done():
			return
		case e := <-grabs:
			d.dispatch(ctx, "grab", ports.Notification{
				Event: "grab", Title: "Release grabbed", Body: e.Title,
				Fields: map[string]string{"indexer": e.Indexer, "protocol": e.Protocol},
			})
		case e := <-fails:
			d.dispatch(ctx, "failed", ports.Notification{
				Event: "failed", Title: "Download failed", Body: e.Release,
				Fields: map[string]string{"reason": e.Reason},
			})
		case e := <-healths:
			d.dispatch(ctx, "health", ports.Notification{
				Event: "health", Title: "Health changed",
				Body: fmt.Sprintf("Overall status: %s", e.Overall),
			})
		}
	}
}

// wants maps an event key to the notifier's opt-in flag.
func wants(cfg ports.NotifierConfig, event string) bool {
	switch event {
	case "grab":
		return cfg.OnGrab
	case "import":
		return cfg.OnImport
	case "failed":
		return cfg.OnFailed
	case "health":
		return cfg.OnHealth
	}
	return false
}

// refreshOnly reports whether this notifier type acts on a media server
// rather than talking to a person. Those fire on imports and nothing else:
// a grab or a health flap gives them nothing to index.
func refreshOnly(cfg ports.NotifierConfig) bool {
	return cfg.Type == "plex" || cfg.Type == "jellyfin" || cfg.Type == "plurx"
}

// dispatch sends a chat-shaped event (grab, failed, health) inline, once, to
// every notifier that wants it. Imports never come through here: every
// notification of an import is an outbox row written in the import's own
// transaction, drained by RunDeliveries (pipeline contract v2 section 4.1).
func (d *Dispatcher) dispatch(ctx context.Context, event string, n ports.Notification) {
	configs, err := d.db.ListNotifiers(ctx)
	if err != nil {
		d.log.Warn("notify: cannot list notifiers", "err", err)
		return
	}
	for _, cfg := range configs {
		if !cfg.Enabled || !wants(cfg, event) {
			continue
		}
		if refreshOnly(cfg) {
			continue // media-server pokes only make sense after imports
		}
		target := d.new(cfg)
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := target.Send(cctx, n)
		cancel()
		if err != nil {
			d.log.Warn("notify: send failed", "notifier", cfg.Name, "type", cfg.Type,
				"transfer", transferOf(n), "err", err)
			continue
		}
		d.log.Debug("notify: sent", "notifier", cfg.Name, "event", event)
	}
}

func transferOf(n ports.Notification) string {
	if n.Import == nil {
		return ""
	}
	return n.Import.Transfer
}
