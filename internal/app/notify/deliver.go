package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Delivery to a media server is queued and persisted, not fired and
// forgotten (plan §5.5, §3.6).
//
// The difference matters for exactly one notifier type. A missed Discord
// message is a missed Discord message; a missed plurx scan is a file that
// never appears in the library. The common way to miss one is also the least
// exotic: a host reboots, both applications come back, and monarr's import
// finishes a few seconds before plurx is listening. An in-memory retry loop
// does not survive that, because the process that owns it is the one that
// just restarted.
//
// So the attempt schedule lives in the database. Three retries after the
// first try, at 5 s, 30 s and 2 m — long enough to ride out a restart,
// short enough that a genuinely dead server is marked failed while somebody
// is still around to read it.
var deliveryBackoff = []time.Duration{
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
}

// deliveryBatch bounds one pass. A backlog drains over several ticks rather
// than opening a hundred connections to a server that has just come back up
// and is, by definition, the least able to take them.
const deliveryBatch = 20

// queued reports whether this notifier's deliveries go through the durable
// queue rather than being attempted inline.
//
// Only the ones whose message IS the work. A chat notifier's retry would
// mean a Discord message arriving two minutes late with no way to tell it
// from a fresh one, which is worse than not arriving.
func queued(cfg ports.NotifierConfig) bool { return cfg.Type == "plurx" }

// enqueue records a delivery to be made by the worker.
func (d *Dispatcher) enqueue(ctx context.Context, cfg ports.NotifierConfig, n ports.Notification) {
	payload, err := json.Marshal(n)
	if err != nil {
		d.log.Warn("notify: cannot encode notification", "notifier", cfg.Name, "err", err)
		return
	}
	var downloadID int64
	if n.Import != nil {
		downloadID = n.Import.DownloadID
	}
	if _, err := d.db.EnqueueDelivery(ctx, cfg.ID, downloadID, n.Event, string(payload)); err != nil {
		d.log.Warn("notify: cannot queue delivery", "notifier", cfg.Name, "err", err)
	}
}

// RunDeliveries drains the delivery queue until ctx ends. Call in a
// goroutine beside Run.
//
// Separate from Run on purpose: Run is driven by the bus and would stall the
// event loop while a dead server timed out, and the queue has to be worked
// on a timer anyway — a delivery scheduled two minutes out has no event to
// wake it.
func (d *Dispatcher) RunDeliveries(ctx context.Context) {
	// One second: fine-grained enough that the 5 s first retry lands when
	// it should, cheap enough to leave running (one indexed query per tick
	// against a table that is almost always empty).
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			d.deliverDue(ctx)
		}
	}
}

func (d *Dispatcher) deliverDue(ctx context.Context) {
	due, err := d.db.DueDeliveries(ctx, deliveryBatch)
	if err != nil {
		d.log.Warn("notify: cannot read the delivery queue", "err", err)
		return
	}
	for _, del := range due {
		if ctx.Err() != nil {
			return
		}
		d.attempt(ctx, del)
	}
}

// attempt makes one delivery attempt and records what happened to it.
func (d *Dispatcher) attempt(ctx context.Context, del sqlite.Delivery) {
	// In flight for the duration of the attempt, including a retry that is
	// still waiting: "imported, but plurx does not know yet" is exactly the
	// handoff state that used to be invisible.
	h := d.inflight.Begin(transfers.Transfer{
		DownloadID: del.DownloadID,
		Stage:      transfers.StageNotifying,
		Peer:       "plurx",
		Outbound:   true,
		Detail:     deliveryDetail(del),
	})
	defer h.End()
	cfg, err := d.db.GetNotifier(ctx, del.NotifierID)
	if err != nil {
		// The notifier was deleted while this sat in the queue. Nothing to
		// deliver to and nothing anybody can act on — settle it rather than
		// retrying into a hole.
		d.settle(ctx, del, "", errors.New("notifier no longer exists"), true)
		return
	}
	var n ports.Notification
	if err := json.Unmarshal([]byte(del.Payload), &n); err != nil {
		d.settle(ctx, del, "", fmt.Errorf("unreadable payload: %w", err), true)
		return
	}

	target := d.new(cfg)
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	sendErr := target.Send(cctx, n)
	cancel()

	result := ""
	if r, ok := target.(ports.DeliveryReporter); ok {
		result = r.Delivery()
	}
	// Terminal failures are not retried: a path outside every library root
	// and a key without the scope are just as wrong on the fourth attempt,
	// and the delay only postpones the moment somebody reads the reason.
	d.settle(ctx, del, result, sendErr, errors.Is(sendErr, ports.ErrNotifyPermanent))
}

// settle writes the attempt's outcome and schedules the next one, or gives
// up and says so on the download's trace.
func (d *Dispatcher) settle(ctx context.Context, del sqlite.Delivery, result string, sendErr error, permanent bool) {
	del.Attempts++
	del.Result = result
	switch {
	case sendErr == nil:
		del.Status, del.LastError, del.NextAt = "ok", "", 0
	case permanent || int(del.Attempts) > len(deliveryBackoff):
		del.Status, del.LastError, del.NextAt = "failed", sendErr.Error(), 0
	default:
		del.Status = "pending"
		del.LastError = sendErr.Error()
		del.NextAt = time.Now().Add(deliveryBackoff[del.Attempts-1]).UnixMilli()
	}
	if err := d.db.SettleDelivery(ctx, del); err != nil {
		d.log.Warn("notify: cannot record delivery outcome", "delivery", del.ID, "err", err)
	}
	if del.Status == "pending" {
		d.log.Info("notify: delivery will be retried", "delivery", del.ID,
			"attempt", del.Attempts, "err", sendErr)
		return
	}
	d.trace(ctx, del, sendErr)
}

// trace writes the terminal outcome onto the download's handoff trace.
//
// This is the entry §8's inventory sends you to for "imported, but not in
// plurx" — so it has to answer that question on its own, without a second
// lookup: what plurx made of it, or why it never got there.
func (d *Dispatcher) trace(ctx context.Context, del sqlite.Delivery, sendErr error) {
	if del.DownloadID == 0 {
		return
	}
	var detail string
	switch {
	case sendErr == nil && del.Result != "":
		detail = "plurx " + del.Result
	case sendErr == nil:
		detail = "plurx notified"
	default:
		detail = fmt.Sprintf("plurx notify failed after %d attempt(s): %s",
			del.Attempts, sendErr)
	}
	if err := d.db.AppendHandoffStep(ctx, del.DownloadID, stepNotifyPlurx, detail); err != nil {
		d.log.Warn("notify: cannot write the handoff step", "download", del.DownloadID, "err", err)
	}
}

// stepNotifyPlurx names the trace step. New, so it extends the handoff
// vocabulary rather than renaming any of it (guardrail §10.9).
const stepNotifyPlurx = "notify_plurx"

// deliveryDetail says what this attempt is, in the words a person would use.
func deliveryDetail(del sqlite.Delivery) string {
	if del.Attempts == 0 {
		return "telling plurx what landed"
	}
	return fmt.Sprintf("retry %d — %s", del.Attempts+1, del.LastError)
}
