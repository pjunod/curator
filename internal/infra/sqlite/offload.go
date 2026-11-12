package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Offload (ADR 0028): the durable operation, its per-file journal, and the
// stored offloaded_at state on movies, seasons and episodes.

// ErrOffloadRequestConflict is a request id reused for a different request.
var ErrOffloadRequestConflict = errors.New("offload request id was already used for a different request")

// OffloadOperation is one offload_operations row.
type OffloadOperation struct {
	ID          string          `json:"operationId"`
	RequestID   string          `json:"requestId"`
	Fingerprint string          `json:"-"`
	State       string          `json:"state"`
	ItemID      int64           `json:"itemId"`
	Scope       string          `json:"scope"`
	Plan        json.RawMessage `json:"plan"`
	Result      json.RawMessage `json:"result"`
	FilesTotal  int             `json:"filesTotal"`
	BytesTotal  int64           `json:"bytesTotal"`
	BytesFreed  int64           `json:"bytesFreed"`
	LastError   string          `json:"lastError,omitempty"`
	CreatedAt   int64           `json:"createdAt"`
	UpdatedAt   int64           `json:"updatedAt"`
	DismissedAt int64           `json:"-"`
}

// OffloadOpen reports whether the operation still has work to do.
func OffloadOpen(state string) bool {
	switch state {
	case "deleting", "waiting", "retrying":
		return true
	}
	return false
}

const offloadSelect = `SELECT operation_id,request_id,fingerprint,state,media_item_id,scope,plan,result,files_total,bytes_total,bytes_freed,last_error,created_at,updated_at,dismissed_at FROM offload_operations`

func scanOffload(row rowScanner) (OffloadOperation, error) {
	var op OffloadOperation
	var plan, result string
	err := row.Scan(&op.ID, &op.RequestID, &op.Fingerprint, &op.State, &op.ItemID, &op.Scope, &plan, &result,
		&op.FilesTotal, &op.BytesTotal, &op.BytesFreed, &op.LastError, &op.CreatedAt, &op.UpdatedAt, &op.DismissedAt)
	op.Plan, op.Result = json.RawMessage(plan), json.RawMessage(result)
	return op, err
}

// OffloadFile is one journaled file: a library media file, a sidecar or an
// untracked subtitle beside it.
type OffloadFile struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"` // media | sidecar
	FileID int64  `json:"fileId,omitempty"`
	Size   int64  `json:"size"`
	State  string `json:"state"` // pending | deleted | failed
	Error  string `json:"error,omitempty"`
}

// OffloadRecord is everything the one transaction that starts an Offload
// writes: the operation and its file list, the offloaded state, and the
// operation-keyed sub-deliveries for Cinema.
type OffloadRecord struct {
	Op    OffloadOperation
	Files []OffloadFile
	// Movie sets the movie row's state; Seasons and Episodes the series'.
	Movie    bool
	Seasons  []int
	Episodes []int64
	Outbox   []OutboxWrite
}

// CreateOffloadOperation records an operation and sets what it offloads, in
// one transaction. The same request id with the same fingerprint returns the
// stored operation (existing true); with another it is
// ErrOffloadRequestConflict.
func (d *DB) CreateOffloadOperation(ctx context.Context, rec OffloadRecord) (OffloadOperation, bool, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return rec.Op, false, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := scanOffload(tx.QueryRowContext(ctx, offloadSelect+` WHERE request_id=?`, rec.Op.RequestID))
	if err == nil {
		if stored.Fingerprint != rec.Op.Fingerprint {
			return rec.Op, false, ErrOffloadRequestConflict
		}
		return stored, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return rec.Op, false, err
	}
	now := time.Now().UnixMilli()
	op := rec.Op
	op.CreatedAt, op.UpdatedAt = now, now
	if len(op.Plan) == 0 {
		op.Plan = json.RawMessage(`{}`)
	}
	if len(op.Result) == 0 {
		op.Result = json.RawMessage(`{}`)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO offload_operations(operation_id,request_id,fingerprint,state,media_item_id,scope,plan,result,files_total,bytes_total,bytes_freed,last_error,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,0,'',?,?)`,
		op.ID, op.RequestID, op.Fingerprint, op.State, op.ItemID, op.Scope, string(op.Plan), string(op.Result), op.FilesTotal, op.BytesTotal, now, now); err != nil {
		return op, false, err
	}
	for _, f := range rec.Files {
		if _, err = tx.ExecContext(ctx, `INSERT INTO offload_operation_files(operation_id,path,kind,file_id,size,state) VALUES(?,?,?,?,?,'pending')`,
			op.ID, f.Path, f.Kind, f.FileID, f.Size); err != nil {
			return op, false, err
		}
	}
	if rec.Movie {
		if _, err = tx.ExecContext(ctx, `UPDATE media_items SET offloaded_at=? WHERE id=?`, now, op.ItemID); err != nil {
			return op, false, err
		}
	}
	for _, n := range rec.Seasons {
		if _, err = tx.ExecContext(ctx, `UPDATE seasons SET offloaded_at=? WHERE media_item_id=? AND number=?`, now, op.ItemID, n); err != nil {
			return op, false, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE episodes SET offloaded_at=? WHERE media_item_id=? AND season_number=?`, now, op.ItemID, n); err != nil {
			return op, false, err
		}
	}
	for _, id := range rec.Episodes {
		if _, err = tx.ExecContext(ctx, `UPDATE episodes SET offloaded_at=? WHERE id=? AND media_item_id=?`, now, id, op.ItemID); err != nil {
			return op, false, err
		}
	}
	if err = insertOutboxTx(ctx, tx, rec.Outbox, now); err != nil {
		return op, false, err
	}
	return op, false, tx.Commit()
}

// GetOffloadOperation returns one operation or ErrNotFound.
func (d *DB) GetOffloadOperation(ctx context.Context, id string) (OffloadOperation, error) {
	op, err := scanOffload(d.R.QueryRowContext(ctx, offloadSelect+` WHERE operation_id=?`, id))
	return op, wrapNotFound(err)
}

// OffloadOperationByRequest returns the operation a request id started.
func (d *DB) OffloadOperationByRequest(ctx context.Context, requestID string) (OffloadOperation, error) {
	op, err := scanOffload(d.R.QueryRowContext(ctx, offloadSelect+` WHERE request_id=?`, requestID))
	return op, wrapNotFound(err)
}

// OpenOffloadOperations lists every operation with deletes still to do,
// oldest first: what a restart resumes.
func (d *DB) OpenOffloadOperations(ctx context.Context) ([]OffloadOperation, error) {
	rows, err := d.R.QueryContext(ctx, offloadSelect+` WHERE state IN ('deleting','waiting','retrying') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OffloadOperation
	for rows.Next() {
		op, err := scanOffload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// ListOffloadFiles is an operation's journal in path order.
func (d *DB) ListOffloadFiles(ctx context.Context, id string) ([]OffloadFile, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT path,kind,file_id,size,state,error FROM offload_operation_files WHERE operation_id=? ORDER BY kind, path`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OffloadFile
	for rows.Next() {
		var f OffloadFile
		if err := rows.Scan(&f.Path, &f.Kind, &f.FileID, &f.Size, &f.State, &f.Error); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetOffloadState moves an open operation between its working states
// (deleting, waiting, retrying), recording why. A finished or stopped
// operation never moves again.
func (d *DB) SetOffloadState(ctx context.Context, id, state, lastError string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE offload_operations SET state=?,last_error=?,updated_at=? WHERE operation_id=? AND state IN ('deleting','waiting','retrying')`,
		state, lastError, time.Now().UnixMilli(), id)
	return err
}

// MarkOffloadFile records one file's outcome and, for a deleted file, adds
// its bytes to what the operation freed.
func (d *DB) MarkOffloadFile(ctx context.Context, id, path, state, errMsg string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var size int64
	var was string
	if err := tx.QueryRowContext(ctx, `SELECT size,state FROM offload_operation_files WHERE operation_id=? AND path=?`, id, path).Scan(&size, &was); err != nil {
		return wrapNotFound(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE offload_operation_files SET state=?,error=? WHERE operation_id=? AND path=?`, state, errMsg, id, path); err != nil {
		return err
	}
	if state == "deleted" && was != "deleted" {
		if _, err := tx.ExecContext(ctx, `UPDATE offload_operations SET bytes_freed=bytes_freed+?,updated_at=? WHERE operation_id=?`, size, time.Now().UnixMilli(), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CompleteOffload ends an operation with its result and writes the
// `offloaded` history event in the same transaction.
func (d *DB) CompleteOffload(ctx context.Context, id string, result json.RawMessage, h HistoryWrite) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE offload_operations SET state='complete',last_error='',result=?,updated_at=? WHERE operation_id=? AND state IN ('deleting','waiting','retrying')`, string(result), now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return tx.Commit() // already ended: nothing more to write
	}
	// The files are gone: Cinema is told now, not before (a path it still
	// finds would be answered present).
	if _, err := tx.ExecContext(ctx, `UPDATE notifier_deliveries SET next_at=?,updated_at=? WHERE operation_id=? AND status='pending' AND next_at>=?`, now, now, id, holdUntil); err != nil {
		return err
	}
	if err := insertHistoryTx(ctx, d.Write.WithTx(tx), h, now); err != nil {
		return err
	}
	return tx.Commit()
}

// StopOffload ends an open operation as cancelled and reports whether it
// was open (false: already ended).
func (d *DB) StopOffload(ctx context.Context, id string, result json.RawMessage) (bool, error) {
	res, err := d.W.ExecContext(ctx, `UPDATE offload_operations SET state='cancelled',last_error='',result=?,updated_at=? WHERE operation_id=? AND state IN ('deleting','waiting','retrying')`,
		string(result), time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// FailOffload ends an operation that cannot run (an unreadable plan).
func (d *DB) FailOffload(ctx context.Context, id, reason string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE offload_operations SET state='failed',last_error=?,updated_at=? WHERE operation_id=? AND state IN ('deleting','waiting','retrying')`, reason, time.Now().UnixMilli(), id)
	return err
}

// OpenOffloadForItem is the newest operation with deletes still to do on the
// item ("" when there is none).
func (d *DB) OpenOffloadForItem(ctx context.Context, itemID int64) (string, error) {
	var id string
	err := d.R.QueryRowContext(ctx, `SELECT operation_id FROM offload_operations WHERE media_item_id=? AND state IN ('deleting','waiting','retrying') ORDER BY created_at DESC LIMIT 1`, itemID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// LatestEndedOffloadForItem is the newest finished operation (complete,
// cancelled or failed) on the item, not dismissed, ended since `since`.
func (d *DB) LatestEndedOffloadForItem(ctx context.Context, itemID, since int64) (OffloadOperation, bool, error) {
	op, err := scanOffload(d.R.QueryRowContext(ctx, offloadSelect+` WHERE media_item_id=? AND state IN ('complete','cancelled','failed') AND dismissed_at=0 AND updated_at>=? ORDER BY created_at DESC LIMIT 1`, itemID, since))
	if errors.Is(err, sql.ErrNoRows) {
		return OffloadOperation{}, false, nil
	}
	return op, err == nil, err
}

// DismissOffload hides a finished operation from the status panel.
func (d *DB) DismissOffload(ctx context.Context, id string) (bool, error) {
	res, err := d.W.ExecContext(ctx, `UPDATE offload_operations SET dismissed_at=? WHERE operation_id=? AND state IN ('complete','cancelled','failed')`, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// OffloadedScope is the offloaded state inside one item that a Re-acquire (or
// the status panel) reads: the movie row, seasons and episodes still offloaded.
type OffloadedScope struct {
	Movie    bool
	Seasons  []int
	Episodes []int64
}

// ClearOffloaded clears the offloaded state inside a scope and monitors what
// it clears (Re-acquire, ADR 0028). Scope: movie (the movie row), whole (every
// season and episode of the series), season (that season and its episodes),
// episode (that episode). It returns what it cleared.
func (d *DB) ClearOffloaded(ctx context.Context, itemID int64, scope string, season int, episodeID int64) (OffloadedScope, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return OffloadedScope{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var out OffloadedScope
	collect := func(q string, args ...any) ([]int64, error) {
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, rows.Err()
	}
	switch scope {
	case "movie":
		res, err := tx.ExecContext(ctx, `UPDATE media_items SET offloaded_at=0,monitored=1 WHERE id=? AND offloaded_at!=0`, itemID)
		if err != nil {
			return out, err
		}
		n, _ := res.RowsAffected()
		out.Movie = n == 1
		if !out.Movie {
			if _, err := tx.ExecContext(ctx, `UPDATE media_items SET monitored=1 WHERE id=?`, itemID); err != nil {
				return out, err
			}
		}
	case "whole", "season":
		cond, args := ``, []any{itemID}
		if scope == "season" {
			cond, args = ` AND season_number=?`, append(args, season)
		}
		eps, err := collect(`SELECT id FROM episodes WHERE media_item_id=? AND offloaded_at!=0`+cond, args...)
		if err != nil {
			return out, err
		}
		out.Episodes = eps
		scond := ``
		if scope == "season" {
			scond = ` AND number=?`
		}
		rows, err := tx.QueryContext(ctx, `SELECT number FROM seasons WHERE media_item_id=? AND offloaded_at!=0`+scond, args...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var n int
			if err := rows.Scan(&n); err != nil {
				_ = rows.Close()
				return out, err
			}
			out.Seasons = append(out.Seasons, n)
		}
		if err := rows.Close(); err != nil {
			return out, err
		}
		// Only what was offloaded comes back, and comes back monitored; a
		// special (season 0) stays as the person had it.
		if _, err := tx.ExecContext(ctx, `UPDATE episodes SET offloaded_at=0,monitored=CASE WHEN season_number=0 THEN monitored ELSE 1 END WHERE media_item_id=? AND offloaded_at!=0`+cond, args...); err != nil {
			return out, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE seasons SET offloaded_at=0 WHERE media_item_id=? AND offloaded_at!=0`+scond, args...); err != nil {
			return out, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE seasons SET monitored=1 WHERE media_item_id=? AND number!=0`+scond+` AND (number IN (SELECT season_number FROM episodes WHERE media_item_id=seasons.media_item_id AND id IN (`+idList(out.Episodes)+`)) OR number IN (`+intList(out.Seasons)+`))`, args...); err != nil {
			return out, err
		}
		if len(out.Episodes) > 0 || len(out.Seasons) > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE media_items SET monitored=1 WHERE id=?`, itemID); err != nil {
				return out, err
			}
		}
	case "episode":
		var sn int
		if err := tx.QueryRowContext(ctx, `SELECT season_number FROM episodes WHERE id=? AND media_item_id=?`, episodeID, itemID).Scan(&sn); err != nil {
			return out, wrapNotFound(err)
		}
		res, err := tx.ExecContext(ctx, `UPDATE episodes SET offloaded_at=0,monitored=1 WHERE id=?`, episodeID)
		if err != nil {
			return out, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			out.Episodes = []int64{episodeID}
		}
		// The season keeps its own flag until no episode of it is offloaded.
		if _, err := tx.ExecContext(ctx, `UPDATE seasons SET offloaded_at=0 WHERE media_item_id=? AND number=? AND offloaded_at!=0
			AND NOT EXISTS (SELECT 1 FROM episodes e WHERE e.media_item_id=seasons.media_item_id AND e.season_number=seasons.number AND e.offloaded_at!=0)`, itemID, sn); err != nil {
			return out, err
		}
		if sn != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE seasons SET monitored=1 WHERE media_item_id=? AND number=?`, itemID, sn); err != nil {
				return out, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE media_items SET monitored=1 WHERE id=?`, itemID); err != nil {
			return out, err
		}
	default:
		return out, fmt.Errorf("unknown offload scope %q", scope)
	}
	return out, tx.Commit()
}

// ClearOffloadedWithFiles clears the offloaded state of every wantable of an
// operation's scope that still has a file (Stop: files not yet deleted stay
// on disk, so those wantables are not offloaded).
func (d *DB) ClearOffloadedWithFiles(ctx context.Context, itemID int64) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`UPDATE media_items SET offloaded_at=0 WHERE id=? AND offloaded_at!=0 AND EXISTS (SELECT 1 FROM media_files f WHERE f.media_item_id=media_items.id)`,
		`UPDATE episodes SET offloaded_at=0 WHERE media_item_id=? AND offloaded_at!=0 AND EXISTS (SELECT 1 FROM media_file_episodes l WHERE l.episode_id=episodes.id)`,
		`UPDATE seasons SET offloaded_at=0 WHERE media_item_id=? AND offloaded_at!=0
			AND NOT EXISTS (SELECT 1 FROM episodes e WHERE e.media_item_id=seasons.media_item_id AND e.season_number=seasons.number AND e.offloaded_at!=0)`,
	} {
		if _, err := tx.ExecContext(ctx, q, itemID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OffloadedItems lists the item ids that have any offloaded state: the movie
// row, a season or an episode (the library filter and pills read this).
func (d *DB) OffloadedItems(ctx context.Context) (map[int64]bool, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id FROM media_items WHERE offloaded_at!=0
		UNION SELECT media_item_id FROM seasons WHERE offloaded_at!=0
		UNION SELECT media_item_id FROM episodes WHERE offloaded_at!=0`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// CancelOffloadDeliveries ends the sub-deliveries of operations (by id) that
// Cinema has not confirmed: they are marked failed with a plain reason, so
// they are never sent or retried.
func (d *DB) CancelOffloadDeliveries(ctx context.Context, operationIDs []string, why string) (int64, error) {
	if len(operationIDs) == 0 {
		return 0, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(operationIDs)), ",")
	args := []any{why, time.Now().UnixMilli()}
	for _, id := range operationIDs {
		args = append(args, id)
	}
	res, err := d.W.ExecContext(ctx, `UPDATE notifier_deliveries SET status='failed',last_error=?,next_at=0,updated_at=? WHERE event IN ('offload','moved') AND status='pending' AND operation_id IN (`+marks+`)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PendingOffloadOperationsForItem lists the operations on an item that still
// have an unconfirmed Cinema sub-delivery, with their plans (Re-acquire cancels
// the ones that overlap its scope).
func (d *DB) PendingOffloadOperationsForItem(ctx context.Context, itemID int64) ([]OffloadOperation, error) {
	rows, err := d.R.QueryContext(ctx, offloadSelect+` WHERE media_item_id=? AND operation_id IN (SELECT operation_id FROM notifier_deliveries WHERE operation_id!='' AND status='pending' AND event IN ('offload','moved'))`, itemID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []OffloadOperation
	for rows.Next() {
		op, err := scanOffload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// RewriteOffloadOutbox replaces an operation's sub-deliveries after Stop: the
// rows in `keep` are refreshed (same key: a new attempt with the narrower
// path list), every other unconfirmed row of the operation is cancelled.
func (d *DB) RewriteOffloadOutbox(ctx context.Context, operationID string, keep []OutboxWrite) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE notifier_deliveries SET status='failed',last_error='cancelled: the offload was stopped',next_at=0,updated_at=? WHERE operation_id=? AND status='pending'`, now, operationID); err != nil {
		return err
	}
	for i := range keep {
		keep[i].Hold = false
	}
	if err := insertOutboxTx(ctx, tx, keep, now); err != nil {
		return err
	}
	return tx.Commit()
}

func idList(ids []int64) string {
	if len(ids) == 0 {
		return "-1"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return strings.Join(parts, ",")
}

func intList(ns []int) string {
	if len(ns) == 0 {
		return "-1"
	}
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, ",")
}
