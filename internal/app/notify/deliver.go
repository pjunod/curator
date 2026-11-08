package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/app/pipeline"
	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Every notification of an import is a row in notifier_deliveries, written in
// the import's own transaction (pipeline contract v2 section 4.1), and this
// worker drains them.
//
// A chat-shaped notifier's row is attempted once: a Discord message arriving
// an hour late with no way to tell it from a fresh one is worse than none.
//
// Cinema's rows are sub-deliveries, one per directory of an import, and for
// them the message IS the work: a missed scan is a file that never appears in
// the library. Each is delivered on its own, under its own deadline, retried
// on the schedule below WITHOUT END for anything transient, and read back:
// it is done only when Cinema reports every expected file placed with the
// bytes and mtime the import left it with.
var deliveryBackoff = []time.Duration{
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
}

// backoffAfter is the wait before the next try of a row that has been tried
// `attempts` times: the schedule, then hourly for as long as it takes.
func backoffAfter(attempts int64) time.Duration {
	i := int(attempts) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(deliveryBackoff) {
		return deliveryBackoff[len(deliveryBackoff)-1]
	}
	return deliveryBackoff[i]
}

// WarningAfter is how long a sub-delivery may stay pending before it is shown
// as a warning, with Cinema's waiting_on or the reason a file is not placed.
// It is surfaced, never given up on.
const WarningAfter = 30 * time.Minute

// sendTimeout is each attempt's own deadline. Per sub-delivery: three slow
// directories cannot starve each other, and a slow third one cannot make the
// first two be posted again.
const sendTimeout = 20 * time.Second

// deliveryBatch bounds one pass. A backlog drains over several ticks rather
// than opening a hundred connections to a server that has just come back up
// and is, by definition, the least able to take them.
const deliveryBatch = 20

// queued reports whether this notifier's deliveries are retried (the ones
// whose message IS the work) rather than attempted once.
func queued(cfg ports.NotifierConfig) bool { return ports.QueuedNotifier(cfg) }

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
	due, err := d.db.DueDeliveries(ctx, d.now().UnixMilli(), deliveryBatch)
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
	if scanner, ok := target.(ports.ScanSender); ok && del.IdempotencyKey != "" {
		d.attemptSub(ctx, del, n, target, scanner)
		return
	}
	cctx, cancel := context.WithTimeout(ctx, d.sendTimeout)
	sendErr := target.Send(cctx, n)
	cancel()

	result := ""
	if r, ok := target.(ports.DeliveryReporter); ok {
		result = r.Delivery()
	}
	// A chat notifier's import is attempted once. For the rest, terminal
	// failures are not retried: a key without the scope is just as wrong on
	// the fourth attempt, and the delay only postpones the moment somebody
	// reads the reason.
	d.settle(ctx, del, result, sendErr, !queued(cfg) || errors.Is(sendErr, ports.ErrNotifyPermanent))
}

// settle writes a row's outcome when it is not a sub-delivery (a chat
// notifier's import, a row written before sub-deliveries existed) and
// schedules the next try, or ends it and says so on the download's trace.
func (d *Dispatcher) settle(ctx context.Context, del sqlite.Delivery, result string, sendErr error, permanent bool) {
	del.Attempts++
	del.Result = result
	switch {
	case sendErr == nil:
		del.Status, del.LastError, del.NextAt = "ok", "", 0
	case permanent:
		del.Status, del.LastError, del.NextAt = "failed", sendErr.Error(), 0
	default:
		del.Status = "pending"
		del.LastError = sendErr.Error()
		del.NextAt = d.now().Add(backoffAfter(del.Attempts)).UnixMilli()
	}
	d.store(ctx, del, sqlite.DeliveryTiming{}, sendErr)
}

// attemptSub makes one attempt of a sub-delivery: post it, or — once Cinema
// has queued it — read the request back.
func (d *Dispatcher) attemptSub(ctx context.Context, del sqlite.Delivery, n ports.Notification, target ports.Notifier, scanner ports.ScanSender) {
	cctx, cancel := context.WithTimeout(ctx, d.sendTimeout)
	defer cancel()
	var res ports.NotifyResult
	var err error
	if del.RemoteRequestID != "" {
		reader, ok := target.(ports.ResultReader)
		if !ok {
			err = &ports.NotifyError{Retry: ports.RetryUnsupported, Msg: "the notifier cannot read results"}
		} else {
			res, err = reader.ReadResult(cctx, del.RemoteRequestID)
		}
	} else {
		n.IdempotencyKey = del.RequestKey()
		res, err = scanner.SendScan(cctx, n)
	}
	transfer := ""
	if n.Import != nil && del.DownloadID != 0 {
		transfer = pipeline.Key(n.Import.Transfer, del.DownloadID)
	}
	d.settleSub(ctx, del, transfer, res, err)
}

// noResults is the delivery log's line for a Cinema that accepted the request
// but does not report what it made of it (contract section 6).
const noResults = "queued (cinema does not report results)"

// settleSub classifies one sub-delivery attempt (contract section 4.2) and
// writes it.
func (d *Dispatcher) settleSub(ctx context.Context, del sqlite.Delivery, transfer string, res ports.NotifyResult, err error) {
	now := d.now()
	del.Attempts++
	timing := sqlite.DeliveryTiming{Transfer: transfer}
	pending := func() { del.Status, del.NextAt = "pending", now.Add(backoffAfter(del.Attempts)).UnixMilli() }
	// A confirmed retryable result: the next try needs a fresh scan, not
	// the cached answer to this attempt.
	freshAttempt := func() {
		del.Attempt++
		del.RemoteRequestID, del.RemoteState, del.WaitingOn = "", "", ""
		pending()
	}
	// Accepted, but the far side will never say what it placed: done as far
	// as anybody can tell, and not measurable.
	unreported := func(result string) {
		del.Status, del.Result, del.LastError, del.NextAt = "ok", result, "", 0
		timing.Discard = true
	}
	var ne *ports.NotifyError
	switch {
	case err != nil && errors.As(err, &ne) && ne.Retry == ports.RetryUnsupported:
		unreported(noResults)
	case err != nil:
		del.LastError = err.Error()
		retry := ports.RetrySameAttempt
		if ne != nil {
			retry = ne.Retry
		}
		switch retry {
		case ports.RetryNever:
			del.Status, del.NextAt = "failed", 0
		case ports.RetryNewAttempt:
			freshAttempt()
		default:
			// Ambiguous (a timeout, a connection error, no response) or
			// refused before it was recorded: the same key again, so a
			// request that did land is replayed rather than duplicated.
			pending()
		}
	case res.State == "queued" || res.State == "running":
		if res.RequestID == "" && del.RemoteRequestID == "" {
			unreported(noResults)
			break
		}
		if res.RequestID != "" {
			del.RemoteRequestID = res.RequestID
		}
		del.RemoteState, del.WaitingOn, del.LastError = res.State, res.WaitingOn, ""
		del.Result = res.State + " as " + del.RemoteRequestID
		if res.WaitingOn != "" {
			del.Result += ", waiting on " + res.WaitingOn
		}
		pending()
	case res.State == "failed":
		del.LastError = "cinema could not complete the scan"
		if res.ErrCode != "" {
			del.LastError += " (" + res.ErrCode + ")"
		}
		if res.Err != "" {
			del.LastError += ": " + res.Err
		}
		del.Result = ""
		freshAttempt()
	case res.State == "done":
		del.ItemIDs = res.ItemIDs
		del.RemoteState, del.WaitingOn = "done", ""
		if !res.FilesReported {
			unreported(scannedText(res.ItemIDs) + " (cinema does not report results)")
			break
		}
		missing := notPlaced(del.ExpectedFiles, res.Files)
		if len(missing) == 0 {
			del.Status, del.NextAt, del.LastError = "ok", 0, ""
			del.VisibleAt = now.UnixMilli()
			del.Result = "visible"
			if ids := itemList(res.ItemIDs); ids != "" {
				del.Result += " → plurx item " + ids
			}
			break
		}
		del.LastError = ""
		del.Result = "warning: " + missing[0]
		if len(missing) > 1 {
			del.Result += fmt.Sprintf(" (and %d more)", len(missing)-1)
		}
		freshAttempt()
	default:
		del.LastError = fmt.Sprintf("cinema answered with an unknown state %q", res.State)
		pending()
	}
	d.store(ctx, del, timing, errors.New(del.LastError))
}

// notPlaced lists, in the delivery log's words, every expected file Cinema
// did not report placed with the same bytes and mtime_ns.
func notPlaced(expected []ports.ExpectedFile, files []ports.PlacedReport) []string {
	byPath := map[string]ports.PlacedReport{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	var out []string
	for _, e := range expected {
		f, ok := byPath[e.Path]
		var reason string
		switch {
		case !ok:
			reason = "not in cinema's report"
		case f.State == "placed" && f.Bytes == e.Bytes && f.MtimeNs == e.MtimeNs:
			continue
		case f.State == "placed":
			reason = fmt.Sprintf("cinema indexed %d bytes, mtime_ns %d; the import placed %d bytes, mtime_ns %d",
				f.Bytes, f.MtimeNs, e.Bytes, e.MtimeNs)
		case f.State == "skipped" && f.Reason != "":
			reason = f.Reason
		default:
			reason = f.State
		}
		out = append(out, fmt.Sprintf("expected %s not placed (%s)", e.Path, reason))
	}
	return out
}

func itemList(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ", ")
}

func scannedText(ids []int64) string {
	if s := itemList(ids); s != "" {
		return "scanned → plurx item " + s
	}
	return "scanned"
}

// store writes a settled attempt and, once it is over, the download's trace.
func (d *Dispatcher) store(ctx context.Context, del sqlite.Delivery, timing sqlite.DeliveryTiming, sendErr error) {
	if err := d.db.SettleDelivery(ctx, del, timing); err != nil {
		d.log.Warn("notify: cannot record delivery outcome", "delivery", del.ID, "err", err)
	}
	if del.Status == "pending" {
		d.log.Info("notify: delivery will be retried", "delivery", del.ID, "directory", del.Directory,
			"attempt", del.Attempt, "tries", del.Attempts, "result", del.Result, "err", del.LastError)
		return
	}
	if del.Status == "ok" {
		sendErr = nil
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
	if del.Directory != "" {
		detail += " — " + del.Directory
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
