package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

// DeliveryReceipt is Curator's durable record of one immutable Runner
// publication. It intentionally outlives the Activity/download row.
type DeliveryReceipt struct {
	ID                           int64
	ClientID                     int64
	Delivery                     ports.RunnerDelivery
	MappedPath                   string
	PublicationDirectoryIdentity string
	State                        string
	AckState                     string
	ConsumerReceiptID            string
	DownloadID                   int64
	GenerationKey                string
	DownloadHandle               string
	ReadyAt                      time.Time
	MediaItemID                  int64
	CopyID                       int64
	Error                        string
	Attempts                     int
	BlockedSince                 time.Time
	NextAttemptAt                time.Time
}

// DeliveryFileOutcome keeps the reason a manifest file is retainable or
// disposable separate from whether Curator has removed its completed copy.
// Path is the full manifest-relative identity, never a basename.
type DeliveryFileOutcome struct {
	ReceiptID            int64
	Path                 string
	SizeBytes            int64
	SHA256               string
	FileGeneration       string
	Disposition          string
	CleanupState         string
	Reason               string
	LibraryPath          string
	ManualAdditional     bool
	CleanupError         string
	CleanupAttempts      int
	CleanupBlockedSince  time.Time
	CleanupNextAttemptAt time.Time
	UpdatedAt            time.Time
}

type DeliveryManifestFile struct {
	Path           string
	SizeBytes      int64
	SHA256         string
	FileGeneration string
}

// ExplicitDeliveryDeleteBatch is the exact user-reviewed selection for one
// immutable publication. It is committed atomically before the first unlink.
type ExplicitDeliveryDeleteBatch struct {
	ID                           string
	ReceiptID                    int64
	PayloadPath                  string
	InventoryFingerprint         string
	PublicationDirectoryIdentity string
	StorageIdentity              string
	GenerationKey                string
	RequestToken                 string
	PublicationGeneration        string
	RetryAttempts                int
	RetryNextAttemptAt           time.Time
	RetryBlockedSince            time.Time
	RetryError                   string
	Files                        []ExplicitDeliveryDeleteFile
}

type ExplicitDeliveryDeleteFile struct {
	ReviewedPath     string
	Path             string
	SnapshotIdentity string
	ParentIdentity   string
	HistoryRecorded  bool
	CompletedAt      time.Time
}

func (d *DB) PrepareExplicitDeliveryDeleteBatch(ctx context.Context, batch ExplicitDeliveryDeleteBatch) error {
	if batch.ID == "" || batch.ReceiptID <= 0 || batch.PayloadPath == "" || batch.InventoryFingerprint == "" ||
		batch.PublicationDirectoryIdentity == "" || batch.StorageIdentity == "" || batch.GenerationKey == "" ||
		batch.RequestToken == "" || batch.PublicationGeneration == "" || len(batch.Files) == 0 {
		return fmt.Errorf("invalid explicit delivery deletion batch")
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var receiptID int64
	var payloadPath, fingerprint, publicationIdentity, storageIdentity, generationKey, requestToken, publicationGeneration string
	created := false
	err = tx.QueryRowContext(ctx, `SELECT receipt_id,payload_path,inventory_fingerprint,publication_directory_identity,storage_identity,generation_key,request_token,publication_generation
		FROM runner_delivery_explicit_delete_batches WHERE batch_id=?`, batch.ID).
		Scan(&receiptID, &payloadPath, &fingerprint, &publicationIdentity, &storageIdentity, &generationKey, &requestToken, &publicationGeneration)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO runner_delivery_explicit_delete_batches
			(batch_id,receipt_id,payload_path,inventory_fingerprint,publication_directory_identity,storage_identity,generation_key,request_token,publication_generation,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?)`, batch.ID, batch.ReceiptID, batch.PayloadPath, batch.InventoryFingerprint,
			batch.PublicationDirectoryIdentity, batch.StorageIdentity, batch.GenerationKey, batch.RequestToken,
			batch.PublicationGeneration, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		created = true
	} else if receiptID != batch.ReceiptID || payloadPath != batch.PayloadPath || fingerprint != batch.InventoryFingerprint ||
		publicationIdentity != batch.PublicationDirectoryIdentity || storageIdentity != batch.StorageIdentity ||
		generationKey != batch.GenerationKey || requestToken != batch.RequestToken || publicationGeneration != batch.PublicationGeneration {
		return ErrDeliveryReceiptStale
	}
	if !created {
		var existingCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_explicit_delete_files WHERE batch_id=?`, batch.ID).Scan(&existingCount); err != nil {
			return err
		}
		if existingCount != len(batch.Files) {
			return ErrDeliveryReceiptStale
		}
	}
	seen := make(map[string]bool, len(batch.Files))
	for _, file := range batch.Files {
		if file.Path == "" || file.ReviewedPath == "" || file.SnapshotIdentity == "" || file.ParentIdentity == "" || seen[file.Path] {
			return fmt.Errorf("invalid or duplicate explicit delivery deletion path")
		}
		seen[file.Path] = true
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO runner_delivery_explicit_delete_files(batch_id,path,reviewed_path,snapshot_identity,parent_identity) VALUES(?,?,?,?,?)`, batch.ID, file.Path, file.ReviewedPath, file.SnapshotIdentity, file.ParentIdentity)
		if err != nil {
			return err
		}
		var savedPath, saved, savedParent string
		if err := tx.QueryRowContext(ctx, `SELECT reviewed_path,snapshot_identity,parent_identity FROM runner_delivery_explicit_delete_files WHERE batch_id=? AND path=?`, batch.ID, file.Path).Scan(&savedPath, &saved, &savedParent); err != nil {
			return err
		}
		if savedPath != file.ReviewedPath || saved != file.SnapshotIdentity || savedParent != file.ParentIdentity {
			return ErrDeliveryReceiptStale
		}
		var generation, cleanupState string
		if err := tx.QueryRowContext(ctx, `SELECT file_generation,cleanup_state FROM runner_delivery_file_outcomes WHERE receipt_id=? AND path=?`, batch.ReceiptID, file.Path).Scan(&generation, &cleanupState); err != nil {
			return err
		}
		if generation == "" {
			return fmt.Errorf("explicit deletion file has no manifest generation")
		}
		if cleanupState != "removed" {
			if _, err := tx.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_state='pending',cleanup_error='operator selected this manifest file for deletion',cleanup_next_attempt_at=0,updated_at=? WHERE receipt_id=? AND path=?`, time.Now().UnixMilli(), batch.ReceiptID, file.Path); err != nil {
				return err
			}
		}
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_explicit_delete_files WHERE batch_id=?`, batch.ID).Scan(&count); err != nil {
		return err
	}
	if count != len(batch.Files) {
		return ErrDeliveryReceiptStale
	}
	return tx.Commit()
}

func (d *DB) ExplicitDeliveryDeleteBatch(ctx context.Context, batchID string) (ExplicitDeliveryDeleteBatch, error) {
	var batch ExplicitDeliveryDeleteBatch
	var next, blocked int64
	err := d.R.QueryRowContext(ctx, `SELECT batch_id,receipt_id,payload_path,inventory_fingerprint,publication_directory_identity,storage_identity,generation_key,request_token,publication_generation,retry_attempts,retry_next_attempt_at,retry_blocked_since,retry_error
		FROM runner_delivery_explicit_delete_batches WHERE batch_id=?`, batchID).
		Scan(&batch.ID, &batch.ReceiptID, &batch.PayloadPath, &batch.InventoryFingerprint, &batch.PublicationDirectoryIdentity, &batch.StorageIdentity, &batch.GenerationKey, &batch.RequestToken, &batch.PublicationGeneration, &batch.RetryAttempts, &next, &blocked, &batch.RetryError)
	if errors.Is(err, sql.ErrNoRows) {
		return batch, ErrNotFound
	}
	if err != nil {
		return batch, err
	}
	if next > 0 {
		batch.RetryNextAttemptAt = time.UnixMilli(next)
	}
	if blocked > 0 {
		batch.RetryBlockedSince = time.UnixMilli(blocked)
	}
	batch.Files, err = d.explicitDeliveryDeleteFiles(ctx, batchID)
	return batch, err
}

// ExplicitDeliveryDeleteBatchesDue returns only incomplete, due batches in
// oldest-deadline order. The caller performs filesystem work on the bounded
// cleanup worker, never on the receipt/control-plane sweep.
func (d *DB) ExplicitDeliveryDeleteBatchesDue(ctx context.Context, receiptID int64, limit int) ([]ExplicitDeliveryDeleteBatch, error) {
	return d.explicitDeliveryDeleteBatches(ctx, receiptID, limit, true)
}

// ExplicitDeliveryDeleteBatchesPending returns every incomplete exact batch;
// callers use it only to keep generic cleanup from bypassing a reviewed batch
// whose persisted retry deadline has not arrived yet.
func (d *DB) ExplicitDeliveryDeleteBatchesPending(ctx context.Context, receiptID int64, limit int) ([]ExplicitDeliveryDeleteBatch, error) {
	return d.explicitDeliveryDeleteBatches(ctx, receiptID, limit, false)
}

// HasPendingExplicitDeliveryDelete reports whether an incomplete reviewed
// batch owns the whole payload (empty path) or one exact manifest path.
func (d *DB) HasPendingExplicitDeliveryDelete(ctx context.Context, receiptID int64, path string) (bool, error) {
	var exists int
	query := `SELECT EXISTS(SELECT 1 FROM runner_delivery_explicit_delete_batches b
		JOIN runner_delivery_explicit_delete_files f ON f.batch_id=b.batch_id
		WHERE b.receipt_id=? AND f.history_recorded=0`
	args := []any{receiptID}
	if path != "" {
		query += ` AND f.path=?`
		args = append(args, path)
	}
	query += `)`
	err := d.R.QueryRowContext(ctx, query, args...).Scan(&exists)
	return exists == 1, err
}

func (d *DB) explicitDeliveryDeleteBatches(ctx context.Context, receiptID int64, limit int, dueOnly bool) ([]ExplicitDeliveryDeleteBatch, error) {
	if receiptID <= 0 || limit <= 0 {
		return nil, nil
	}
	query := `SELECT b.batch_id,b.receipt_id,b.payload_path,b.inventory_fingerprint,b.publication_directory_identity,b.storage_identity,b.generation_key,b.request_token,b.publication_generation,b.retry_attempts,b.retry_next_attempt_at,b.retry_blocked_since,b.retry_error
		FROM runner_delivery_explicit_delete_batches b
		WHERE b.receipt_id=? AND EXISTS (
			SELECT 1 FROM runner_delivery_explicit_delete_files f WHERE f.batch_id=b.batch_id AND f.history_recorded=0
		)
		`
	args := []any{receiptID}
	if dueOnly {
		query += ` AND b.retry_next_attempt_at<=?`
		args = append(args, time.Now().UnixMilli())
	}
	query += ` ORDER BY CASE WHEN b.retry_next_attempt_at=0 THEN b.created_at ELSE b.retry_next_attempt_at END,b.created_at LIMIT ?`
	args = append(args, limit)
	rows, err := d.R.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ExplicitDeliveryDeleteBatch
	for rows.Next() {
		var batch ExplicitDeliveryDeleteBatch
		var next, blocked int64
		if err := rows.Scan(&batch.ID, &batch.ReceiptID, &batch.PayloadPath, &batch.InventoryFingerprint, &batch.PublicationDirectoryIdentity,
			&batch.StorageIdentity, &batch.GenerationKey, &batch.RequestToken, &batch.PublicationGeneration, &batch.RetryAttempts, &next, &blocked, &batch.RetryError); err != nil {
			return nil, err
		}
		if next > 0 {
			batch.RetryNextAttemptAt = time.UnixMilli(next)
		}
		if blocked > 0 {
			batch.RetryBlockedSince = time.UnixMilli(blocked)
		}
		files, err := d.explicitDeliveryDeleteFiles(ctx, batch.ID)
		if err != nil {
			return nil, err
		}
		batch.Files = files
		out = append(out, batch)
	}
	return out, rows.Err()
}

func (d *DB) explicitDeliveryDeleteFiles(ctx context.Context, batchID string) ([]ExplicitDeliveryDeleteFile, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT path,reviewed_path,snapshot_identity,parent_identity,history_recorded,completed_at FROM runner_delivery_explicit_delete_files WHERE batch_id=? ORDER BY path`, batchID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var files []ExplicitDeliveryDeleteFile
	for rows.Next() {
		var file ExplicitDeliveryDeleteFile
		var history int
		var completed int64
		if err := rows.Scan(&file.Path, &file.ReviewedPath, &file.SnapshotIdentity, &file.ParentIdentity, &history, &completed); err != nil {
			return nil, err
		}
		file.HistoryRecorded = history == 1
		if completed > 0 {
			file.CompletedAt = time.UnixMilli(completed)
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

// ScheduleExplicitDeliveryDeleteRetry records local retry timing and surfaces
// the failure in the existing per-file lifecycle rows. It never creates new
// path authority: the immutable batch rows remain the only selection source.
func (d *DB) ScheduleExplicitDeliveryDeleteRetry(ctx context.Context, batchID, message string) error {
	if batchID == "" {
		return fmt.Errorf("explicit delivery deletion batch is required")
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var receiptID int64
	var attempts int
	if err := tx.QueryRowContext(ctx, `SELECT receipt_id,retry_attempts FROM runner_delivery_explicit_delete_batches WHERE batch_id=?`, batchID).Scan(&receiptID, &attempts); err != nil {
		return err
	}
	now := time.Now()
	next := now.Add(deliveryCleanupRetryDelay(attempts + 1)).UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE runner_delivery_explicit_delete_batches SET retry_attempts=retry_attempts+1,retry_next_attempt_at=?,retry_blocked_since=CASE WHEN retry_blocked_since=0 THEN ? ELSE retry_blocked_since END,retry_error=? WHERE batch_id=?`, next, now.UnixMilli(), message, batchID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_state=CASE WHEN cleanup_state='removed' THEN 'removed' ELSE 'retry' END,cleanup_error=?,cleanup_attempts=cleanup_attempts+1,cleanup_blocked_since=CASE WHEN cleanup_blocked_since=0 THEN ? ELSE cleanup_blocked_since END,cleanup_next_attempt_at=?,updated_at=?
		WHERE receipt_id=? AND path IN (SELECT path FROM runner_delivery_explicit_delete_files WHERE batch_id=? AND history_recorded=0)`, message, now.UnixMilli(), next, now.UnixMilli(), receiptID, batchID); err != nil {
		return err
	}
	return tx.Commit()
}

// CompleteExplicitDeliveryFileDelete commits the removed outcome and its
// operator-visible history event together. The batch membership row is the
// durable authorization for both the unlink replay and this final stamp.
func (d *DB) CompleteExplicitDeliveryFileDelete(ctx context.Context, batchID string, receiptID int64, path, eventType string, mediaItemID int64, releaseTitle string, data any) error {
	if batchID == "" || receiptID <= 0 || path == "" || eventType == "" {
		return fmt.Errorf("invalid explicit delivery delete completion")
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var batchReceipt int64
	var historyRecorded int
	err = tx.QueryRowContext(ctx, `SELECT b.receipt_id,f.history_recorded
		FROM runner_delivery_explicit_delete_batches b
		JOIN runner_delivery_explicit_delete_files f ON f.batch_id=b.batch_id
		WHERE b.batch_id=? AND f.path=?`, batchID, path).Scan(&batchReceipt, &historyRecorded)
	if err != nil {
		return err
	}
	if batchReceipt != receiptID {
		return ErrDeliveryReceiptStale
	}
	var cleanupState string
	if err := tx.QueryRowContext(ctx, `SELECT cleanup_state FROM runner_delivery_file_outcomes WHERE receipt_id=? AND path=?`, receiptID, path).Scan(&cleanupState); err != nil {
		return err
	}
	if cleanupState != "removed" {
		res, err := tx.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes
			SET cleanup_state='removed',cleanup_error='operator explicitly selected this manifest file',cleanup_next_attempt_at=0,updated_at=?
			WHERE receipt_id=? AND path=? AND file_generation!='' AND cleanup_state!='removed'`, time.Now().UnixMilli(), receiptID, path)
		if err := requireDeliveryTransition(res, err); err != nil {
			return err
		}
	}
	if historyRecorded == 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_events (ts,type,media_item_id,release_title,data) VALUES(?,?,?,?,?)`,
			time.Now().UnixMilli(), eventType, mediaItemID, releaseTitle, string(raw)); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE runner_delivery_explicit_delete_files SET history_recorded=1,completed_at=? WHERE batch_id=? AND path=? AND history_recorded=0`, time.Now().UnixMilli(), batchID, path)
		if err := requireDeliveryTransition(res, err); err != nil {
			return err
		}
	}
	var incomplete int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_explicit_delete_files WHERE batch_id=? AND history_recorded=0`, batchID).Scan(&incomplete); err != nil {
		return err
	}
	if incomplete == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE runner_delivery_explicit_delete_batches SET retry_next_attempt_at=0,retry_error='' WHERE batch_id=?`, batchID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeliveryFileImport is immutable provenance for one attempt to place a
// manifest entry in the library. Multiple rows may refer to one manifest
// path when a later Resolve operation places that source again.
type DeliveryFileImport struct {
	ID                      int64
	ReceiptID               int64
	Path                    string
	SourceGeneration        string
	SourceTransfer          string
	SourcePayloadGeneration string
	ReplacementOperationID  string
	ReplacementDisposition  string
	PlacementID             string
	LibraryFileGeneration   string
	LibraryFileIdentity     string
	LibraryPath             string
	CreatedAt               time.Time
}

// DeliveryAttemptEvent records work performed by Curator for one receipt.
// FinishedAt is zero for an interrupted or currently unclosed attempt; that
// is an unknown outcome, never evidence of successful completion.
type DeliveryAttemptEvent struct {
	ID         int64
	ReceiptID  int64
	Stage      string
	Attempt    int
	StartedAt  time.Time
	FinishedAt time.Time
	Result     string
	Error      string
}

func (d *DB) BeginDeliveryAttempt(ctx context.Context, receiptID int64, stage string) (DeliveryAttemptEvent, error) {
	if receiptID <= 0 || strings.TrimSpace(stage) == "" {
		return DeliveryAttemptEvent{}, fmt.Errorf("receipt and stage are required")
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryAttemptEvent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var attempt int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt),0)+1 FROM runner_delivery_attempt_events WHERE receipt_id=? AND stage=?`, receiptID, stage).Scan(&attempt); err != nil {
		return DeliveryAttemptEvent{}, err
	}
	now := time.Now()
	res, err := tx.ExecContext(ctx, `INSERT INTO runner_delivery_attempt_events(receipt_id,stage,attempt,started_at,result) VALUES(?,?,?,?, 'running')`, receiptID, stage, attempt, now.UnixMilli())
	if err != nil {
		return DeliveryAttemptEvent{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return DeliveryAttemptEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryAttemptEvent{}, err
	}
	return DeliveryAttemptEvent{ID: id, ReceiptID: receiptID, Stage: stage, Attempt: attempt, StartedAt: now, Result: "running"}, nil
}

func (d *DB) FinishDeliveryAttempt(ctx context.Context, eventID int64, result, detail string) error {
	if eventID <= 0 || strings.TrimSpace(result) == "" || result == "running" {
		return fmt.Errorf("attempt and terminal result are required")
	}
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_attempt_events SET finished_at=?,result=?,error=? WHERE id=? AND finished_at IS NULL AND result='running'`, time.Now().UnixMilli(), result, detail, eventID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrDeliveryReceiptStale
	}
	return nil
}

func (d *DB) DeliveryAttemptEvents(ctx context.Context, receiptID int64) ([]DeliveryAttemptEvent, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id,receipt_id,stage,attempt,started_at,COALESCE(finished_at,0),result,error FROM runner_delivery_attempt_events WHERE receipt_id=? ORDER BY id`, receiptID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	events := []DeliveryAttemptEvent{}
	for rows.Next() {
		var event DeliveryAttemptEvent
		var started, finished int64
		if err := rows.Scan(&event.ID, &event.ReceiptID, &event.Stage, &event.Attempt, &started, &finished, &event.Result, &event.Error); err != nil {
			return nil, err
		}
		event.StartedAt = time.UnixMilli(started)
		if finished > 0 {
			event.FinishedAt = time.UnixMilli(finished)
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

var ErrDeliveryReceiptStale = errors.New("delivery receipt transition is stale")

// SaveDiscoveredDelivery inserts a discovery before its cursor may advance.
// delivery_id is scoped to the authenticated Runner client and immutable: a
// later page that changes any publication fact becomes a conflict.
func (d *DB) SaveDiscoveredDelivery(ctx context.Context, clientID int64, v ports.RunnerDelivery, mappedPath string) (DeliveryReceipt, error) {
	files, err := json.Marshal(v.Files)
	if err != nil {
		return DeliveryReceipt{}, err
	}
	fileOutcomes, err := json.Marshal(v.FileOutcomes)
	if err != nil {
		return DeliveryReceipt{}, err
	}
	cleanup := string(v.SourceCleanup)
	if cleanup == "" {
		cleanup = "null"
	}
	now := time.Now().UnixMilli()
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return DeliveryReceipt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var existing DeliveryReceipt
	err = scanDeliveryReceipt(tx.QueryRowContext(ctx, `SELECT id, client_id, sequence, delivery_id,
		publication_generation, source_artifact_id, source_generation, manifest_digest, artifact_id, job_id, request_token,
		runner_instance_id, ready_at, publication_path, mapped_path, files_json, source_cleanup_json,
		terminal_outcome, failure_stage, failure_cause, keep_source_on_failure, file_outcomes_json,
		state, ack_state, consumer_receipt_id, download_id, generation_key, download_handle,
		media_item_id, copy_id, error, attempts, blocked_since, next_attempt_at, publication_directory_identity
		FROM runner_delivery_receipts WHERE client_id=? AND delivery_id=?`, clientID, v.DeliveryID), &existing)
	if err == nil {
		changed := !sameDeliveryImmutable(existing.Delivery, v)
		if changed {
			_, err = tx.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='conflict', error=?, updated_at=? WHERE id=?`, "Runner reused a delivery ID with changed immutable publication facts", now, existing.ID)
			if err != nil {
				return DeliveryReceipt{}, err
			}
			if err = tx.Commit(); err != nil {
				return DeliveryReceipt{}, err
			}
			existing.State, existing.Error = "conflict", "Runner reused a delivery ID with changed immutable publication facts"
		} else {
			// Cleanup progress and per-file cleanup statuses are mutable Runner
			// state. Refresh them without changing the immutable association.
			_, err = tx.ExecContext(ctx, `UPDATE runner_delivery_receipts SET source_cleanup_json=?,file_outcomes_json=?,updated_at=? WHERE id=?`, cleanup, string(fileOutcomes), now, existing.ID)
			if err != nil {
				return DeliveryReceipt{}, err
			}
			existing.Delivery.SourceCleanup = v.SourceCleanup
			existing.Delivery.FileOutcomes = v.FileOutcomes
			if err = tx.Commit(); err != nil {
				return DeliveryReceipt{}, err
			}
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return DeliveryReceipt{}, err
	}
	instanceMismatch := false
	if v.RunnerInstanceID != "" {
		var pinned string
		pinErr := tx.QueryRowContext(ctx, `SELECT runner_instance_id FROM runner_client_instances WHERE client_id=?`, clientID).Scan(&pinned)
		if errors.Is(pinErr, sql.ErrNoRows) {
			_, pinErr = tx.ExecContext(ctx, `INSERT INTO runner_client_instances(client_id,runner_instance_id,updated_at) VALUES(?,?,?)`, clientID, v.RunnerInstanceID, now)
		} else if pinErr == nil && pinned == "" {
			_, pinErr = tx.ExecContext(ctx, `UPDATE runner_client_instances SET runner_instance_id=?,updated_at=? WHERE client_id=?`, v.RunnerInstanceID, now, clientID)
		} else if pinErr == nil && pinned != v.RunnerInstanceID {
			instanceMismatch = true
		}
		if pinErr != nil {
			return DeliveryReceipt{}, pinErr
		}
	}
	state, receiptErr := "unresolved", ""
	if instanceMismatch {
		state, receiptErr = "conflict", "authenticated client now points to a different Runner installation"
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO runner_delivery_receipts
		(client_id, delivery_id, sequence, publication_generation, source_artifact_id, source_generation, manifest_digest,
		artifact_id, job_id, request_token, runner_instance_id, ready_at, publication_path,
		mapped_path, files_json, source_cleanup_json, terminal_outcome, failure_stage, failure_cause,
		keep_source_on_failure, file_outcomes_json, state, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, clientID, v.DeliveryID,
		v.Sequence, v.PublicationGeneration, v.SourceArtifactID, v.SourceGeneration, v.ManifestDigest, v.ArtifactID, v.JobID,
		v.RequestToken, v.RunnerInstanceID, v.ReadyAt.UnixMilli(), v.PublicationPath, mappedPath, string(files), cleanup,
		v.TerminalOutcome, v.FailureStage, v.FailureCause, v.KeepSourceOnFailure, string(fileOutcomes), state, receiptErr, now, now)
	if err != nil {
		return DeliveryReceipt{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return DeliveryReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeliveryReceipt{}, err
	}
	return DeliveryReceipt{ID: id, ClientID: clientID, Delivery: v, MappedPath: mappedPath, State: state, AckState: "not_accepted", ReadyAt: v.ReadyAt, Error: receiptErr}, nil
}

func sameDeliveryImmutable(a, b ports.RunnerDelivery) bool {
	if a.Sequence != b.Sequence || a.DeliveryID != b.DeliveryID || a.PublicationGeneration != b.PublicationGeneration ||
		a.SourceArtifactID != b.SourceArtifactID || a.SourceGeneration != b.SourceGeneration ||
		a.ManifestDigest != b.ManifestDigest || a.ArtifactID != b.ArtifactID || a.JobID != b.JobID ||
		a.RequestToken != b.RequestToken || a.RunnerInstanceID != b.RunnerInstanceID ||
		a.PublicationPath != b.PublicationPath || a.ReadyAt.UnixMilli() != b.ReadyAt.UnixMilli() ||
		a.TerminalOutcome != b.TerminalOutcome || a.FailureStage != b.FailureStage || a.FailureCause != b.FailureCause ||
		a.KeepSourceOnFailure != b.KeepSourceOnFailure || len(a.Files) != len(b.Files) || len(a.FileOutcomes) != len(b.FileOutcomes) {
		return false
	}
	af, bf := append([]ports.RunnerFile(nil), a.Files...), append([]ports.RunnerFile(nil), b.Files...)
	sort.Slice(af, func(i, j int) bool { return bytes.Compare([]byte(af[i].Path), []byte(af[j].Path)) < 0 })
	sort.Slice(bf, func(i, j int) bool { return bytes.Compare([]byte(bf[i].Path), []byte(bf[j].Path)) < 0 })
	for i := range af {
		if af[i] != bf[i] {
			return false
		}
	}
	ao, bo := append([]ports.RunnerFileOutcome(nil), a.FileOutcomes...), append([]ports.RunnerFileOutcome(nil), b.FileOutcomes...)
	sort.Slice(ao, func(i, j int) bool { return bytes.Compare([]byte(ao[i].SourcePath), []byte(ao[j].SourcePath)) < 0 })
	sort.Slice(bo, func(i, j int) bool { return bytes.Compare([]byte(bo[i].SourcePath), []byte(bo[j].SourcePath)) < 0 })
	for i := range ao {
		ao[i].SourceCleanupStatus, bo[i].SourceCleanupStatus = "", ""
		if !sameRunnerFileOutcomeIdentity(ao[i], bo[i]) {
			return false
		}
	}
	return true
}

func sameRunnerFileOutcomeIdentity(a, b ports.RunnerFileOutcome) bool {
	if a.SourcePath != b.SourcePath || a.Classification != b.Classification || a.Outcome != b.Outcome || a.Reason != b.Reason || a.RepairDependency != b.RepairDependency {
		return false
	}
	if (a.PublicationPath == nil) != (b.PublicationPath == nil) {
		return false
	}
	return a.PublicationPath == nil || *a.PublicationPath == *b.PublicationPath
}

func scanDeliveryReceipt(row interface{ Scan(...any) error }, out *DeliveryReceipt) error {
	var seq, next, readyAt, blockedSince int64
	var id, clientID, downloadID, mediaID, copyID int64
	var attempts int
	var deliveryID, generation, sourceArtifact, sourceGeneration, digest, artifact, job, token, runnerID, pubPath, mapped, filesRaw, cleanupRaw, terminalOutcome, failureStage, failureCause, fileOutcomesRaw, state, ack, consumer, genKey, handle, message, directoryIdentity string
	var keepSourceOnFailure bool
	if err := row.Scan(&id, &clientID, &seq, &deliveryID, &generation, &sourceArtifact, &sourceGeneration, &digest, &artifact, &job, &token, &runnerID, &readyAt, &pubPath, &mapped, &filesRaw, &cleanupRaw, &terminalOutcome, &failureStage, &failureCause, &keepSourceOnFailure, &fileOutcomesRaw, &state, &ack, &consumer, &downloadID, &genKey, &handle, &mediaID, &copyID, &message, &attempts, &blockedSince, &next, &directoryIdentity); err != nil {
		return err
	}
	var files []ports.RunnerFile
	if err := json.Unmarshal([]byte(filesRaw), &files); err != nil {
		return fmt.Errorf("decode delivery files: %w", err)
	}
	var cleanup json.RawMessage
	if cleanupRaw != "" {
		cleanup = json.RawMessage(cleanupRaw)
	}
	var fileOutcomes []ports.RunnerFileOutcome
	if fileOutcomesRaw != "" {
		if err := json.Unmarshal([]byte(fileOutcomesRaw), &fileOutcomes); err != nil {
			return fmt.Errorf("decode delivery file outcomes: %w", err)
		}
	}
	ready := time.UnixMilli(readyAt)
	blocked := time.Time{}
	if blockedSince != 0 {
		blocked = time.UnixMilli(blockedSince)
	}
	*out = DeliveryReceipt{ID: id, ClientID: clientID, Delivery: ports.RunnerDelivery{Sequence: uint64(seq), DeliveryID: deliveryID, PublicationGeneration: generation, SourceArtifactID: sourceArtifact, SourceGeneration: sourceGeneration, ManifestDigest: digest, ArtifactID: artifact, JobID: job, RequestToken: token, RunnerInstanceID: runnerID, PublicationPath: pubPath, Files: files, TerminalOutcome: terminalOutcome, FailureStage: failureStage, FailureCause: failureCause, KeepSourceOnFailure: keepSourceOnFailure, FileOutcomes: fileOutcomes, ReadyAt: ready, SourceCleanup: cleanup}, MappedPath: mapped, PublicationDirectoryIdentity: directoryIdentity, State: state, AckState: ack, ConsumerReceiptID: consumer, DownloadID: downloadID, GenerationKey: genKey, DownloadHandle: handle, MediaItemID: mediaID, CopyID: copyID, ReadyAt: ready, Error: message, Attempts: attempts, BlockedSince: blocked, NextAttemptAt: time.UnixMilli(next)}
	return nil
}

func (d *DB) DeliveryCursor(ctx context.Context, clientID int64) (uint64, error) {
	var cursor int64
	err := d.R.QueryRowContext(ctx, `SELECT after_sequence FROM runner_delivery_cursors WHERE client_id=?`, clientID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return uint64(cursor), err
}

func (d *DB) SetDeliveryCursor(ctx context.Context, clientID int64, cursor uint64) error {
	_, err := d.W.ExecContext(ctx, `INSERT INTO runner_delivery_cursors(client_id,after_sequence,updated_at) VALUES(?,?,?) ON CONFLICT(client_id) DO UPDATE SET after_sequence=excluded.after_sequence,updated_at=excluded.updated_at`, clientID, cursor, time.Now().UnixMilli())
	return err
}

func (d *DB) EnableRunnerDeliveryV1(ctx context.Context, clientID int64) error {
	_, err := d.W.ExecContext(ctx, `INSERT INTO runner_client_instances(client_id,runner_instance_id,delivery_v1_enabled,updated_at) VALUES(?,'',1,?) ON CONFLICT(client_id) DO UPDATE SET delivery_v1_enabled=1,updated_at=excluded.updated_at`, clientID, time.Now().UnixMilli())
	return err
}

func (d *DB) RunnerDeliveryV1Enabled(ctx context.Context, clientID int64) (bool, error) {
	var enabled int
	err := d.R.QueryRowContext(ctx, `SELECT delivery_v1_enabled FROM runner_client_instances WHERE client_id=?`, clientID).Scan(&enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return enabled == 1, err
}

// Narrow receipt updates keep discovery, retry scheduling and workers from
// overwriting one another's independent durable progress.
func (d *DB) AssociateDelivery(ctx context.Context, r DeliveryReceipt) error {
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET mapped_path=?,download_id=?,generation_key=?,download_handle=?,media_item_id=?,copy_id=?,updated_at=?
		WHERE id=? AND state!='conflict' AND (download_id=0 OR (download_id=? AND generation_key=? AND media_item_id=? AND copy_id=?))`,
		r.MappedPath, r.DownloadID, r.GenerationKey, r.DownloadHandle, r.MediaItemID, r.CopyID, time.Now().UnixMilli(), r.ID,
		r.DownloadID, r.GenerationKey, r.MediaItemID, r.CopyID)
	return requireDeliveryTransition(res, err)
}

// EnsureDeliveryDirectoryIdentity pins the accepted publication directory to
// its original inode identity. Directory timestamps are mutable during
// authorized child cleanup, so the caller pairs this with storage-source
// verification on every cleanup attempt.
func (d *DB) EnsureDeliveryDirectoryIdentity(ctx context.Context, receiptID int64, identity string) error {
	if identity == "" {
		return fmt.Errorf("publication directory identity is unavailable")
	}
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET publication_directory_identity=?,updated_at=?
		WHERE id=? AND publication_directory_identity=''`, identity, time.Now().UnixMilli(), receiptID)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 {
		return nil
	}
	var persisted string
	if err := d.R.QueryRowContext(ctx, `SELECT publication_directory_identity FROM runner_delivery_receipts WHERE id=?`, receiptID).Scan(&persisted); err != nil {
		return err
	}
	if persisted != identity {
		return ErrDeliveryReceiptStale
	}
	return nil
}

func (d *DB) SetDeliveryRetry(ctx context.Context, r DeliveryReceipt, providerDeadline ...time.Time) error {
	deadline := int64(0)
	if len(providerDeadline) > 0 && providerDeadline[0].After(time.Now()) {
		deadline = providerDeadline[0].UnixMilli()
	}
	next := r.NextAttemptAt.UnixMilli()
	if deadline > next {
		next = deadline
	}
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET error=?,attempts=?,blocked_since=?,next_attempt_at=?,provider_retry_after=MAX(provider_retry_after,?),updated_at=? WHERE id=? AND state!='conflict'`,
		r.Error, r.Attempts, r.BlockedSince.UnixMilli(), next, deadline, time.Now().UnixMilli(), r.ID)
	return requireDeliveryTransition(res, err)
}

func (d *DB) DeliveryAcknowledgementRetryDue(ctx context.Context, receiptID int64, now time.Time) (bool, error) {
	var due int
	err := d.R.QueryRowContext(ctx, `SELECT CASE WHEN attempts>0 AND MAX(next_attempt_at,provider_retry_after)>? THEN 0 WHEN provider_retry_after>? THEN 0 ELSE 1 END FROM runner_delivery_receipts WHERE id=? AND ack_state='pending' AND consumer_receipt_id!=''`, now.UnixMilli(), now.UnixMilli(), receiptID).Scan(&due)
	if err != nil {
		return false, err
	}
	return due == 1, nil
}

func (d *DB) PrepareDeliveryAcceptance(ctx context.Context, r DeliveryReceipt) error {
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='ready',ack_state='pending',consumer_receipt_id=?,error='',attempts=0,blocked_since=0,next_attempt_at=?,updated_at=?
		WHERE id=? AND state!='conflict' AND ack_state='not_accepted'`, r.ConsumerReceiptID, r.NextAttemptAt.UnixMilli(), time.Now().UnixMilli(), r.ID)
	return requireDeliveryTransition(res, err)
}

func (d *DB) SetDeliveryAccepted(ctx context.Context, r DeliveryReceipt) error {
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET ack_state='accepted',error='',attempts=0,blocked_since=0,next_attempt_at=0,updated_at=?
		WHERE id=? AND state!='conflict' AND ack_state='pending' AND consumer_receipt_id=?`, time.Now().UnixMilli(), r.ID, r.ConsumerReceiptID)
	return requireDeliveryTransition(res, err)
}

func (d *DB) SetDeliveryConflict(ctx context.Context, id int64, message string) error {
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='conflict',error=?,next_attempt_at=0,updated_at=? WHERE id=?`, message, time.Now().UnixMilli(), id)
	return requireDeliveryTransition(res, err)
}

func (d *DB) SetDeliveryImported(ctx context.Context, id int64) error {
	// Import is independent of Runner ack. Preserve pending acknowledgement
	// retry state so it remains durable after local publication commits.
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='imported',updated_at=? WHERE id=? AND state!='conflict'`, time.Now().UnixMilli(), id)
	return requireDeliveryTransition(res, err)
}

// QueueDeliveryReimport atomically records an explicit selected-file request
// and reopens only the corresponding already-imported receipt. If the process
// stops after this commit but before enqueue, the ready receipt and persisted
// handoff selection are both available to the ordinary recovery sweep.
func (d *DB) QueueDeliveryReimport(ctx context.Context, expected DeliveryReceipt, dl Download) error {
	if expected.ID <= 0 || dl.ID <= 0 || len(dl.Handoff) == 0 || expected.DownloadID != dl.ID {
		return fmt.Errorf("receipt, download, and selected handoff are required")
	}
	handoff, err := json.Marshal(dl.Handoff)
	if err != nil {
		return err
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE downloads SET state=?,progress=?,error=?,save_path=?,import_path=?,handoff_log=?,updated_at=? WHERE id=? AND generation_key=? AND client_id=? AND handle=? AND transfer=? AND media_item_id=? AND COALESCE(copy_id,0)=?`,
		dl.State, dl.Progress, dl.Error, dl.SavePath, dl.ImportPath, string(handoff), now, dl.ID, expected.GenerationKey, expected.ClientID, expected.DownloadHandle, expected.Delivery.RequestToken, expected.MediaItemID, expected.CopyID)
	if err := requireDeliveryTransition(res, err); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='ready',error='',next_attempt_at=0,updated_at=? WHERE id=? AND download_id=? AND state='imported' AND (ack_state='accepted' OR (ack_state='pending' AND consumer_receipt_id!='')) AND generation_key=? AND client_id=? AND download_handle=? AND request_token=? AND publication_generation=? AND manifest_digest=? AND mapped_path=?`,
		now, expected.ID, dl.ID, dl.GenerationKey, expected.ClientID, expected.DownloadHandle, expected.Delivery.RequestToken, expected.Delivery.PublicationGeneration, expected.Delivery.ManifestDigest, expected.MappedPath)
	if err := requireDeliveryTransition(res, err); err != nil {
		return err
	}
	return tx.Commit()
}

func requireDeliveryTransition(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return ErrDeliveryReceiptStale
	}
	return nil
}

func (d *DB) DeliveryReceiptsForClient(ctx context.Context, clientID int64, limit int) ([]DeliveryReceipt, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id,client_id,sequence,delivery_id,publication_generation,source_artifact_id,source_generation,manifest_digest,artifact_id,job_id,request_token,runner_instance_id,ready_at,publication_path,mapped_path,files_json,source_cleanup_json,terminal_outcome,failure_stage,failure_cause,keep_source_on_failure,file_outcomes_json,state,ack_state,consumer_receipt_id,download_id,generation_key,download_handle,media_item_id,copy_id,error,attempts,blocked_since,next_attempt_at,publication_directory_identity FROM runner_delivery_receipts WHERE client_id=? AND (state IN ('unresolved','ready','importing') OR ack_state='pending') AND next_attempt_at<=? ORDER BY next_attempt_at,sequence LIMIT ?`, clientID, time.Now().UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeliveryReceipt
	for rows.Next() {
		var v DeliveryReceipt
		if err := scanDeliveryReceipt(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (d *DB) DeliveryReceiptByID(ctx context.Context, id int64) (DeliveryReceipt, error) {
	var v DeliveryReceipt
	err := scanDeliveryReceipt(d.R.QueryRowContext(ctx, `SELECT id,client_id,sequence,delivery_id,publication_generation,source_artifact_id,source_generation,manifest_digest,artifact_id,job_id,request_token,runner_instance_id,ready_at,publication_path,mapped_path,files_json,source_cleanup_json,terminal_outcome,failure_stage,failure_cause,keep_source_on_failure,file_outcomes_json,state,ack_state,consumer_receipt_id,download_id,generation_key,download_handle,media_item_id,copy_id,error,attempts,blocked_since,next_attempt_at,publication_directory_identity FROM runner_delivery_receipts WHERE id=?`, id), &v)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

// DeliveryReceiptPage returns all durable delivery records in descending
// receipt-ID order. The opaque numeric cursor makes old unresolved receipts
// reachable without a fixed truncation window.
func (d *DB) DeliveryReceiptPage(ctx context.Context, beforeID int64, limit int) ([]DeliveryReceipt, int64, error) {
	return d.deliveryReceiptPage(ctx, beforeID, 0, limit)
}

func (d *DB) DeliveryReceiptPageForDownload(ctx context.Context, downloadID, beforeID int64, limit int) ([]DeliveryReceipt, int64, error) {
	if downloadID <= 0 {
		return nil, 0, fmt.Errorf("download ID must be positive")
	}
	return d.deliveryReceiptPage(ctx, beforeID, downloadID, limit)
}

func (d *DB) deliveryReceiptPage(ctx context.Context, beforeID, downloadID int64, limit int) ([]DeliveryReceipt, int64, error) {
	if limit < 1 || limit > 100 {
		return nil, 0, fmt.Errorf("delivery page limit must be between 1 and 100")
	}
	rows, err := d.R.QueryContext(ctx, `SELECT id FROM runner_delivery_receipts WHERE (?=0 OR download_id=?) AND (?=0 OR id<?) ORDER BY id DESC LIMIT ?`, downloadID, downloadID, beforeID, beforeID, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0, limit+1)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(ids) > limit {
		ids = ids[:limit]
		next = ids[len(ids)-1]
	}
	out := make([]DeliveryReceipt, 0, len(ids))
	for _, id := range ids {
		v, err := d.DeliveryReceiptByID(ctx, id)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
	}
	return out, next, nil
}

type DeliveryRetryDetails struct {
	DirectoryState, DirectoryError       string
	DirectoryAttempts                    int
	DirectoryBlockedSince, DirectoryNext time.Time
	SourceAttempts                       int
	SourceError                          string
	SourceBlockedSince, SourceNext       time.Time
}

func (d *DB) DeliveryRetryDetails(ctx context.Context, receiptID int64) (DeliveryRetryDetails, error) {
	var v DeliveryRetryDetails
	var directoryBlocked, directoryNext, sourceBlocked, sourceNext int64
	err := d.R.QueryRowContext(ctx, `SELECT r.directory_cleanup_state,r.directory_cleanup_error,r.directory_cleanup_attempts,r.directory_cleanup_blocked_since,r.directory_cleanup_next_attempt_at,
		COALESCE(s.attempts,0),COALESCE(s.last_error,''),COALESCE(s.blocked_since,0),COALESCE(s.next_attempt_at,0)
		FROM runner_delivery_receipts r LEFT JOIN runner_delivery_source_refresh s ON s.receipt_id=r.id WHERE r.id=?`, receiptID).Scan(
		&v.DirectoryState, &v.DirectoryError, &v.DirectoryAttempts, &directoryBlocked, &directoryNext,
		&v.SourceAttempts, &v.SourceError, &sourceBlocked, &sourceNext)
	if err != nil {
		return v, err
	}
	if directoryBlocked > 0 {
		v.DirectoryBlockedSince = time.UnixMilli(directoryBlocked)
	}
	if directoryNext > 0 {
		v.DirectoryNext = time.UnixMilli(directoryNext)
	}
	if sourceBlocked > 0 {
		v.SourceBlockedSince = time.UnixMilli(sourceBlocked)
	}
	if sourceNext > 0 {
		v.SourceNext = time.UnixMilli(sourceNext)
	}
	return v, nil
}

func (d *DB) ForceDeliveryRetryNow(ctx context.Context, receiptID int64) error {
	now := time.Now().UnixMilli()
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET next_attempt_at=CASE WHEN provider_retry_after>? THEN provider_retry_after ELSE 0 END,updated_at=? WHERE id=? AND state!='conflict'`, now, now, receiptID)
	return requireDeliveryTransition(res, err)
}

func (d *DB) ForceDeliveryCleanupRetryNow(ctx context.Context, receiptID int64) error {
	now := time.Now().UnixMilli()
	_, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_explicit_delete_batches SET retry_next_attempt_at=0 WHERE receipt_id=? AND EXISTS (SELECT 1 FROM runner_delivery_explicit_delete_files f WHERE f.batch_id=runner_delivery_explicit_delete_batches.batch_id AND f.history_recorded=0)`, receiptID)
	if err != nil {
		return err
	}
	_, err = d.W.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_next_attempt_at=0,updated_at=? WHERE receipt_id=? AND cleanup_state IN ('pending','retry') AND disposition IN ('placed','redundant','disposable_selected_scope')`, now, receiptID)
	if err != nil {
		return err
	}
	_, err = d.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET directory_cleanup_next_attempt_at=0,updated_at=? WHERE id=? AND directory_cleanup_state IN ('pending','retry') AND state!='conflict'`, now, receiptID)
	return err
}

// EnsureDeliveryFileOutcomes journals every immutable manifest entry before
// import begins. Existing rows are checked rather than rewritten, so a
// changed manifest cannot silently replace the cleanup audit.
func (d *DB) EnsureDeliveryFileOutcomes(ctx context.Context, receiptID int64, files []DeliveryManifestFile, selected map[string]bool, manualAdditional bool) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixMilli()
	for _, f := range files {
		disposition := "pending"
		if selected != nil && !selected[f.Path] {
			disposition = "unselected_recoverable"
		}
		manual := 0
		if manualAdditional {
			manual = 1
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO runner_delivery_file_outcomes
			(receipt_id,path,size_bytes,sha256,file_generation,disposition,cleanup_state,manual_additional,updated_at)
			VALUES(?,?,?,?,?,?,'protected',?,?)`, receiptID, f.Path, f.SizeBytes, f.SHA256, f.FileGeneration, disposition, manual, now)
		if err != nil {
			return err
		}
		var size int64
		var digest string
		var storedIdentity string
		var storedManual int
		if err := tx.QueryRowContext(ctx, `SELECT size_bytes,sha256,file_generation,manual_additional FROM runner_delivery_file_outcomes WHERE receipt_id=? AND path=?`, receiptID, f.Path).Scan(&size, &digest, &storedIdentity, &storedManual); err != nil {
			return err
		}
		if size != f.SizeBytes || digest != f.SHA256 || storedIdentity != f.FileGeneration || (storedManual == 1) != manualAdditional {
			return fmt.Errorf("delivery file ledger immutable manifest mismatch for %q", f.Path)
		}
	}
	return tx.Commit()
}

func (d *DB) EnsureExplicitDeliveryFileOutcome(ctx context.Context, receiptID int64, file DeliveryManifestFile) error {
	cleanPath := filepath.ToSlash(filepath.Clean(filepath.FromSlash(file.Path)))
	_, digestErr := hex.DecodeString(file.SHA256)
	if receiptID <= 0 || file.Path == "" || filepath.IsAbs(file.Path) || cleanPath != file.Path || cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, "../") ||
		file.FileGeneration == "" || file.SizeBytes < 0 || len(file.SHA256) != 64 || digestErr != nil {
		return fmt.Errorf("invalid explicit delivery file identity")
	}
	now := time.Now().UnixMilli()
	_, err := d.W.ExecContext(ctx, `INSERT OR IGNORE INTO runner_delivery_file_outcomes
		(receipt_id,path,size_bytes,sha256,file_generation,disposition,cleanup_state,updated_at)
		VALUES(?,?,?,?,?,'pending','protected',?)`, receiptID, file.Path, file.SizeBytes, file.SHA256, file.FileGeneration, now)
	if err != nil {
		return err
	}
	var size int64
	var digest, generation string
	if err := d.R.QueryRowContext(ctx, `SELECT size_bytes,sha256,file_generation FROM runner_delivery_file_outcomes WHERE receipt_id=? AND path=?`, receiptID, file.Path).Scan(&size, &digest, &generation); err != nil {
		return err
	}
	if size != file.SizeBytes || digest != file.SHA256 || generation != file.FileGeneration {
		return ErrDeliveryReceiptStale
	}
	return nil
}

// UpdateDeliveryFileDisposition changes only import rationale. It preserves
// cleanup state, including an already committed unlink.
func (d *DB) UpdateDeliveryFileDisposition(ctx context.Context, receiptID int64, path, disposition, reason, libraryPath string) error {
	if disposition != "placed" && disposition != "redundant" && disposition != "disposable_selected_scope" && disposition != "unselected_recoverable" && disposition != "known_incidental" && disposition != "unknown_recoverable" {
		return fmt.Errorf("invalid delivery file disposition %q", disposition)
	}
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET disposition=?,reason=?,library_path=?,updated_at=?
		WHERE receipt_id=? AND path=? AND (
			disposition IN ('pending','unknown_recoverable') OR
			(disposition IN ('unselected_recoverable','known_incidental') AND ? IN ('known_incidental','placed','redundant','disposable_selected_scope'))
		)`, disposition, reason, libraryPath, time.Now().UnixMilli(), receiptID, path, disposition)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var count int
		if err := d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_file_outcomes WHERE receipt_id=? AND path=?`, receiptID, path).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return ErrDeliveryReceiptStale
		}
	}
	return nil
}

// SetDeliveryFileCleanup is a narrow monotone cleanup transition. A removed
// file is never put back into the retry queue by a stale worker.
func (d *DB) SetDeliveryFileCleanup(ctx context.Context, receiptID int64, path, state, message string) error {
	if state != "pending" && state != "removed" && state != "retry" && state != "protected" {
		return fmt.Errorf("invalid delivery file cleanup state %q", state)
	}
	now := time.Now()
	nowMS := now.UnixMilli()
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var delay time.Duration
	if state == "retry" {
		var attempts int
		if err := tx.QueryRowContext(ctx, `SELECT cleanup_attempts FROM runner_delivery_file_outcomes WHERE receipt_id=? AND path=?`, receiptID, path).Scan(&attempts); err != nil {
			return err
		}
		delay = deliveryCleanupRetryDelay(attempts + 1)
	}
	next := int64(0)
	if delay > 0 {
		next = now.Add(delay).UnixMilli()
	}
	res, err := tx.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_state=?,cleanup_error=?,updated_at=?,
		cleanup_attempts=cleanup_attempts+CASE WHEN ?='retry' THEN 1 ELSE 0 END,
		cleanup_blocked_since=CASE WHEN ?='retry' AND cleanup_blocked_since=0 THEN ? ELSE cleanup_blocked_since END,
		cleanup_next_attempt_at=?
		WHERE receipt_id=? AND path=? AND cleanup_state!='removed'
		AND disposition IN ('placed','redundant','disposable_selected_scope')`, state, message, nowMS, state, state, nowMS, next, receiptID, path)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_file_outcomes WHERE receipt_id=? AND path=? AND cleanup_state='removed'`, receiptID, path).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return ErrDeliveryReceiptStale
		}
	}
	if state == "removed" {
		res, err = tx.ExecContext(ctx, `UPDATE runner_delivery_receipts SET directory_cleanup_state='pending',directory_cleanup_next_attempt_at=0,updated_at=? WHERE id=?`, nowMS, receiptID)
		if err != nil {
			return err
		}
		if err := requireDeliveryTransition(res, nil); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ScheduleProtectedDeliveryFileRetry records a future recheck for an excess
// file whose replacement operation completed but whose current-placement
// proof is temporarily unavailable. It keeps the bytes protected and lets the
// receipt scheduler move on to other due work until the persisted deadline.
func (d *DB) ScheduleProtectedDeliveryFileRetry(ctx context.Context, receiptID int64, path, message string) error {
	if receiptID <= 0 || path == "" {
		return fmt.Errorf("receipt and path are required")
	}
	var attempts int
	if err := d.R.QueryRowContext(ctx, `SELECT cleanup_attempts FROM runner_delivery_file_outcomes WHERE receipt_id=? AND path=?`, receiptID, path).Scan(&attempts); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDeliveryReceiptStale
		}
		return err
	}
	now := time.Now()
	next := now.Add(deliveryCleanupRetryDelay(attempts + 1)).UnixMilli()
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_state='protected',cleanup_error=?,updated_at=?,
		cleanup_attempts=cleanup_attempts+1,
		cleanup_blocked_since=CASE WHEN cleanup_blocked_since=0 THEN ? ELSE cleanup_blocked_since END,
		cleanup_next_attempt_at=?
		WHERE receipt_id=? AND path=? AND cleanup_state!='removed' AND disposition IN ('unknown_recoverable','known_incidental','disposable_selected_scope')`,
		message, now.UnixMilli(), now.UnixMilli(), next, receiptID, path)
	if err != nil {
		return err
	}
	return requireDeliveryTransition(res, nil)
}

func deliveryCleanupRetryDelay(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return 30 * time.Second
	case attempt == 2:
		return time.Minute
	case attempt == 3:
		return 2 * time.Minute
	default:
		return 5 * time.Minute
	}
}

func (d *DB) SetDeliveryDirectoryCleanup(ctx context.Context, receiptID int64, state, message string) error {
	if state != "pending" && state != "removed" && state != "retry" && state != "protected" {
		return fmt.Errorf("invalid delivery directory cleanup state %q", state)
	}
	now := time.Now()
	var attempts int
	if state == "retry" {
		if err := d.R.QueryRowContext(ctx, `SELECT directory_cleanup_attempts FROM runner_delivery_receipts WHERE id=?`, receiptID).Scan(&attempts); err != nil {
			return err
		}
	}
	next := int64(0)
	if state == "retry" {
		next = now.Add(deliveryCleanupRetryDelay(attempts + 1)).UnixMilli()
	}
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET directory_cleanup_state=?,directory_cleanup_error=?,updated_at=?,directory_cleanup_attempts=directory_cleanup_attempts+CASE WHEN ?='retry' THEN 1 ELSE 0 END,directory_cleanup_blocked_since=CASE WHEN ?='retry' AND directory_cleanup_blocked_since=0 THEN ? ELSE directory_cleanup_blocked_since END,directory_cleanup_next_attempt_at=? WHERE id=? AND state!='conflict'`, state, message, now.UnixMilli(), state, state, now.UnixMilli(), next, receiptID)
	return requireDeliveryTransition(res, err)
}

func scanDeliveryFileOutcome(row interface{ Scan(...any) error }, out *DeliveryFileOutcome) error {
	var updated int64
	var manual int
	var blocked, next int64
	if err := row.Scan(&out.ReceiptID, &out.Path, &out.SizeBytes, &out.SHA256, &out.FileGeneration, &out.Disposition, &out.CleanupState, &out.Reason, &out.LibraryPath, &out.CleanupError, &out.CleanupAttempts, &blocked, &next, &manual, &updated); err != nil {
		return err
	}
	out.ManualAdditional = manual == 1
	out.UpdatedAt = time.UnixMilli(updated)
	if blocked > 0 {
		out.CleanupBlockedSince = time.UnixMilli(blocked)
	}
	if next > 0 {
		out.CleanupNextAttemptAt = time.UnixMilli(next)
	}
	return nil
}

func (d *DB) DeliveryFileOutcomes(ctx context.Context, receiptID int64) ([]DeliveryFileOutcome, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT receipt_id,path,size_bytes,sha256,file_generation,disposition,cleanup_state,reason,library_path,cleanup_error,cleanup_attempts,cleanup_blocked_since,cleanup_next_attempt_at,manual_additional,updated_at FROM runner_delivery_file_outcomes WHERE receipt_id=? ORDER BY path`, receiptID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []DeliveryFileOutcome{}
	for rows.Next() {
		var v DeliveryFileOutcome
		if err := scanDeliveryFileOutcome(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (d *DB) DeliveryFileOutcomeByPath(ctx context.Context, receiptID int64, path string) (DeliveryFileOutcome, error) {
	var v DeliveryFileOutcome
	err := scanDeliveryFileOutcome(d.R.QueryRowContext(ctx, `SELECT receipt_id,path,size_bytes,sha256,file_generation,disposition,cleanup_state,reason,library_path,cleanup_error,cleanup_attempts,cleanup_blocked_since,cleanup_next_attempt_at,manual_additional,updated_at FROM runner_delivery_file_outcomes WHERE receipt_id=? AND path=?`, receiptID, path), &v)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

func (d *DB) RecordDeliveryFileImport(ctx context.Context, v DeliveryFileImport) error {
	if v.ReceiptID <= 0 || v.Path == "" || (v.ReplacementDisposition != "" && v.ReplacementDisposition != "selected" && v.ReplacementDisposition != "excess") {
		return fmt.Errorf("invalid delivery file import provenance")
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now()
	}
	_, err := d.W.ExecContext(ctx, `INSERT OR IGNORE INTO runner_delivery_file_imports
		(receipt_id,path,source_generation,source_transfer,source_payload_generation,replacement_operation_id,replacement_disposition,placement_id,library_file_generation,library_file_identity,library_path,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, v.ReceiptID, v.Path, v.SourceGeneration, v.SourceTransfer, v.SourcePayloadGeneration,
		v.ReplacementOperationID, v.ReplacementDisposition, v.PlacementID, v.LibraryFileGeneration,
		v.LibraryFileIdentity, v.LibraryPath, v.CreatedAt.UnixMilli())
	return err
}

func (d *DB) DeliveryFileImports(ctx context.Context, receiptID int64) ([]DeliveryFileImport, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id,receipt_id,path,source_generation,source_transfer,source_payload_generation,replacement_operation_id,replacement_disposition,placement_id,library_file_generation,library_file_identity,library_path,created_at
		FROM runner_delivery_file_imports WHERE receipt_id=? ORDER BY id`, receiptID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeliveryFileImport
	for rows.Next() {
		var v DeliveryFileImport
		var created int64
		if err := rows.Scan(&v.ID, &v.ReceiptID, &v.Path, &v.SourceGeneration, &v.SourceTransfer, &v.SourcePayloadGeneration, &v.ReplacementOperationID, &v.ReplacementDisposition, &v.PlacementID, &v.LibraryFileGeneration, &v.LibraryFileIdentity, &v.LibraryPath, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = time.UnixMilli(created)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (d *DB) DeliveryFileCleanupReceipts(ctx context.Context, clientID int64, limit int) ([]int64, error) {
	now := time.Now().UnixMilli()
	rows, err := d.R.QueryContext(ctx, `SELECT r.id FROM runner_delivery_receipts r LEFT JOIN runner_delivery_file_outcomes f ON f.receipt_id=r.id WHERE (?=0 OR r.client_id=?) AND r.state!='conflict' AND ((f.cleanup_state IN ('pending','retry') AND f.cleanup_next_attempt_at<=? AND f.disposition IN ('placed','redundant','disposable_selected_scope')) OR (r.directory_cleanup_state IN ('pending','retry') AND r.directory_cleanup_next_attempt_at<=?) OR EXISTS (
		SELECT 1 FROM runner_delivery_explicit_delete_batches b JOIN runner_delivery_explicit_delete_files bf ON bf.batch_id=b.batch_id
		WHERE b.receipt_id=r.id AND bf.history_recorded=0 AND b.retry_next_attempt_at<=?
	) OR (r.state='imported' AND EXISTS (
		SELECT 1 FROM runner_delivery_file_imports i JOIN replacement_operations o ON o.operation_id=i.replacement_operation_id
		JOIN runner_delivery_file_outcomes x ON x.receipt_id=i.receipt_id AND x.path=i.path
		WHERE i.receipt_id=r.id AND i.replacement_disposition='excess' AND o.state='complete' AND x.cleanup_state='protected' AND x.cleanup_next_attempt_at<=? AND x.disposition IN ('unknown_recoverable','known_incidental','disposable_selected_scope')
	))) GROUP BY r.id ORDER BY COALESCE(
		(SELECT MIN(CASE WHEN b.retry_next_attempt_at=0 THEN b.created_at ELSE b.retry_next_attempt_at END) FROM runner_delivery_explicit_delete_batches b JOIN runner_delivery_explicit_delete_files bf ON bf.batch_id=b.batch_id WHERE b.receipt_id=r.id AND bf.history_recorded=0 AND b.retry_next_attempt_at<=?),
		MIN(CASE WHEN f.cleanup_next_attempt_at=0 THEN f.updated_at ELSE f.cleanup_next_attempt_at END),r.sequence,r.id) LIMIT ?`, clientID, clientID, now, now, now, now, now, limit)
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

func (d *DB) DeliveryReceiptsNeedingSourceRefresh(ctx context.Context, clientID int64, limit int) ([]DeliveryReceipt, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT r.id,r.client_id,r.sequence,r.delivery_id,r.publication_generation,r.source_artifact_id,r.source_generation,r.manifest_digest,r.artifact_id,r.job_id,r.request_token,r.runner_instance_id,r.ready_at,r.publication_path,r.mapped_path,r.files_json,r.source_cleanup_json,r.terminal_outcome,r.failure_stage,r.failure_cause,r.keep_source_on_failure,r.file_outcomes_json,r.state,r.ack_state,r.consumer_receipt_id,r.download_id,r.generation_key,r.download_handle,r.media_item_id,r.copy_id,r.error,r.attempts,r.blocked_since,r.next_attempt_at,r.publication_directory_identity
		FROM runner_delivery_receipts r LEFT JOIN runner_delivery_source_refresh s ON s.receipt_id=r.id
		WHERE r.client_id=? AND r.state!='conflict' AND (r.ack_state='accepted' OR (r.ack_state='pending' AND r.consumer_receipt_id!='')) AND COALESCE(s.next_attempt_at,0)<=?
		ORDER BY COALESCE(s.next_attempt_at,r.source_cleanup_refreshed_at),r.sequence LIMIT ?`, clientID, time.Now().UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []DeliveryReceipt
	for rows.Next() {
		var v DeliveryReceipt
		if err := scanDeliveryReceipt(rows, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (d *DB) RefreshDeliverySourceCleanup(ctx context.Context, receiptID int64, sourceCleanup json.RawMessage, fileOutcomes []ports.RunnerFileOutcome, retryAfter time.Duration) error {
	if len(sourceCleanup) == 0 {
		sourceCleanup = json.RawMessage("null")
	}
	fileOutcomesJSON, err := json.Marshal(fileOutcomes)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `UPDATE runner_delivery_receipts SET source_cleanup_json=?,file_outcomes_json=?,source_cleanup_refreshed_at=?,updated_at=? WHERE id=?`, string(sourceCleanup), string(fileOutcomesJSON), now, now, receiptID)
	if err := requireDeliveryTransition(res, err); err != nil {
		return err
	}
	if retryAfter <= 0 {
		retryAfter = 5 * time.Minute
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO runner_delivery_source_refresh(receipt_id,attempts,blocked_since,next_attempt_at,last_error,updated_at)
		VALUES(?,0,0,?,'',?) ON CONFLICT(receipt_id) DO UPDATE SET attempts=0,blocked_since=0,next_attempt_at=MAX(excluded.next_attempt_at,runner_delivery_source_refresh.provider_retry_after),last_error='',updated_at=excluded.updated_at`, receiptID, now+retryAfter.Milliseconds(), now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) FailDeliverySourceRefresh(ctx context.Context, receiptID int64, message string, providerDeadline ...time.Time) error {
	now := time.Now()
	delay := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}
	next := now.Add(delay[0])
	if len(providerDeadline) > 0 && providerDeadline[0].After(next) {
		next = providerDeadline[0]
	}
	provider := int64(0)
	if next.After(now.Add(delay[0])) {
		provider = providerDeadline[0].UnixMilli()
	}
	res, err := d.W.ExecContext(ctx, `INSERT INTO runner_delivery_source_refresh(receipt_id,attempts,blocked_since,next_attempt_at,last_error,updated_at,provider_retry_after)
		VALUES(?,1,?,?,?, ?,?) ON CONFLICT(receipt_id) DO UPDATE SET attempts=attempts+1,
		blocked_since=CASE WHEN blocked_since=0 THEN excluded.blocked_since ELSE blocked_since END,
		next_attempt_at=MAX(excluded.next_attempt_at,excluded.provider_retry_after),provider_retry_after=MAX(provider_retry_after,excluded.provider_retry_after),last_error=excluded.last_error,updated_at=excluded.updated_at`,
		receiptID, now.UnixMilli(), next.UnixMilli(), message, now.UnixMilli(), provider)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	var attempts int
	if err := d.R.QueryRowContext(ctx, `SELECT attempts FROM runner_delivery_source_refresh WHERE receipt_id=?`, receiptID).Scan(&attempts); err != nil {
		return err
	}
	if attempts > 1 {
		wait := delay[min(attempts-1, len(delay)-1)]
		_, err = d.W.ExecContext(ctx, `UPDATE runner_delivery_source_refresh SET next_attempt_at=MAX(?,provider_retry_after),updated_at=? WHERE receipt_id=?`, now.Add(wait).UnixMilli(), now.UnixMilli(), receiptID)
	}
	return err
}

func (d *DB) DeliverySourceRefreshError(ctx context.Context, receiptID int64) (string, time.Time, error) {
	var message string
	var next int64
	err := d.R.QueryRowContext(ctx, `SELECT last_error,next_attempt_at FROM runner_delivery_source_refresh WHERE receipt_id=?`, receiptID).Scan(&message, &next)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, nil
	}
	return message, time.UnixMilli(next), err
}

func (d *DB) DeliverySourceProviderRetryAfter(ctx context.Context, receiptID int64) (time.Time, error) {
	var deadline int64
	err := d.R.QueryRowContext(ctx, `SELECT provider_retry_after FROM runner_delivery_source_refresh WHERE receipt_id=?`, receiptID).Scan(&deadline)
	if errors.Is(err, sql.ErrNoRows) || deadline == 0 {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(deadline), nil
}

// FindDownloadsForDelivery resolves the Runner-visible NZBd handle and the
// existing monarr-transfer token together. A job id alone is reusable and is
// not a sufficient association.
func (d *DB) FindDownloadsForDelivery(ctx context.Context, clientID int64, jobID, requestToken string) ([]Download, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id FROM downloads WHERE client_id=? AND handle=? AND transfer=? AND generation_key!='' ORDER BY id`, clientID, jobID, requestToken)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]Download, 0, len(ids))
	for _, id := range ids {
		dl, err := d.GetDownload(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, nil
}
