package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

var ErrRequestChanged = errors.New("request changed; review it again")
var ErrRequestOverlap = errors.New("another approved request already owns this scope")

func requireHouseholdActor(ctx context.Context, tx *sql.Tx, a domain.Account, digest string, owner bool) error {
	var allowed bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM household_accounts a JOIN household_sessions s ON s.account_id=a.id AND s.credential_generation=a.credential_generation WHERE a.id=? AND a.credential_generation=? AND a.status='active' AND s.token_digest=? AND s.expires_at>? AND (?=0 OR (a.role='owner' AND s.method='local')))`, a.ID, a.CredentialGeneration, digest, time.Now().UnixMilli(), boolInt(owner)).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrAccountConflict
	}
	return nil
}

const householdRequestColumns = `r.id,r.revision,r.kind,r.tmdb_id,r.identity_json,r.scope_json,r.title,r.year,r.poster_path,r.overview,r.state,r.public_note,r.created_at,r.updated_at,r.library_item_id,r.fingerprint,r.policy_json,
 (SELECT count(*) FROM household_request_subscriptions sub WHERE sub.request_id=r.id AND sub.active=1),
 EXISTS(SELECT 1 FROM household_request_subscriptions sub WHERE sub.request_id=r.id AND sub.account_id=?1 AND sub.active=1),
 CASE WHEN ?2 THEN COALESCE((SELECT note FROM household_request_notes n WHERE n.request_id=r.id AND n.account_id=?1),'') ELSE '' END`

func scanHouseholdRequest(row accountScanner) (domain.HouseholdRequest, error) {
	var v domain.HouseholdRequest
	var ids, scope string
	var created, updated int64
	err := row.Scan(&v.ID, &v.Revision, &v.Kind, &v.TMDBID, &ids, &scope, &v.Title, &v.Year, &v.PosterPath, &v.Overview, &v.State, &v.PublicNote, &created, &updated, &v.LibraryItemID, &v.Fingerprint, &v.PolicyJSON, &v.SubscriberCount, &v.Subscribed, &v.PrivateNote)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	if err != nil {
		return v, err
	}
	if err = json.Unmarshal([]byte(ids), &v.IDs); err != nil {
		return v, err
	}
	if err = json.Unmarshal([]byte(scope), &v.Scope); err != nil {
		return v, err
	}
	v.CreatedAt = time.UnixMilli(created)
	v.UpdatedAt = time.UnixMilli(updated)
	return v, nil
}
func (d *DB) HouseholdRequests(ctx context.Context, accountID int64, owner bool, before int64) ([]domain.HouseholdRequest, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT `+householdRequestColumns+` FROM household_requests r WHERE (?3=0 OR r.id<?3) ORDER BY r.id DESC LIMIT 100`, accountID, boolInt(owner), before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.HouseholdRequest{}
	for rows.Next() {
		v, err := scanHouseholdRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (d *DB) HouseholdRequest(ctx context.Context, id, accountID int64, owner bool) (domain.HouseholdRequest, error) {
	return scanHouseholdRequest(d.R.QueryRowContext(ctx, `SELECT `+householdRequestColumns+` FROM household_requests r WHERE r.id=?3`, accountID, boolInt(owner), id))
}
func (d *DB) SubmitHouseholdRequest(ctx context.Context, a domain.Account, digest string, v domain.HouseholdRequest) (int64, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = requireHouseholdActor(ctx, tx, a, digest, false); err != nil {
		return 0, err
	}
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM household_requests WHERE fingerprint=? AND state IN('pending','approved','acquiring','needs_review')`, v.Fingerprint).Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		return 0, err
	}
	now := time.Now().UnixMilli()
	if err == sql.ErrNoRows {
		ids, e := json.Marshal(v.IDs)
		if e != nil {
			return 0, e
		}
		scope, e := json.Marshal(v.Scope)
		if e != nil {
			return 0, e
		}
		err = tx.QueryRowContext(ctx, `INSERT INTO household_requests(fingerprint,kind,tmdb_id,identity_json,scope_json,title,year,poster_path,overview,created_by,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) RETURNING id`, v.Fingerprint, string(v.Kind), v.TMDBID, string(ids), string(scope), v.Title, v.Year, v.PosterPath, v.Overview, a.ID, now, now).Scan(&id)
		if err != nil {
			return 0, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO household_request_subscriptions(request_id,account_id,active,created_at) VALUES(?,?,1,?) ON CONFLICT(request_id,account_id) DO UPDATE SET active=1`, id, a.ID, now)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE household_requests SET revision=revision+1,updated_at=? WHERE id=?`, now, id); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}
func (d *DB) JoinHouseholdRequest(ctx context.Context, a domain.Account, digest string, id int64) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = requireHouseholdActor(ctx, tx, a, digest, false); err != nil {
		return err
	}
	var valid bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM household_requests WHERE id=? AND state IN('pending','approved','acquiring','needs_review'))`, id).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrRequestChanged
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO household_request_subscriptions(request_id,account_id,active,created_at) VALUES(?,?,1,?) ON CONFLICT(request_id,account_id) DO UPDATE SET active=1`, id, a.ID, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE household_requests SET revision=revision+1,updated_at=? WHERE id=?`, time.Now().UnixMilli(), id); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) WithdrawHouseholdRequest(ctx context.Context, a domain.Account, digest string, id int64) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = requireHouseholdActor(ctx, tx, a, digest, false); err != nil {
		return err
	}
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM household_requests WHERE id=?`, id).Scan(&state); err != nil {
		return err
	}
	if state != "pending" && state != "needs_review" {
		return ErrRequestChanged
	}
	res, err := tx.ExecContext(ctx, `UPDATE household_request_subscriptions SET active=0 WHERE request_id=? AND account_id=? AND active=1`, id, a.ID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrRequestChanged
	}
	if _, err = tx.ExecContext(ctx, `UPDATE household_requests SET state=CASE WHEN EXISTS(SELECT 1 FROM household_request_subscriptions WHERE request_id=? AND active=1) THEN state WHEN approved_by IS NOT NULL THEN state ELSE 'withdrawn' END,revision=revision+1,updated_at=? WHERE id=?`, id, time.Now().UnixMilli(), id); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) DeclineHouseholdRequest(ctx context.Context, a domain.Account, digest string, id, revision int64, publicNote, privateNote string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = requireHouseholdActor(ctx, tx, a, digest, true); err != nil {
		return err
	}
	if busy, e := householdRequestHasWork(ctx, tx, id); e != nil {
		return e
	} else if busy {
		return ErrRequestChanged
	}
	result, err := tx.ExecContext(ctx, `UPDATE household_requests SET state='declined',public_note=?,revision=revision+1,updated_at=? WHERE id=? AND revision=? AND state IN('pending','needs_review')`, publicNote, time.Now().UnixMilli(), id, revision)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrRequestChanged
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM household_scope_claims WHERE request_id=?`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO household_request_notes(request_id,account_id,note) VALUES(?,?,?) ON CONFLICT(request_id,account_id) DO UPDATE SET note=excluded.note`, id, a.ID, privateNote); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO household_request_decisions(request_id,revision,account_id,decision,policy_json,created_at) SELECT id,revision,?,'declined',policy_json,? FROM household_requests WHERE id=?`, a.ID, time.Now().UnixMilli(), id); err != nil {
		return err
	}
	return tx.Commit()
}
func (d *DB) SaveHouseholdRequestNote(ctx context.Context, a domain.Account, digest string, id int64, note string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = requireHouseholdActor(ctx, tx, a, digest, true); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO household_request_notes(request_id,account_id,note) SELECT id,?,? FROM household_requests WHERE id=? ON CONFLICT(request_id,account_id) DO UPDATE SET note=excluded.note`, a.ID, note, id); err != nil {
		return err
	}
	return tx.Commit()
}
