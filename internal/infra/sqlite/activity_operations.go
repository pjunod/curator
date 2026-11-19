package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const ActivityActionJobKind = "activity.action"

type ActivityIdentity struct {
	ID            int64  `json:"id"`
	GenerationKey string `json:"generationKey"`
	Epoch         string `json:"epoch"`
}
type ActivityTarget struct {
	DispositionRevision int64 `json:"dispositionRevision,omitempty"`
	ActivityIdentity
	Title   string `json:"title"`
	State   string `json:"state"`
	Message string `json:"message"`
	// The exact immutable owner request survives network loss and restart.
	OwnerPayload string `json:"ownerPayload,omitempty"`
}
type ActivityOperation struct {
	Version   int64            `json:"-"`
	ID        string           `json:"id"`
	Action    string           `json:"action"`
	State     string           `json:"state"`
	Total     int              `json:"total"`
	Completed int              `json:"completed"`
	Succeeded int              `json:"succeeded"`
	Blocked   int              `json:"blocked"`
	Targets   []ActivityTarget `json:"targets"`
}

func (o *ActivityOperation) Summarize() {
	o.Total = len(o.Targets)
	o.Completed = 0
	o.Succeeded = 0
	o.Blocked = 0
	o.State = "running"
	for _, t := range o.Targets {
		if t.State == "succeeded" {
			o.Succeeded++
			o.Completed++
		}
		if t.State == "blocked" {
			o.Blocked++
			o.Completed++
		}
	}
	if o.Completed == o.Total {
		o.State = "completed"
	}
}
func (d *DB) SaveActivitySelection(ctx context.Context, id string, targets []ActivityIdentity) error {
	if _, e := d.W.ExecContext(ctx, `DELETE FROM activity_selections WHERE created_at<?`, time.Now().Add(-24*time.Hour).UnixMilli()); e != nil {
		return e
	}

	b, e := json.Marshal(targets)
	if e != nil {
		return e
	}
	_, e = d.W.ExecContext(ctx, `INSERT INTO activity_selections(id,targets,created_at) VALUES(?,?,?)`, id, string(b), time.Now().UnixMilli())
	return e
}
func (d *DB) ActivitySelection(ctx context.Context, id string) ([]ActivityIdentity, error) {
	var b string
	if e := d.R.QueryRowContext(ctx, `SELECT targets FROM activity_selections WHERE id=? AND created_at>?`, id, time.Now().Add(-24*time.Hour).UnixMilli()).Scan(&b); e != nil {
		return nil, e
	}
	var targets []ActivityIdentity
	e := json.Unmarshal([]byte(b), &targets)
	return targets, e
}

// Replay precedes selection expiry validation: an accepted command remains
// recoverable by its request ID after its short-lived selection has expired.
func (d *DB) ReplayActivityOperation(ctx context.Context, id string, payload any) (ActivityOperation, bool, error) {
	raw, e := json.Marshal(payload)
	if e != nil {
		return ActivityOperation{}, false, e
	}
	h := sha256.Sum256(raw)
	var prior string
	e = d.R.QueryRowContext(ctx, `SELECT fingerprint FROM activity_operations WHERE id=?`, id).Scan(&prior)
	if errors.Is(e, sql.ErrNoRows) {
		return ActivityOperation{}, false, nil
	}
	if e != nil {
		return ActivityOperation{}, false, e
	}
	if prior != hex.EncodeToString(h[:]) {
		return ActivityOperation{}, true, fmt.Errorf("request ID already belongs to a different action")
	}
	o, e := d.ActivityOperation(ctx, id)
	return o, true, e
}
func (d *DB) CreateActivityOperation(ctx context.Context, id, action string, payload any, targets []ActivityTarget) (ActivityOperation, error) {
	raw, e := json.Marshal(payload)
	if e != nil {
		return ActivityOperation{}, e
	}
	h := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(h[:])
	tx, e := d.W.BeginTx(ctx, nil)
	if e != nil {
		return ActivityOperation{}, e
	}
	defer func() { _ = tx.Rollback() }()
	var prior string
	e = tx.QueryRowContext(ctx, `SELECT fingerprint FROM activity_operations WHERE id=?`, id).Scan(&prior)
	if e == nil {
		if prior != fingerprint {
			return ActivityOperation{}, fmt.Errorf("request ID already belongs to a different action")
		}
		_ = tx.Rollback()
		return d.ActivityOperation(ctx, id)
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return ActivityOperation{}, e
	}
	now := time.Now().UnixMilli()
	for i := range targets {
		t := &targets[i]
		t.State = "pending"
		var generation, title string
		var epoch int64
		e = tx.QueryRowContext(ctx, `SELECT d.generation_key,d.release_title,COALESCE(c.attempt_epoch,0) FROM downloads d LEFT JOIN import_attempt_controls c ON c.download_id=d.id AND c.generation_key=d.generation_key WHERE d.id=?`, t.ID).Scan(&generation, &title, &epoch)
		if errors.Is(e, sql.ErrNoRows) || (e == nil && (generation != t.GenerationKey || fmt.Sprint(epoch) != t.Epoch)) {
			t.State = "blocked"
			t.Message = "This attempt changed. Refresh and select it again."
			continue
		}
		if e != nil {
			return ActivityOperation{}, e
		}
		t.Title = title
		if action == "delete" {
			if _, e = tx.ExecContext(ctx, `INSERT INTO download_dispositions(download_id,generation_key,requested_at) VALUES(?,?,?) ON CONFLICT DO NOTHING`, t.ID, generation, now); e != nil {
				return ActivityOperation{}, e
			}
			if _, e = tx.ExecContext(ctx, `UPDATE replacement_grabs SET result='cancelled by operator deletion',next_at=0 WHERE download_id=? AND result=''`, t.ID); e != nil {
				return ActivityOperation{}, e
			}
		}
	}
	b, e := json.Marshal(targets)
	if e != nil {
		return ActivityOperation{}, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO activity_operations VALUES(?,?,?,?,?,?)`, id, fingerprint, action, string(b), now, now); e != nil {
		return ActivityOperation{}, e
	}
	for _, t := range targets {
		b, e := json.Marshal(t)
		if e != nil {
			return ActivityOperation{}, e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO activity_operation_targets VALUES(?,?,?,?,?)`, id, t.ID, t.GenerationKey, t.State, string(b)); e != nil {
			return ActivityOperation{}, e
		}
	}
	if action == "delete" {
		for _, target := range targets {
			if target.State == "blocked" {
				continue
			}
			_, e = tx.ExecContext(ctx, `WITH RECURSIVE obligations(id,generation) AS (
 SELECT ?,? UNION SELECT r.child_id,r.child_generation FROM replacement_admissions r JOIN obligations p ON r.parent_id=p.id AND r.parent_generation=p.generation WHERE r.phase='cancel_pending')
 INSERT INTO activity_disposition_links(download_id,generation_key,operation_id,target_id,target_generation) SELECT id,generation,?,?,? FROM obligations`, target.ID, target.GenerationKey, id, target.ID, target.GenerationKey)
			if e != nil {
				return ActivityOperation{}, e
			}
		}
	}
	jobPayload, _ := json.Marshal(map[string]string{"id": id})
	if _, e = tx.ExecContext(ctx, `INSERT INTO jobs(kind,payload,priority,run_after,max_attempts,dedupe_key,created_at,updated_at) VALUES(?,?,10,?,1000000,?,?,?)`, ActivityActionJobKind, string(jobPayload), now, "activity:"+id, now, now); e != nil {
		return ActivityOperation{}, e
	}
	if e = tx.Commit(); e != nil {
		return ActivityOperation{}, e
	}
	return d.ActivityOperation(ctx, id)
}
func (d *DB) ActivityOperation(ctx context.Context, id string) (ActivityOperation, error) {
	o := ActivityOperation{ID: id, Targets: []ActivityTarget{}}
	if e := d.R.QueryRowContext(ctx, `SELECT action,updated_at FROM activity_operations WHERE id=?`, id).Scan(&o.Action, &o.Version); e != nil {
		return o, e
	}
	rows, e := d.R.QueryContext(ctx, `SELECT payload FROM activity_operation_targets WHERE operation_id=? ORDER BY download_id`, id)
	if e != nil {
		return o, e
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b string
		var t ActivityTarget
		if e = rows.Scan(&b); e != nil {
			return o, e
		}
		if e = json.Unmarshal([]byte(b), &t); e != nil {
			return o, e
		}
		o.Targets = append(o.Targets, t)
	}
	if e = rows.Err(); e != nil {
		return o, e
	}
	o.Summarize()
	return o, nil
}

// SaveActivityTarget touches only the active member, so a large bulk request
// cannot repeatedly rewrite its entire membership on the single writer lane.
func (d *DB) SaveActivityTarget(ctx context.Context, o *ActivityOperation, index int) error {
	return d.saveActivityTargets(ctx, o, []ActivityTarget{o.Targets[index]})
}
func (d *DB) SaveActivityOperation(ctx context.Context, o *ActivityOperation) error {
	return d.saveActivityTargets(ctx, o, o.Targets)
}
func (d *DB) saveActivityTargets(ctx context.Context, o *ActivityOperation, targets []ActivityTarget) error {
	tx, e := d.W.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixNano()
	res, e := tx.ExecContext(ctx, `UPDATE activity_operations SET updated_at=? WHERE id=? AND updated_at=?`, now, o.ID, o.Version)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return fmt.Errorf("operation changed while reconciling; retry from its durable snapshot")
	}
	for _, t := range targets {
		var old string
		if e = tx.QueryRowContext(ctx, `SELECT payload FROM activity_operation_targets WHERE operation_id=? AND download_id=? AND generation_key=?`, o.ID, t.ID, t.GenerationKey).Scan(&old); e != nil {
			return e
		}
		b, e := json.Marshal(t)
		if e != nil {
			return e
		}
		if string(b) == old {
			continue
		}
		if _, e = tx.ExecContext(ctx, `UPDATE activity_operation_targets SET state=?,payload=? WHERE operation_id=? AND download_id=? AND generation_key=?`, t.State, string(b), o.ID, t.ID, t.GenerationKey); e != nil {
			return e
		}
		var prior ActivityTarget
		if e = json.Unmarshal([]byte(old), &prior); e != nil {
			return e
		}
		if prior.State != t.State || prior.Message != t.Message {
			if _, e = tx.ExecContext(ctx, `INSERT INTO activity_action_events(operation_id,download_id,generation_key,at,step,message) VALUES(?,?,?,?,?,?)`, o.ID, t.ID, t.GenerationKey, time.Now().UnixMilli(), t.State, t.Message); e != nil {
				return e
			}
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	o.Version = now
	return nil
}
func (d *DB) DownloadDeletionRequested(ctx context.Context, id int64, generation string) (bool, error) {
	var v bool
	e := d.R.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM download_dispositions WHERE download_id=? AND generation_key=?)`, id, generation).Scan(&v)
	return v, e
}
func (d *DB) DownloadDeletionStatus(ctx context.Context, id int64, generation string) (requested, verified bool, err error) {
	var at int64
	err = d.R.QueryRowContext(ctx, `SELECT verified_at FROM download_dispositions WHERE download_id=? AND generation_key=?`, id, generation).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return err == nil, at > 0, err
}
func (d *DB) VerifyDownloadDeletion(ctx context.Context, id int64, generation string, revision int64) error {
	res, e := d.W.ExecContext(ctx, `UPDATE download_dispositions SET verified_at=?,state='verified' WHERE download_id=? AND generation_key=? AND revision=? AND NOT EXISTS(SELECT 1 FROM publications p WHERE p.download_id=download_dispositions.download_id AND p.state!='removed')`, time.Now().UnixMilli(), id, generation, revision)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return fmt.Errorf("new publication arrived while deleting; cleanup is still pending")
	}
	_, e = d.W.ExecContext(ctx, `UPDATE downloads SET updated_at=? WHERE id=? AND generation_key=?`, time.Now().UnixMilli(), id, generation)
	return e
}

func (d *DB) DispositionRevision(ctx context.Context, id int64, generation string) (int64, error) {
	var revision int64
	e := d.R.QueryRowContext(ctx, `SELECT revision FROM download_dispositions WHERE download_id=? AND generation_key=?`, id, generation).Scan(&revision)
	return revision, e
}
func (d *DB) SetDispositionProgress(ctx context.Context, t ActivityTarget) error {
	state := "requested"
	if t.State == "blocked" {
		state = "blocked"
	}
	_, e := d.W.ExecContext(ctx, `UPDATE download_dispositions SET state=?,message=? WHERE download_id=? AND generation_key=? AND revision=? AND verified_at=0`, state, t.Message, t.ID, t.GenerationKey, t.DispositionRevision)
	return e
}
