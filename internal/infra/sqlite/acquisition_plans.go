package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// AcquisitionPlan has public bounded decision facts, never transport URLs.
type AcquisitionPlan struct {
	ID                  int64           `json:"id"`
	MediaItemID         int64           `json:"mediaItemId"`
	CopyID              int64           `json:"copyId"`
	Season              int             `json:"season"`
	State               string          `json:"state"`
	Revision            int64           `json:"revision"`
	PolicyVersion       int             `json:"policyVersion"`
	SnapshotFingerprint string          `json:"snapshotFingerprint"`
	Decision            json.RawMessage `json:"decision"`
	AddedAt             time.Time       `json:"addedAt"`
}

// Admission requires a library/configuration revision read before discovery.
// The scope index and episode overlap read share the write transaction.
func (d *DB) AdmitAcquisitionPlan(ctx context.Context, p AcquisitionPlan, revision int64, rows []Download) (int64, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var current int64
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM lifecycle_library_revision WHERE id=1`).Scan(&current); err != nil {
		return 0, err
	}
	if current != revision {
		return 0, fmt.Errorf("acquisition snapshot changed")
	}
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `INSERT INTO acquisition_plans(media_item_id,copy_id,season,state,snapshot_fingerprint,decision,added_at,updated_at) VALUES(?,?,?,'admitted',?,?,?,?)`, p.MediaItemID, p.CopyID, p.Season, p.SnapshotFingerprint, string(p.Decision), now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, dl := range rows {
		// Exact episode identities on planned rows prevent incidental pack payload
		// from reserving protected siblings. Legacy season rows remain conservative.
		for _, want := range dl.WantableIDs {
			var n int
			err = tx.QueryRowContext(ctx, `SELECT count(*) FROM downloads WHERE media_item_id=? AND COALESCE(copy_id,0)=? AND superseded=0 AND (state IN ('planned','grabbed','downloading','downloaded','awaiting_import','importing') OR submission_phase IN ('submitting','uncertain') OR parked_at>0) AND EXISTS(SELECT 1 FROM json_each(wantables) WHERE value=? OR value=?)`, dl.MediaItemID, dl.CopyID, want, seasonIdentity(dl.MediaItemID, dl.CopyID, dl.Season)).Scan(&n)
			if err != nil {
				return 0, err
			}
			if n > 0 {
				return 0, fmt.Errorf("episode custody changed")
			}
		}
		for _, ep := range dl.ReservedEpisodes {
			var n int
			err = tx.QueryRowContext(ctx, `SELECT count(*) FROM downloads WHERE media_item_id=? AND COALESCE(copy_id,0)=? AND superseded=0 AND (state IN ('planned','grabbed','downloading','downloaded','awaiting_import','importing') OR submission_phase IN ('submitting','uncertain') OR parked_at>0) AND EXISTS(SELECT 1 FROM json_each(reserved_episodes) WHERE value=?)`, dl.MediaItemID, dl.CopyID, ep).Scan(&n)
			if err != nil {
				return 0, err
			}
			if n > 0 {
				return 0, fmt.Errorf("episode custody changed")
			}
		}
		wants, _ := json.Marshal(dl.WantableIDs)
		eps, _ := json.Marshal(dl.ReservedEpisodes)
		evidence, _ := json.Marshal(dl.MatchEvidence)
		transfer := dl.Transfer
		if transfer == "" {
			var tokenErr error
			transfer, tokenErr = ports.NewTransferToken(0)
			if tokenErr != nil {
				return 0, tokenErr
			}
		}
		_, e := tx.ExecContext(ctx, `INSERT INTO downloads(media_item_id,copy_id,wantables,season,release_title,indexer,protocol,quality,size,client_id,state,match_evidence,plan_id,candidate_key,submission_phase,execution_payload,reserved_episodes,transfer,source_guid,source_indexer_id,added_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,'planned',?,?,?,'pending',?,?,?,?,?,?,?)`, dl.MediaItemID, nullableCopy(dl.CopyID), string(wants), dl.Season, dl.ReleaseTitle, dl.Indexer, dl.Protocol, dl.Quality.String(), dl.Size, dl.ClientID, string(evidence), id, dl.CandidateKey, dl.ExecutionPayload, string(eps), transfer, dl.SourceGUID, dl.SourceIndexerID, now, now)
		if e != nil {
			return 0, e
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}
func nullableCopy(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
func seasonIdentity(item, copy int64, season int) string {
	s := fmt.Sprintf("season:%d:%d", item, season)
	if copy != 0 {
		s += fmt.Sprintf(":c%d", copy)
	}
	return s
}
func (d *DB) AcquisitionRevision(ctx context.Context) (int64, error) {
	var n int64
	err := d.R.QueryRowContext(ctx, `SELECT revision FROM lifecycle_library_revision WHERE id=1`).Scan(&n)
	return n, err
}
func (d *DB) GetAcquisitionPlan(ctx context.Context, id int64) (AcquisitionPlan, error) {
	var p AcquisitionPlan
	var raw string
	var added int64
	err := d.R.QueryRowContext(ctx, `SELECT id,media_item_id,copy_id,season,state,revision,policy_version,snapshot_fingerprint,decision,added_at FROM acquisition_plans WHERE id=?`, id).Scan(&p.ID, &p.MediaItemID, &p.CopyID, &p.Season, &p.State, &p.Revision, &p.PolicyVersion, &p.SnapshotFingerprint, &raw, &added)
	p.Decision = json.RawMessage(raw)
	p.AddedAt = time.UnixMilli(added)
	return p, err
}
func (d *DB) PlanDownloads(ctx context.Context, id int64) ([]Download, error) {
	rows, err := d.Read.ListPlanDownloads(ctx, sql.NullInt64{Int64: id, Valid: true})
	if err != nil {
		return nil, err
	}
	out := make([]Download, 0, len(rows))
	for _, r := range rows {
		out = append(out, downloadFromRow(r))
	}
	return out, nil
}
func (d *DB) ListDownloadReservations(ctx context.Context) ([]Download, error) {
	rows, err := d.Read.ListDownloadReservations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Download, 0, len(rows))
	for _, r := range rows {
		out = append(out, downloadFromRow(r))
	}
	return out, nil
}
func (d *DB) RecentFailedDownloads(ctx context.Context, since time.Time) ([]Download, error) {
	rows, err := d.Read.ListFailedDownloadsSince(ctx, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	out := make([]Download, 0, len(rows))
	for _, r := range rows {
		out = append(out, downloadFromRow(r))
	}
	return out, nil
}
func (d *DB) ClaimPlannedSubmission(ctx context.Context, id int64) (bool, error) {
	res, err := d.W.ExecContext(ctx, `UPDATE downloads SET state='grabbed',submission_phase='submitting',updated_at=? WHERE id=? AND state='planned' AND submission_phase='pending' AND superseded=0 AND EXISTS(SELECT 1 FROM acquisition_plans WHERE id=downloads.plan_id AND state IN ('admitted','dispatching','active'))`, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetPlannedSubmissionCapabilities pins the negotiated correlation semantics
// before a planned row can be claimed and dispatched. These fields are an
// admission snapshot; later client configuration changes cannot weaken them.
func (d *DB) SetPlannedSubmissionCapabilities(ctx context.Context, id int64, deliveryExpected, requestTokenIdempotent, exactTransferCorrelation bool) (bool, error) {
	res, err := d.W.ExecContext(ctx, `UPDATE downloads SET delivery_expected=?,request_token_idempotent=?,exact_transfer_correlation=?,updated_at=?
		WHERE id=? AND state='planned' AND submission_phase='pending' AND superseded=0`, boolInt(deliveryExpected), boolInt(requestTokenIdempotent), boolInt(exactTransferCorrelation), time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
func (d *DB) SetSubmissionPhase(ctx context.Context, id int64, phase string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE downloads SET submission_phase=?,manual_retry_at=CASE WHEN ?='pending' OR ?='submitted' THEN 0 ELSE manual_retry_at END,manual_retry_attempts=CASE WHEN ?='submitted' THEN 0 ELSE manual_retry_attempts END,manual_blocked_at=CASE WHEN ?='submitted' THEN 0 ELSE manual_blocked_at END,updated_at=? WHERE id=?`, phase, phase, phase, phase, phase, time.Now().UnixMilli(), id)
	return err
}
func (d *DB) DownloadProviderRetryAfter(ctx context.Context, id int64) (time.Time, error) {
	var due int64
	err := d.R.QueryRowContext(ctx, `SELECT provider_retry_after FROM downloads WHERE id=?`, id).Scan(&due)
	if err != nil {
		return time.Time{}, err
	}
	if due == 0 {
		return time.Time{}, nil
	}
	return time.UnixMilli(due), nil
}

// ListManualAdmissionRetries returns durable one-shot manual admissions that
// are safe to submit or replay with their persisted token.
type ManualAdmissionRetry struct {
	DownloadID int64
	BlockedAt  time.Time
}

func (d *DB) ListManualAdmissionRetries(ctx context.Context, now time.Time, limit int) ([]ManualAdmissionRetry, error) {
	if limit <= 0 || limit > 128 {
		limit = 32
	}
	rows, err := d.W.QueryContext(ctx, `SELECT id,manual_blocked_at FROM downloads
		WHERE intent='manual_additional' AND state='grabbed' AND MAX(manual_retry_at,provider_retry_after)<=?
		AND (submission_phase IN ('pending','submitting','uncertain') OR (submission_phase='submitted' AND exact_transfer_correlation=1 AND handle='')) AND parked_at=0 AND superseded=0
		ORDER BY manual_retry_at,id LIMIT ?`, now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var retries []ManualAdmissionRetry
	for rows.Next() {
		var retry ManualAdmissionRetry
		var blockedAt int64
		if err := rows.Scan(&retry.DownloadID, &blockedAt); err != nil {
			return nil, err
		}
		if blockedAt != 0 {
			retry.BlockedAt = time.UnixMilli(blockedAt)
		}
		retries = append(retries, retry)
	}
	return retries, rows.Err()
}

func (d *DB) ScheduleManualAdmissionRetry(ctx context.Context, id int64, now time.Time, reason string, providerDeadline ...time.Time) error {
	var attempts int
	if err := d.R.QueryRowContext(ctx, `SELECT manual_retry_attempts FROM downloads WHERE id=? AND intent='manual_additional'`, id).Scan(&attempts); err != nil {
		return err
	}
	backoff := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}
	delay := backoff[min(attempts, len(backoff)-1)]
	next := now.Add(delay)
	providerAt := int64(0)
	if len(providerDeadline) > 0 && providerDeadline[0].After(now) {
		providerAt = providerDeadline[0].UnixMilli()
		if providerDeadline[0].After(next) {
			next = providerDeadline[0]
		}
	}
	_, err := d.W.ExecContext(ctx, `UPDATE downloads SET
		manual_retry_at=?,provider_retry_after=MAX(provider_retry_after,?),
		manual_retry_attempts=manual_retry_attempts+1,
		manual_blocked_at=CASE WHEN manual_blocked_at=0 THEN ? ELSE manual_blocked_at END,
		error=?,updated_at=? WHERE id=? AND intent='manual_additional' AND manual_retry_at<=?
		AND state='grabbed' AND (submission_phase IN ('pending','submitting','uncertain') OR (submission_phase='submitted' AND exact_transfer_correlation=1 AND handle='')) AND parked_at=0 AND superseded=0`, next.UnixMilli(), providerAt, now.UnixMilli(), reason, now.UnixMilli(), id, now.UnixMilli())
	return err
}

func (d *DB) SetManualAdmissionWarning(ctx context.Context, id int64, reason string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE downloads SET error=?,updated_at=? WHERE id=? AND intent='manual_additional' AND state='grabbed' AND (submission_phase IN ('pending','submitting','uncertain') OR (submission_phase='submitted' AND exact_transfer_correlation=1 AND handle='')) AND parked_at=0 AND superseded=0`, reason, time.Now().UnixMilli(), id)
	return err
}

func (d *DB) SetExactTransferCorrelationWarning(ctx context.Context, id int64, generation, transfer, warning string) error {
	res, err := d.W.ExecContext(ctx, `UPDATE downloads SET error=?,updated_at=? WHERE id=? AND generation_key=? AND transfer=? AND exact_transfer_correlation=1 AND submission_phase='submitted' AND handle='' AND state='grabbed' AND parked_at=0 AND superseded=0`, warning, time.Now().UnixMilli(), id, generation, transfer)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrNotFound
	}
	return nil
}
func (d *DB) UpdatePlanState(ctx context.Context, id int64, state string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE acquisition_plans SET state=?,revision=revision+1,updated_at=? WHERE id=?`, state, time.Now().UnixMilli(), id)
	return err
}
func (d *DB) UpdateDownloadObservation(ctx context.Context, id int64, observation any) error {
	b, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	_, err = d.W.ExecContext(ctx, `UPDATE downloads SET observation=? WHERE id=?`, string(b), id)
	return err
}
func (d *DB) ParkDownload(ctx context.Context, id int64, reason string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE downloads SET parked_at=CASE WHEN parked_at=0 THEN ? ELSE parked_at END,parked_reason=CASE WHEN parked_reason LIKE 'operator cancellation%' THEN parked_reason ELSE ? END WHERE id=? AND superseded=0`, time.Now().UnixMilli(), reason, id)
	return err
}
func (d *DB) UpdateJobCheckpoint(ctx context.Context, id int64, owner, payload string, manualSpent bool, backgroundPriority int64) error {
	if len(payload) > 2<<20 {
		return fmt.Errorf("discovery checkpoint exceeds 2 MiB")
	}
	// The request can land before or after this write. Keep a newer stored
	// generation, or let the later promotion overwrite this checkpoint. A
	// consumed manual epoch demotes only when no fresh request superseded it.
	now := time.Now().UnixMilli()
	res, err := d.W.ExecContext(ctx, `
        WITH incoming(payload,manual_spent,background_priority,ts) AS (VALUES(?,?,?,?))
        UPDATE jobs SET
          payload=CASE WHEN COALESCE(json_extract(jobs.payload,'$.manualGeneration'),0)
                              > COALESCE(json_extract(incoming.payload,'$.manualGeneration'),0)
            THEN json_set(incoming.payload,'$.manual',json('true'),
                          '$.manualGeneration',json_extract(jobs.payload,'$.manualGeneration'))
            ELSE incoming.payload END,
          priority=CASE WHEN COALESCE(json_extract(jobs.payload,'$.manualGeneration'),0)
                               > COALESCE(json_extract(incoming.payload,'$.manualGeneration'),0)
                            OR COALESCE(json_extract(incoming.payload,'$.manual'),0)=1
            THEN ? WHEN incoming.manual_spent THEN incoming.background_priority
            ELSE jobs.priority END,
          updated_at=incoming.ts
        FROM incoming
        WHERE jobs.id=? AND state='leased' AND lease_owner=? AND lease_expires_at>incoming.ts`,
		payload, manualSpent, backgroundPriority, now, domain.JobPriorityInteractive, id, owner)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("discovery lease lost")
	}
	return nil
}
