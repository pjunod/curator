package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

var ErrHouseholdPresent = errors.New("requested primary scope is already present")

func householdRequestHasWork(ctx context.Context, q scopeQuerier, id int64) (bool, error) {
	var busy bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM household_request_targets t JOIN downloads d ON d.id=t.download_id AND d.generation_key=t.download_generation WHERE t.request_id=? AND (d.state NOT IN('failed','imported') OR d.parked_at>0 OR d.cleanup_pending=1 OR d.submission_phase IN('pending','submitting','uncertain') OR (d.payload_removed=0 AND (d.import_path<>'' OR d.save_path<>'')) OR EXISTS(SELECT 1 FROM publications p WHERE p.download_id=d.id AND p.state<>'removed') OR EXISTS(SELECT 1 FROM import_attempt_stages a WHERE a.download_id=d.id AND a.state='held') OR EXISTS(SELECT 1 FROM import_placements p WHERE json_extract(p.data,'$.download_id')=d.id AND p.state IN('prepared','retained'))))`, id).Scan(&busy)
	return busy, err
}
func readHouseholdTarget(ctx context.Context, q scopeQuerier, id int64) (domain.HouseholdTarget, domain.HouseholdPolicy, string, error) {
	var v domain.HouseholdTarget
	var policy domain.HouseholdPolicy
	var data, state string
	err := q.QueryRowContext(ctx, `SELECT t.id,t.request_id,t.media_item_id,t.episode_id,t.season,t.episode,t.state,t.approval_generation,COALESCE(t.download_id,0),t.download_generation,t.offload_epoch,r.policy_json,r.state FROM household_request_targets t JOIN household_requests r ON r.id=t.request_id WHERE t.id=?`, id).Scan(&v.ID, &v.RequestID, &v.MediaItemID, &v.EpisodeID, &v.Season, &v.Episode, &v.State, &v.ApprovalGeneration, &v.DownloadID, &v.DownloadGeneration, &v.OffloadEpoch, &data, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return v, policy, state, ErrNotFound
	}
	if err != nil {
		return v, policy, state, err
	}
	err = json.Unmarshal([]byte(data), &policy)
	return v, policy, state, err
}
func checkHouseholdPolicy(ctx context.Context, q scopeQuerier, p domain.HouseholdPolicy, t domain.HouseholdTarget) error {
	var kind domain.MediaKind
	var tmdb, tvdb, root, profile, generation, shared, offloaded int64
	var imdb, path, rootPath, identity string
	err := q.QueryRowContext(ctx, `SELECT m.kind,m.tmdb_id,m.tvdb_id,m.imdb_id,m.root_folder_id,m.quality_profile_id,m.path,rf.path,pin.identity,pin.generation,s.revision,m.offloaded_at FROM media_items m JOIN root_folders rf ON rf.id=m.root_folder_id JOIN library_presence_roots pin ON pin.root_id=rf.id AND pin.root_path=rf.path CROSS JOIN lifecycle_shared_revision s WHERE m.id=? AND s.id=1`, t.MediaItemID).Scan(&kind, &tmdb, &tvdb, &imdb, &root, &profile, &path, &rootPath, &identity, &generation, &shared, &offloaded)
	if err != nil {
		return err
	}
	if kind != p.Kind || tmdb != p.TMDBID || root != p.RootID || profile != p.ProfileID || path != p.Path || rootPath != p.RootPath || identity != p.RootIdentity || generation != p.RootGeneration || shared != p.SharedRevision || !householdIDsMatch(p.IDs, domain.ExternalIDs{TMDB: tmdb, TVDB: tvdb, IMDB: imdb}) {
		return ErrRequestChanged
	}
	var conflicting bool
	if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM media_items WHERE kind=? AND id<>? AND (tmdb_id=? OR (?<>0 AND tvdb_id=?) OR (?<>'' AND lower(imdb_id)=lower(?))))`, string(kind), t.MediaItemID, tmdb, tvdb, tvdb, imdb, imdb).Scan(&conflicting); err != nil {
		return err
	}
	if conflicting {
		return ErrRequestChanged
	}
	if kind == domain.KindMovie && (offloaded != t.OffloadEpoch || offloaded > 0 && !p.Reacquire) {
		return ErrRequestChanged
	}
	if kind == domain.KindSeries {
		var season, episode int
		var air string
		if err = q.QueryRowContext(ctx, `SELECT season_number,episode_number,air_date,offloaded_at FROM episodes WHERE id=? AND media_item_id=?`, t.EpisodeID, t.MediaItemID).Scan(&season, &episode, &air, &offloaded); err != nil {
			return err
		}
		date, e := time.Parse("2006-01-02", air)
		if e != nil || date.After(time.Now().UTC()) || season != t.Season || episode != t.Episode || offloaded != t.OffloadEpoch || offloaded > 0 && !p.Reacquire {
			return ErrRequestChanged
		}
	}
	profileHash, e := householdProfileFingerprint(ctx, q, p.ProfileID)
	if e != nil {
		return e
	}
	if profileHash != p.ProfileFingerprint {
		return ErrRequestChanged
	}
	var claimed bool
	if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM household_scope_claims WHERE kind=? AND tmdb_id=? AND copy_id=0 AND season=? AND episode=? AND request_id=?)`, string(p.Kind), p.TMDBID, t.Season, t.Episode, t.RequestID).Scan(&claimed); err != nil {
		return err
	}
	if !claimed {
		return ErrRequestChanged
	}
	return nil
}
func (d *DB) HouseholdTargetPolicy(ctx context.Context, id int64) (domain.HouseholdTarget, domain.HouseholdPolicy, error) {
	v, p, _, err := readHouseholdTarget(ctx, d.R, id)
	return v, p, err
}

// CheckHouseholdAdmission is called after a targeted filesystem check. The
// final insert repeats it inside the writer transaction around immutable intent.
func (d *DB) CheckHouseholdAdmission(ctx context.Context, id, generation int64) error {
	tx, err := d.R.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, _, err = validateHouseholdAdmission(ctx, tx, id, generation)
	return err
}
func validateHouseholdAdmission(ctx context.Context, q scopeQuerier, id, generation int64) (domain.HouseholdTarget, domain.HouseholdPolicy, error) {
	t, p, state, err := readHouseholdTarget(ctx, q, id)
	if err != nil {
		return t, p, err
	}
	if t.ApprovalGeneration != generation || t.State != "approved" || (state != "approved" && state != "acquiring") {
		return t, p, ErrRequestChanged
	}
	if err = checkHouseholdPolicy(ctx, q, p, t); err != nil {
		return t, p, err
	}
	proof, err := readHouseholdEvidence(ctx, q, t.MediaItemID)
	if err != nil {
		return t, p, err
	}
	present := proof.PresentMovie
	if p.Kind == domain.KindSeries {
		present = proof.PresentEpisodes[t.EpisodeID]
	}
	if present {
		return t, p, ErrHouseholdPresent
	}
	return t, p, nil
}

// InsertHouseholdDownload commits the existing automatic transfer intent and
// the household binding together, before Add can reach any client.
func (d *DB) InsertHouseholdDownload(ctx context.Context, id, generation int64, dl Download) (Download, bool, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return Download{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	old, _, _, err := readHouseholdTarget(ctx, tx, id)
	if err != nil {
		return Download{}, false, err
	}
	if old.ApprovalGeneration != generation {
		return Download{}, false, ErrRequestChanged
	}
	if old.DownloadID != 0 {
		_ = tx.Rollback()
		existing, e := d.GetDownload(ctx, old.DownloadID)
		if e != nil || existing.GenerationKey != old.DownloadGeneration {
			return Download{}, false, ErrRequestChanged
		}
		return existing, false, nil
	}
	target, policy, err := validateHouseholdAdmission(ctx, tx, id, generation)
	if err != nil {
		return Download{}, false, err
	}
	want := fmt.Sprintf("movie:%d", target.MediaItemID)
	if policy.Kind == domain.KindSeries {
		want = fmt.Sprintf("episode:%d:%d:%d", target.MediaItemID, target.Season, target.Episode)
	}
	if dl.MediaItemID != target.MediaItemID || dl.CopyID != 0 || len(dl.WantableIDs) != 1 || dl.WantableIDs[0] != want || dl.Intent != "automatic" || dl.SubmissionPhase != "submitting" {
		return Download{}, false, ErrRequestChanged
	}
	params, err := downloadInsertParams(dl)
	if err != nil {
		return Download{}, false, err
	}
	downloadID, err := d.Write.WithTx(tx).InsertDownload(ctx, params)
	if err != nil {
		return Download{}, false, err
	}
	origin := domain.HouseholdOrigin{OffloadEpoch: target.OffloadEpoch, Season: target.Season, Episode: target.Episode, Policy: policy, ItemID: target.MediaItemID, Kind: policy.Kind, TMDBID: policy.TMDBID, Path: policy.Path, RootID: policy.RootID, RootPath: policy.RootPath, RootIdentity: policy.RootIdentity, RootGeneration: policy.RootGeneration}
	if target.EpisodeID != 0 {
		origin.EpisodeIDs = []int64{target.EpisodeID}
	}
	scopeJSON, err := json.Marshal(origin)
	if err != nil {
		return Download{}, false, err
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return Download{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO household_download_origins(download_id,generation,target_id,approval_generation,scope_json,policy_json) VALUES(?,?,?,?,?,?)`, downloadID, params.GenerationKey, id, generation, string(scopeJSON), string(policyJSON)); err != nil {
		return Download{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE household_request_targets SET state='admitted',download_id=?,download_generation=? WHERE id=? AND approval_generation=?`, downloadID, params.GenerationKey, id, generation); err != nil {
		return Download{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE household_requests SET state='acquiring',updated_at=?,revision=revision+1 WHERE id=?`, time.Now().UnixMilli(), target.RequestID); err != nil {
		return Download{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Download{}, false, err
	}
	result, err := d.GetDownload(ctx, downloadID)
	return result, true, err
}
func (d *DB) HouseholdDownloadOrigin(ctx context.Context, id int64, generation string) (*domain.HouseholdOrigin, error) {
	var data, policy string
	err := d.R.QueryRowContext(ctx, `SELECT scope_json,policy_json FROM household_download_origins WHERE download_id=? AND generation=?`, id, generation).Scan(&data, &policy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var origin domain.HouseholdOrigin
	if err = json.Unmarshal([]byte(data), &origin); err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(policy), &origin.Policy); err != nil {
		return nil, err
	}
	return &origin, nil
}

// BindExistingHouseholdDownload follows an already authorized owner transfer.
// It creates no household origin and never broadens or narrows that transfer.
func (d *DB) BindExistingHouseholdDownload(ctx context.Context, targetID, generation, downloadID int64) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	target, policy, err := validateHouseholdAdmission(ctx, tx, targetID, generation)
	if err != nil {
		return err
	}
	row, err := d.Write.WithTx(tx).GetDownload(ctx, downloadID)
	if err != nil {
		return err
	}
	download := downloadFromRow(row)
	if download.MediaItemID != target.MediaItemID || download.CopyID != 0 || download.Superseded || download.ParkedAt.UnixMilli() > 0 || !slices.Contains([]string{"planned", "submitting", "grabbed", "downloading", "downloaded", "awaiting_import", "importing"}, download.State) {
		return ErrRequestChanged
	}
	want := fmt.Sprintf("movie:%d", target.MediaItemID)
	if policy.Kind == domain.KindSeries {
		want = fmt.Sprintf("episode:%d:%d:%d", target.MediaItemID, target.Season, target.Episode)
	}
	stopped, err := householdDownloadNeedsReview(ctx, tx, download.ID, download.GenerationKey)
	if err != nil {
		return err
	}
	if stopped {
		return ErrRequestChanged
	}
	if !slices.Contains(download.WantableIDs, want) && (policy.Kind != domain.KindSeries || download.PlanID == 0 || !slices.Contains(download.ReservedEpisodes, target.EpisodeID)) {
		return ErrRequestOverlap
	}
	binding, err := tx.ExecContext(ctx, `UPDATE household_request_targets SET state='admitted',download_id=?,download_generation=? WHERE id=? AND approval_generation=? AND download_id IS NULL`, download.ID, download.GenerationKey, targetID, generation)
	if err != nil {
		return err
	}
	if changed, e := binding.RowsAffected(); e != nil {
		return e
	} else if changed != 1 {
		return ErrRequestChanged
	}
	if _, err = tx.ExecContext(ctx, `UPDATE household_requests SET state='acquiring',updated_at=?,revision=revision+1 WHERE id=?`, time.Now().UnixMilli(), target.RequestID); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) SetHouseholdTargetResult(ctx context.Context, id, generation int64, state, note string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	target, policy, requestStatus, err := readHouseholdTarget(ctx, tx, id)
	if err != nil {
		return err
	}
	if target.ApprovalGeneration != generation {
		return ErrRequestChanged
	}
	if state == "fulfilled" {
		// Fulfillment may follow owner-authorized moves, but must never follow a
		// rematch of the durable item or episode to another catalogue identity.
		var kind domain.MediaKind
		var ids domain.ExternalIDs
		if err = tx.QueryRowContext(ctx, `SELECT kind,tmdb_id,tvdb_id,imdb_id FROM media_items WHERE id=?`, target.MediaItemID).Scan(&kind, &ids.TMDB, &ids.TVDB, &ids.IMDB); err != nil {
			return err
		}
		if kind != policy.Kind || ids.TMDB != policy.TMDBID || domain.ExternalIDsConflict(ids, policy.IDs) {
			return ErrRequestChanged
		}
		if kind == domain.KindSeries {
			var sameLeaf bool
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM episodes WHERE id=? AND media_item_id=? AND season_number=? AND episode_number=?)`, target.EpisodeID, target.MediaItemID, target.Season, target.Episode).Scan(&sameLeaf); err != nil {
				return err
			}
			if !sameLeaf {
				return ErrRequestChanged
			}
		}
		proof, e := readHouseholdEvidence(ctx, tx, target.MediaItemID)
		if e != nil {
			return e
		}
		present := proof.PresentMovie
		if policy.Kind == domain.KindSeries {
			present = proof.PresentEpisodes[target.EpisodeID]
		}
		if !present {
			return ErrHouseholdVerification
		}
		if _, err = tx.ExecContext(ctx, `UPDATE household_request_targets SET state='fulfilled' WHERE id=? AND approval_generation=?`, id, generation); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM household_scope_claims WHERE request_id=? AND season=? AND episode=?`, target.RequestID, target.Season, target.Episode); err != nil {
			return err
		}
	} else if state == "needs_review" {
		if _, err = tx.ExecContext(ctx, `UPDATE household_request_targets SET state=CASE WHEN download_id IS NULL THEN 'needs_review' ELSE state END WHERE id=? AND approval_generation=?`, id, generation); err != nil {
			return err
		}
	} else if state != "waiting" {
		return ErrRequestChanged
	}
	if _, err = tx.ExecContext(ctx, `UPDATE household_request_targets SET next_check_at=? WHERE id=? AND approval_generation=?`, time.Now().Add(time.Minute).UnixMilli(), id, generation); err != nil {
		return err
	}
	var unfinished bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM household_request_targets WHERE request_id=? AND state<>'fulfilled')`, target.RequestID).Scan(&unfinished); err != nil {
		return err
	}
	requestState := requestStatus
	if !unfinished {
		requestState, note = "fulfilled", ""
	} else if requestStatus == "needs_review" {
		// Progress on another leaf must preserve the unresolved review reason.
		if err = tx.QueryRowContext(ctx, `SELECT public_note FROM household_requests WHERE id=?`, target.RequestID).Scan(&note); err != nil {
			return err
		}
	} else if state == "needs_review" {
		requestState = "needs_review"
	}
	leafChanged := state == "fulfilled" && target.State != "fulfilled" || state == "needs_review" && target.DownloadID == 0 && target.State != "needs_review"
	_, err = tx.ExecContext(ctx, `UPDATE household_requests SET state=?,public_note=?,revision=revision+1,updated_at=? WHERE id=? AND (state<>? OR public_note<>? OR ?)`, requestState, note, time.Now().UnixMilli(), target.RequestID, requestState, note, leafChanged)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) IsHouseholdDownload(ctx context.Context, id int64, generation string) (bool, error) {
	var yes bool
	err := d.R.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM household_download_origins WHERE download_id=? AND generation=?)`, id, generation).Scan(&yes)
	return yes, err
}
func (d *DB) FindHouseholdOwnerDownload(ctx context.Context, t domain.HouseholdTarget) (int64, error) {
	want := fmt.Sprintf("movie:%d", t.MediaItemID)
	if t.EpisodeID != 0 {
		want = fmt.Sprintf("episode:%d:%d:%d", t.MediaItemID, t.Season, t.Episode)
	}
	var id int64
	var generation string
	err := d.R.QueryRowContext(ctx, `SELECT d.id,d.generation_key FROM downloads d WHERE d.media_item_id=? AND COALESCE(d.copy_id,0)=0 AND d.superseded=0 AND d.state IN('planned','submitting','grabbed','downloading','downloaded','awaiting_import','importing') AND NOT EXISTS(SELECT 1 FROM household_download_origins h WHERE h.download_id=d.id AND h.generation=d.generation_key) AND (EXISTS(SELECT 1 FROM json_each(d.wantables) w WHERE w.value=?) OR (?<>0 AND d.plan_id<>0 AND EXISTS(SELECT 1 FROM json_each(d.reserved_episodes) e WHERE e.value=?))) ORDER BY d.id LIMIT 1`, t.MediaItemID, want, t.EpisodeID, t.EpisodeID).Scan(&id, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	stopped, e := householdDownloadNeedsReview(ctx, d.R, id, generation)
	if e != nil {
		return 0, e
	}
	if stopped {
		return id, ErrRequestChanged
	}
	return id, nil
}
func (d *DB) HouseholdDownloadNeedsReview(ctx context.Context, id int64, generation string) (bool, error) {
	return householdDownloadNeedsReview(ctx, d.R, id, generation)
}
func householdDownloadNeedsReview(ctx context.Context, q scopeQuerier, id int64, generation string) (bool, error) {
	var stopped bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM downloads d WHERE d.id=? AND d.generation_key=? AND (d.superseded=1 OR d.parked_at>0 OR d.runner_outcome IN('cancelled','failed') OR d.state NOT IN('planned','submitting','grabbed','downloading','downloaded','awaiting_import','importing') OR EXISTS(SELECT 1 FROM import_attempt_controls i WHERE i.download_id=d.id AND i.generation_key=d.generation_key AND i.outcome IN('cancelled','completed')) OR EXISTS(SELECT 1 FROM publications p WHERE p.download_id=d.id AND (p.state='import_failed' OR p.holds>0))))`, id, generation).Scan(&stopped)
	return stopped, err
}
