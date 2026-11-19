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
	o := ActivityOperation{ID: id}
	var b string
	if e := d.R.QueryRowContext(ctx, `SELECT action,targets,updated_at FROM activity_operations WHERE id=?`, id).Scan(&o.Action, &b, &o.Version); e != nil {
		return o, e
	}
	if e := json.Unmarshal([]byte(b), &o.Targets); e != nil {
		return o, e
	}
	o.Summarize()
	return o, nil
}
func (d *DB) SaveActivityOperation(ctx context.Context, o *ActivityOperation) error {
	b, e := json.Marshal(o.Targets)
	if e != nil {
		return e
	}
	now := time.Now().UnixNano()
	tx, e := d.W.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer func() { _ = tx.Rollback() }()
	res, e := tx.ExecContext(ctx, `UPDATE activity_operations SET targets=?,updated_at=? WHERE id=? AND updated_at=?`, string(b), now, o.ID, o.Version)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return fmt.Errorf("operation changed while reconciling; retrying from its durable snapshot")
	}
	if o.Action == "delete" || o.Action == "retry" {
		for _, t := range o.Targets {
			state := "requested"
			if t.State == "blocked" {
				state = "blocked"
			}
			if t.State == "succeeded" {
				state = "verified"
			}
			if _, e = tx.ExecContext(ctx, `UPDATE download_dispositions SET state=?,message=? WHERE download_id=? AND generation_key=?`, state, t.Message, t.ID, t.GenerationKey); e != nil {
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
func (d *DB) VerifyDownloadDeletion(ctx context.Context, id int64, generation string) error {
	res, e := d.W.ExecContext(ctx, `UPDATE download_dispositions SET verified_at=?,state='verified' WHERE download_id=? AND generation_key=? AND NOT EXISTS(SELECT 1 FROM publications p WHERE p.download_id=download_dispositions.download_id AND p.state!='removed')`, time.Now().UnixMilli(), id, generation)
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
	return nil
}
