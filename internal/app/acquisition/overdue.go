package acquisition

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// ---- every non-terminal download has an exit at its deadline ---------------

// Until v0.45.2 the deadline sweep acted only on held rows, and absence
// detection (detectGone) gave up on a job only when Runner's request receipt
// was missing too. A receipt that still existed but went stale — Runner
// moved hosts (nuc3 -> nuc4, 2026-11-12) and kept receipts saying "paused"
// or "fetching" for jobs no longer in its queue, or "completed" with no
// publication — was read as "still alive" forever, so 30 rows sat 45-91 h
// past their deadlines and only the pipeline:overdue warning said so.
//
// exitOverdue decides each overdue row of one Runner client on Runner's
// word, after a successful queue poll:
//
//   - Runner's receipt carries a terminal: it is applied (the reducer's own
//     terminal path, with its replacement grab and blocklist rules);
//   - the job is in Runner's queue and progressing (bytes, progress or stage
//     moved since the last sweep): the deadline is extended; one Runner holds
//     keeps waiting on Runner's own hold deadline (the row is held (stale));
//   - it is in the queue but has not moved for overdueStallLimit, or Runner
//     lists it blocked past Runner's own retry deadline: failed;
//   - it has been absent from Runner's queue for goneGrace across successful
//     polls and no terminal came (unknown, or a stale receipt), or the receipt
//     says completed and no publication arrived within overdueCompletedGrace:
//     failed (an empty queue decides nothing);
//
// When the job is still listed in Runner's queue (stalled or blocked), the
// failure is followed by one best-effort cancel in Runner; its outcome never
// changes the failure or the regrab.
//
// "Failed" is blameless (no blocklist) with the want grabbed again
// (FailOverdueDownload). An unreachable Runner decides nothing. A row whose
// client is gone or disabled is ended by exitOrphanedOverdue.

// overdueStallLimit is how long a job Runner still lists may sit without
// any progress past its deadline before Curator stops waiting for it.
const overdueStallLimit = 24 * time.Hour

// overdueCompletedGrace is how long a receipt that says completed without a
// publication is given for the publication to arrive by the deliveries feed
// (several polls) before the row is failed.
const overdueCompletedGrace = 15 * time.Minute

type overdueMark struct {
	client   int64
	bytes    int64
	progress float64
	stage    string
	state    ports.DownloadState
	since    time.Time
}

// overdueClock is a per-row "first seen at" clock of the overdue exit,
// scoped to the client whose poll keeps it.
type overdueClock struct {
	client int64
	since  time.Time
}

// progressing records st as the row's latest observation and reports
// whether Runner moved it since the previous one (any change counts: a
// restart from 0 is movement, not a stall; a first observation counts as
// moving), and for how long it has been still.
func (s *Service) progressing(client, id int64, st ports.DownloadStatus, now time.Time) (bool, time.Duration) {
	var bytes int64
	if st.BytesCompleted != nil {
		bytes = *st.BytesCompleted
	}
	cur := overdueMark{client: client, bytes: bytes, progress: st.Progress, stage: st.Stage, state: st.State, since: now}
	v, loaded := s.overdueMarks.Load(id)
	if !loaded {
		s.overdueMarks.Store(id, cur)
		return true, 0
	}
	prev := v.(overdueMark)
	if cur.bytes != prev.bytes || cur.progress != prev.progress || cur.stage != prev.stage || cur.state != prev.state {
		s.overdueMarks.Store(id, cur)
		return true, 0
	}
	return false, now.Sub(prev.since)
}

// heldFor reports how long a row's clock in m has run, starting it now.
func heldFor(m *sync.Map, client, id int64, now time.Time) time.Duration {
	v, _ := m.LoadOrStore(id, overdueClock{client: client, since: now})
	return now.Sub(v.(overdueClock).since)
}

// forgetOverdue drops every clock the overdue exit keeps for a row.
func (s *Service) forgetOverdue(id int64) {
	s.overdueMarks.Delete(id)
	s.overdueAbsent.Delete(id)
	s.overdueCompleted.Delete(id)
}

// pruneOverdueClocks drops the clocks of client's rows that are no longer
// overdue, so a row that leaves and re-enters starts afresh.
func (s *Service) pruneOverdueClocks(client int64, overdue map[int64]bool) {
	drop := func(m *sync.Map, owner func(any) int64) {
		m.Range(func(k, v any) bool {
			if id := k.(int64); owner(v) == client && !overdue[id] {
				m.Delete(id)
			}
			return true
		})
	}
	drop(&s.overdueMarks, func(v any) int64 { return v.(overdueMark).client })
	clock := func(v any) int64 { return v.(overdueClock).client }
	drop(&s.overdueAbsent, clock)
	drop(&s.overdueCompleted, clock)
}

// exitOverdue is the overdue exit for one Runner client (see above). It runs
// only after a successful queue poll; a queue that came back empty while
// rows are overdue is not trusted as "Runner lost them all" and decides
// nothing.
func (s *Service) exitOverdue(ctx context.Context, cfg ports.ClientConfig, feeds ports.RequestAdmissionProtocol, byTransfer, byHandle map[string]ports.DownloadStatus) {
	now := time.Now()
	rows, err := s.db.OverdueDownloads(ctx, cfg.ID, now)
	if err != nil {
		s.log.Warn("overdue rows unreadable", "client", cfg.Name, "err", err)
		return
	}
	overdue := make(map[int64]bool, len(rows))
	for _, dl := range rows {
		overdue[dl.ID] = true
	}
	s.pruneOverdueClocks(cfg.ID, overdue)
	if len(rows) > 0 && len(byTransfer) == 0 && len(byHandle) == 0 {
		s.log.Info("overdue exit skipped: Runner's queue came back empty", "client", cfg.Name, "overdue", len(rows))
		return
	}
	var cancels []overdueCancel
	// Best-effort Runner cancels of the jobs failed below run after the loop,
	// in the background, so they can never stall the poll.
	defer func() { s.cancelOverdueInRunner(cfg, cancels) }()
	for _, dl := range rows {
		if ctx.Err() != nil {
			return
		}
		if s.ImportRunning(dl.ID) || s.publicationOwnsExit(ctx, dl) {
			continue
		}
		st, inQueue := byTransfer[dl.Transfer]
		if !inQueue && dl.Transfer == "" && dl.Handle != "" {
			st, inQueue = byHandle[dl.Handle]
		}
		if dl.RunnerOutcome != "" && !dl.Provisional {
			// Runner's terminal is recorded but the row never left its state.
			s.failOverdue(ctx, dl, fmt.Sprintf("Runner's terminal (%s) is recorded but the row never left %s", dl.RunnerOutcome, dl.State), false)
			continue
		}
		lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
		adm, known, err := feeds.GetRequestAdmission(lookup, dl.Transfer)
		cancel()
		if err != nil {
			continue // unreachable: nothing is decided
		}
		if known {
			if _, terminal := admissionTerminal(adm); terminal {
				for _, f := range admissionFacts(adm) {
					_ = s.observe(ctx, dl, cfg, f)
				}
				if fresh, err := s.db.GetDownload(ctx, dl.ID); err != nil || fresh.State == "failed" || fresh.State == "imported" ||
					fresh.State == "notified" || fresh.RunnerOutcome != "" {
					s.forgetOverdue(dl.ID)
					continue
				}
			}
		}
		if inQueue {
			s.overdueAbsent.Delete(dl.ID)
		}
		switch {
		case inQueue && st.Control != nil && st.Control.Cause != "" && st.Control.DeadlineUnix != nil && now.Unix() > *st.Control.DeadlineUnix:
			// Runner's own retry deadline for what blocks it has passed and it
			// still lists the job blocked: it is not going to finish it.
			if s.failOverdue(ctx, dl, fmt.Sprintf("Runner has held it on %s past Runner's own deadline (%s)", st.Control.Cause,
				time.Unix(*st.Control.DeadlineUnix, 0).UTC().Format(time.RFC3339)), true) {
				cancels = append(cancels, overdueCancel{dl.ID, overdueHandle(dl, st)})
			}
		case inQueue && heldStatus(st):
			// Runner holds it: its hold is self-clearing and has its own
			// deadline (hold_expired), and the row stays held (stale)
			// meanwhile. Only a hold with no deadline of Runner's ends here,
			// after the stall limit without progress.
			if st.Control != nil && st.Control.DeadlineUnix != nil {
				continue
			}
			if _, still := s.progressing(cfg.ID, dl.ID, st, now); still >= overdueStallLimit {
				if s.failOverdue(ctx, dl, fmt.Sprintf("Runner has held it without progress for %s past its deadline", overdueStallLimit), true) {
					cancels = append(cancels, overdueCancel{dl.ID, overdueHandle(dl, st)})
				}
			}
		case inQueue:
			moved, still := s.progressing(cfg.ID, dl.ID, st, now)
			if moved {
				at := s.downloadingDeadline(dl)
				if ok, err := s.db.ExtendDownloadDeadline(ctx, dl, at); err == nil && ok {
					s.log.Info("overdue download still progressing in Runner; deadline extended", "download", dl.ID, "until", at)
				}
				continue
			}
			if still < overdueStallLimit {
				continue
			}
			if s.failOverdue(ctx, dl, fmt.Sprintf("Runner has made no progress on it for %s past its deadline", overdueStallLimit), true) {
				cancels = append(cancels, overdueCancel{dl.ID, overdueHandle(dl, st)})
			}
		case known && adm.Outcome == "completed":
			// Its publication may still be on its way by the deliveries feed.
			if heldFor(&s.overdueCompleted, cfg.ID, dl.ID, now) < overdueCompletedGrace {
				continue
			}
			s.failOverdue(ctx, dl, "Runner reports it completed but names no publication Curator can import, and none arrived by its deliveries feed", true)
		default:
			// Absent from the queue: decided only after goneGrace of
			// continuous absence across successful polls.
			if heldFor(&s.overdueAbsent, cfg.ID, dl.ID, now) < goneGrace {
				continue
			}
			if known {
				s.failOverdue(ctx, dl, fmt.Sprintf("Runner no longer has this job in its queue; its receipt still says %q", adm.Outcome), true)
			} else {
				s.failOverdue(ctx, dl, "Runner no longer knows this transfer (not in its queue and no receipt)", true)
			}
		}
	}
}

// overdueCancelTimeout bounds the best-effort Runner cancel.
const overdueCancelTimeout = 3 * time.Second

// overdueCancel is a job Curator failed while Runner still listed it.
type overdueCancel struct {
	id     int64
	handle string
}

func overdueHandle(dl sqlite.Download, st ports.DownloadStatus) string {
	if dl.Handle != "" {
		return dl.Handle
	}
	return string(st.Handle)
}

// cancelOverdueInRunner asks Runner, once and in the background, to cancel
// jobs Curator has already failed (the fail is committed before this runs).
// It uses the queue-only cancel: a job that finished since the poll answers
// 404, which is not an error, and its history record is never deleted. A
// client without that capability is not cancelled at all (no Remove
// fallback). A handle a live row (such as the replacement) now owns is
// skipped. Failures are logged and dropped, never retried.
func (s *Service) cancelOverdueInRunner(cfg ports.ClientConfig, cancels []overdueCancel) {
	if len(cancels) == 0 {
		return
	}
	go func() {
		client, ok := s.newClient(cfg).(ports.QueueCanceller)
		if !ok {
			return
		}
		for _, c := range cancels {
			if c.handle == "" {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), overdueCancelTimeout)
			if live, found, err := s.db.DownloadByHandle(ctx, cfg.ID, c.handle); err != nil || (found && live.ID != c.id) {
				cancel()
				continue
			}
			if err := client.CancelQueued(ctx, ports.Handle(c.handle)); err != nil {
				s.log.Warn("overdue download failed in Curator; Runner cancel not accepted (not retried)", "download", c.id, "client", cfg.Name, "err", err)
			} else {
				s.log.Info("overdue download failed in Curator; cancelled in Runner", "download", c.id, "client", cfg.Name)
			}
			cancel()
		}
	}()
}

// exitOrphanedOverdue ends overdue rows whose client is gone or disabled:
// nothing can ever report on them again.
func (s *Service) exitOrphanedOverdue(ctx context.Context) (int, error) {
	rows, err := s.db.OverdueDownloadsAnyClient(ctx, time.Now())
	if err != nil {
		return 0, err
	}
	ended := 0
	for _, dl := range rows {
		if s.ImportRunning(dl.ID) || s.publicationOwnsExit(ctx, dl) {
			continue
		}
		cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID)
		if err == nil && cfg.Enabled {
			continue // its client's poll decides it
		}
		if s.failOverdue(ctx, dl, "its download client is gone or disabled; nothing can report on it", false) {
			ended++
		}
	}
	return ended, nil
}

// publicationOwnsExit reports a row whose import owns its exit: a
// publication waiting for or in its import (the publication's own deadline
// and attempt cap end it), or import custody still open on an unfinished
// attempt — a held stage or a pending or retained authorization, which the
// import's reconciliation settles.
func (s *Service) publicationOwnsExit(ctx context.Context, dl sqlite.Download) bool {
	pubs, err := s.db.ListPublicationsForDownload(ctx, dl.ID)
	if err != nil {
		return true // unreadable: decide nothing now
	}
	for _, p := range pubs {
		if p.State == "discovered" || p.State == "importing" {
			return true
		}
	}
	if dl.GenerationKey != "" {
		// A finished attempt (outcome completed) owns nothing: no import will
		// move the row again. Custody it leaves — such as an earlier epoch's
		// stage still prepared, which placement reconciliation keeps for the
		// title's next import or a person's disposal in Library review — stays
		// with the placement journal and never pins the row's state
		// (download 1196: salvage imported in epoch 1, epoch 0's stage left
		// prepared, the row held in downloaded past its deadline for ever).
		a, err := s.db.GetImportAttempt(ctx, dl.ID, dl.GenerationKey)
		if err == nil && a.Outcome != "completed" && (a.HeldStages > 0 || a.PendingPublications > 0 || a.RetainedPublications > 0) {
			return true
		}
	}
	return false
}

// failOverdue fails a row blamelessly and grabs its want again (only what is
// still wanted: the replacement grab worker filters).
//
// queueDerived marks a decision drawn from Runner's queue or receipt: it
// never overwrites a Runner terminal that landed meanwhile.
func (s *Service) failOverdue(ctx context.Context, dl sqlite.Download, why string, queueDerived bool) bool {
	unlock := s.lockDownload(dl.ID)
	regrab, _ := s.replacementDue(dl, ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonLost}, false)
	reason := "overdue: " + why + "; not the release's fault, grabbing again"
	ok, err := s.db.FailOverdueDownload(ctx, dl, reason, regrab, sqlite.HandoffEntry{Step: stepFailed, At: time.Now().UnixMilli(), Detail: reason}, queueDerived)
	unlock()
	if err != nil || !ok {
		if err != nil {
			s.log.Warn("overdue download not ended", "download", dl.ID, "err", err)
		}
		return false
	}
	s.forgetOverdue(dl.ID)
	_ = s.db.AddHistory(ctx, "failed", dl.MediaItemID, dl.ReleaseTitle, map[string]any{"reason": reason, "overdue": true,
		"blames_release": false, "regrab": len(regrab) > 0, "deadline": dl.DeadlineAt.UnixMilli()})
	s.log.Warn("overdue download ended; its want is grabbed again", "download", dl.ID, "state", dl.State, "reason", why)
	s.kickReplacementGrabs()
	s.InvalidateWanted()
	return true
}
