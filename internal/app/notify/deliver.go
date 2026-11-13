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
	"github.com/pjunod/monarr/internal/domain/backoff"
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
// on backoff.Outbox (see backoffAfter) WITHOUT END for anything transient,
// and read back: it is done only when Cinema reports every expected file
// placed with the bytes and mtime the import left it with.

// backoffAfter is the wait before the next try of a row that has been tried
// `attempts` times: the outbox ladder, then hourly for as long as it takes.
func backoffAfter(attempts int64) time.Duration {
	return backoff.Outbox.After(int(attempts) - 1)
}

// WarningAfter is how long a sub-delivery may stay pending before it is shown
// as a warning, with Cinema's waiting_on or the reason a file is not placed.
// It is surfaced, never given up on.
const WarningAfter = 30 * time.Minute

// sendTimeout is each attempt's own deadline. Per sub-delivery: three slow
// directories cannot starve each other, and a slow third one cannot make the
// first two be posted again.
const sendTimeout = 20 * time.Second

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
			d.wg.Wait() // let the attempts in flight settle their rows
			return
		case <-tick.C:
			// Never waits: a lane still busy with a slow server skips its
			// turn, the other lanes go on.
			d.dispatchDue(ctx)
		}
	}
}

// laneWidth is how many deliveries of ONE notifier may be in flight at once.
// Each notifier is its own lane: a Discord that times out cannot hold
// Cinema's sub-deliveries behind it, and three slow directories of one
// import are posted side by side, each under its own deadline. Bounded, so a
// backlog drains over several ticks rather than opening a hundred
// connections to a server that has just come back up and is, by definition,
// the least able to take them.
const laneWidth = 4

// dueScan bounds one read of the queue; rows already in flight are skipped.
const dueScan = 64

// dispatchDue starts every due delivery that has room in its notifier's lane
// and is not already in flight, and returns without waiting for them.
func (d *Dispatcher) dispatchDue(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	due, err := d.db.DueDeliveries(ctx, d.now().UnixMilli(), dueScan)
	if err != nil {
		d.log.Warn("notify: cannot read the delivery queue", "err", err)
		return
	}
	for _, del := range due {
		if ctx.Err() != nil {
			return
		}
		d.mu.Lock()
		busy := d.running[del.ID]
		width := d.lanes[del.NotifierID]
		if busy || width >= laneWidth {
			d.mu.Unlock()
			continue
		}
		d.running[del.ID] = true
		d.lanes[del.NotifierID]++
		d.mu.Unlock()
		d.wg.Add(1)
		go func(del sqlite.Delivery) {
			defer d.wg.Done()
			defer func() {
				d.mu.Lock()
				delete(d.running, del.ID)
				d.lanes[del.NotifierID]--
				d.mu.Unlock()
			}()
			d.attempt(ctx, del)
		}(del)
	}
}

// deliverDue starts the due deliveries and waits for every attempt in
// flight to settle — one synchronous pass, for tests and shutdown.
func (d *Dispatcher) deliverDue(ctx context.Context) {
	d.dispatchDue(ctx)
	d.wg.Wait()
}

// attempt makes one delivery attempt and records what happened to it.
func (d *Dispatcher) attempt(ctx context.Context, del sqlite.Delivery) {
	cfg, err := d.db.GetNotifier(ctx, del.NotifierID)
	if err != nil {
		// The notifier was deleted while this sat in the queue. Nothing to
		// deliver to and nothing anybody can act on — settle it rather than
		// retrying into a hole.
		d.settle(ctx, del, "", "", errors.New("notifier no longer exists"), true)
		return
	}
	// In flight for the duration of the attempt, including a retry that is
	// still waiting: "imported, but plurx does not know yet" is exactly the
	// handoff state that used to be invisible.
	h := d.inflight.Begin(transfers.Transfer{
		DownloadID: del.DownloadID,
		Stage:      transfers.StageNotifying,
		Peer:       cfg.Type,
		Outbound:   true,
		Detail:     deliveryDetail(cfg.Type, del),
	})
	defer h.End()
	var n ports.Notification
	if err := json.Unmarshal([]byte(del.Payload), &n); err != nil {
		d.settle(ctx, del, cfg.Type, "", fmt.Errorf("unreadable payload: %w", err), true)
		return
	}

	target := d.new(cfg)
	if n.Event == "moved" && n.Import != nil && del.RemoteRequestID == "" {
		var fresh bool
		target, fresh = d.refreshFeatures(ctx, cfg, target)
		if !fresh && cfg.HasFeature(ports.FeatureRemovedPaths) {
			// A common-parent request is never sent on a stored feature that
			// could not be confirmed: a downgraded Cinema would full-scan it.
			d.settleSub(ctx, del, subAttempt{peer: cfg.Type, notifierID: del.NotifierID}, ports.NotifyResult{},
				&ports.NotifyError{Retry: ports.RetrySameAttempt, Msg: "cinema's features could not be re-read before a moved request; trying again"})
			return
		}
	}
	if scanner, ok := target.(ports.ScanSender); ok && del.IdempotencyKey != "" {
		d.attemptSub(ctx, del, cfg.Type, n, target, scanner)
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
	d.settle(ctx, del, cfg.Type, result, sendErr, !queued(cfg) || errors.Is(sendErr, ports.ErrNotifyPermanent))
}

// settle writes a row's outcome when it is not a sub-delivery (a chat
// notifier's import, a row written before sub-deliveries existed) and
// schedules the next try, or ends it and says so on the download's trace.
func (d *Dispatcher) settle(ctx context.Context, del sqlite.Delivery, peer, result string, sendErr error, permanent bool) {
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
	d.store(ctx, del, peer, sqlite.DeliveryTiming{}, sendErr)
}

// attemptSub makes one attempt of a sub-delivery: post it, or — once Cinema
// has queued it — read the request back.
func (d *Dispatcher) attemptSub(ctx context.Context, del sqlite.Delivery, peer string, n ports.Notification, target ports.Notifier, scanner ports.ScanSender) {
	cctx, cancel := context.WithTimeout(ctx, d.sendTimeout)
	defer cancel()
	var res ports.NotifyResult
	var err error
	polled := del.RemoteRequestID != ""
	if polled {
		reader, ok := target.(ports.ResultReader)
		if !ok {
			err = &ports.NotifyError{Retry: ports.RetryUnsupported, Msg: "the notifier cannot read results"}
		} else {
			res, err = reader.ReadResult(cctx, del.RemoteRequestID)
		}
		var ne *ports.NotifyError
		if errors.As(err, &ne) && ne.Retry == ports.RetryUnsupported && del.RemoteState != "" {
			// A read of this request answered before, so this Cinema does
			// report results: a refusal now is a request it no longer knows
			// (pruned, lost). Post it again as a fresh attempt.
			err = &ports.NotifyError{Retry: ports.RetryNewAttempt, Status: ne.Status,
				Msg: "cinema no longer knows request " + del.RemoteRequestID + ": " + ne.Msg}
		}
	} else {
		n.IdempotencyKey = del.RequestKey()
		res, err = scanner.SendScan(cctx, n)
	}
	sub := subAttempt{peer: peer, polled: polled, notifierID: del.NotifierID}
	if n.Import != nil {
		sub.total = len(n.Import.Paths)
		sub.removed = n.Import.RemovedPaths
		sub.moved = n.Event == "moved"
		// A read of a queued request is judged by what its POST sent.
		sub.sentRemoved = res.RemovedPathsSent
		if polled {
			sub.sentRemoved = del.RemovedSent
		} else if sub.moved {
			if err := d.db.SetDeliveryRemovedSent(ctx, del.ID, res.RemovedPathsSent); err != nil {
				d.log.Warn("notify: cannot record what a moved request sent", "delivery", del.ID, "err", err)
			}
		}
		if del.DownloadID != 0 {
			sub.transfer = pipeline.Key(n.Import.Transfer, del.DownloadID)
		}
	}
	d.settleSub(ctx, del, sub, res, err)
}

// subAttempt is what settling a sub-delivery attempt needs to know beyond
// the row: who it went to, whether it was a read of a queued request, the
// timing it belongs to, and how many files the directory holds.
type subAttempt struct {
	peer     string
	polled   bool
	transfer string
	total    int
	// A Re-match's moved request (contract amendment 4.2): the paths that
	// left the directory, and whether Cinema was sent them.
	notifierID  int64
	removed     []string
	moved       bool // a moved request, whether or not this chunk names removed paths
	sentRemoved bool
}

// oldCinemaNote is what a move against a Cinema without removed_paths says
// on the delivery (contract amendment 6). Not a failure, no retry loop.
const oldCinemaNote = "warning: cinema does not support removed paths; the old entry stays until its next scan"

// featuresFresh is how long a feature read is trusted for a moved request.
const featuresFresh = 10 * time.Minute

// refreshFeatures re-reads what Cinema supports before a moved request
// (contract amendment 5.3) unless this session read it within featuresFresh,
// stores it and rebuilds the notifier with it. fresh is false when the probe
// failed: the stored features are unconfirmed.
func (d *Dispatcher) refreshFeatures(ctx context.Context, cfg ports.NotifierConfig, target ports.Notifier) (ports.Notifier, bool) {
	prober, ok := target.(ports.FeatureProber)
	if !ok {
		return target, true
	}
	d.mu.Lock()
	if d.featuresRead == nil {
		d.featuresRead = map[int64]time.Time{}
	}
	last := d.featuresRead[cfg.ID]
	d.mu.Unlock()
	if !last.IsZero() && d.now().Sub(last) < featuresFresh {
		return target, true
	}
	pctx, cancel := context.WithTimeout(ctx, d.sendTimeout)
	features, err := prober.ProbeFeatures(pctx)
	cancel()
	if err != nil {
		d.log.Info("notify: cinema features not re-read", "notifier", cfg.ID, "err", err)
		return target, false
	}
	d.mu.Lock()
	d.featuresRead[cfg.ID] = d.now()
	d.mu.Unlock()
	if err := d.db.SetNotifierFeatures(ctx, cfg.ID, features); err != nil {
		d.log.Warn("notify: cannot store cinema features", "notifier", cfg.ID, "err", err)
	}
	cfg.Features = features
	return d.new(cfg), true
}

// removedVerdict is what Cinema said of a moved request's removed paths
// (contract 4.4). gone and unknown satisfy; retry holds the sub-delivery
// (transient: try again on the schedule); warn is a lasting answer, kept on
// the row as a structured entry without holding it.
type removedVerdict struct {
	retry    []string
	warn     []sqlite.DeliveryFileResult
	answered bool // false: some removed path has no entry (an older Cinema)
}

// presentVerdict words a present answer. transient says the file only
// reappeared before cinema removed it, so asking again will find it gone.
func presentVerdict(path, reason string) (words string, transient bool) {
	switch reason {
	case ports.ReasonSymlinkInPath:
		return fmt.Sprintf("cinema found a symlink on the path of %s and kept the entry", path), false
	case ports.ReasonCaseVariant:
		return fmt.Sprintf("the path of %s differs only in case on cinema's filesystem", path), false
	case ports.ReasonReappeared:
		return fmt.Sprintf("cinema is holding %s: it reappeared before cinema removed it", path), true
	case ports.ReasonExists, "":
		return fmt.Sprintf("cinema still has %s", path), false
	}
	return fmt.Sprintf("cinema still has %s: %s", path, reason), false
}

// retainedVerdict words a retained answer and says whether to ask again.
func retainedVerdict(path string, f ports.PlacedReport) (words string, retry bool) {
	why := f.Reason
	if why == "" {
		why = "code " + f.Code
	}
	switch f.Code {
	case ports.RetainedExpectedNotPlaced:
		return fmt.Sprintf("cinema is holding %s: an expected file is not placed yet", path), true
	case ports.RetainedPruneBound:
		return fmt.Sprintf("cinema is holding %s: it is at its prune limit", path), true
	case ports.RetainedConversionHold:
		return fmt.Sprintf("cinema is holding %s: the file is held for a conversion", path), true
	case ports.RetainedRootIdentityUnset:
		return fmt.Sprintf("cinema kept %s: its library root has no recorded identity yet; it will drop it at its next full scan", path), false
	case ports.RetainedRootIdentityMismatch:
		return fmt.Sprintf("cinema kept %s: its library root is not the one it indexed; it will drop it at its next full scan", path), false
	case ports.RetainedRecordKept:
		if f.Reason == "" {
			why = "it kept the record on purpose"
		}
		return fmt.Sprintf("cinema kept the record of %s: %s", path, why), false
	}
	// A code this curator does not know: cinema's own word on retrying, and
	// its own reason text.
	if f.Retryable {
		return fmt.Sprintf("cinema is holding %s: %s", path, why), true
	}
	return fmt.Sprintf("cinema kept %s: %s", path, why), false
}

func judgeRemoved(removed []string, files []ports.PlacedReport) removedVerdict {
	byPath := map[string]ports.PlacedReport{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	v := removedVerdict{answered: true}
	for _, r := range removed {
		f, ok := byPath[r]
		switch {
		case !ok:
			v.answered = false
		case f.State == "present":
			words, transient := presentVerdict(r, f.Reason)
			if transient {
				v.retry = append(v.retry, words)
				break
			}
			v.warn = append(v.warn, sqlite.DeliveryFileResult{Path: r, State: "present", Reason: f.Reason, Warning: words})
		case f.State == "retained":
			words, retry := retainedVerdict(r, f)
			if retry {
				v.retry = append(v.retry, words)
				break
			}
			v.warn = append(v.warn, sqlite.DeliveryFileResult{Path: r, State: "retained", Code: f.Code, Reason: f.Reason, ItemID: f.ItemID, Warning: words})
		}
	}
	return v
}

// noResults is the delivery log's line for a Cinema that accepted the request
// but does not report what it made of it (contract section 6).
const noResults = "queued (cinema does not report results)"

// settleSub classifies one sub-delivery attempt (contract section 4.2) and
// writes it.
func (d *Dispatcher) settleSub(ctx context.Context, del sqlite.Delivery, sub subAttempt, res ports.NotifyResult, err error) {
	now := d.now()
	del.Attempts++
	timing := sqlite.DeliveryTiming{Transfer: sub.transfer}
	var wait time.Duration // what the far side asked for, if more than the schedule
	pending := func() {
		next := backoffAfter(del.Attempts)
		if wait > next {
			next = wait
		}
		del.Status, del.NextAt = "pending", now.Add(next).UnixMilli()
	}
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
			retry, wait = ne.Retry, ne.RetryAfter
		}
		switch retry {
		case ports.RetryNever:
			del.Status, del.NextAt = "failed", 0
			// Never visible, so not a sample of how long visible takes —
			// and leaving it open would make the timing's end depend on
			// which directory settled first.
			timing.Discard = true
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
		// remote_state is what a READ of the request said; a 202 to the
		// POST leaves it empty, so a refused first read means an older
		// Cinema and a refused later one means a lost request.
		if sub.polled {
			del.RemoteState = res.State
		}
		del.WaitingOn, del.LastError = res.WaitingOn, ""
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
		del.FileResults = fileResults(del.ExpectedFiles, res.Files)
		missing := notPlaced(del.ExpectedFiles, res.Files)
		compat := ""
		var warns []string
		if len(sub.removed) > 0 {
			verdict := judgeRemoved(sub.removed, res.Files)
			switch {
			case !sub.sentRemoved:
				compat = oldCinemaNote
			case !verdict.answered:
				// The backstop: a Cinema that took the request but did not
				// answer for each removed path does not support them.
				compat = oldCinemaNote
				d.forgetRemovedPaths(ctx, sub.notifierID)
			default:
				missing = append(missing, verdict.retry...)
				for _, w := range verdict.warn {
					warns = append(warns, w.Warning)
				}
				del.FileResults = append(del.FileResults, verdict.warn...)
			}
		}
		if len(missing) == 0 {
			del.Status, del.NextAt, del.LastError = "ok", 0, ""
			del.VisibleAt = now.UnixMilli()
			del.Result = "visible"
			if ids := itemList(res.ItemIDs); ids != "" {
				del.Result += " → plurx item " + ids
			}
			if compat != "" {
				del.Result += "; " + compat
			}
			if len(warns) > 0 {
				// Lasting answers: shown, not retried forever.
				del.Result += "; warning: " + warns[0]
				if len(warns) > 1 {
					del.Result += fmt.Sprintf(" (and %d more)", len(warns)-1)
				}
			}
			if sub.total > len(del.ExpectedFiles) {
				// Cinema takes at most 64 expected files a request; the
				// rest of the directory was scanned but not checked.
				del.Result += fmt.Sprintf(" (verified %d of %d files)", len(del.ExpectedFiles), sub.total)
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
	d.store(ctx, del, sub.peer, timing, errors.New(del.LastError))
}

// forgetRemovedPaths drops removed_paths from a notifier's stored features
// after an answer showed it does not support them.
func (d *Dispatcher) forgetRemovedPaths(ctx context.Context, id int64) {
	cfg, err := d.db.GetNotifier(ctx, id)
	if err != nil {
		return
	}
	var kept []string
	for _, f := range cfg.Features {
		if f != ports.FeatureRemovedPaths {
			kept = append(kept, f)
		}
	}
	if err := d.db.SetNotifierFeatures(ctx, id, kept); err != nil {
		d.log.Warn("notify: cannot store cinema features", "notifier", id, "err", err)
	}
}

// fileResults keeps Cinema's answer for every expected file, so the item page
// can say which episode of a season pack is in Cinema and which is not. A
// file Cinema did not mention is missing.
func fileResults(expected []ports.ExpectedFile, files []ports.PlacedReport) []sqlite.DeliveryFileResult {
	byPath := map[string]ports.PlacedReport{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	out := make([]sqlite.DeliveryFileResult, 0, len(expected))
	for _, e := range expected {
		f, ok := byPath[e.Path]
		if !ok {
			out = append(out, sqlite.DeliveryFileResult{Path: e.Path, State: "missing"})
			continue
		}
		out = append(out, sqlite.DeliveryFileResult{Path: e.Path, State: f.State, ItemID: f.ItemID, Bytes: f.Bytes,
			MtimeNs: f.MtimeNs, Reason: f.Reason, Matched: f.State == "placed" && f.Bytes == e.Bytes && f.MtimeNs == e.MtimeNs})
	}
	return out
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
func (d *Dispatcher) store(ctx context.Context, del sqlite.Delivery, peer string, timing sqlite.DeliveryTiming, sendErr error) {
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
	d.trace(ctx, del, peer, sendErr)
}

// trace writes the terminal outcome onto the download's handoff trace.
//
// This is the entry §8's inventory sends you to for "imported, but not in
// plurx" — so it has to answer that question on its own, without a second
// lookup: what plurx made of it, or why it never got there.
func (d *Dispatcher) trace(ctx context.Context, del sqlite.Delivery, peer string, sendErr error) {
	if del.DownloadID == 0 {
		return
	}
	if peer == "" {
		peer = "notifier" // its notifier is gone; the row still says why
	}
	var detail string
	switch {
	case sendErr == nil && del.Result != "":
		detail = peer + " " + del.Result
	case sendErr == nil:
		detail = peer + " notified"
	default:
		detail = fmt.Sprintf("%s notify failed after %d attempt(s): %s",
			peer, del.Attempts, sendErr)
	}
	if del.Directory != "" {
		detail += " — " + del.Directory
	}
	if err := d.db.AppendHandoffStep(ctx, del.DownloadID, notifyStep(peer), detail); err != nil {
		d.log.Warn("notify: cannot write the handoff step", "download", del.DownloadID, "err", err)
	}
}

// stepNotifyPlurx names Cinema's trace step. New in its time, so it extends
// the handoff vocabulary rather than renaming any of it (guardrail §10.9);
// every other notifier type gets notify_<type> beside it.
const stepNotifyPlurx = "notify_plurx"

// notifyStep is the handoff step a notifier type writes.
func notifyStep(peer string) string {
	if peer == "plurx" {
		return stepNotifyPlurx
	}
	return "notify_" + peer
}

// deliveryDetail says what this attempt is, in the words a person would use.
func deliveryDetail(peer string, del sqlite.Delivery) string {
	if del.Attempts == 0 {
		return "telling " + peer + " what landed"
	}
	return fmt.Sprintf("retry %d — %s", del.Attempts+1, del.LastError)
}
