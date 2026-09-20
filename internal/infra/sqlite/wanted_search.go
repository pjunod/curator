package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

// ErrWantedSearchActive means the database's single active-run slot is
// occupied. The caller reads ActiveWantedSearch to distinguish idempotent
// reuse from a conflicting scope.
var ErrWantedSearchActive = errors.New("wanted search already active")

// WantedSearchRun is the durable progress row. Times use time.Time at the
// boundary even though SQLite stores Unix milliseconds.
type WantedSearchRun struct {
	RunID, Scope, Reason, WantableID, ScopeLabel, Status, Error string
	MediaItemID                                                 int64
	CreatedAt, StartedAt, FinishedAt, CancelRequestedAt         time.Time
	TargetDelay                                                 time.Duration
	Selected, Processed, Searched, Skipped, Failed, Grabbed     int
	Cursor                                                      int
	NextReadyAt                                                 time.Time
	JobID                                                       int64
}

type WantedSearchTarget struct {
	RunID, WantableID, SelectedReason, Label, State, Skipped string
	Ordinal, Seen, Matched, Accepted                         int
	Grabbed, Error                                           string
	FinishedAt                                               time.Time
}

func wantedTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func scanWantedRun(row interface{ Scan(...any) error }) (WantedSearchRun, error) {
	var r WantedSearchRun
	var created, started, finished, cancelled, delay, ready int64
	err := row.Scan(&r.RunID, &r.Scope, &r.Reason, &r.MediaItemID, &r.WantableID,
		&r.ScopeLabel, &r.Status, &created, &started, &finished, &cancelled,
		&delay, &r.Selected, &r.Processed, &r.Searched, &r.Skipped, &r.Failed,
		&r.Grabbed, &r.Cursor, &ready, &r.JobID, &r.Error)
	r.CreatedAt, r.StartedAt = wantedTime(created), wantedTime(started)
	r.FinishedAt, r.CancelRequestedAt = wantedTime(finished), wantedTime(cancelled)
	r.TargetDelay, r.NextReadyAt = time.Duration(delay)*time.Millisecond, wantedTime(ready)
	return r, err
}

const wantedRunColumns = `run_id, scope, reason, media_item_id, wantable_id,
scope_label, status, created_at, started_at, finished_at, cancel_requested_at,
target_delay_ms, selected, processed, searched, skipped, failed, grabbed,
cursor, next_ready_at, job_id, error`

// CreateWantedSearch atomically claims the active slot and snapshots targets.
func (d *DB) CreateWantedSearch(ctx context.Context, r WantedSearchRun, targets []WantedSearchTarget) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO wanted_search_runs (
        run_id, scope, reason, media_item_id, wantable_id, scope_label, status,
        created_at, target_delay_ms, selected, next_ready_at
    ) VALUES (?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?)`,
		r.RunID, r.Scope, r.Reason, r.MediaItemID, r.WantableID, r.ScopeLabel,
		r.CreatedAt.UnixMilli(), r.TargetDelay.Milliseconds(), len(targets), r.CreatedAt.UnixMilli())
	if err != nil {
		if isConstraint(err) {
			return ErrWantedSearchActive
		}
		return err
	}
	for i, target := range targets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO wanted_search_targets
            (run_id, ordinal, wantable_id, selected_reason, label)
            VALUES (?, ?, ?, ?, ?)`, r.RunID, i, target.WantableID,
			target.SelectedReason, target.Label); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) GetWantedSearch(ctx context.Context, runID string) (WantedSearchRun, error) {
	row := d.R.QueryRowContext(ctx, `SELECT `+wantedRunColumns+` FROM wanted_search_runs WHERE run_id = ?`, runID)
	r, err := scanWantedRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

func (d *DB) ActiveWantedSearch(ctx context.Context) (WantedSearchRun, error) {
	row := d.R.QueryRowContext(ctx, `SELECT `+wantedRunColumns+` FROM wanted_search_runs
        WHERE status IN ('queued', 'running') ORDER BY created_at LIMIT 1`)
	r, err := scanWantedRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

func (d *DB) ListActiveWantedSearches(ctx context.Context) ([]WantedSearchRun, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT `+wantedRunColumns+` FROM wanted_search_runs
        WHERE status IN ('queued', 'running') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WantedSearchRun
	for rows.Next() {
		r, err := scanWantedRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) WantedSearchTargetAt(ctx context.Context, runID string, ordinal int) (WantedSearchTarget, error) {
	var t WantedSearchTarget
	var finished int64
	err := d.R.QueryRowContext(ctx, `SELECT run_id, ordinal, wantable_id,
        selected_reason, label, state, skipped, seen, matched, accepted, grabbed,
        error, finished_at FROM wanted_search_targets WHERE run_id = ? AND ordinal = ?`,
		runID, ordinal).Scan(&t.RunID, &t.Ordinal, &t.WantableID, &t.SelectedReason,
		&t.Label, &t.State, &t.Skipped, &t.Seen, &t.Matched, &t.Accepted,
		&t.Grabbed, &t.Error, &finished)
	t.FinishedAt = wantedTime(finished)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

func (d *DB) ListWantedSearchResults(ctx context.Context, runID string, limit, offset int) ([]WantedSearchTarget, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT run_id, ordinal, wantable_id,
        selected_reason, label, state, skipped, seen, matched, accepted, grabbed,
        error, finished_at FROM wanted_search_targets
        WHERE run_id = ? AND state != 'pending' ORDER BY ordinal LIMIT ? OFFSET ?`,
		runID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WantedSearchTarget
	for rows.Next() {
		var t WantedSearchTarget
		var finished int64
		if err := rows.Scan(&t.RunID, &t.Ordinal, &t.WantableID, &t.SelectedReason,
			&t.Label, &t.State, &t.Skipped, &t.Seen, &t.Matched, &t.Accepted,
			&t.Grabbed, &t.Error, &finished); err != nil {
			return nil, err
		}
		t.FinishedAt = wantedTime(finished)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (d *DB) StartWantedSearch(ctx context.Context, runID string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE wanted_search_runs SET status = 'running',
        started_at = CASE WHEN started_at = 0 THEN ? ELSE started_at END
        WHERE run_id = ? AND status = 'queued'`, time.Now().UnixMilli(), runID)
	return err
}

func (d *DB) LinkWantedSearchJob(ctx context.Context, runID string, ordinal int, jobID int64) error {
	res, err := d.W.ExecContext(ctx, `UPDATE wanted_search_runs SET job_id = ?
        WHERE run_id = ? AND cursor = ? AND status IN ('queued', 'running')`, jobID, runID, ordinal)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("wanted search %s no longer owns cursor %d", runID, ordinal)
	}
	return nil
}

// CommitWantedSearchTarget checkpoints exactly one owned ordinal. A retried
// old chunk cannot consume a newer cursor.
func (d *DB) CommitWantedSearchTarget(ctx context.Context, runID string, ordinal int, target WantedSearchTarget, nextReady time.Time) (bool, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE wanted_search_targets SET state = ?,
        skipped = ?, seen = ?, matched = ?, accepted = ?, grabbed = ?, error = ?, finished_at = ?
        WHERE run_id = ? AND ordinal = ? AND state = 'pending'`, target.State,
		target.Skipped, target.Seen, target.Matched, target.Accepted, target.Grabbed,
		target.Error, time.Now().UnixMilli(), runID, ordinal)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	searched, skipped, failed, grabbed := 0, 0, 0, 0
	switch target.State {
	case "searched":
		searched = 1
	case "skipped":
		skipped = 1
	case "failed":
		failed = 1
	}
	if target.Grabbed != "" {
		grabbed = 1
	}
	res, err = tx.ExecContext(ctx, `UPDATE wanted_search_runs SET
        processed = processed + 1, searched = searched + ?, skipped = skipped + ?,
        failed = failed + ?, grabbed = grabbed + ?, cursor = cursor + 1,
        next_ready_at = ?
        WHERE run_id = ? AND cursor = ? AND status IN ('queued', 'running')`,
		searched, skipped, failed, grabbed, nextReady.UnixMilli(), runID, ordinal)
	if err != nil {
		return false, err
	}
	n, _ = res.RowsAffected()
	if n == 0 {
		return false, fmt.Errorf("wanted search %s lost cursor %d", runID, ordinal)
	}
	return true, tx.Commit()
}

func (d *DB) RequestWantedSearchCancel(ctx context.Context, runID string) (WantedSearchRun, error) {
	now := time.Now().UnixMilli()
	_, err := d.W.ExecContext(ctx, `UPDATE wanted_search_runs SET
        cancel_requested_at = CASE WHEN cancel_requested_at = 0 THEN ? ELSE cancel_requested_at END
        WHERE run_id = ? AND status IN ('queued', 'running')`, now, runID)
	if err != nil {
		return WantedSearchRun{}, err
	}
	return d.GetWantedSearch(ctx, runID)
}

func (d *DB) FinishWantedSearch(ctx context.Context, runID, status, message string, cancelRemaining bool) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if cancelRemaining {
		res, err := tx.ExecContext(ctx, `UPDATE wanted_search_targets SET state = 'skipped',
            skipped = 'cancelled', finished_at = ? WHERE run_id = ? AND state = 'pending'`,
			time.Now().UnixMilli(), runID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if _, err := tx.ExecContext(ctx, `UPDATE wanted_search_runs SET processed = processed + ?,
            skipped = skipped + ?, cursor = selected WHERE run_id = ?`, n, n, runID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE wanted_search_runs SET status = ?, error = ?,
        finished_at = ?, job_id = 0 WHERE run_id = ? AND status IN ('queued', 'running')`,
		status, message, time.Now().UnixMilli(), runID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) FindLiveJobByDedupe(ctx context.Context, key string) (domain.Job, error) {
	row := d.R.QueryRowContext(ctx, `SELECT id, kind, payload, state, priority,
        run_after, attempts, max_attempts, last_error, dedupe_key,
        required_capability, affinity_node, lease_owner, lease_expires_at,
        created_at, updated_at, finished_at FROM jobs
        WHERE dedupe_key = ? AND state IN ('queued', 'leased') ORDER BY id DESC LIMIT 1`, key)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return job, ErrNotFound
	}
	return job, err
}

func (d *DB) PruneWantedSearches(ctx context.Context, before time.Time) (int64, error) {
	res, err := d.W.ExecContext(ctx, `DELETE FROM wanted_search_runs
        WHERE status NOT IN ('queued', 'running') AND finished_at > 0 AND finished_at < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
