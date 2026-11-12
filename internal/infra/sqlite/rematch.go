package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Re-match (ADR 0027): the operation record, its journaled file moves and
// the one transaction that commits them.

// ErrRematchRequestConflict is a request id reused for a different request.
var ErrRematchRequestConflict = errors.New("rematch request id was already used for a different request")

// ErrRematchStale is a commit whose files no longer stand where the plan saw
// them.
var ErrRematchStale = errors.New("rematch: a file changed since the plan was made")

// RematchOperation is one rematch_operations row.
type RematchOperation struct {
	ID           string          `json:"operationId"`
	RequestID    string          `json:"requestId"`
	Fingerprint  string          `json:"-"`
	State        string          `json:"state"`
	SourceItemID int64           `json:"sourceItemId"`
	TargetItemID int64           `json:"targetItemId"`
	Plan         json.RawMessage `json:"plan"`
	Result       json.RawMessage `json:"result"`
	BytesDone    int64           `json:"bytesDone"`
	BytesTotal   int64           `json:"bytesTotal"`
	LastError    string          `json:"lastError,omitempty"`
	CreatedAt    int64           `json:"createdAt"`
	UpdatedAt    int64           `json:"updatedAt"`
}

const rematchSelect = `SELECT operation_id,request_id,fingerprint,state,source_item_id,target_item_id,plan,result,bytes_done,bytes_total,last_error,created_at,updated_at FROM rematch_operations`

func scanRematch(row rowScanner) (RematchOperation, error) {
	var op RematchOperation
	var plan, result string
	err := row.Scan(&op.ID, &op.RequestID, &op.Fingerprint, &op.State, &op.SourceItemID, &op.TargetItemID, &plan, &result,
		&op.BytesDone, &op.BytesTotal, &op.LastError, &op.CreatedAt, &op.UpdatedAt)
	op.Plan, op.Result = json.RawMessage(plan), json.RawMessage(result)
	return op, err
}

// RematchMove is one journaled file move, kept in import_placements as kind
// rematch_move. Its states never overlap the import's (prepared, committed,
// retained), so no import path reads it.
type RematchMove struct {
	ID          string      `json:"id"`
	Kind        string      `json:"kind"` // rematch_move
	OperationID string      `json:"operation_id"`
	FileID      int64       `json:"file_id"`
	ItemID      int64       `json:"item_id"`
	Source      string      `json:"source"`
	Target      string      `json:"target"`
	Temporary   string      `json:"temporary,omitempty"`
	Mode        string      `json:"mode"` // rename | copy | relink
	Identity    string      `json:"source_identity"`
	Size        int64       `json:"size"`
	SHA256      string      `json:"sha256,omitempty"`
	Sidecars    [][2]string `json:"sidecars,omitempty"`
	// Replace says the move replaces the target's files for this copy (and
	// episodes) once it is committed, by import's removeExistingFiles.
	Replace    bool    `json:"replace,omitempty"`
	CopyID     int64   `json:"copy_id,omitempty"`
	EpisodeIDs []int64 `json:"episode_ids,omitempty"`
	ReplaceIDs []int64 `json:"replace_ids,omitempty"`
	State      string  `json:"state"` // rematch_planned | rematch_moved | rematch_committed | rematch_done | rematch_rolled_back | rematch_failed
	Error      string  `json:"error,omitempty"`
}

// CreateRematchOperation records an operation and its planned moves. The same
// request id with the same fingerprint returns the stored operation (existing
// true); with another fingerprint it is ErrRematchRequestConflict.
func (d *DB) CreateRematchOperation(ctx context.Context, op RematchOperation, moves []RematchMove) (RematchOperation, bool, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return op, false, err
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := scanRematch(tx.QueryRowContext(ctx, rematchSelect+` WHERE request_id=?`, op.RequestID))
	if err == nil {
		if stored.Fingerprint != op.Fingerprint {
			return op, false, ErrRematchRequestConflict
		}
		return stored, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return op, false, err
	}
	now := time.Now().UnixMilli()
	op.CreatedAt, op.UpdatedAt = now, now
	if len(op.Plan) == 0 {
		op.Plan = json.RawMessage(`{}`)
	}
	if len(op.Result) == 0 {
		op.Result = json.RawMessage(`{}`)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO rematch_operations(operation_id,request_id,fingerprint,state,source_item_id,target_item_id,plan,result,bytes_done,bytes_total,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,0,?,'',?,?)`,
		op.ID, op.RequestID, op.Fingerprint, op.State, op.SourceItemID, op.TargetItemID, string(op.Plan), string(op.Result), op.BytesTotal, now, now); err != nil {
		return op, false, err
	}
	for _, m := range moves {
		if err = putRematchMove(ctx, tx, m); err != nil {
			return op, false, err
		}
	}
	return op, false, tx.Commit()
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func putRematchMove(ctx context.Context, x execer, m RematchMove) error {
	m.Kind = "rematch_move"
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = x.ExecContext(ctx, `INSERT INTO import_placements(id,target,state,data,updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET target=excluded.target,state=excluded.state,data=excluded.data,updated_at=excluded.updated_at`,
		m.ID, m.Target, m.State, string(raw), time.Now().UnixMilli())
	return err
}

// PutRematchMove writes one journaled move as it now stands.
func (d *DB) PutRematchMove(ctx context.Context, m RematchMove) error {
	return putRematchMove(ctx, d.W, m)
}

// ListRematchMoves returns an operation's journaled moves in plan order.
func (d *DB) ListRematchMoves(ctx context.Context, operationID string) ([]RematchMove, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT data FROM import_placements WHERE json_extract(data,'$.kind')='rematch_move' AND json_extract(data,'$.operation_id')=? ORDER BY id`, operationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RematchMove
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var m RematchMove
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetRematchOperation returns one operation or ErrNotFound.
func (d *DB) GetRematchOperation(ctx context.Context, id string) (RematchOperation, error) {
	op, err := scanRematch(d.R.QueryRowContext(ctx, rematchSelect+` WHERE operation_id=?`, id))
	return op, wrapNotFound(err)
}

// OpenRematchOperations lists every operation not yet complete, cancelled or
// failed, oldest first: what a restart resumes.
func (d *DB) OpenRematchOperations(ctx context.Context) ([]RematchOperation, error) {
	rows, err := d.R.QueryContext(ctx, rematchSelect+` WHERE state NOT IN ('complete','cancelled','failed') ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RematchOperation
	for rows.Next() {
		op, err := scanRematch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// SetRematchState moves an operation on. A cancelled or failed operation
// never moves again.
func (d *DB) SetRematchState(ctx context.Context, id, state, lastError string, result json.RawMessage) error {
	q := `UPDATE rematch_operations SET state=?,last_error=?,updated_at=?`
	args := []any{state, lastError, time.Now().UnixMilli()}
	if len(result) > 0 {
		q += `,result=?`
		args = append(args, string(result))
	}
	q += ` WHERE operation_id=? AND state NOT IN ('cancelled','failed','complete')`
	if state == "moving" || state == "moved" {
		// Stop wins over progress: a cancel_pending operation is rolled back.
		q += ` AND state != 'cancel_pending'`
	}
	args = append(args, id)
	_, err := d.W.ExecContext(ctx, q, args...)
	return err
}

// RequestRematchCancel marks an operation cancel_pending while it has not yet
// committed; reports false when it is past that point (or unknown).
func (d *DB) RequestRematchCancel(ctx context.Context, id string) (bool, error) {
	res, err := d.W.ExecContext(ctx, `UPDATE rematch_operations SET state='cancel_pending',updated_at=? WHERE operation_id=? AND state IN ('planned','moving','moved')`, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetRematchError records why an operation is not moving on (waiting for a
// title, a file it could not remove) without changing its state; "" clears it.
func (d *DB) SetRematchError(ctx context.Context, id, msg string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE rematch_operations SET last_error=?,updated_at=? WHERE operation_id=? AND last_error!=?`, msg, time.Now().UnixMilli(), id, msg)
	return err
}

// PruneRematchMoves drops a finished operation's journal rows; the operation
// row keeps the result.
func (d *DB) PruneRematchMoves(ctx context.Context, operationID string) error {
	_, err := d.W.ExecContext(ctx, `DELETE FROM import_placements WHERE json_extract(data,'$.kind')='rematch_move' AND json_extract(data,'$.operation_id')=?`, operationID)
	return err
}

// OpenRematchScope is what a library scan leaves alone while a Re-match
// runs: the titles of every open operation and the folders it chose.
func (d *DB) OpenRematchScope(ctx context.Context) (map[int64]bool, []string, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT source_item_id,target_item_id,COALESCE(json_extract(plan,'$.targetPath'),'') FROM rematch_operations WHERE state NOT IN ('complete','cancelled','failed')`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	items := map[int64]bool{}
	var folders []string
	for rows.Next() {
		var src, dst int64
		var folder string
		if err := rows.Scan(&src, &dst, &folder); err != nil {
			return nil, nil, err
		}
		items[src], items[dst] = true, true
		if folder != "" {
			folders = append(folders, folder)
		}
	}
	return items, folders, rows.Err()
}

// OpenRematchForItem is the newest operation not yet finished that moves
// files out of or into the item ("" when there is none).
func (d *DB) OpenRematchForItem(ctx context.Context, itemID int64) (string, error) {
	var id string
	err := d.R.QueryRowContext(ctx, `SELECT operation_id FROM rematch_operations WHERE (source_item_id=? OR target_item_id=?) AND state NOT IN ('complete','cancelled','failed') ORDER BY created_at DESC LIMIT 1`, itemID, itemID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// LatestEndedRematchForItem is the newest finished operation (complete,
// cancelled or failed) that moved files out of or into the item, not
// dismissed and ended since `since` (unix ms).
func (d *DB) LatestEndedRematchForItem(ctx context.Context, itemID, since int64) (RematchOperation, bool, error) {
	op, err := scanRematch(d.R.QueryRowContext(ctx, rematchSelect+` WHERE (source_item_id=? OR target_item_id=?) AND state IN ('complete','cancelled','failed') AND dismissed_at=0 AND updated_at>=? ORDER BY created_at DESC LIMIT 1`, itemID, itemID, since))
	if errors.Is(err, sql.ErrNoRows) {
		return RematchOperation{}, false, nil
	}
	return op, err == nil, err
}

// DismissRematch hides a finished operation from the status panel.
func (d *DB) DismissRematch(ctx context.Context, id string) (bool, error) {
	res, err := d.W.ExecContext(ctx, `UPDATE rematch_operations SET dismissed_at=? WHERE operation_id=? AND state IN ('complete','cancelled','failed')`, time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetRematchProgress records bytes copied so far.
func (d *DB) SetRematchProgress(ctx context.Context, id string, done int64) error {
	_, err := d.W.ExecContext(ctx, `UPDATE rematch_operations SET bytes_done=?,updated_at=? WHERE operation_id=?`, done, time.Now().UnixMilli(), id)
	return err
}

// ItemRevision is a title's lifecycle_item_revision (0 when it has none).
func (d *DB) ItemRevision(ctx context.Context, itemID int64) (int64, error) {
	var rev int64
	err := d.R.QueryRowContext(ctx, `SELECT revision FROM lifecycle_item_revision WHERE media_item_id=?`, itemID).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rev, err
}

// SetMediaItemOrigin records who created an item (user | adopted).
func (d *DB) SetMediaItemOrigin(ctx context.Context, itemID int64, origin string) error {
	_, err := d.W.ExecContext(ctx, `UPDATE media_items SET origin=? WHERE id=?`, origin, itemID)
	return err
}

// MediaItemOrigin returns user | adopted.
func (d *DB) MediaItemOrigin(ctx context.Context, itemID int64) (string, error) {
	var origin string
	err := d.R.QueryRowContext(ctx, `SELECT origin FROM media_items WHERE id=?`, itemID).Scan(&origin)
	return origin, wrapNotFound(err)
}

// RematchBlocker is work in flight that a Re-match must not race.
type RematchBlocker struct {
	ItemID int64
	Reason string // download | import | replacement | rematch
	Ref    string
	State  string
}

// RematchBlockers lists what keeps a Re-match of these items from running:
// a download not yet terminal, a running import attempt, a replacement
// operation still open, another Re-match still open.
func (d *DB) RematchBlockers(ctx context.Context, itemIDs ...int64) ([]RematchBlocker, error) {
	var out []RematchBlocker
	for _, id := range itemIDs {
		rows, err := d.R.QueryContext(ctx, `
			SELECT 'download', CAST(id AS TEXT), state FROM downloads
			 WHERE media_item_id=? AND state NOT IN ('imported','notified','failed')
			UNION ALL
			SELECT 'import', CAST(c.download_id AS TEXT), c.outcome FROM import_attempt_controls c JOIN downloads d ON d.id=c.download_id
			 WHERE d.media_item_id=? AND c.outcome='pending'
			UNION ALL
			SELECT 'replacement', operation_id, state FROM replacement_operations
			 WHERE media_item_id=? AND state NOT IN ('complete','cancelled','failed')
			UNION ALL
			SELECT 'rematch', operation_id, state FROM rematch_operations
			 WHERE (source_item_id=? OR target_item_id=?) AND state NOT IN ('complete','cancelled','failed')`, id, id, id, id, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			b := RematchBlocker{ItemID: id}
			if err := rows.Scan(&b.Reason, &b.Ref, &b.State); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, b)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ItemDownloadCount is how many download rows an item has, in any state.
func (d *DB) ItemDownloadCount(ctx context.Context, itemID int64) (int, error) {
	var n int
	err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads WHERE media_item_id=?`, itemID).Scan(&n)
	return n, err
}

// RematchFileCommit is one moved file's new place in the library.
type RematchFileCommit struct {
	FileID       int64
	OldPath      string
	NewPath      string
	TargetItemID int64
	CopyID       int64
	EpisodeIDs   []int64
	Sidecars     [][2]string
}

// RematchBlocklist is one release blocklisted for the item a file left,
// recorded as a replacement operation (intent rematch) already complete.
type RematchBlocklist struct {
	OperationID      string
	MediaItemID      int64
	CopyID           int64
	Scope            string // episodes | download
	EpisodeIDs       []int64
	SourceGeneration string
	SourceTransfer   string
	SourceRelease    string
	SourceIndexer    string
	SourceGUID       string
	SourceIndexerID  int64
	Reason           string
}

// RematchCommit is everything a Re-match commits in one transaction.
type RematchCommit struct {
	OperationID  string
	SourceItemID int64
	TargetItemID int64
	TargetPath   string // "" leaves the target's folder as it is
	Files        []RematchFileCommit
	LeftOut      []int64 // file rows removed from the library (bytes stay)
	Blocklist    []RematchBlocklist
	RemoveSource bool
	History      []HistoryWrite
	Outbox       []OutboxWrite
	Result       json.RawMessage
}

// CommitRematch moves the file rows and their episode links to the target,
// removes left-out rows, writes the blocklist, history and Cinema outbox,
// removes an emptied adopted source and marks the operation committed, all at
// once. A file row that no longer stands at its planned old path refuses the
// whole commit (ErrRematchStale).
func (d *DB) CommitRematch(ctx context.Context, c RematchCommit) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := d.Write.WithTx(tx)
	now := time.Now().UnixMilli()
	if c.TargetPath != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE media_items SET path=?,updated_at=? WHERE id=?`, c.TargetPath, now, c.TargetItemID); err != nil {
			if isPathConstraint(err) {
				return fmt.Errorf("%w: %s", ErrFolderTaken, c.TargetPath)
			}
			return err
		}
	}
	for _, f := range c.Files {
		var copyID any
		if f.CopyID != 0 {
			copyID = f.CopyID
		}
		res, err := tx.ExecContext(ctx, `UPDATE media_files SET path=?,media_item_id=?,copy_id=? WHERE id=? AND path=?`, f.NewPath, f.TargetItemID, copyID, f.FileID, f.OldPath)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("%w: file %d is no longer at %s", ErrRematchStale, f.FileID, f.OldPath)
		}
		if err := q.ClearFileEpisodeLinks(ctx, f.FileID); err != nil {
			return err
		}
		for _, ep := range f.EpisodeIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO media_file_episodes(media_file_id,episode_id) VALUES(?,?)`, f.FileID, ep); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE media_file_placements SET path=? WHERE media_file_id=? AND path=?`, f.NewPath, f.FileID, f.OldPath); err != nil {
			return err
		}
		for _, pair := range append([][2]string{{f.OldPath, f.NewPath}}, f.Sidecars...) {
			if _, err := tx.ExecContext(ctx, `UPDATE publication_files SET library_path=?,updated_at=? WHERE library_path=? AND outcome='placed'`, pair[1], now, pair[0]); err != nil {
				return err
			}
		}
	}
	for _, id := range c.LeftOut {
		if _, err := tx.ExecContext(ctx, `DELETE FROM media_files WHERE id=?`, id); err != nil {
			return err
		}
	}
	for _, b := range c.Blocklist {
		eps, _ := json.Marshal(b.EpisodeIDs)
		if b.EpisodeIDs == nil {
			eps = []byte("[]")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO replacement_operations(operation_id,request_id,fingerprint,state,intent,scope,media_item_id,copy_id,source_generation,source_transfer,source_release,source_indexer,source_guid,source_indexer_id,blacklist_source_release,reason_code,reason,episode_ids,result,last_error,created_at,updated_at)
			VALUES(?,?,?,'complete','rematch',?,?,?,?,?,?,?,?,?,1,'rematch',?,?,?,'',?,?) ON CONFLICT(operation_id) DO NOTHING`,
			b.OperationID, b.OperationID, c.OperationID, b.Scope, b.MediaItemID, b.CopyID, b.SourceGeneration, b.SourceTransfer, b.SourceRelease, b.SourceIndexer,
			b.SourceGUID, b.SourceIndexerID, b.Reason, string(eps), `{"rematch":"`+c.OperationID+`"}`, now, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO release_blacklist_identity(media_item_id,guid,indexer_id,reason,added_at) VALUES(?,?,?,?,?) ON CONFLICT(media_item_id,indexer_id,guid) DO UPDATE SET reason=excluded.reason`,
			b.MediaItemID, b.SourceGUID, b.SourceIndexerID, b.Reason, now); err != nil {
			return err
		}
	}
	for _, h := range c.History {
		if err := insertHistoryTx(ctx, q, h, now); err != nil {
			return err
		}
	}
	if err := insertOutboxTx(ctx, tx, c.Outbox, now); err != nil {
		return err
	}
	if c.RemoveSource && c.SourceItemID != c.TargetItemID {
		if err := q.DeleteMediaItem(ctx, c.SourceItemID); err != nil {
			return err
		}
		if _, err := q.BumpIdentityRevision(ctx); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE import_placements SET state='rematch_committed',data=json_set(data,'$.state','rematch_committed'),updated_at=? WHERE json_extract(data,'$.kind')='rematch_move' AND json_extract(data,'$.operation_id')=? AND state='rematch_moved'`, now, c.OperationID); err != nil {
		return err
	}
	result := "{}"
	if len(c.Result) > 0 {
		result = string(c.Result)
	}
	res, err := tx.ExecContext(ctx, `UPDATE rematch_operations SET state='committed',last_error='',target_item_id=?,result=?,updated_at=? WHERE operation_id=? AND state IN ('moving','moved')`, c.TargetItemID, result, now, c.OperationID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: operation %s is no longer moving", ErrRematchStale, c.OperationID)
	}
	return tx.Commit()
}

// OperationDeliveries returns every notifier delivery a Re-match wrote.
func (d *DB) OperationDeliveries(ctx context.Context, operationID string) ([]Delivery, error) {
	rows, err := d.Read.ListOperationDeliveries(ctx, operationID)
	if err != nil {
		return nil, err
	}
	out := make([]Delivery, 0, len(rows))
	for _, r := range rows {
		out = append(out, deliveryFrom(r))
	}
	return out, nil
}
