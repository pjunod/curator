// Package notify is the Phase 3 notification dispatcher: it subscribes to
// bus events and fans them out to every enabled notifier whose event flags
// match. Adapters are injected as a factory, like indexers and clients.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/monarr-media/monarr/internal/app/acquisition"
	"github.com/monarr-media/monarr/internal/app/health"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
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
}

// New returns a Dispatcher.
func New(db *sqlite.DB, b *bus.Bus, log *slog.Logger, f Factory) *Dispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{db: db, bus: b, log: log, new: f}
}

// Run subscribes and dispatches until ctx ends. Call in a goroutine.
func (d *Dispatcher) Run(ctx context.Context) {
	grabs, cancelG := bus.Subscribe[acquisition.ReleaseGrabbed](d.bus, 32)
	defer cancelG()
	imports, cancelI := bus.Subscribe[acquisition.ImportCompleted](d.bus, 32)
	defer cancelI()
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
		case e := <-imports:
			d.dispatch(ctx, "import", ports.Notification{
				Event: "import", Title: "Import completed", Body: e.Release,
				Fields: map[string]string{
					"files":   fmt.Sprintf("%d", e.Files),
					"upgrade": fmt.Sprintf("%v", e.Upgrade),
				},
				// The same event twice, in two registers: a sentence for the
				// notifiers that render text, and the facts for the ones
				// that act on it.
				Import: &ports.ImportInfo{
					MediaItemID: e.MediaItemID,
					Paths:       e.Paths,
					Episode:     e.Episode,
					TMDBID:      e.TMDBID,
					IMDBID:      e.IMDBID,
					Transfer:    e.Transfer,
				},
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
		if refreshOnly(cfg) && event != "import" {
			continue // media-server pokes only make sense after imports
		}
		target := d.new(cfg)
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := target.Send(cctx, n)
		cancel()
		// What the far side said, when it said anything worth keeping.
		// Asked for after Send either way: a partial delivery has both a
		// failure and a result, and dropping the result would lose the half
		// that worked.
		result := ""
		if r, ok := target.(ports.DeliveryReporter); ok {
			result = r.Delivery()
		}
		if err != nil {
			d.log.Warn("notify: send failed", "notifier", cfg.Name, "type", cfg.Type,
				"transfer", transferOf(n), "err", err)
			d.record(ctx, cfg, n, result, err)
			continue
		}
		d.log.Info("notify: sent", "notifier", cfg.Name, "event", event,
			"transfer", transferOf(n), "result", result)
		d.record(ctx, cfg, n, result, nil)
	}
}

// record writes an import delivery into the item's history.
//
// A notification that failed is otherwise visible only in the server log,
// which is the one place nobody looks until they already suspect something.
// On the item's own history it sits beside the import it belongs to, which
// is where somebody asking "why hasn't this shown up in plurx" is looking
// anyway. Only import deliveries are recorded: history is per media item,
// and a health notification has no item to belong to.
func (d *Dispatcher) record(ctx context.Context, cfg ports.NotifierConfig, n ports.Notification, result string, sendErr error) {
	if n.Import == nil || n.Import.MediaItemID == 0 || !refreshOnly(cfg) {
		return
	}
	detail := map[string]any{
		"notifier": cfg.Name,
		"type":     cfg.Type,
		"paths":    len(n.Import.Paths),
	}
	if n.Import.Transfer != "" {
		detail["transfer"] = n.Import.Transfer
	}
	if result != "" {
		// The far end's own words. This is where "imported" stops being the
		// end of the story and becomes "…and plurx made item 1201 of it".
		detail["result"] = result
	}
	kind := "notified"
	if sendErr != nil {
		kind = "notify_failed"
		detail["error"] = sendErr.Error()
	}
	if err := d.db.AddHistory(ctx, kind, n.Import.MediaItemID, n.Body, detail); err != nil {
		d.log.Debug("notify: could not record delivery", "err", err)
	}
}

func transferOf(n ports.Notification) string {
	if n.Import == nil {
		return ""
	}
	return n.Import.Transfer
}
