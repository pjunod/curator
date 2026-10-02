package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
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
		result, e := tx.ExecContext(ctx, `INSERT INTO downloads(media_item_id,copy_id,wantables,season,release_title,indexer,protocol,quality,size,client_id,state,match_evidence,plan_id,candidate_key,submission_phase,execution_payload,reserved_episodes,added_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,'planned',?,?,?,'pending',?,?,?,?)`, dl.MediaItemID, nullableCopy(dl.CopyID), string(wants), dl.Season, dl.ReleaseTitle, dl.Indexer, dl.Protocol, dl.Quality.String(), dl.Size, dl.ClientID, string(evidence), id, dl.CandidateKey, dl.ExecutionPayload, string(eps), now, now)
		if e != nil {
			return 0, e
		}
		did, e := result.LastInsertId()
		if e != nil {
			return 0, e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE downloads SET transfer=? WHERE id=?`, fmt.Sprintf("t-%d-plan-%d", did, id), did); e != nil {
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
func (d *DB) SetSubmissionPhase(ctx context.Context, id int64, phase string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE downloads SET submission_phase=?,updated_at=? WHERE id=?`, phase, time.Now().UnixMilli(), id)
	return err
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
	_, err := d.W.ExecContext(ctx, `UPDATE downloads SET parked_at=CASE WHEN parked_at=0 THEN ? ELSE parked_at END,parked_reason=? WHERE id=? AND superseded=0`, time.Now().UnixMilli(), reason, id)
	return err
}
func (d *DB) UpdateJobCheckpoint(ctx context.Context, id int64, owner, payload string) error {
	if len(payload) > 2<<20 {
		return fmt.Errorf("discovery checkpoint exceeds 2 MiB")
	}
	res, err := d.W.ExecContext(ctx, `UPDATE jobs SET payload=?,updated_at=? WHERE id=? AND state='leased' AND lease_owner=? AND lease_expires_at>?`, payload, time.Now().UnixMilli(), id, owner, time.Now().UnixMilli())
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
