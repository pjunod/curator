package acquisition

import (
	"context"
	"fmt"
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
//   - it is not in Runner's queue and no terminal came (unknown, or a stale
//     receipt): failed;
//
// "Failed" is blameless (no blocklist) with the want grabbed again
// (FailOverdueDownload). An unreachable Runner decides nothing. A row whose
// client is gone or disabled is ended by exitOrphanedOverdue.

// overdueStallLimit is how long a job Runner still lists may sit without
// any progress past its deadline before Curator stops waiting for it.
const overdueStallLimit = 24 * time.Hour

type overdueMark struct {
	bytes    int64
	progress float64
	stage    string
	state    ports.DownloadState
	since    time.Time
}

// progressing records st as the row's latest observation and reports
// whether Runner moved it since the previous one (a first observation counts
// as moving), and for how long it has been still.
func (s *Service) progressing(id int64, st ports.DownloadStatus, now time.Time) (bool, time.Duration) {
	var bytes int64
	if st.BytesCompleted != nil {
		bytes = *st.BytesCompleted
	}
	cur := overdueMark{bytes: bytes, progress: st.Progress, stage: st.Stage, state: st.State, since: now}
	v, loaded := s.overdueMarks.Load(id)
	if !loaded {
		s.overdueMarks.Store(id, cur)
		return true, 0
	}
	prev := v.(overdueMark)
	if cur.bytes > prev.bytes || cur.progress > prev.progress || cur.stage != prev.stage || cur.state != prev.state {
		s.overdueMarks.Store(id, cur)
		return true, 0
	}
	return false, now.Sub(prev.since)
}

// exitOverdue is the overdue exit for one Runner client (see above).
func (s *Service) exitOverdue(ctx context.Context, cfg ports.ClientConfig, feeds ports.RequestAdmissionProtocol, byTransfer, byHandle map[string]ports.DownloadStatus) {
	now := time.Now()
	rows, err := s.db.OverdueDownloads(ctx, cfg.ID, now)
	if err != nil {
		s.log.Warn("overdue rows unreadable", "client", cfg.Name, "err", err)
		return
	}
	for _, dl := range rows {
		if ctx.Err() != nil {
			return
		}
		if s.ImportRunning(dl.ID) || s.publicationOwnsExit(ctx, dl.ID) {
			continue
		}
		st, inQueue := byTransfer[dl.Transfer]
		if !inQueue && dl.Transfer == "" && dl.Handle != "" {
			st, inQueue = byHandle[dl.Handle]
		}
		if dl.RunnerOutcome != "" && !dl.Provisional {
			// Runner's terminal is recorded but the row never left its state.
			s.failOverdue(ctx, dl, fmt.Sprintf("Runner's terminal (%s) is recorded but the row never left %s", dl.RunnerOutcome, dl.State))
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
					s.overdueMarks.Delete(dl.ID)
					continue
				}
			}
		}
		switch {
		case inQueue && st.Control != nil && st.Control.Cause != "" && st.Control.DeadlineUnix != nil && now.Unix() > *st.Control.DeadlineUnix:
			// Runner's own retry deadline for what blocks it has passed and it
			// still lists the job blocked: it is not going to finish it.
			s.failOverdue(ctx, dl, fmt.Sprintf("Runner has held it on %s past Runner's own deadline (%s)", st.Control.Cause,
				time.Unix(*st.Control.DeadlineUnix, 0).UTC().Format(time.RFC3339)))
		case inQueue && heldStatus(st):
			// Runner holds it: its hold is self-clearing and has its own
			// deadline (hold_expired); the row stays held (stale) meanwhile.
			// Only a hold that has not moved for the stall limit ends here.
			if _, still := s.progressing(dl.ID, st, now); still >= overdueStallLimit {
				s.failOverdue(ctx, dl, fmt.Sprintf("Runner has held it without progress for %s past its deadline", overdueStallLimit))
			}
		case inQueue:
			moved, still := s.progressing(dl.ID, st, now)
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
			s.failOverdue(ctx, dl, fmt.Sprintf("Runner has made no progress on it for %s past its deadline", overdueStallLimit))
		case known && adm.Outcome == "completed":
			s.failOverdue(ctx, dl, "Runner reports it completed but names no publication Curator can import, and the job is no longer in its queue")
		case known:
			s.failOverdue(ctx, dl, fmt.Sprintf("Runner no longer has this job in its queue; its receipt still says %q", adm.Outcome))
		default:
			s.failOverdue(ctx, dl, "Runner no longer knows this transfer (not in its queue and no receipt)")
		}
	}
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
		if s.ImportRunning(dl.ID) || s.publicationOwnsExit(ctx, dl.ID) {
			continue
		}
		cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID)
		if err == nil && cfg.Enabled {
			continue // its client's poll decides it
		}
		if s.failOverdue(ctx, dl, "its download client is gone or disabled; nothing can report on it") {
			ended++
		}
	}
	return ended, nil
}

// publicationOwnsExit reports a row whose publication is waiting for or in
// its import: the publication's own deadline and attempt cap end it.
func (s *Service) publicationOwnsExit(ctx context.Context, downloadID int64) bool {
	pubs, err := s.db.ListPublicationsForDownload(ctx, downloadID)
	if err != nil {
		return true // unreadable: decide nothing now
	}
	for _, p := range pubs {
		if p.State == "discovered" || p.State == "importing" {
			return true
		}
	}
	return false
}

// failOverdue fails a row blamelessly and grabs its want again (only what is
// still wanted: the replacement grab worker filters).
func (s *Service) failOverdue(ctx context.Context, dl sqlite.Download, why string) bool {
	unlock := s.lockDownload(dl.ID)
	regrab, _ := s.replacementDue(dl, ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonLost}, false)
	reason := "overdue: " + why + "; not the release's fault, grabbing again"
	ok, err := s.db.FailOverdueDownload(ctx, dl, reason, regrab, sqlite.HandoffEntry{Step: stepFailed, At: time.Now().UnixMilli(), Detail: reason})
	unlock()
	if err != nil || !ok {
		if err != nil {
			s.log.Warn("overdue download not ended", "download", dl.ID, "err", err)
		}
		return false
	}
	s.overdueMarks.Delete(dl.ID)
	_ = s.db.AddHistory(ctx, "failed", dl.MediaItemID, dl.ReleaseTitle, map[string]any{"reason": reason, "overdue": true,
		"blames_release": false, "regrab": len(regrab) > 0, "deadline": dl.DeadlineAt.UnixMilli()})
	s.log.Warn("overdue download ended; its want is grabbed again", "download", dl.ID, "state", dl.State, "reason", why)
	s.kickReplacementGrabs()
	s.InvalidateWanted()
	return true
}
