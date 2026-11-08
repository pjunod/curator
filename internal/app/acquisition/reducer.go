package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/app/pipeline"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// The reducer (pipeline contract v2, docs/PIPELINE_CONTRACT.md section 3.2).
//
// observe is the only function that changes a Runner download row from a
// client observation. Facts come from the event stream and from the poll's
// three reads (queue, ordered history, deliveries feed) and are reduced the
// same way:
//
//   - identity: a fact is admitted only if its transfer equals the row's
//     token; a mismatch is dropped and counted;
//   - once, durably: a terminal is recorded by a SQL once-guard together
//     with the work it implies (a publication or a replacement grab), in one
//     transaction, so a replay is a no-op and a crash cannot lose the work;
//   - authorship: Runner's terminals are final, Curator's (submit, lost, the
//     migrated parks) are provisional and any later Runner fact re-opens them;
//     a hold past its deadline is "held (stale)", never a Curator failure.

// runnerFeedsOf reports whether a client is on the reducer.
func runnerFeedsOf(c ports.DownloadClient) (ports.RunnerFeeds, bool) {
	f, ok := c.(ports.RunnerFeeds)
	return f, ok
}

// isRunnerClient reports a client on the reducer. Curator never parks or
// stall-retires a Runner row: the legacy park and stall paths check this.
func isRunnerClient(c ports.DownloadClient) bool {
	_, ok := runnerFeedsOf(c)
	return ok
}

// Handoff steps the reducer adds to the trace.
const (
	stepHeld        = "held"
	stepReopened    = "reopened"
	stepSalvage     = "salvage_published"
	stepPublication = "publication"
)

// HistoryRunnerFactMismatch records a dropped fact (once per row and token).
const HistoryRunnerFactMismatch = "runner_fact_mismatch"

// observe applies one fact to one row.
func (s *Service) observe(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, f ports.RunnerFact) error {
	if f.Transfer == "" && dl.Transfer != "" {
		// A fact naming only a job handle (job_control_changed, job_pp_stage)
		// reaches a tokened row only when the queue says that handle is this
		// row's token: after a Runner state wipe job ids are reused, and a
		// handle alone would land a new job's facts on an old row.
		token, known := s.queueToken(cfg.ID, string(f.Handle))
		if token != dl.Transfer {
			if known {
				s.countFactMismatch(dl.ID, token)
			}
			s.log.Debug("runner fact dropped: no token, and the queue does not name this row's token for its handle",
				"download", dl.ID, "handle", f.Handle, "queue_token", token, "source", f.Source)
			return nil
		}
		f.Transfer = token
	}
	if f.Transfer != "" && dl.Transfer != "" && f.Transfer != dl.Transfer {
		s.countFactMismatch(dl.ID, f.Transfer)
		s.log.Debug("runner fact dropped: transfer token does not match the row", "download", dl.ID, "fact", f.Transfer, "source", f.Source)
		return nil
	}
	unlock := s.lockDownload(dl.ID)
	defer unlock()
	fresh, err := s.db.GetDownload(ctx, dl.ID)
	if err != nil {
		return err
	}
	if fresh.Transfer != dl.Transfer || fresh.ClientID != dl.ClientID {
		return nil
	}
	dl = fresh
	if dl.SubmissionPhase == "pending" || dl.State == "planned" {
		return nil
	}
	// A completed row from the feed for a download Runner finally failed is
	// a salvage, whatever the feed row says (a pre-M1a Runner wrote no kind).
	if f.Kind == ports.FactTerminal && f.Terminal != nil && f.Terminal.Outcome == ports.OutcomeCompleted &&
		dl.RunnerOutcome == ports.OutcomeFailed && !dl.Provisional && strings.HasPrefix(f.Source, "deliveries") {
		f.Kind, f.Publication = ports.FactPublicationReady, f.Terminal.Publication
		if f.Publication != nil {
			f.Publication.Kind = "salvage"
		}
	}
	// reopen: any Runner fact for a provisional terminal reverts it first. A
	// terminal needs no revert (its once-guard accepts a provisional row); a
	// status or a salvage does.
	if dl.Provisional && f.Kind != ports.FactGone && f.Kind != ports.FactTerminal {
		state := "downloading"
		if dl.Handle == "" && f.Handle == "" {
			state = "submitting"
		}
		ok, err := s.db.ReopenProvisional(ctx, dl.ID, dl.Transfer, state, s.downloadingDeadline(dl),
			sqlite.HandoffEntry{Step: stepReopened, At: time.Now().UnixMilli(), Detail: traced("Runner still knows this transfer; the provisional failure is withdrawn", f.Source)})
		if err != nil {
			return err
		}
		if ok {
			if dl, err = s.db.GetDownload(ctx, dl.ID); err != nil {
				return err
			}
		}
	}
	switch f.Kind {
	case ports.FactAdmitted:
		return s.admit(ctx, &dl, string(f.Handle), f.Source)
	case ports.FactStatus:
		if f.Status == nil {
			return nil
		}
		return s.applyStatus(ctx, dl, cfg, *f.Status, f.Source)
	case ports.FactTerminal:
		if f.Terminal == nil {
			return nil
		}
		if dl.Handle == "" && f.Handle != "" {
			_ = s.admit(ctx, &dl, string(f.Handle), f.Source)
		}
		_, err := s.recordTerminal(ctx, dl, cfg, *f.Terminal, f.Source, f.HistorySeq, false)
		return err
	case ports.FactPublicationReady:
		if f.Publication == nil {
			return nil
		}
		return s.recordSalvage(ctx, dl, cfg, *f.Publication, f.Source)
	case ports.FactGone:
		_, err := s.recordTerminal(ctx, dl, cfg, ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonLost,
			Message: "Runner no longer knows this transfer (not in its queue, its history or its request receipts)"}, f.Source, 0, true)
		return err
	}
	return nil
}

// countFactMismatch counts a dropped fact once per row and foreign token.
func (s *Service) countFactMismatch(downloadID int64, token string) {
	if _, seen := s.mismatchSeen.LoadOrStore(mismatchKey{downloadID, token}, struct{}{}); !seen {
		s.factMismatches.Add(1)
	}
}

// admit binds a Runner job handle to the row (contract: admitted{job}).
func (s *Service) admit(ctx context.Context, dl *sqlite.Download, handle, source string) error {
	if handle == "" || dl.Handle != "" {
		return nil
	}
	var err error
	if dl.Transfer != "" && dl.GenerationKey != "" && (dl.SubmissionPhase == "submitting" || dl.SubmissionPhase == "uncertain" || dl.SubmissionPhase == "submitted") {
		if dl.Intent == GrabIntentManualAdditional {
			err = s.db.CommitManualSubmission(ctx, dl.ID, dl.GenerationKey, handle, dl.Transfer)
		} else {
			err = s.db.RecordDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, handle)
		}
	} else {
		err = s.db.SetDownloadHandle(ctx, dl.ID, handle, dl.Transfer)
	}
	if err != nil {
		return err
	}
	fresh, err := s.db.GetDownload(ctx, dl.ID)
	if err == nil {
		*dl = fresh
	}
	return err
}

func (s *Service) downloadingDeadline(dl sqlite.Download) time.Time {
	if dl.Protocol == "torrent" {
		return time.Now().Add(sqlite.DeadlineTorrent)
	}
	return time.Now().Add(sqlite.DeadlineDownloading)
}

// heldStatus reports whether a status says Runner is holding the job.
func heldStatus(st ports.DownloadStatus) bool {
	return st.OperationalState == "held" || (st.Control != nil && st.Control.Held())
}

// applyStatus is a status fact: progress, a stage, a hold. Never a decision,
// and never applied to a row whose terminal is recorded (the SQL guard in
// UpdateRunnerStatus is what enforces it).
func (s *Service) applyStatus(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, st ports.DownloadStatus, source string) error {
	if dl.RunnerOutcome != "" {
		return nil
	}
	switch dl.State {
	case "grabbed", "submitting", "downloading", "held":
	default:
		return nil
	}
	if st.State == ports.StateRemoved || st.State == ports.StateCompleted {
		// job_deleted carries no transfer and the queue never says
		// completed: the terminal comes from Runner's own record.
		return nil
	}
	if dl.Handle == "" && st.Handle != "" {
		if err := s.admit(ctx, &dl, string(st.Handle), source); err != nil {
			return err
		}
	}
	now := time.Now()
	w := sqlite.RunnerStatusWrite{DownloadID: dl.ID, Transfer: dl.Transfer, Progress: st.Progress, Stage: st.Stage}
	if st.RateBps != nil {
		w.RateBps = max(0, *st.RateBps)
	}
	if st.RemainingBytes != nil {
		w.RemainingBytes = max(0, *st.RemainingBytes)
	}
	if st.Progress == 0 && dl.Progress > 0 {
		w.Progress = dl.Progress
	}
	if st.Control != nil {
		raw, err := json.Marshal(st.Control)
		if err == nil {
			w.Control = string(raw) // display only: no refusal matrix (contract 3.2)
		}
	}
	if heldStatus(st) {
		w.State = "held"
		w.Error = st.Message
		if w.Error == "" && st.Control != nil {
			w.Error = st.Control.Message
		}
		switch {
		case st.Control != nil && st.Control.DeadlineInS != nil:
			w.DeadlineAt = now.Add(time.Duration(*st.Control.DeadlineInS)*time.Second + sqlite.DeadlineHeldGrace)
		case dl.State == "held" && !dl.DeadlineAt.IsZero():
			w.DeadlineAt = dl.DeadlineAt // an older Runner's hold keeps its first 24 h
		default:
			w.DeadlineAt = now.Add(sqlite.DeadlineHeldLegacy)
		}
		if dl.State != "held" {
			detail := "Runner is holding the job"
			if w.Error != "" {
				detail += ": " + w.Error
			}
			w.Handoff = &sqlite.HandoffEntry{Step: stepHeld, At: now.UnixMilli(), Detail: traced(detail, source)}
		}
	} else {
		w.State = "downloading"
		if st.State == ports.StateFailed {
			w.Error = st.Message
		}
		w.DeadlineAt = dl.DeadlineAt
		// Movement is progress, a state change or a NEW post-processing
		// stage; a job reporting the same stage poll after poll is not
		// moving, so a stalled unpack goes overdue (review #2).
		moved := w.Progress != dl.Progress || dl.State != "downloading" || dl.DeadlineAt.IsZero() || (st.Stage != "" && st.Stage != dl.RunnerStage)
		if moved {
			w.DeadlineAt = s.downloadingDeadline(dl)
		}
		switch dl.State {
		case "grabbed", "submitting":
			detail := "download client is fetching the release"
			if st.Message != "" {
				detail = st.Message
			}
			w.Handoff = &sqlite.HandoffEntry{Step: stepDownloading, At: now.UnixMilli(), Detail: traced(detail, source)}
		case "held":
			w.Handoff = &sqlite.HandoffEntry{Step: stepDownloading, At: now.UnixMilli(), Detail: traced("Runner released the hold", source)}
		}
	}
	if _, err := s.db.UpdateRunnerStatus(ctx, w); err != nil {
		return err
	}
	s.watchDownloading(dl, st, st.Stage)
	return nil
}

// fetchedProgress is the share of the release a failed job fetched, for the
// re-grab budget, or -1 when Runner did not say.
func fetchedProgress(f ports.Fetched, size int64) float64 {
	switch {
	case f.TotalArticles > 0:
		return min(1, max(0, float64(f.SuccessArticles)/float64(f.TotalArticles)))
	case f.Bytes > 0 && size > 0:
		return min(1, float64(f.Bytes)/float64(size))
	}
	return -1
}

// recordTerminal records one terminal with the work it implies. It is the
// only writer of runner_outcome. provisional marks a Curator-authored one.
func (s *Service) recordTerminal(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, t ports.RunnerTerminal, source string, seq int64, provisional bool) (bool, error) {
	return s.recordTerminalWith(ctx, dl, cfg, t, source, seq, provisional, -1)
}

// recordTerminalWith is recordTerminal with a legacy client's own measure of
// what it fetched (progress >= 0), used when the terminal carries none.
func (s *Service) recordTerminalWith(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, t ports.RunnerTerminal, source string, seq int64, provisional bool, progress float64) (bool, error) {
	now := time.Now()
	w := sqlite.RunnerTerminalWrite{DownloadID: dl.ID, Transfer: dl.Transfer, Outcome: t.Outcome, Reason: t.Reason,
		FetchCause: t.FetchCause, Salvage: t.Salvage, HistorySeq: seq, Provisional: provisional, Progress: dl.Progress}
	if raw, err := json.Marshal(t.Fetched); err == nil {
		w.FetchedJSON = string(raw)
	}
	var pub *ports.RunnerPublication
	switch t.Outcome {
	case ports.OutcomeCompleted:
		pub = t.Publication
		if pub == nil || strings.TrimSpace(pub.Path) == "" {
			// A completion without its publication is not actionable; the
			// path arrives with history or the deliveries feed.
			return false, nil
		}
		mapped := ports.MapRemotePath(cfg.PathMappings, pub.Path)
		if container, err := s.isPayloadContainer(ctx, mapped); err != nil {
			return false, err // roots unreadable: the fact is read again
		} else if container {
			// A completed root or its storage container is never one
			// payload: refused, like a completion without its publication;
			// the real path arrives with history or the deliveries feed.
			s.log.Warn("runner publication path refused: it is a completed root or its storage container", "download", dl.ID, "path", mapped, "source", source)
			return false, nil
		}
		w.State, w.Progress, w.SavePath, w.ImportPath = "downloaded", 1, pub.Path, mapped
		w.Publication = s.publicationWrite(dl.ID, mapped, *pub, "completed")
		detail := fmt.Sprintf("%s finished; publication at %s", clientLabel(cfg), pub.Path)
		if mapped != pub.Path {
			detail += fmt.Sprintf(" (mapped to %s)", mapped)
		}
		w.Handoff = &sqlite.HandoffEntry{Step: stepDownloaded, At: now.UnixMilli(), Detail: traced(detail, source)}
		// The import clock starts when Runner's terminal arrives, in the same
		// transaction (a publication waiting hours for its manifest is
		// measured); a row held for approval starts it at approval instead.
		if !cfg.ManualApproval && s.timings != nil {
			w.TimingStarts = append(w.TimingStarts, sqlite.TimingStart{Kind: pipeline.RunnerDoneToImport, Transfer: pipeline.Key(dl.Transfer, dl.ID), DownloadID: dl.ID})
		}
	case ports.OutcomeCancelled:
		w.State = "failed"
		w.Error = "cancelled in " + clientLabel(cfg)
		if t.Message != "" {
			w.Error = t.Message
		}
		w.CancelPlanID = dl.PlanID
		w.Handoff = &sqlite.HandoffEntry{Step: stepFailed, At: now.UnixMilli(), Detail: traced(w.Error+"; not blocklisted and not replaced", source)}
		w.History = append(w.History, sqlite.HistoryWrite{Type: "failed", MediaItemID: dl.MediaItemID, ReleaseTitle: dl.ReleaseTitle,
			Data: map[string]any{"reason": w.Error, "removed": true, "outcome": t.Outcome}})
	default: // failed
		w.State = "failed"
		msg := t.Reason
		switch {
		case t.Reason == "" || t.Reason == "client":
			// A legacy client's own words are the whole reason.
			msg = t.Message
		case t.Message != "" && t.Message != t.Reason:
			msg = t.Reason + ": " + t.Message
		}
		if msg == "" {
			msg = "failed"
		}
		w.Error = msg
		// A failed payload's location stays on the row for inventory and a
		// manual import (it is never a reason to import automatically).
		w.SavePath, w.ImportPath = dl.SavePath, dl.ImportPath
		w.BlamesRelease = t.BlamesRelease && !provisional
		if p := fetchedProgress(t.Fetched, dl.Size); p >= 0 {
			w.Progress = p
		} else if progress >= 0 {
			w.Progress = min(1, progress)
		} else {
			w.Progress = 1 // nothing measured what was fetched: count it in full
		}
		detail := "download failed: " + msg
		if provisional {
			detail += " (provisional: a later Runner fact re-opens it)"
		}
		w.Handoff = &sqlite.HandoffEntry{Step: stepFailed, At: now.UnixMilli(), Detail: traced(detail, source)}
		w.History = append(w.History, sqlite.HistoryWrite{Type: "failed", MediaItemID: dl.MediaItemID, ReleaseTitle: dl.ReleaseTitle,
			Data: map[string]any{"reason": msg, "failure_reason": t.Reason, "fetch_cause": t.FetchCause, "stage": t.Stage,
				"blames_release": w.BlamesRelease, "salvage": t.Salvage, "provisional": provisional}})
		if w.BlamesRelease && dl.SubmissionPhase != "rejected" {
			w.Blocklist = &sqlite.BlocklistWrite{MediaItemID: dl.MediaItemID, SourceIndexerID: dl.SourceIndexerID,
				SourceGUID: dl.SourceGUID, Title: dl.ReleaseTitle, Indexer: dl.Indexer, Reason: msg}
		}
		if t.Reason == ports.ReasonFetch && t.FetchCause == ports.FetchCauseIndexerUnavailable && !provisional {
			// The indexer is down, not the release: defer it (the existing
			// request-budget gate) instead of grabbing an alternative from it.
			w.IndexerOutage = &sqlite.IndexerOutageWrite{IndexerID: dl.SourceIndexerID, Until: now.Add(indexerOutageRetry)}
		}
		if wants, why := s.replacementDue(dl, t, provisional); len(wants) > 0 {
			w.ReplacementGrab = &sqlite.ReplacementGrabWrite{WantableIDs: wants}
			// failure_to_next_grab starts with the failure itself; the grab
			// worker ends it on submit or discards it with nothing to grab.
			if s.timings != nil {
				w.TimingStarts = append(w.TimingStarts, sqlite.TimingStart{Kind: pipeline.FailureToNextGrab, Transfer: pipeline.Key(dl.Transfer, dl.ID), DownloadID: dl.ID})
			}
			if dl.PlanID != 0 {
				w.ReplanPlanID = dl.PlanID
			}
		} else if why != "" {
			w.History = append(w.History, sqlite.HistoryWrite{Type: HistoryNoReplacement, MediaItemID: dl.MediaItemID, ReleaseTitle: dl.ReleaseTitle,
				Data: map[string]any{"reason": why}})
		}
	}
	w.Provisional = provisional
	won, err := s.db.RecordRunnerTerminal(ctx, w)
	if err != nil {
		return false, err
	}
	// The import worker and the grab worker are kicked only after commit;
	// both are idempotent on the rows the transaction wrote.
	if !won {
		if pub != nil {
			s.kickPublicationImports(ctx, dl, cfg)
		}
		return false, nil
	}
	s.registry.EndClientStages(dl.ID)
	switch t.Outcome {
	case ports.OutcomeCompleted:
		s.kickPublicationImports(ctx, dl, cfg)
	default:
		s.publish(ImportFailed{MediaItemID: dl.MediaItemID, Release: dl.ReleaseTitle, Reason: w.Error})
		if dl.ReplacementOperationID != "" && t.Outcome == ports.OutcomeFailed && !provisional {
			// The reviewed operation owns its next grab and advances with
			// the failed bytes retained (contract section 3.3, item 3).
			if err := s.db.TransitionReplacementOperation(ctx, dl.ReplacementOperationID, "replacing", "replacement attempt failed; choosing another release"); err != nil {
				s.log.Warn("replacement operation retry scheduling failed", "operation", dl.ReplacementOperationID, "err", err)
			}
		}
		if w.ReplacementGrab != nil {
			s.kickReplacementGrabs()
		}
	}
	return true, nil
}

// indexerOutageRetry is how long an indexer Runner could not fetch from is
// deferred when it named no retry_at of its own: Runner had already
// exhausted its own retries against it.
const indexerOutageRetry = 15 * time.Minute

// HistoryNoReplacement explains why a failure starts no next grab.
const HistoryNoReplacement = "no_replacement"

// replacementDue decides whether a failed terminal writes a replacement grab
// (contract section 3.3) and, when it does not, why.
func (s *Service) replacementDue(dl sqlite.Download, t ports.RunnerTerminal, provisional bool) ([]string, string) {
	switch {
	case provisional:
		return nil, "the failure is provisional (" + t.Reason + "); the want stays wanted for the planner's next pass"
	case t.Outcome == ports.OutcomeCancelled:
		return nil, "cancelled"
	case t.Reason == ports.ReasonFetch && t.FetchCause == ports.FetchCauseIndexerUnavailable:
		return nil, "the indexer was unavailable; the same want is searched again once it is back, not replaced"
	case dl.SubmissionPhase == "rejected":
		return nil, "the client refused the submission"
	case dl.Intent == GrabIntentManualAdditional:
		return nil, "a manual additional copy is one-shot"
	case dl.ReplacementOperationID != "":
		return nil, "the replacement operation owns the next grab"
	case len(dl.WantableIDs) == 0:
		return nil, "the row names no wantable"
	}
	return append([]string(nil), dl.WantableIDs...), ""
}

// publicationWrite builds the publications row for a fact.
func (s *Service) publicationWrite(downloadID int64, mapped string, pub ports.RunnerPublication, kind string) *sqlite.PublicationWrite {
	w := &sqlite.PublicationWrite{DownloadID: downloadID, Generation: pub.Generation, Kind: kind, Path: mapped,
		RemotePath: pub.Path, CrossVolume: pub.CrossVolume, Cohort: "live"}
	w.Source = "none"
	return w
}

// recordSalvage is publication_ready: a salvage publication of a failed job.
// The row stays failed for re-grab purposes; the publication imports.
func (s *Service) recordSalvage(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, pub ports.RunnerPublication, source string) error {
	mapped := ports.MapRemotePath(cfg.PathMappings, pub.Path)
	if strings.TrimSpace(mapped) == "" {
		return nil
	}
	if container, err := s.isPayloadContainer(ctx, mapped); err != nil {
		return err
	} else if container {
		s.log.Warn("runner salvage path refused: it is a completed root or its storage container", "download", dl.ID, "path", mapped, "source", source)
		return nil
	}
	if existing, ok, err := s.db.PublicationByPath(ctx, mapped); err != nil {
		return err
	} else if ok {
		if existing.DownloadID == dl.ID {
			s.kickPublicationImports(ctx, dl, cfg)
		}
		return nil
	}
	w := s.publicationWrite(dl.ID, mapped, pub, "salvage")
	if _, err := s.db.InsertPublication(ctx, *w); err != nil {
		return err
	}
	_ = s.db.AddHistory(ctx, stepSalvage, dl.MediaItemID, dl.ReleaseTitle,
		map[string]any{"path": mapped, "generation": pub.Generation, "source": source})
	s.kickPublicationImports(ctx, dl, cfg)
	return nil
}

// kickPublicationImports offers a row's waiting publications to the import
// workers. Each publication import is idempotent on its row.
func (s *Service) kickPublicationImports(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig) {
	pubs, err := s.db.ListPublicationsForDownload(ctx, dl.ID)
	if err != nil {
		s.log.Warn("publications could not be listed", "download", dl.ID, "err", err)
		return
	}
	for _, p := range pubs {
		if p.State == "discovered" {
			s.enqueuePublicationImport(ctx, dl, cfg, p.ID)
		}
	}
}

// ---- the poll: reconcile only (contract section 3.2, IMPL section 5.5) ----

// withOutstandingRunnerRows adds a Runner client's provisional terminals
// (failed{submit|lost|parked}, which ListActiveDownloads leaves out) to the
// poll's rows, so a job Runner still lists re-opens them from the poll and
// not only from an event (review #8).
func (s *Service) withOutstandingRunnerRows(ctx context.Context, clientID int64, rows []sqlite.Download) []sqlite.Download {
	outstanding, err := s.db.OutstandingRunnerDownloads(ctx, clientID)
	if err != nil {
		s.log.Warn("queue: outstanding Runner rows unreadable", "client", clientID, "err", err)
		return rows
	}
	have := make(map[int64]bool, len(rows))
	for _, dl := range rows {
		have[dl.ID] = true
	}
	for _, dl := range outstanding {
		if !have[dl.ID] {
			rows = append(rows, dl)
		}
	}
	return rows
}

// goneGrace is how long a job must be absent from every Runner feed, over
// successful polls only, before Curator gives up on it (provisionally).
const goneGrace = 10 * time.Minute

// historyPagesPerPoll bounds one poll's catch-up through Runner's history.
const historyPagesPerPoll = 10

// reconcileRunnerClient is one poll of one Runner client: the queue, the
// ordered history, the deliveries feed, absence, and the deadline sweep.
func (s *Service) reconcileRunnerClient(ctx context.Context, cfg ports.ClientConfig, client ports.DownloadClient, feeds ports.RunnerFeeds, rows []sqlite.Download) {
	statuses, err := client.Statuses(ctx)
	s.noteContact(cfg.ID, err)
	if err != nil {
		s.log.Warn("queue: client poll failed", "client", cfg.Name, "err", err)
		s.resetAbsence(rows)
		return
	}
	// (a) the queue: status facts, by exact token (handle for a legacy row).
	seen := map[int64]bool{}
	byTransfer := map[string]ports.DownloadStatus{}
	byHandle := map[string]ports.DownloadStatus{}
	tokens := map[string]string{}
	for _, st := range statuses {
		if st.Transfer != "" {
			byTransfer[st.Transfer] = st
		}
		if st.Handle != "" {
			byHandle[string(st.Handle)] = st
			if st.Transfer != "" {
				tokens[string(st.Handle)] = st.Transfer
			}
		}
	}
	s.queueTokens.Store(cfg.ID, tokens)
	for _, dl := range rows {
		if s.ImportRunning(dl.ID) {
			seen[dl.ID] = true
			continue
		}
		st, ok := byTransfer[dl.Transfer]
		if !ok && dl.Transfer == "" && dl.Handle != "" {
			st, ok = byHandle[dl.Handle]
		}
		if !ok && dl.Handle != "" {
			if other, present := byHandle[dl.Handle]; present && other.Transfer != "" && other.Transfer != dl.Transfer {
				s.countFactMismatch(dl.ID, other.Transfer)
			}
		}
		if !ok {
			continue
		}
		seen[dl.ID] = true
		if err := s.observe(ctx, dl, cfg, ports.RunnerFact{Kind: ports.FactStatus, Transfer: st.Transfer, Handle: st.Handle, Status: &st, Source: "poll"}); err != nil {
			s.log.Warn("queue: status fact not applied", "download", dl.ID, "err", err)
		}
	}
	// (b) the ordered history: terminals in seq order; the cursor moves only
	// after a page's terminals are committed, and only here.
	s.pollHistory(ctx, cfg, feeds)
	// (c) the deliveries feed: publications the stream may have missed.
	s.pollDeliveries(ctx, cfg, feeds)
	// (d) absence -> gone, (e) deadlines.
	s.detectGone(ctx, cfg, feeds, rows, seen)
	s.confirmAdmissions(ctx, cfg, feeds, rows, seen)
	s.sweepDeadlines(ctx, cfg, feeds)
	// (f) every other overdue row exits on Runner's word.
	s.exitOverdue(ctx, cfg, feeds, byTransfer, byHandle)
	// Waiting publications (a directory not yet carrying its manifest) are
	// re-checked on every poll.
	if pubs, err := s.db.ImportablePublications(ctx, cfg.ID); err == nil {
		for _, p := range pubs {
			if dl, err := s.db.GetDownload(ctx, p.DownloadID); err == nil {
				s.enqueuePublicationImport(ctx, dl, cfg, p.ID)
			}
		}
	}
	s.endUnseenTransfers(rows, seen)
}

// pollHistory is the ordered history reader: the one owner of the cursor.
func (s *Service) pollHistory(ctx context.Context, cfg ports.ClientConfig, feeds ports.HistoryReader) {
	stored, err := s.db.GetHistoryCursor(ctx, cfg.ID)
	if err != nil {
		s.log.Warn("history cursor read failed", "client", cfg.Name, "err", err)
		return
	}
	cur := ports.HistoryCursor{Incarnation: stored.Incarnation, SinceSeq: stored.SinceSeq, HeadSeq: stored.HeadSeq, RetainedMinSeq: stored.RetainedMinSeq}
	for page := 0; page < historyPagesPerPoll; page++ {
		facts, next, lost, err := feeds.HistorySince(ctx, cur, 100)
		if err != nil {
			s.log.Warn("history read failed", "client", cfg.Name, "err", err)
			return
		}
		if lost || !stored.Stored {
			// Continuity lost (a rebuilt index, a pruned gap) or never had:
			// every outstanding transfer is looked up by its token before the
			// cursor is trusted again (contract section 2.5).
			if err := s.reconcileAfterContinuityLoss(ctx, cfg); err != nil {
				s.log.Warn("continuity reconciliation incomplete; cursor not moved", "client", cfg.Name, "err", err)
				return
			}
			head := next.HeadSeq
			if head == 0 && !lost {
				head = next.SinceSeq
			}
			if err := s.db.UpsertHistoryCursor(ctx, sqlite.RunnerHistoryCursor{ClientID: cfg.ID, Incarnation: next.Incarnation,
				SinceSeq: head, HeadSeq: next.HeadSeq, RetainedMinSeq: next.RetainedMinSeq}); err != nil {
				s.log.Warn("history cursor write failed", "client", cfg.Name, "err", err)
			}
			return
		}
		for _, f := range facts {
			if err := s.applyFeedFact(ctx, cfg, f); err != nil {
				// The page is not committed: the cursor stays, and the next
				// poll reads the same rows again (replays are no-ops).
				s.log.Warn("history fact not applied; cursor held", "client", cfg.Name, "source", f.Source, "err", err)
				return
			}
		}
		if next.SinceSeq == cur.SinceSeq && next.Incarnation == cur.Incarnation {
			if next.HeadSeq != cur.HeadSeq || next.RetainedMinSeq != cur.RetainedMinSeq {
				_ = s.db.UpsertHistoryCursor(ctx, sqlite.RunnerHistoryCursor{ClientID: cfg.ID, Incarnation: next.Incarnation,
					SinceSeq: next.SinceSeq, HeadSeq: next.HeadSeq, RetainedMinSeq: next.RetainedMinSeq})
			}
			return
		}
		if err := s.db.UpsertHistoryCursor(ctx, sqlite.RunnerHistoryCursor{ClientID: cfg.ID, Incarnation: next.Incarnation,
			SinceSeq: next.SinceSeq, HeadSeq: next.HeadSeq, RetainedMinSeq: next.RetainedMinSeq}); err != nil {
			s.log.Warn("history cursor write failed", "client", cfg.Name, "err", err)
			return
		}
		cur = next
		if len(facts) == 0 && next.SinceSeq >= next.HeadSeq {
			return
		}
	}
}

// queueToken is the transfer token the client's last queue poll gave a job
// handle, and whether the queue named that handle at all.
func (s *Service) queueToken(clientID int64, handle string) (string, bool) {
	if handle == "" {
		return "", false
	}
	v, ok := s.queueTokens.Load(clientID)
	if !ok {
		return "", false
	}
	token, known := v.(map[string]string)[handle]
	return token, known
}

// applyFeedFact routes a feed fact to its row by token. A fact for a
// transfer Curator has no row for is not Curator's.
func (s *Service) applyFeedFact(ctx context.Context, cfg ports.ClientConfig, f ports.RunnerFact) error {
	dl, ok, err := s.db.DownloadByTransfer(ctx, cfg.ID, f.Transfer)
	if err != nil {
		return err
	}
	if !ok && f.Transfer == "" {
		dl, ok, err = s.db.DownloadByHandle(ctx, cfg.ID, string(f.Handle))
		if err != nil {
			return err
		}
	}
	if !ok {
		return nil
	}
	return s.observe(ctx, dl, cfg, f)
}

// deliveriesPagesPerPoll bounds one poll's read of the deliveries feed.
const deliveriesPagesPerPoll = 10

// pollDeliveries reads the deliveries feed only to raise facts; it never
// drives an import of its own when the event already did.
func (s *Service) pollDeliveries(ctx context.Context, cfg ports.ClientConfig, feeds ports.DeliveriesReader) {
	after, err := s.db.DeliveryCursor(ctx, cfg.ID)
	if err != nil {
		s.log.Warn("delivery cursor read failed", "client", cfg.Name, "err", err)
		return
	}
	for page := 0; page < deliveriesPagesPerPoll; page++ {
		facts, next, err := feeds.DeliveriesSince(ctx, after, 100)
		if err != nil {
			s.log.Debug("deliveries feed read failed", "client", cfg.Name, "err", err)
			return
		}
		for _, f := range facts {
			if err := s.applyFeedFact(ctx, cfg, f); err != nil {
				s.log.Warn("delivery fact not applied; cursor held", "client", cfg.Name, "source", f.Source, "err", err)
				return
			}
		}
		if next <= after {
			return
		}
		if err := s.db.SetDeliveryCursor(ctx, cfg.ID, next); err != nil {
			s.log.Warn("delivery cursor write failed", "client", cfg.Name, "err", err)
			return
		}
		after = next
	}
}

// reconcileAfterContinuityLoss looks up every outstanding transfer once
// through Runner's request receipt, the one lookup that survives a history
// rebuild, and applies what it says (contract section 2.5).
func (s *Service) reconcileAfterContinuityLoss(ctx context.Context, cfg ports.ClientConfig) error {
	rows, err := s.db.OutstandingRunnerDownloads(ctx, cfg.ID)
	if err != nil {
		return err
	}
	feeds, ok := runnerFeedsOf(s.newClient(cfg))
	if !ok {
		return nil
	}
	for _, dl := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
		adm, found, err := feeds.GetRequestAdmission(lookup, dl.Transfer)
		cancel()
		if err != nil {
			return fmt.Errorf("request admission %s: %w", dl.Transfer, err)
		}
		if !found {
			continue
		}
		for _, f := range admissionFacts(adm) {
			if err := s.observe(ctx, dl, cfg, f); err != nil {
				return err
			}
		}
	}
	return nil
}

// admissionFacts reads a request receipt as facts: admitted, and the
// terminal when the receipt records one.
func admissionFacts(adm ports.RequestAdmission) []ports.RunnerFact {
	var out []ports.RunnerFact
	if adm.JobID != "" && adm.JobID != "0" {
		out = append(out, ports.RunnerFact{Kind: ports.FactAdmitted, Transfer: adm.RequestToken, Handle: ports.Handle(adm.JobID), Source: "admission"})
	}
	if t, ok := admissionTerminal(adm); ok {
		out = append(out, ports.RunnerFact{Kind: ports.FactTerminal, Transfer: adm.RequestToken, Handle: ports.Handle(adm.JobID), Terminal: &t, Source: "admission"})
	}
	return out
}

// admissionTerminal maps a receipt's outcome to a terminal: typed fields from
// Runner M1a, else the legacy string rule.
func admissionTerminal(adm ports.RequestAdmission) (ports.RunnerTerminal, bool) {
	var t ports.RunnerTerminal
	switch adm.Outcome {
	case "completed":
		if adm.PublicationPath == "" {
			return t, false
		}
		t = ports.RunnerTerminal{Outcome: ports.OutcomeCompleted, Salvage: "none",
			Publication: &ports.RunnerPublication{Path: adm.PublicationPath, Generation: adm.SourceGeneration, Kind: "completed"}}
	case "deleted", "cancelled":
		t = ports.RunnerTerminal{Outcome: ports.OutcomeCancelled, Salvage: "none", Message: "removed in Runner"}
	case "failed":
		if adm.FailureReason != "" {
			t = ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: adm.FailureReason, FetchCause: adm.FetchCause,
				Stage: adm.FailureStage, Message: adm.FailureCause, Salvage: adm.Salvage}
			if adm.BlamesRelease != nil {
				t.BlamesRelease = *adm.BlamesRelease
			}
		} else {
			// A receipt that names no typed reason: the bytes are intact
			// but unexplained, so it parks like any other io failure.
			t = ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonIO, Message: strings.Join(nonEmpty(adm.TerminalOutcome, adm.FailureCause), ": ")}
		}
		if t.Salvage == "" {
			t.Salvage = "none"
		}
	default:
		return t, false
	}
	return t, true
}

// absence tracks, per row, when it was first missing from a successful poll.
func (s *Service) resetAbsence(rows []sqlite.Download) {
	for _, dl := range rows {
		s.absentSince.Delete(dl.ID)
	}
}

// detectGone gives up, provisionally, on a job that is in none of Runner's
// feeds for goneGrace of successful polls and whose receipt is not found.
func (s *Service) detectGone(ctx context.Context, cfg ports.ClientConfig, feeds ports.RequestAdmissionProtocol, rows []sqlite.Download, seen map[int64]bool) {
	now := time.Now()
	for _, dl := range rows {
		if seen[dl.ID] || dl.Transfer == "" || dl.Handle == "" || (dl.State != "downloading" && dl.State != "held") {
			s.absentSince.Delete(dl.ID)
			continue
		}
		first, _ := s.absentSince.LoadOrStore(dl.ID, now)
		if now.Sub(first.(time.Time)) < goneGrace {
			continue
		}
		lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
		adm, found, err := feeds.GetRequestAdmission(lookup, dl.Transfer)
		cancel()
		if err != nil {
			s.absentSince.Delete(dl.ID) // unreachable does not count
			continue
		}
		if found {
			for _, f := range admissionFacts(adm) {
				_ = s.observe(ctx, dl, cfg, f)
			}
			continue
		}
		s.absentSince.Delete(dl.ID)
		if err := s.observe(ctx, dl, cfg, ports.RunnerFact{Kind: ports.FactGone, Transfer: dl.Transfer, Source: "poll"}); err != nil {
			s.log.Warn("gone fact not applied", "download", dl.ID, "err", err)
		}
	}
}

// confirmAdmissions is the submitting half of the old receipt path: "did my
// POST /jobs land, and as which job". Ten minutes of Runner positively
// answering "no such token" (an unreachable Runner does not count) ends the
// row as a provisional failed{submit}.
func (s *Service) confirmAdmissions(ctx context.Context, cfg ports.ClientConfig, feeds ports.RequestAdmissionProtocol, rows []sqlite.Download, seen map[int64]bool) {
	now := time.Now()
	for _, dl := range rows {
		awaiting := dl.State == "submitting" || (dl.State == "grabbed" && dl.Handle == "")
		if seen[dl.ID] || dl.Transfer == "" || !awaiting {
			s.notFoundSince.Delete(dl.ID)
			continue
		}
		lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
		adm, found, err := feeds.GetRequestAdmission(lookup, dl.Transfer)
		cancel()
		if err != nil {
			s.notFoundSince.Delete(dl.ID)
			continue
		}
		if found {
			s.notFoundSince.Delete(dl.ID)
			for _, f := range admissionFacts(adm) {
				_ = s.observe(ctx, dl, cfg, f)
			}
			continue
		}
		first, _ := s.notFoundSince.LoadOrStore(dl.ID, now)
		if now.Sub(first.(time.Time)) < sqlite.DeadlineSubmitting {
			continue
		}
		s.notFoundSince.Delete(dl.ID)
		if _, err := s.recordTerminal(ctx, dl, cfg, ports.RunnerTerminal{Outcome: ports.OutcomeFailed, Reason: ports.ReasonSubmit,
			Message: "Runner has answered \"no such transfer\" for ten minutes; the submission did not land"}, "admission", 0, true); err != nil {
			s.log.Warn("submit failure not recorded", "download", dl.ID, "err", err)
		}
	}
}

// sweepDeadlines is the overdue sweep: a held row past its deadline is held
// (stale) and reconciled now; a publication past its six hours is
// import_failed{stalled}. Neither is a Runner terminal.
func (s *Service) sweepDeadlines(ctx context.Context, cfg ports.ClientConfig, feeds ports.RequestAdmissionProtocol) {
	now := time.Now()
	rows, err := s.db.OverdueHeldDownloads(ctx, cfg.ID, now)
	if err != nil {
		return
	}
	for _, dl := range rows {
		stale := "held (stale): Runner has not reported since " + dl.UpdatedAt.Format(time.RFC3339)
		if !strings.HasPrefix(dl.Error, "held (stale)") {
			if err := s.db.MarkHeldStale(ctx, dl.ID, stale); err == nil {
				_ = s.db.AddHistory(ctx, "held_stale", dl.MediaItemID, dl.ReleaseTitle, map[string]any{"download": dl.ID, "deadline": dl.DeadlineAt})
			}
		}
		lookup, cancel := context.WithTimeout(ctx, 5*time.Second)
		adm, found, err := feeds.GetRequestAdmission(lookup, dl.Transfer)
		cancel()
		if err == nil && found {
			for _, f := range admissionFacts(adm) {
				_ = s.observe(ctx, dl, cfg, f)
			}
		}
	}
	pubs, err := s.db.OverduePublications(ctx, now)
	if err != nil {
		return
	}
	for _, p := range pubs {
		if s.ImportRunning(p.DownloadID) {
			continue
		}
		dl, err := s.db.GetDownload(ctx, p.DownloadID)
		if err != nil || dl.ClientID != cfg.ID {
			continue
		}
		reason := "stalled: not imported within 6 h of its import starting"
		if p.Error != "" {
			reason += " (" + p.Error + ")"
		}
		// Decided again under the row's lock, on what the publication is
		// now: one that finished, was held or started meanwhile is not ended.
		unlock := s.lockDownload(p.DownloadID)
		cur, err := s.db.GetPublication(ctx, p.ID)
		if err == nil && !s.ImportRunning(p.DownloadID) && (cur.State == "discovered" || cur.State == "importing") &&
			!cur.Held() && !cur.DeadlineAt.IsZero() && cur.DeadlineAt.Before(now) {
			if fresh, ferr := s.db.GetDownload(ctx, p.DownloadID); ferr == nil {
				dl = fresh
			}
			s.endPublication(ctx, dl, cfg, cur, nil, reason)
		}
		unlock()
	}
}

// ---- the event stream ------------------------------------------------------

// handleRunnerEvent applies one pushed fact. Push never moves the history
// cursor: an event applies its terminal now, through the same once-guard,
// and the ordered reader confirms it later.
func (s *Service) handleRunnerEvent(ctx context.Context, cfg ports.ClientConfig, ev ports.ClientEvent, pending map[int64]float64, flushed *time.Time) {
	f := *ev.Fact
	dl, ok, err := s.db.DownloadByTransfer(ctx, cfg.ID, f.Transfer)
	if err == nil && !ok && f.Transfer == "" {
		dl, ok, err = s.db.DownloadByHandle(ctx, cfg.ID, string(f.Handle))
	}
	if err != nil {
		return
	}
	if !ok {
		// A fact naming a handle that a row carries under another token is
		// a mismatch (a job id reused after a Runner state wipe), not a fact
		// about that row: dropped and counted.
		if f.Transfer != "" && f.Handle != "" {
			if other, found, _ := s.db.DownloadByHandle(ctx, cfg.ID, string(f.Handle)); found && other.Transfer != "" && other.Transfer != f.Transfer {
				s.countFactMismatch(other.ID, f.Transfer)
			}
		}
		return
	}
	if f.Kind == ports.FactStatus && f.Status != nil && dl.State == "downloading" && !heldStatus(*f.Status) &&
		f.Status.State == ports.StateDownloading && f.Status.Stage == "" && ev.Kind == ports.EventProgress {
		// Coalesce progress: a percentage is not a decision.
		pending[dl.ID] = f.Status.Progress
		if time.Since(*flushed) < progressFlush {
			return
		}
		*flushed = time.Now()
		for id, progress := range pending {
			delete(pending, id)
			row, err := s.db.GetDownload(ctx, id)
			if err != nil {
				continue
			}
			st := ports.DownloadStatus{Handle: ports.Handle(row.Handle), Transfer: row.Transfer, State: ports.StateDownloading, Progress: progress}
			_ = s.observe(ctx, row, cfg, ports.RunnerFact{Kind: ports.FactStatus, Transfer: row.Transfer, Status: &st, Source: "push progress flush"})
		}
		return
	}
	delete(pending, dl.ID)
	if err := s.observe(ctx, dl, cfg, f); err != nil {
		s.log.Warn("subscribe: fact not applied", "download", dl.ID, "source", f.Source, "err", err)
	}
}

// operatorCancelled reports a row a person stopped: the legacy operator
// fence, or a recorded cancelled terminal. Neither is re-grabbed or
// published automatically.
func operatorCancelled(dl *sqlite.Download) bool {
	return strings.HasPrefix(dl.ParkedReason, "operator cancellation") || dl.RunnerOutcome == ports.OutcomeCancelled
}

// nonEmpty drops the empty strings, so a joined message has no dangling ": ".
func nonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
