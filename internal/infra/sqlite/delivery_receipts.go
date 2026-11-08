package sqlite

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

// DeliveryReceipt is Curator's durable record of one immutable Runner
// publication. It intentionally outlives the Activity/download row.
type DeliveryReceipt struct {
	OwnedByClientID              int64
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
	AbandonedAt                  time.Time
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
	UnlinkStarted    bool
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
		var abandoned int64
		if err := tx.QueryRowContext(ctx, `SELECT abandoned_at FROM runner_delivery_explicit_delete_batches WHERE batch_id=?`, batch.ID).Scan(&abandoned); err != nil {
			return err
		}
		if abandoned != 0 {
			return ErrDeliveryReceiptStale
		}
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
		var competing bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runner_delivery_explicit_delete_batches b JOIN runner_delivery_explicit_delete_files f ON f.batch_id=b.batch_id WHERE b.receipt_id=? AND b.batch_id!=? AND b.abandoned_at=0 AND f.path=? AND f.history_recorded=0)`, batch.ReceiptID, batch.ID, file.Path).Scan(&competing); err != nil {
			return err
		}
		if competing {
			return fmt.Errorf("another pending deletion request owns this file; cancel it before reviewing a new selection")
		}
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
	var next, blocked, abandoned int64
	err := d.R.QueryRowContext(ctx, `SELECT batch_id,receipt_id,payload_path,inventory_fingerprint,publication_directory_identity,storage_identity,generation_key,request_token,publication_generation,retry_attempts,retry_next_attempt_at,retry_blocked_since,retry_error,abandoned_at
		FROM runner_delivery_explicit_delete_batches WHERE batch_id=?`, batchID).
		Scan(&batch.ID, &batch.ReceiptID, &batch.PayloadPath, &batch.InventoryFingerprint, &batch.PublicationDirectoryIdentity, &batch.StorageIdentity, &batch.GenerationKey, &batch.RequestToken, &batch.PublicationGeneration, &batch.RetryAttempts, &next, &blocked, &batch.RetryError, &abandoned)
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
	if abandoned > 0 {
		batch.AbandonedAt = time.UnixMilli(abandoned)
	}
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
		WHERE b.receipt_id=? AND b.abandoned_at=0 AND f.history_recorded=0`
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
		WHERE b.receipt_id=? AND b.abandoned_at=0 AND EXISTS (
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
	rows, err := d.R.QueryContext(ctx, `SELECT path,reviewed_path,snapshot_identity,parent_identity,history_recorded,completed_at,unlink_started FROM runner_delivery_explicit_delete_files WHERE batch_id=? ORDER BY path`, batchID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var files []ExplicitDeliveryDeleteFile
	for rows.Next() {
		var file ExplicitDeliveryDeleteFile
		var history, started int
		var completed int64
		if err := rows.Scan(&file.Path, &file.ReviewedPath, &file.SnapshotIdentity, &file.ParentIdentity, &history, &completed, &started); err != nil {
			return nil, err
		}
		file.HistoryRecorded = history == 1
		file.UnlinkStarted = started == 1
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
	if _, err := tx.ExecContext(ctx, `UPDATE runner_delivery_explicit_delete_batches SET retry_attempts=retry_attempts+1,retry_next_attempt_at=?,retry_blocked_since=CASE WHEN retry_blocked_since=0 THEN ? ELSE retry_blocked_since END,retry_error=? WHERE batch_id=? AND abandoned_at=0`, next, now.UnixMilli(), message, batchID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_state=CASE WHEN cleanup_state='removed' THEN 'removed' ELSE 'retry' END,cleanup_error=?,cleanup_attempts=cleanup_attempts+1,cleanup_blocked_since=CASE WHEN cleanup_blocked_since=0 THEN ? ELSE cleanup_blocked_since END,cleanup_next_attempt_at=?,updated_at=?
		WHERE receipt_id=? AND path IN (SELECT f.path FROM runner_delivery_explicit_delete_files f JOIN runner_delivery_explicit_delete_batches b ON b.batch_id=f.batch_id WHERE f.batch_id=? AND f.history_recorded=0 AND b.abandoned_at=0)`, message, now.UnixMilli(), next, now.UnixMilli(), receiptID, batchID); err != nil {
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
		WHERE b.batch_id=? AND b.abandoned_at=0 AND f.path=?`, batchID, path).Scan(&batchReceipt, &historyRecorded)
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

// InterruptUnfinishedDeliveryAttempts marks attempts that an earlier process
// started and never finished. They stay unfinished — no finish time is
// invented and no outcome inferred — but they are no longer "running": the
// process that ran them is gone. Called once at process start.
func (d *DB) InterruptUnfinishedDeliveryAttempts(ctx context.Context, startedBefore time.Time) (int64, error) {
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_attempt_events SET result='interrupted',
		error=CASE WHEN error='' THEN 'Curator stopped before this attempt finished; its outcome is unknown' ELSE error END
		WHERE result='running' AND finished_at IS NULL AND started_at<?`, startedBefore.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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

func scanDeliveryReceipt(row interface{ Scan(...any) error }, out *DeliveryReceipt) error {
	var seq, next, readyAt, blockedSince int64
	var id, clientID, downloadID, mediaID, copyID, ownedByClientID int64
	var attempts int
	var deliveryID, generation, sourceArtifact, sourceGeneration, digest, artifact, job, token, runnerID, pubPath, mapped, filesRaw, cleanupRaw, terminalOutcome, failureStage, failureCause, fileOutcomesRaw, state, ack, consumer, genKey, handle, message, directoryIdentity string
	var stagesRaw, copiesRaw, timingRaw string
	var keepSourceOnFailure bool
	if err := row.Scan(&id, &clientID, &seq, &deliveryID, &generation, &sourceArtifact, &sourceGeneration, &digest, &artifact, &job, &token, &runnerID, &readyAt, &pubPath, &mapped, &filesRaw, &cleanupRaw, &terminalOutcome, &failureStage, &failureCause, &keepSourceOnFailure, &fileOutcomesRaw, &state, &ack, &consumer, &downloadID, &genKey, &handle, &mediaID, &copyID, &message, &attempts, &blockedSince, &next, &directoryIdentity, &stagesRaw, &copiesRaw, &timingRaw, &ownedByClientID); err != nil {
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
	var timing *ports.RunnerLifecycleTiming
	if err := json.Unmarshal([]byte(timingRaw), &timing); err != nil {
		return fmt.Errorf("decode Runner lifecycle timing: %w", err)
	}
	var stages *ports.RunnerStages
	var copies []ports.RunnerSourceCopy
	if err := json.Unmarshal([]byte(stagesRaw), &stages); err != nil {
		return fmt.Errorf("decode Runner stage snapshot: %w", err)
	}
	if err := json.Unmarshal([]byte(copiesRaw), &copies); err != nil {
		return fmt.Errorf("decode Runner source copies: %w", err)
	}
	ready := time.UnixMilli(readyAt)
	blocked := time.Time{}
	if blockedSince != 0 {
		blocked = time.UnixMilli(blockedSince)
	}
	*out = DeliveryReceipt{OwnedByClientID: ownedByClientID, ID: id, ClientID: clientID, Delivery: ports.RunnerDelivery{RunnerStages: stages, SourceCopies: copies, LifecycleTiming: timing, Sequence: uint64(seq), DeliveryID: deliveryID, PublicationGeneration: generation, SourceArtifactID: sourceArtifact, SourceGeneration: sourceGeneration, ManifestDigest: digest, ArtifactID: artifact, JobID: job, RequestToken: token, RunnerInstanceID: runnerID, PublicationPath: pubPath, Files: files, TerminalOutcome: terminalOutcome, FailureStage: failureStage, FailureCause: failureCause, KeepSourceOnFailure: keepSourceOnFailure, FileOutcomes: fileOutcomes, ReadyAt: ready, SourceCleanup: cleanup}, MappedPath: mapped, PublicationDirectoryIdentity: directoryIdentity, State: state, AckState: ack, ConsumerReceiptID: consumer, DownloadID: downloadID, GenerationKey: genKey, DownloadHandle: handle, MediaItemID: mediaID, CopyID: copyID, ReadyAt: ready, Error: message, Attempts: attempts, BlockedSince: blocked, NextAttemptAt: time.UnixMilli(next)}
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

func (d *DB) DeliveryReceiptByID(ctx context.Context, id int64) (DeliveryReceipt, error) {
	var v DeliveryReceipt
	err := scanDeliveryReceipt(d.R.QueryRowContext(ctx, `SELECT id,client_id,sequence,delivery_id,publication_generation,source_artifact_id,source_generation,manifest_digest,artifact_id,job_id,request_token,runner_instance_id,ready_at,publication_path,mapped_path,files_json,source_cleanup_json,terminal_outcome,failure_stage,failure_cause,keep_source_on_failure,file_outcomes_json,state,ack_state,consumer_receipt_id,download_id,generation_key,download_handle,media_item_id,copy_id,error,attempts,blocked_since,next_attempt_at,publication_directory_identity,runner_stages_json,source_copies_json,lifecycle_timing_json,owned_by_client_id FROM runner_delivery_receipts WHERE id=?`, id), &v)
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

func (d *DB) ForceDeliveryCleanupRetryNow(ctx context.Context, receiptID int64) error {
	now := time.Now().UnixMilli()
	_, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_explicit_delete_batches SET retry_next_attempt_at=0 WHERE receipt_id=? AND abandoned_at=0 AND EXISTS (SELECT 1 FROM runner_delivery_explicit_delete_files f WHERE f.batch_id=runner_delivery_explicit_delete_batches.batch_id AND f.history_recorded=0)`, receiptID)
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

func recordDeliveryFileImport(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, v DeliveryFileImport) error {
	if v.ReceiptID <= 0 || v.Path == "" || (v.ReplacementDisposition != "" && v.ReplacementDisposition != "selected" && v.ReplacementDisposition != "excess") {
		return fmt.Errorf("invalid delivery file import provenance")
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now()
	}
	_, err := q.ExecContext(ctx, `INSERT OR IGNORE INTO runner_delivery_file_imports
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

func commitDeliveryFileOutcome(ctx context.Context, tx *sql.Tx, v DeliveryFileImport, disposition, reason, libraryPath string) error {
	switch disposition {
	case "placed", "redundant", "disposable_selected_scope", "unselected_recoverable", "known_incidental", "unknown_recoverable":
	default:
		return fmt.Errorf("invalid delivery file disposition %q", disposition)
	}
	if v.ReceiptID <= 0 || v.Path == "" || (v.ReplacementDisposition != "" && v.ReplacementDisposition != "selected" && v.ReplacementDisposition != "excess") {
		return fmt.Errorf("invalid delivery file import provenance")
	}
	now := time.Now().UnixMilli()
	eligible := disposition == "placed" || disposition == "redundant" || disposition == "disposable_selected_scope"
	res, err := tx.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET disposition=?,reason=?,library_path=?,updated_at=?,
 cleanup_state=CASE WHEN ? AND cleanup_state='protected' AND cleanup_error='' THEN 'pending' ELSE cleanup_state END
 WHERE receipt_id=? AND path=? AND (disposition IN ('pending','unknown_recoverable') OR
 (disposition IN ('unselected_recoverable','known_incidental') AND ? IN ('known_incidental','placed','redundant','disposable_selected_scope')) OR
 (disposition=? AND library_path=?))`, disposition, reason, libraryPath, now, eligible, v.ReceiptID, v.Path, disposition, disposition, libraryPath)
	if err := requireDeliveryTransition(res, err); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `INSERT INTO runner_delivery_file_imports
 (receipt_id,path,source_generation,source_transfer,source_payload_generation,replacement_operation_id,replacement_disposition,placement_id,library_file_generation,library_file_identity,library_path,created_at)
 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(receipt_id,path,replacement_operation_id,placement_id,library_file_generation) DO UPDATE SET
 source_generation=excluded.source_generation,source_transfer=excluded.source_transfer,source_payload_generation=excluded.source_payload_generation,
 library_file_identity=excluded.library_file_identity,library_path=excluded.library_path
 WHERE (runner_delivery_file_imports.source_generation='' OR runner_delivery_file_imports.source_generation=excluded.source_generation)
 AND (runner_delivery_file_imports.source_transfer='' OR runner_delivery_file_imports.source_transfer=excluded.source_transfer)
 AND (runner_delivery_file_imports.source_payload_generation='' OR runner_delivery_file_imports.source_payload_generation=excluded.source_payload_generation)
 AND (runner_delivery_file_imports.library_file_identity='' OR runner_delivery_file_imports.library_file_identity=excluded.library_file_identity)
 AND (runner_delivery_file_imports.library_path='' OR runner_delivery_file_imports.library_path=excluded.library_path)`, v.ReceiptID, v.Path, v.SourceGeneration, v.SourceTransfer, v.SourcePayloadGeneration, v.ReplacementOperationID, v.ReplacementDisposition, v.PlacementID, v.LibraryFileGeneration, v.LibraryFileIdentity, v.LibraryPath, now)
	return requireDeliveryTransition(res, err)
}

// recordPlacementDeliveryOutcome joins the placement transaction to its exact
// accepted manifest row. Ordinary legacy/recovery placements have no matching
// Delivery and keep their existing contract.
func recordPlacementDeliveryOutcome(ctx context.Context, tx *sql.Tx, p Placement) error {
	if p.SourceGeneration == "" || p.SourceTransfer == "" || p.SourcePayloadGeneration == "" || p.SourceFileGeneration == "" {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.id,r.mapped_path,f.path FROM runner_delivery_receipts r
 JOIN runner_delivery_file_outcomes f ON f.receipt_id=r.id
 WHERE r.generation_key=? AND r.request_token=? AND r.publication_generation=? AND r.media_item_id=? AND r.copy_id=?
 AND f.file_generation=? AND f.sha256=? AND f.size_bytes=?`, p.SourceGeneration, p.SourceTransfer, p.SourcePayloadGeneration, p.ItemID, p.CopyID, p.SourceFileGeneration, p.SHA256, p.Size)
	if err != nil {
		return err
	}
	type match struct {
		id         int64
		root, path string
	}
	var matches []match
	for rows.Next() {
		var m match
		if err := rows.Scan(&m.id, &m.root, &m.path); err != nil {
			_ = rows.Close()
			return err
		}
		matches = append(matches, m)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, m := range matches {
		if filepath.Clean(p.Source) != filepath.Join(m.root, filepath.FromSlash(m.path)) {
			continue
		}
		replacementDisposition := ""
		if p.ReplacementOperationID != "" {
			replacementDisposition = "selected"
		}
		if err := commitDeliveryFileOutcome(ctx, tx, DeliveryFileImport{ReceiptID: m.id, Path: m.path, SourceGeneration: p.SourceGeneration, SourceTransfer: p.SourceTransfer, SourcePayloadGeneration: p.SourcePayloadGeneration, ReplacementOperationID: p.ReplacementOperationID, ReplacementDisposition: replacementDisposition, PlacementID: p.ID, LibraryFileGeneration: p.ID, LibraryFileIdentity: p.PublishedFileGeneration, LibraryPath: p.Target}, "placed", "placement and manifest provenance committed together", p.Target); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) BeginExplicitDeliveryFileUnlink(ctx context.Context, batchID, path string) error {
	res, err := d.W.ExecContext(ctx, `UPDATE runner_delivery_explicit_delete_files SET unlink_started=1 WHERE batch_id=? AND path=? AND history_recorded=0 AND EXISTS(SELECT 1 FROM runner_delivery_explicit_delete_batches b WHERE b.batch_id=? AND b.abandoned_at=0)`, batchID, path, batchID)
	return requireDeliveryTransition(res, err)
}

// AbandonExplicitDeliveryDelete cancels remaining deletion authority, preserving
// the immutable selection and every already-recorded outcome. It unlinks nothing.
func (d *DB) AbandonExplicitDeliveryDelete(ctx context.Context, receiptID int64, batchID string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var owner, abandoned int64
	if err := tx.QueryRowContext(ctx, `SELECT receipt_id,abandoned_at FROM runner_delivery_explicit_delete_batches WHERE batch_id=?`, batchID).Scan(&owner, &abandoned); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if owner != receiptID {
		return ErrDeliveryReceiptStale
	}
	if abandoned != 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE runner_delivery_explicit_delete_batches SET abandoned_at=?,retry_next_attempt_at=0,retry_error='operator cancelled remaining deletion request' WHERE batch_id=? AND abandoned_at=0`, now, batchID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_state='protected',cleanup_error='deletion request cancelled; review current files before another action',cleanup_next_attempt_at=0,updated_at=? WHERE receipt_id=? AND cleanup_state!='removed' AND path IN (SELECT path FROM runner_delivery_explicit_delete_files WHERE batch_id=? AND history_recorded=0)`, now, receiptID, batchID); err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]any{"receiptId": receiptID, "deletionRequestId": batchID, "remainingFilesRetained": true})
	if _, err := tx.ExecContext(ctx, `INSERT INTO history_events(ts,type,media_item_id,release_title,data) VALUES(?,'delivery_delete_request_cancelled',0,?,?)`, now, batchID, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}
