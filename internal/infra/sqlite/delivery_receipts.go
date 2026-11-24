package sqlite

import (
	"context"
	"database/sql"
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
	err := d.R.QueryRowContext(ctx, `SELECT deliveries_after FROM runner_history_cursors WHERE client_id=?`, clientID).Scan(&cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return uint64(cursor), err
}

func (d *DB) SetDeliveryCursor(ctx context.Context, clientID int64, cursor uint64) error {
	_, err := d.W.ExecContext(ctx, `INSERT INTO runner_history_cursors(client_id,deliveries_after,updated_at,history_read) VALUES(?,?,?,0) ON CONFLICT(client_id) DO UPDATE SET deliveries_after=excluded.deliveries_after,updated_at=excluded.updated_at`, clientID, cursor, time.Now().UnixMilli())
	return err
}

func (d *DB) EnableRunnerDeliveryV1(ctx context.Context, clientID int64) error {
	_, err := d.W.ExecContext(ctx, `INSERT INTO runner_client_instances(client_id,runner_instance_id,delivery_v1_enabled,updated_at) VALUES(?,'',1,?) ON CONFLICT(client_id) DO UPDATE SET delivery_v1_enabled=1,updated_at=excluded.updated_at`, clientID, time.Now().UnixMilli())
	return err
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
