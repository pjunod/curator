package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/acquisitionplan"
	"github.com/pjunod/monarr/internal/domain/decision"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type plannedExecution struct {
	URL               string
	Category          string
	Priority          int
	ClientFingerprint string
}
type executionObservation struct {
	Config               string        `json:"config"`
	LastInventory        time.Time     `json:"lastInventory"`
	AbsenceSince         time.Time     `json:"absenceSince"`
	Inventories          int           `json:"inventories"`
	LastProgress         time.Time     `json:"lastProgress"`
	LastBytes            int64         `json:"lastBytes"`
	BytesKnown           bool          `json:"bytesKnown"`
	ShortStall           time.Duration `json:"shortStall"`
	LongStall            time.Duration `json:"longStall"`
	LastStallObservation time.Time     `json:"lastStallObservation"`
	DuplicateRisk        bool          `json:"duplicateRisk"`
}

func clientFingerprint(cfg ports.ClientConfig) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%s|%s|%s", cfg.ID, cfg.Type, cfg.URL, cfg.Username, cfg.Password, cfg.Category)))
	return fmt.Sprintf("%x", sum)
}
func (s *Service) dispatchPlan(ctx context.Context, id int64) error {
	s.planDispatchMu.Lock()
	defer s.planDispatchMu.Unlock()
	p, err := s.db.GetAcquisitionPlan(ctx, id)
	if err != nil {
		return err
	}
	if p.State != "admitted" && p.State != "dispatching" && p.State != "active" {
		return nil
	}
	rows, err := s.db.PlanDownloads(ctx, id)
	if err != nil {
		return err
	}
	if _, err = s.db.W.ExecContext(ctx, `UPDATE acquisition_plans SET state='dispatching',revision=revision+1,updated_at=? WHERE id=? AND state IN ('admitted','dispatching','active')`, time.Now().UnixMilli(), id); err != nil {
		return err
	}
	for _, dl := range rows {
		if dl.SubmissionPhase != "pending" || dl.State != "planned" {
			continue
		}
		cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID)
		if err != nil {
			return err
		}
		if !cfg.Enabled {
			return s.settleSeasonPlan(ctx, p, rows, "needs_replan")
		}
		var payload plannedExecution
		if err = json.Unmarshal([]byte(dl.ExecutionPayload), &payload); err != nil {
			return err
		}
		if payload.Category != cfg.Category || (payload.ClientFingerprint != "" && payload.ClientFingerprint != clientFingerprint(cfg)) {
			return s.settleSeasonPlan(ctx, p, rows, "needs_replan")
		}
		for _, want := range dl.WantableIDs {
			w, e := s.wantableFromID(ctx, want)
			if e != nil {
				return e
			}
			profile, e := s.db.GetProfile(ctx, w.ProfileID())
			if e != nil {
				return e
			}
			if !decision.Decide(releaseOf(parser.Parse(dl.ReleaseTitle)), w, profile).Accepted {
				return s.settleSeasonPlan(ctx, p, rows, "needs_replan")
			}
		}
		claimed, err := s.db.ClaimPlannedSubmission(ctx, dl.ID)
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}
		dl.State = "grabbed"
		s.advance(ctx, &dl, "grabbed", 0, "", stepGrabbed, "submitting planned release as "+dl.Transfer)
		handle, err := addToClient(ctx, s.newClient(cfg), payload.URL, cfg.Category, dl.ReleaseTitle, dl.Transfer, payload.Priority)
		if err != nil {
			// Transport errors cannot prove nonacceptance. Retain the intent and
			// identity even when there is no returned handle.
			_ = s.db.SetSubmissionPhase(context.WithoutCancel(ctx), dl.ID, "uncertain")
			_ = s.db.UpdateDownloadState(context.WithoutCancel(ctx), dl.ID, "grabbed", 0, "submission uncertain; reconciling before any retry")
			continue
		}
		if err = s.db.SetDownloadHandle(ctx, dl.ID, string(handle), dl.Transfer); err != nil {
			return err
		}
		if err = s.db.SetSubmissionPhase(ctx, dl.ID, "submitted"); err != nil {
			return err
		}
		_ = s.db.AddHistory(ctx, "grabbed", dl.MediaItemID, dl.ReleaseTitle, map[string]any{"planId": id, "downloadId": dl.ID})
		s.publish(ReleaseGrabbed{MediaItemID: dl.MediaItemID, Title: dl.ReleaseTitle, Indexer: dl.Indexer, Protocol: dl.Protocol})
	}
	_, err = s.db.W.ExecContext(ctx, `UPDATE acquisition_plans SET state='active',revision=revision+1,updated_at=? WHERE id=? AND state IN ('admitted','dispatching','active')`, time.Now().UnixMilli(), id)
	return err
}

// RunSeasonCoordinator repairs pending dispatch and reconciles bounded claims.
func (s *Service) RecoverSeasonSubmissions(ctx context.Context) error {
	_, err := s.db.W.ExecContext(ctx, `UPDATE downloads SET submission_phase='uncertain' WHERE submission_phase='submitting'`)
	return err
}
func (s *Service) RunSeasonCoordinator(ctx context.Context) {
	timer := time.NewTicker(30 * time.Second)
	defer timer.Stop()
	for {
		s.reconcileSeasonPlans(ctx)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
func (s *Service) reconcileSeasonPlans(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM acquisition_plans WHERE state IN ('admitted','dispatching','active','cancel_requested') ORDER BY id`)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if e := rows.Scan(&id); e == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	for _, id := range ids {
		p, e := s.db.GetAcquisitionPlan(ctx, id)
		if e != nil {
			continue
		}
		dls, e := s.db.PlanDownloads(ctx, id)
		if e != nil {
			continue
		}
		pending := false
		active := false
		parked := false
		terminal := true
		for _, dl := range dls {

			if dl.SubmissionPhase == "pending" {
				pending = true
				terminal = false
			}
			if dl.State != "imported" && dl.State != "failed" && !dl.Superseded {
				terminal = false
				active = true
			}
			if dl.ParkedAt.UnixMilli() > 0 {
				parked = true
			}
		}
		if pending && p.State != "cancel_requested" && time.Since(p.AddedAt) < 48*time.Hour {
			if e = s.dispatchPlan(ctx, id); e != nil {
				s.log.Warn("planned dispatch deferred", "plan", id, "err", e)
			}
		}
		if terminal || parked || time.Since(p.AddedAt) >= 48*time.Hour || p.State == "cancel_requested" {
			state := "settled_with_reservations"
			if terminal {
				state = "completed"
			}
			if p.State == "cancel_requested" {
				state = "cancelled"
			}
			if e = s.settleSeasonPlan(ctx, p, dls, state); e != nil {
				s.log.Warn("plan settlement pending", "plan", id, "err", e)
			}
		} else if !active && !pending {
			_ = s.db.UpdatePlanState(ctx, id, "needs_replan")
		}
	}
}
func (s *Service) settleSeasonPlan(ctx context.Context, p sqlite.AcquisitionPlan, dls []sqlite.Download, state string) error {
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	for _, dl := range dls {
		var phase, actualState string
		var superseded bool
		if err = tx.QueryRowContext(ctx, `SELECT submission_phase,state,superseded FROM downloads WHERE id=?`, dl.ID).Scan(&phase, &actualState, &superseded); err != nil {
			return err
		}
		dl.SubmissionPhase, dl.State, dl.Superseded = phase, actualState, superseded
		if dl.SubmissionPhase == "pending" {
			if _, err = tx.ExecContext(ctx, `UPDATE downloads SET state='failed',submission_phase='rejected',superseded=1,reserved_episodes='[]',wantables='[]',error='unsubmitted intent closed at scope settlement' WHERE id=? AND submission_phase='pending'`, dl.ID); err != nil {
				return err
			}
		} else if dl.State != "imported" && dl.State != "failed" && !dl.Superseded {
			if _, err = tx.ExecContext(ctx, `UPDATE downloads SET parked_at=CASE WHEN parked_at=0 THEN ? ELSE parked_at END,parked_reason=CASE WHEN parked_reason='' THEN 'scope settled; episode custody retained' ELSE parked_reason END WHERE id=?`, now, dl.ID); err != nil {
				return err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE acquisition_plans SET state=?,revision=revision+1,updated_at=? WHERE id=? AND state IN ('admitted','dispatching','active','cancel_requested')`, state, now, p.ID); err != nil {
		return err
	}
	// Enqueue intent and scope transition share a transaction. A no-work successor
	// terminates immediately; dedupe prevents restart from multiplying work.
	if state == "cancelled" {
		return tx.Commit()
	}
	checkpoint, _ := json.Marshal(seasonCheckpoint{Version: 1, ItemID: p.MediaItemID, CopyID: p.CopyID, Season: p.Season, Trigger: "plan_settlement"})
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO jobs(kind,payload,priority,run_after,dedupe_key,created_at,updated_at) VALUES(?,?,50,?,?,?,?)`, SeasonSearchJobKind, string(checkpoint), now, seasonJobKey(p.MediaItemID, p.CopyID, p.Season), now, now); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) observeUncertain(ctx context.Context, dl sqlite.Download, cfg ports.ClientConfig, statuses []ports.DownloadStatus, complete bool) {
	unlock := s.lockDownload(dl.ID)
	defer unlock()
	fresh, err := s.db.GetDownload(ctx, dl.ID)
	if err != nil || fresh.Superseded {
		return
	}
	dl = fresh
	if dl.SubmissionPhase != "uncertain" && dl.SubmissionPhase != "submitting" {
		return
	}
	now := time.Now()
	var ob executionObservation
	_ = json.Unmarshal([]byte(dl.Observation), &ob)
	fp := clientFingerprint(cfg)
	matches := 0
	for _, st := range statuses {
		if dl.Handle != "" && string(st.Handle) == dl.Handle || normalizeRelease(st.Name) == normalizeRelease(dl.ReleaseTitle) {
			matches++
		}
	}
	if matches > 0 {
		ob.AbsenceSince = time.Time{}
		ob.Inventories = 0
		_ = s.db.UpdateDownloadObservation(ctx, dl.ID, ob)
		if matches > 1 {
			_ = s.db.ParkDownload(ctx, dl.ID, "ambiguous client inventory match; review required")
		}
		return
	}
	if !complete {
		ob.AbsenceSince = time.Time{}
		ob.Inventories = 0
		if now.Sub(dl.AddedAt) >= 30*time.Minute {
			_ = s.db.ParkDownload(ctx, dl.ID, "uncertain submission; client inventory unavailable")
		}
		_ = s.db.UpdateDownloadObservation(ctx, dl.ID, ob)
		return
	}
	if ob.Config != fp || ob.LastInventory.IsZero() || now.Sub(ob.LastInventory) > 6*time.Hour {
		ob.AbsenceSince = now
		ob.Inventories = 0
	}
	ob.Config = fp
	ob.LastInventory = now
	ob.Inventories++
	if now.Sub(dl.AddedAt) >= 30*time.Minute && ob.Inventories >= 3 && now.Sub(ob.AbsenceSince) >= 30*time.Minute {
		_ = s.db.ParkDownload(ctx, dl.ID, "submission uncertain; episode reserved while inventories continue")
	}
	if !ob.AbsenceSince.IsZero() && now.Sub(ob.AbsenceSince) >= 7*24*time.Hour && dl.PlanID != 0 && !persistedDownloadHeld(dl.RunnerControl) && !strings.HasPrefix(dl.ParkedReason, "operator cancellation") {
		if err = s.releaseUncertainRisk(ctx, dl, ob); err != nil {
			s.log.Warn("uncertain risk retry pending", "download", dl.ID, "err", err)
		}
		return
	}
	_ = s.db.UpdateDownloadObservation(ctx, dl.ID, ob)
}
func (s *Service) releaseUncertainRisk(ctx context.Context, dl sqlite.Download, ob executionObservation) error {
	if time.Since(ob.LastInventory) > 5*time.Minute {
		return fmt.Errorf("final inventory stale")
	}
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	ob.DuplicateRisk = true
	raw, _ := json.Marshal(ob)
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `UPDATE downloads SET superseded=1,parked_at=0,parked_reason='retry_with_duplicate_risk',reserved_episodes='[]',wantables='[]',observation=?,error='Possible duplicate: uncertain original submission retried after seven days of complete successful inventories',updated_at=? WHERE id=? AND superseded=0 AND submission_phase IN ('submitting','uncertain')`, string(raw), now, dl.ID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	p, _ := json.Marshal(seasonCheckpoint{Version: 1, ItemID: dl.MediaItemID, CopyID: dl.CopyID, Season: dl.Season, Trigger: "retry_with_duplicate_risk", OriginalDownloadID: dl.ID})
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO jobs(kind,payload,priority,run_after,dedupe_key,created_at,updated_at) VALUES(?,?,50,?,?,?,?)`, SeasonSearchJobKind, string(p), now, seasonJobKey(dl.MediaItemID, dl.CopyID, dl.Season), now, now); err != nil {
		return err
	}
	warning, _ := json.Marshal(map[string]any{"originalDownloadId": dl.ID, "evidenceSince": ob.AbsenceSince, "warning": "Possible duplicate: absence is not proof of nonacceptance"})
	if _, err = tx.ExecContext(ctx, `INSERT INTO history_events(ts,type,media_item_id,release_title,data) VALUES(?,'retry_with_duplicate_risk',?,?,?)`, now, dl.MediaItemID, dl.ReleaseTitle, string(warning)); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) effectiveImportAllowlist(ctx context.Context, dl sqlite.Download) ([]int64, error) {
	if strings.HasPrefix(dl.ParkedReason, "operator cancellation") {
		return nil, fmt.Errorf("operator cancellation prohibits automatic publication")
	}
	if dl.Superseded {
		return nil, fmt.Errorf("superseded submission cannot publish; possible duplicate requires review")
	}
	p, err := s.db.GetAcquisitionPlan(ctx, dl.PlanID)
	if err != nil {
		return nil, err
	}
	if p.State == "cancel_requested" || p.State == "cancelled" {
		return nil, fmt.Errorf("cancelled plan cannot publish automatically")
	}
	var facts seasonDecision
	if err = json.Unmarshal(p.Decision, &facts); err != nil {
		return nil, err
	}
	allowed := slices.Clone(facts.ImportAllowlists[acquisitionplan.CandidateKey(dl.CandidateKey)])
	var pack bool
	var payload []acquisitionplan.EpisodeKey
	for _, c := range facts.Candidates {
		if string(c.Key) == dl.CandidateKey {
			pack = c.Pack
			payload = c.Payload
		}
	}
	if !pack {
		return allowed, nil
	}
	rows, err := s.db.PlanDownloads(ctx, dl.PlanID)
	if err != nil {
		return nil, err
	}
	reservations, err := s.db.ListDownloadReservations(ctx)
	if err != nil {
		return nil, err
	}
	for ep, key := range facts.Plan.Providers {
		if string(key) == dl.CandidateKey || !slices.Contains(payload, ep) {
			continue
		}
		var failed *sqlite.Download
		for i := range rows {
			r := &rows[i]
			if r.CandidateKey == string(key) && r.State == "failed" && r.SubmissionPhase != "uncertain" && r.SubmissionPhase != "submitting" && !persistedDownloadHeld(r.RunnerControl) && !r.Superseded && !strings.HasPrefix(r.ParkedReason, "operator cancellation") {
				failed = r
				break
			}
		}
		if failed == nil {
			continue
		}
		occupied := false
		for _, r := range reservations {
			if r.ID != dl.ID && r.ID != failed.ID && r.MediaItemID == dl.MediaItemID && r.CopyID == dl.CopyID && (slices.Contains(r.ReservedEpisodes, int64(ep)) || slices.Contains(r.WantableIDs, seasonIdentityForDownload(dl)) || s.downloadReservesEpisode(ctx, r, int64(ep))) {
				occupied = true
			}
		}
		if !occupied {
			allowed = append(allowed, int64(ep))
			_ = s.db.AddHistory(ctx, "planned_pack_fallback", dl.MediaItemID, dl.ReleaseTitle, map[string]any{"episodeId": ep, "failedDownloadId": failed.ID})
		}
	}
	// Updating the row's exact reservation under the import lock fences successor
	// admission until publication. The immutable decision itself is unchanged.
	raw, _ := json.Marshal(allowed)
	wants := slices.Clone(dl.WantableIDs)
	item, e := s.db.GetMediaItemFull(ctx, dl.MediaItemID)
	if e != nil {
		return nil, e
	}
	for _, season := range item.Seasons {
		for _, episode := range season.Episodes {
			if !slices.Contains(allowed, episode.ID) {
				continue
			}
			want := fmt.Sprintf("episode:%d:%d:%d", dl.MediaItemID, season.Number, episode.EpisodeNumber)
			if dl.CopyID != 0 {
				want += fmt.Sprintf(":c%d", dl.CopyID)
			}
			if !slices.Contains(wants, want) {
				wants = append(wants, want)
			}
		}
	}
	wantJSON, _ := json.Marshal(wants)
	_, err = s.db.W.ExecContext(ctx, `UPDATE downloads SET reserved_episodes=?,wantables=? WHERE id=? AND superseded=0`, string(raw), string(wantJSON), dl.ID)
	return allowed, err
}

func seasonIdentityForDownload(dl sqlite.Download) string {
	id := fmt.Sprintf("season:%d:%d", dl.MediaItemID, dl.Season)
	if dl.CopyID != 0 {
		id += fmt.Sprintf(":c%d", dl.CopyID)
	}
	return id
}
func (s *Service) downloadReservesEpisode(ctx context.Context, dl sqlite.Download, episodeID int64) bool {
	for _, id := range dl.WantableIDs {
		w, err := s.wantableFromID(ctx, id)
		if ep, ok := w.(domain.EpisodeWantable); err == nil && ok && ep.EpisodeID == episodeID {
			return true
		}
	}
	return false
}

// ResolveReservation is a deliberate operator acknowledgment, never absence
// proof. Keep the original identity and block its late automatic publication.
func (s *Service) ResolveReservation(ctx context.Context, id int64) error {
	unlock := s.lockDownload(id)
	defer unlock()
	s.importTargetMu.Lock()
	defer s.importTargetMu.Unlock()
	dl, err := s.db.GetDownload(ctx, id)
	if err != nil {
		return err
	}
	if dl.Superseded {
		return nil
	}
	if dl.PlanID == 0 || dl.CleanupPending || dl.State == "imported" || dl.State == "importing" || (dl.SubmissionPhase != "uncertain" && dl.ParkedAt.UnixMilli() <= 0) {
		return fmt.Errorf("row has no resolvable uncertain or cancelled reservation")
	}
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `UPDATE downloads SET superseded=1,parked_at=0,parked_reason='operator_acknowledged_duplicate_risk',wantables='[]',reserved_episodes='[]',error='Possible duplicate: operator released original custody; deliberate Search may acquire a replacement',updated_at=? WHERE id=? AND superseded=0`, time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	warning, _ := json.Marshal(map[string]any{"originalDownloadId": id, "warning": "Possible duplicate: operator acknowledgment, not proof of nonacceptance"})
	_, err = tx.ExecContext(ctx, `INSERT INTO history_events(ts,type,media_item_id,release_title,data) VALUES(?,'operator_acknowledged_duplicate_risk',?,?,?)`, time.Now().UnixMilli(), dl.MediaItemID, dl.ReleaseTitle, string(warning))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Called under the common admission/import mutex. Only a conditional pending
// claim can yield to an explicit manual choice; possibly sent rows keep custody.
func (s *Service) closePendingForManual(ctx context.Context, dl sqlite.Download) (bool, error) {
	tx, err := s.db.W.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE downloads SET state='failed',submission_phase='rejected',superseded=1,wantables='[]',reserved_episodes='[]',error='unsubmitted intent superseded by explicit manual choice' WHERE id=? AND submission_phase='pending' AND state='planned'`, dl.ID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE acquisition_plans SET replan_pending=1 WHERE id=?`, dl.PlanID)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func wantablesOverlap(row sqlite.Download, wants []string) bool {
	for _, want := range wants {
		if wantableOnRow(row, want) {
			return true
		}
		candidate := sqlite.Download{WantableIDs: []string{want}}
		for _, existing := range row.WantableIDs {
			if wantableOnRow(candidate, existing) {
				return true
			}
		}
	}
	return false
}
