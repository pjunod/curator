package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var ErrReplacementRequestConflict = errors.New("replacement request id was already used with different content")
var ErrReplacementScopeConflict = errors.New("an active replacement already owns part of this selected scope")
var ErrReplacementStorageIdentityConflict = errors.New("replacement storage identity conflicts with its durable pin")

// ReplacementOperation is an idempotent, durable replacement request. Payload
// preserves the validated request exactly; Activity is only a view of it.
type ReplacementOperation struct {
	ID                     string
	RequestID              string
	Fingerprint            string
	State                  string
	Intent                 string
	Scope                  string
	MediaItemID            int64
	CopyID                 int64
	SourceGeneration       string
	SourceTransfer         string
	SourceRelease          string
	SourceIndexer          string
	SourceGUID             string
	SourceIndexerID        int64
	BlacklistSourceRelease bool
	ReasonCode             string
	Reason                 string
	EpisodeIDs             []int64
	Result                 json.RawMessage
	LastError              string
	RetryAttempts          int
	BlockedSince           time.Time
	NextAttemptAt          time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

type ReplacementFileObservation struct {
	ID, RequestID, SourceGeneration, Path, StorageIdentity, State, SHA256, FileIdentity, FilesystemGeneration, Error string
	MediaItemID, MediaFileID, Size                                                                                   int64
	FileGeneration                                                                                                   string
	CreatedAt, UpdatedAt                                                                                             time.Time
}

type LibraryReviewFlag struct {
	ID, MediaItemID, MediaFileID int64
	FileGeneration               string
	ReasonCode, Reason, State    string
	CreatedAt, UpdatedAt         time.Time
}

type ReplacementFile struct {
	MediaFileID      int64
	FileGeneration   string
	SourceGeneration string
	ObservationID    string
	FileIdentity     string
	StorageIdentity  string
	Size             int64
	Path             string
	SHA256           string
	EpisodeIDs       []int64
	Disposition      string
	RemovalState     string
}

type MediaFileCustody struct {
	MediaItemID             int64
	ID                      int64
	FileGeneration          string
	SourceGeneration        string
	SourceTransfer          string
	SourcePayloadGeneration string
	PlacementID             string
	SHA256                  string
}

// ReplacementPlacement is a current library file whose exact generation was
// committed by a replacement operation. EpisodeIDs are the current links for
// that generation, not the historical links captured by the operation.
type ReplacementPlacement struct {
	MediaItemID    int64
	MediaFileID    int64
	CopyID         int64
	PlacementID    string
	FileGeneration string
	Path           string
	SHA256         string
	EpisodeIDs     []int64
}

func (d *DB) CurrentReplacementPlacements(ctx context.Context, operationID string) ([]ReplacementPlacement, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT f.media_item_id,f.id,coalesce(f.copy_id,0),p.placement_id,
		p.file_generation,p.path,p.sha256,coalesce(mfe.episode_id,0)
		FROM media_file_placements p JOIN media_files f ON f.id=p.media_file_id AND f.file_generation=p.file_generation
		LEFT JOIN media_file_episodes mfe ON mfe.media_file_id=f.id
		WHERE p.replacement_operation_id=? ORDER BY f.id,mfe.episode_id`, operationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ReplacementPlacement
	byID := map[int64]int{}
	for rows.Next() {
		var p ReplacementPlacement
		var episodeID int64
		if err = rows.Scan(&p.MediaItemID, &p.MediaFileID, &p.CopyID, &p.PlacementID, &p.FileGeneration, &p.Path, &p.SHA256, &episodeID); err != nil {
			return nil, err
		}
		idx, ok := byID[p.MediaFileID]
		if !ok {
			idx = len(out)
			byID[p.MediaFileID] = idx
			out = append(out, p)
		}
		if episodeID > 0 {
			out[idx].EpisodeIDs = append(out[idx].EpisodeIDs, episodeID)
		}
	}
	return out, rows.Err()
}

func (d *DB) GetMediaFileCustodyByPath(ctx context.Context, path string) (MediaFileCustody, error) {
	var out MediaFileCustody
	err := d.R.QueryRowContext(ctx, `SELECT f.media_item_id,f.id,f.file_generation,f.source_generation,f.source_transfer,f.source_payload_generation,coalesce(p.placement_id,''),coalesce(p.sha256,'') FROM media_files f LEFT JOIN media_file_placements p ON p.file_generation=f.file_generation WHERE f.path=?`, path).Scan(&out.MediaItemID, &out.ID, &out.FileGeneration, &out.SourceGeneration, &out.SourceTransfer, &out.SourcePayloadGeneration, &out.PlacementID, &out.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	return out, err
}

// CreateReplacementOperation atomically reserves the request id and snapshots
// every file identity the caller approved. Replays with changed content fail.
func (d *DB) CreateReplacementOperation(ctx context.Context, op ReplacementOperation, files []ReplacementFile) (ReplacementOperation, bool, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return op, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var oldFingerprint string
	err = tx.QueryRowContext(ctx, `SELECT fingerprint FROM replacement_operations WHERE request_id=?`, op.RequestID).Scan(&oldFingerprint)
	if err == nil {
		if oldFingerprint != op.Fingerprint {
			return op, false, ErrReplacementRequestConflict
		}
		stored, e := scanReplacementOperation(tx.QueryRowContext(ctx, replacementSelect+` WHERE request_id=?`, op.RequestID))
		if e != nil {
			return op, false, e
		}
		return stored, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return op, false, err
	}
	// Admission is serialized with request reservation. Distinct operations may
	// work on sibling episodes, but the same selected episode scope has one owner.
	activeRows, e := tx.QueryContext(ctx, `SELECT scope,episode_ids FROM replacement_operations WHERE media_item_id=? AND copy_id=? AND state NOT IN ('complete','cancelled','failed')`, op.MediaItemID, op.CopyID)
	if e != nil {
		return op, false, e
	}
	requested := map[int64]bool{}
	for _, id := range op.EpisodeIDs {
		requested[id] = true
	}
	for activeRows.Next() {
		var scope, episodeJSON string
		if e = activeRows.Scan(&scope, &episodeJSON); e != nil {
			_ = activeRows.Close()
			return op, false, e
		}
		var ids []int64
		_ = json.Unmarshal([]byte(episodeJSON), &ids)
		if scope == "download" || op.Scope == "download" {
			_ = activeRows.Close()
			return op, false, ErrReplacementScopeConflict
		}
		for _, id := range ids {
			if requested[id] {
				_ = activeRows.Close()
				return op, false, ErrReplacementScopeConflict
			}
		}
	}
	if e = activeRows.Err(); e != nil {
		_ = activeRows.Close()
		return op, false, e
	}
	_ = activeRows.Close()
	now := time.Now().UnixMilli()
	if op.CreatedAt.IsZero() {
		op.CreatedAt = time.Now()
	}
	op.UpdatedAt = op.CreatedAt
	if len(op.Result) == 0 {
		op.Result = json.RawMessage(`{}`)
	}
	ids, err := json.Marshal(op.EpisodeIDs)
	if err != nil {
		return op, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO replacement_operations(operation_id,request_id,fingerprint,state,intent,scope,media_item_id,copy_id,source_generation,source_transfer,source_release,source_indexer,source_guid,source_indexer_id,blacklist_source_release,reason_code,reason,episode_ids,result,last_error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		op.ID, op.RequestID, op.Fingerprint, op.State, op.Intent, op.Scope, op.MediaItemID, op.CopyID, op.SourceGeneration, op.SourceTransfer, op.SourceRelease, op.SourceIndexer, op.SourceGUID, op.SourceIndexerID, boolToInt(op.BlacklistSourceRelease), op.ReasonCode, op.Reason, string(ids), string(op.Result), op.LastError, now, now)
	if err != nil {
		return op, false, err
	}
	for _, f := range files {
		var currentGeneration, currentSource string
		if e := tx.QueryRowContext(ctx, `SELECT file_generation,source_generation FROM media_files WHERE id=? AND path=?`, f.MediaFileID, f.Path).Scan(&currentGeneration, &currentSource); e != nil {
			return op, false, e
		}
		if currentGeneration != f.FileGeneration || currentSource != f.SourceGeneration {
			return op, false, fmt.Errorf("replacement file identity changed before operation acceptance")
		}
		epIDs, e := json.Marshal(f.EpisodeIDs)
		if e != nil {
			return op, false, e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO replacement_operation_files(operation_id,media_file_id,file_generation,source_generation,path,sha256,episode_ids,disposition,removal_state,observation_id,file_identity,storage_identity,size_bytes) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, op.ID, f.MediaFileID, f.FileGeneration, f.SourceGeneration, f.Path, f.SHA256, string(epIDs), f.Disposition, f.RemovalState, f.ObservationID, f.FileIdentity, f.StorageIdentity, f.Size)
		if e != nil {
			return op, false, e
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO replacement_operation_steps(operation_id,step,state,started_at) VALUES(?,?,?,?)`, op.ID, "accepted", "complete", now); err != nil {
		return op, false, err
	}
	if op.BlacklistSourceRelease {
		if op.SourceGUID == "" || op.SourceIndexerID == 0 {
			return op, false, fmt.Errorf("exact source release identity is required to blacklist")
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO release_blacklist_identity(media_item_id,guid,indexer_id,reason,added_at) VALUES(?,?,?,?,?) ON CONFLICT(media_item_id,indexer_id,guid) DO UPDATE SET reason=excluded.reason`, op.MediaItemID, op.SourceGUID, op.SourceIndexerID, op.Reason, now); err != nil {
			return op, false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return op, false, err
	}
	op.CreatedAt = time.UnixMilli(now)
	op.UpdatedAt = op.CreatedAt
	return op, false, nil
}

const replacementSelect = `SELECT operation_id,request_id,fingerprint,state,intent,scope,media_item_id,copy_id,source_generation,source_transfer,source_release,source_indexer,source_guid,source_indexer_id,blacklist_source_release,reason_code,reason,episode_ids,result,last_error,created_at,updated_at,retry_attempts,blocked_since,next_attempt_at FROM replacement_operations`

type rowScanner interface{ Scan(...any) error }

func scanReplacementOperation(row rowScanner) (ReplacementOperation, error) {
	var op ReplacementOperation
	var blacklisted int
	var episodes, result string
	var created, updated int64
	var blockedSince, nextAttempt int64
	err := row.Scan(&op.ID, &op.RequestID, &op.Fingerprint, &op.State, &op.Intent, &op.Scope, &op.MediaItemID, &op.CopyID, &op.SourceGeneration, &op.SourceTransfer, &op.SourceRelease, &op.SourceIndexer, &op.SourceGUID, &op.SourceIndexerID, &blacklisted, &op.ReasonCode, &op.Reason, &episodes, &result, &op.LastError, &created, &updated, &op.RetryAttempts, &blockedSince, &nextAttempt)
	if err != nil {
		return op, err
	}
	op.BlacklistSourceRelease = blacklisted != 0
	_ = json.Unmarshal([]byte(episodes), &op.EpisodeIDs)
	op.Result = json.RawMessage(result)
	op.CreatedAt, op.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
	if blockedSince > 0 {
		op.BlockedSince = time.UnixMilli(blockedSince)
	}
	if nextAttempt > 0 {
		op.NextAttemptAt = time.UnixMilli(nextAttempt)
	}
	return op, nil
}

func (d *DB) GetReplacementOperation(ctx context.Context, id string) (ReplacementOperation, error) {
	op, err := scanReplacementOperation(d.R.QueryRowContext(ctx, replacementSelect+` WHERE operation_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return op, ErrNotFound
	}
	return op, err
}

func (d *DB) GetReplacementOperationByRequestID(ctx context.Context, requestID string) (ReplacementOperation, error) {
	op, err := scanReplacementOperation(d.R.QueryRowContext(ctx, replacementSelect+` WHERE request_id=?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return op, ErrNotFound
	}
	return op, err
}

func (d *DB) ListReplacementFiles(ctx context.Context, id string) ([]ReplacementFile, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT media_file_id,file_generation,source_generation,path,sha256,episode_ids,disposition,removal_state,observation_id,file_identity,storage_identity,size_bytes FROM replacement_operation_files WHERE operation_id=? ORDER BY media_file_id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ReplacementFile
	for rows.Next() {
		var f ReplacementFile
		var eps string
		if err = rows.Scan(&f.MediaFileID, &f.FileGeneration, &f.SourceGeneration, &f.Path, &f.SHA256, &eps, &f.Disposition, &f.RemovalState, &f.ObservationID, &f.FileIdentity, &f.StorageIdentity, &f.Size); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(eps), &f.EpisodeIDs)
		out = append(out, f)
	}
	return out, rows.Err()
}

// ReplacementHasCommittedCoverage checks the selected episode scope against
// placements committed by this exact operation. This survives Activity removal.
func (d *DB) ReplacementHasCommittedCoverage(ctx context.Context, op ReplacementOperation) (bool, error) {
	if op.Scope == "download" && len(op.EpisodeIDs) == 0 {
		var count int
		err := d.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_placements p JOIN media_files f
			ON f.id=p.media_file_id AND f.file_generation=p.file_generation
			WHERE p.replacement_operation_id=? AND f.media_item_id=? AND COALESCE(f.copy_id,0)=?`, op.ID, op.MediaItemID, op.CopyID).Scan(&count)
		return count > 0, err
	}
	for _, episodeID := range op.EpisodeIDs {
		var count int
		err := d.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_episodes mfe JOIN media_file_placements p ON p.media_file_id=mfe.media_file_id AND p.file_generation=(SELECT file_generation FROM media_files WHERE id=mfe.media_file_id) WHERE mfe.episode_id=? AND p.replacement_operation_id=?`, episodeID, op.ID).Scan(&count)
		if err != nil {
			return false, err
		}
		if count == 0 {
			return false, nil
		}
	}
	return true, nil
}

func (d *DB) ReplacementHasEpisodeCoverage(ctx context.Context, operationID string, episodeID int64) bool {
	var count int
	err := d.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_episodes mfe JOIN media_file_placements p ON p.media_file_id=mfe.media_file_id JOIN media_files f ON f.id=mfe.media_file_id AND f.file_generation=p.file_generation WHERE mfe.episode_id=? AND p.replacement_operation_id=?`, episodeID, operationID).Scan(&count)
	return err == nil && count > 0
}

func (d *DB) CreateLibraryReviewFlag(ctx context.Context, itemID, fileID int64, generation, reasonCode, reason, evidence string) error {
	if itemID <= 0 || fileID <= 0 || generation == "" || reasonCode == "" || reason == "" {
		return fmt.Errorf("item, file generation, reason code, and reason are required")
	}
	now := time.Now().UnixMilli()
	result, err := d.W.ExecContext(ctx, `INSERT INTO library_review_flags(media_item_id,media_file_id,file_generation,reason_code,reason,evidence,state,created_at,updated_at) SELECT ?,?,?,?,?,?,'open',?,? WHERE EXISTS (SELECT 1 FROM media_files WHERE id=? AND media_item_id=? AND file_generation=?) ON CONFLICT(media_file_id,file_generation) DO UPDATE SET reason_code=excluded.reason_code,reason=excluded.reason,evidence=excluded.evidence,updated_at=excluded.updated_at WHERE library_review_flags.state='open'`, itemID, fileID, generation, reasonCode, reason, evidence, now, now, fileID, itemID, generation)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DB) HasOpenMediaFileReviewFlag(ctx context.Context, mediaFileID int64, fileGeneration string) (bool, error) {
	var exists int
	err := d.R.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM library_review_flags WHERE media_file_id=? AND file_generation=? AND state='open')`, mediaFileID, fileGeneration).Scan(&exists)
	return exists != 0, err
}

func (d *DB) ListOpenMediaFileReviewFlags(ctx context.Context, itemID int64) ([]LibraryReviewFlag, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id,media_item_id,media_file_id,file_generation,reason_code,reason,state,created_at,updated_at
		FROM library_review_flags WHERE media_item_id=? AND state='open' ORDER BY updated_at DESC,id DESC`, itemID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LibraryReviewFlag
	for rows.Next() {
		var flag LibraryReviewFlag
		var created, updated int64
		if err = rows.Scan(&flag.ID, &flag.MediaItemID, &flag.MediaFileID, &flag.FileGeneration, &flag.ReasonCode, &flag.Reason, &flag.State, &created, &updated); err != nil {
			return nil, err
		}
		flag.CreatedAt, flag.UpdatedAt = time.UnixMilli(created), time.UnixMilli(updated)
		out = append(out, flag)
	}
	return out, rows.Err()
}

func (d *DB) ResolveMediaFileReviewFlag(ctx context.Context, itemID, flagID int64, fileGeneration string) error {
	result, err := d.W.ExecContext(ctx, `UPDATE library_review_flags SET state='resolved',updated_at=?
		WHERE id=? AND media_item_id=? AND file_generation=? AND state='open'`,
		time.Now().UnixMilli(), flagID, itemID, fileGeneration)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		var currentGeneration, state string
		if err := d.R.QueryRowContext(ctx, `SELECT file_generation,state FROM library_review_flags WHERE id=? AND media_item_id=?`, flagID, itemID).Scan(&currentGeneration, &state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if currentGeneration != fileGeneration {
			return ErrReplacementScopeConflict
		}
		if state != "open" {
			return ErrNotFound
		}
		return ErrReplacementScopeConflict
	}
	return nil
}

func (d *DB) ListReplacementOperationsForItem(ctx context.Context, itemID int64, limit int) ([]ReplacementOperation, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	rows, err := d.R.QueryContext(ctx, replacementSelect+` WHERE media_item_id=? ORDER BY updated_at DESC,created_at DESC LIMIT ?`, itemID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ReplacementOperation
	for rows.Next() {
		op, scanErr := scanReplacementOperation(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func (d *DB) ListOpenReplacementOperations(ctx context.Context, limit int) ([]ReplacementOperation, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := d.R.QueryContext(ctx, replacementSelect+` WHERE state IN ('accepted','removing','cleanup_blocked','searching','waiting_for_release','replacing') AND next_attempt_at<=? ORDER BY next_attempt_at,updated_at LIMIT ?`, time.Now().UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ReplacementOperation
	for rows.Next() {
		op, e := scanReplacementOperation(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func (d *DB) ListReplacementDownloads(ctx context.Context, operationID string) ([]Download, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id FROM downloads WHERE replacement_operation_id=? ORDER BY id`, operationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Download, 0, len(ids))
	for _, id := range ids {
		dl, e := d.GetDownload(ctx, id)
		if e != nil {
			return nil, e
		}
		out = append(out, dl)
	}
	return out, nil
}

func (d *DB) TransitionReplacementOperation(ctx context.Context, id, state, detail string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now()
	var attempts int
	var blockedSince int64
	if err = tx.QueryRowContext(ctx, `SELECT retry_attempts,blocked_since FROM replacement_operations WHERE operation_id=?`, id).Scan(&attempts, &blockedSince); err != nil {
		return err
	}
	next := int64(0)
	switch state {
	case "waiting_for_release", "cleanup_blocked":
		attempts++
		delay := 30 * time.Second
		switch attempts {
		case 2:
			delay = time.Minute
		case 3:
			delay = 2 * time.Minute
		case 4:
			delay = 5 * time.Minute
		default:
			if attempts > 4 {
				delay = 5 * time.Minute
			}
		}
		next = now.Add(delay).UnixMilli()
		if blockedSince == 0 {
			blockedSince = now.UnixMilli()
		}
	case "replacing":
		next = now.Add(30 * time.Second).UnixMilli()
	}
	if state == "searching" || state == "accepted" {
		attempts = 0
		blockedSince = 0
	}
	result, err := tx.ExecContext(ctx, `UPDATE replacement_operations SET state=?,last_error=?,updated_at=?,retry_attempts=?,blocked_since=?,next_attempt_at=? WHERE operation_id=?`, state, detail, now.UnixMilli(), attempts, blockedSince, next, id)
	if err != nil {
		return err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return e
		}
		return ErrNotFound
	}
	stepState := "complete"
	if detail != "" {
		stepState = "blocked"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO replacement_operation_steps(operation_id,step,state,error,started_at,ended_at,next_attempt_at) VALUES(?,?,?,?,?,?,?)`, id, state, stepState, detail, time.Now().UnixMilli(), time.Now().UnixMilli(), 0); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) StartReplacementFileObservation(ctx context.Context, observation ReplacementFileObservation) (ReplacementFileObservation, bool, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return observation, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var prior ReplacementFileObservation
	var priorCreated, priorUpdated int64
	err = tx.QueryRowContext(ctx, `SELECT observation_id,request_id,media_item_id,media_file_id,file_generation,source_generation,path,storage_identity,state,sha256,file_identity,size,error,created_at,updated_at,filesystem_generation FROM replacement_file_observations WHERE request_id=?`, observation.RequestID).Scan(&prior.ID, &prior.RequestID, &prior.MediaItemID, &prior.MediaFileID, &prior.FileGeneration, &prior.SourceGeneration, &prior.Path, &prior.StorageIdentity, &prior.State, &prior.SHA256, &prior.FileIdentity, &prior.Size, &prior.Error, &priorCreated, &priorUpdated, &prior.FilesystemGeneration)
	if err == nil {
		prior.CreatedAt = time.UnixMilli(priorCreated)
		prior.UpdatedAt = time.UnixMilli(priorUpdated)
		if prior.MediaFileID != observation.MediaFileID || prior.FileGeneration != observation.FileGeneration {
			return prior, false, ErrReplacementRequestConflict
		}
		return prior, true, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return observation, false, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT path,source_generation FROM media_files WHERE id=? AND media_item_id=? AND file_generation=?`, observation.MediaFileID, observation.MediaItemID, observation.FileGeneration).Scan(&observation.Path, &observation.SourceGeneration); err != nil {
		return observation, false, err
	}
	now := time.Now().UnixMilli()
	observation.CreatedAt = time.UnixMilli(now)
	observation.UpdatedAt = observation.CreatedAt
	observation.State = "pending"
	_, err = tx.ExecContext(ctx, `INSERT INTO replacement_file_observations(observation_id,request_id,media_item_id,media_file_id,file_generation,source_generation,path,storage_identity,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, observation.ID, observation.RequestID, observation.MediaItemID, observation.MediaFileID, observation.FileGeneration, observation.SourceGeneration, observation.Path, observation.StorageIdentity, observation.State, now, now)
	if err != nil {
		return observation, false, err
	}
	return observation, false, tx.Commit()
}

func scanReplacementObservation(row rowScanner) (ReplacementFileObservation, error) {
	var o ReplacementFileObservation
	var created, updated int64
	err := row.Scan(&o.ID, &o.RequestID, &o.MediaItemID, &o.MediaFileID, &o.FileGeneration, &o.SourceGeneration, &o.Path, &o.StorageIdentity, &o.State, &o.SHA256, &o.FileIdentity, &o.Size, &o.Error, &created, &updated, &o.FilesystemGeneration)
	if err == nil {
		o.CreatedAt = time.UnixMilli(created)
		o.UpdatedAt = time.UnixMilli(updated)
	}
	return o, err
}

const replacementObservationSelect = `SELECT observation_id,request_id,media_item_id,media_file_id,file_generation,source_generation,path,storage_identity,state,sha256,file_identity,size,error,created_at,updated_at,filesystem_generation FROM replacement_file_observations`

func (d *DB) GetReplacementFileObservation(ctx context.Context, id string) (ReplacementFileObservation, error) {
	o, err := scanReplacementObservation(d.R.QueryRowContext(ctx, replacementObservationSelect+` WHERE observation_id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	return o, err
}
func (d *DB) ListPendingReplacementFileObservations(ctx context.Context, limit int) ([]ReplacementFileObservation, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := d.R.QueryContext(ctx, replacementObservationSelect+` WHERE state='pending' ORDER BY created_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ReplacementFileObservation
	for rows.Next() {
		o, e := scanReplacementObservation(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
func (d *DB) FinishReplacementFileObservation(ctx context.Context, o ReplacementFileObservation) error {
	var result sql.Result
	var err error
	if o.State == "stale" {
		// A stale observation is specifically the durable record that the
		// selected library tuple stopped matching while the worker was hashing
		// or waiting in the queue. Require the immutable observation tuple, but
		// do not require the mutable media_files row to still exist. A deleted,
		// moved, or regenerated row must terminate this observation instead of
		// leaving it pending for every future worker sweep. Stale observations
		// never carry filesystem evidence.
		result, err = d.W.ExecContext(ctx, `UPDATE replacement_file_observations SET state='stale',sha256='',file_identity='',size=0,error=?,filesystem_generation='',updated_at=? WHERE observation_id=? AND state='pending' AND media_item_id=? AND media_file_id=? AND file_generation=? AND path=?`, o.Error, time.Now().UnixMilli(), o.ID, o.MediaItemID, o.MediaFileID, o.FileGeneration, o.Path)
	} else {
		result, err = d.W.ExecContext(ctx, `UPDATE replacement_file_observations SET state=?,sha256=?,file_identity=?,size=?,error=?,filesystem_generation=?,updated_at=? WHERE observation_id=? AND state='pending' AND media_item_id=? AND media_file_id=? AND file_generation=? AND path=? AND EXISTS(SELECT 1 FROM media_files WHERE id=? AND media_item_id=? AND file_generation=? AND path=?)`, o.State, o.SHA256, o.FileIdentity, o.Size, o.Error, o.FilesystemGeneration, time.Now().UnixMilli(), o.ID, o.MediaItemID, o.MediaFileID, o.FileGeneration, o.Path, o.MediaFileID, o.MediaItemID, o.FileGeneration, o.Path)
	}
	if err != nil {
		return err
	}
	n, e := result.RowsAffected()
	if e != nil {
		return e
	}
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// GetReplacementStorageIdentity returns the previously trusted identity for a
// library root. Pins are enrolled only by successful observations of present
// files; absence must never establish a new identity.
func (d *DB) GetReplacementStorageIdentity(ctx context.Context, root string) (string, error) {
	var identity string
	err := d.R.QueryRowContext(ctx, `SELECT storage_identity FROM replacement_storage_identity_pins WHERE root_path=?`, root).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return identity, err
}

// PinReplacementStorageIdentity atomically enrolls or verifies the mounted
// storage identity associated with a root. Existing pins are immutable.
func (d *DB) PinReplacementStorageIdentity(ctx context.Context, root, identity, observationID string) error {
	if root == "" || identity == "" || observationID == "" {
		return fmt.Errorf("replacement storage pin requires root, identity, and source observation")
	}
	_, err := d.W.ExecContext(ctx, `INSERT INTO replacement_storage_identity_pins(root_path,storage_identity,source_observation_id,pinned_at)
		VALUES(?,?,?,?) ON CONFLICT(root_path) DO NOTHING`, root, identity, observationID, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	pinned, err := d.GetReplacementStorageIdentity(ctx, root)
	if err != nil {
		return err
	}
	if pinned != identity {
		return ErrReplacementStorageIdentityConflict
	}
	return nil
}

// PriorVerifiedReplacementStorageIdentity is a migration bridge for a root
// that has a ready, hashed, present-file observation from before durable pins
// were introduced. It is scoped to the exact library row generation.
func (d *DB) PriorVerifiedReplacementStorageIdentity(ctx context.Context, fileID int64, fileGeneration string) (string, string, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT DISTINCT storage_identity,observation_id FROM replacement_file_observations
		WHERE media_file_id=? AND file_generation=? AND state='ready' AND sha256!='' AND storage_identity!=''`, fileID, fileGeneration)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = rows.Close() }()
	var identity, observationID string
	for rows.Next() {
		var candidate, candidateID string
		if err = rows.Scan(&candidate, &candidateID); err != nil {
			return "", "", err
		}
		if identity != "" && identity != candidate {
			return "", "", ErrReplacementStorageIdentityConflict
		}
		identity = candidate
		if observationID == "" || candidateID < observationID {
			observationID = candidateID
		}
	}
	if err = rows.Err(); err != nil {
		return "", "", err
	}
	if identity == "" {
		return "", "", ErrNotFound
	}
	return identity, observationID, nil
}

func (d *DB) SetReplacementFileRemovalState(ctx context.Context, opID string, f ReplacementFile, state string) error {
	result, err := d.W.ExecContext(ctx, `UPDATE replacement_operation_files SET removal_state=? WHERE operation_id=? AND media_file_id=? AND file_generation=? AND path=?`, state, opID, f.MediaFileID, f.FileGeneration, f.Path)
	if err != nil {
		return err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return e
		}
		return ErrNotFound
	}
	return nil
}

// DetachReplacementEpisodes removes only the selected bad mapping. It checks
// the immutable generation inside the same transaction as the link edit.
func (d *DB) DetachReplacementEpisodes(ctx context.Context, opID string, f ReplacementFile, selected []int64) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var generation, sourceGeneration string
	if err = tx.QueryRowContext(ctx, `SELECT file_generation,source_generation FROM media_files WHERE id=? AND path=?`, f.MediaFileID, f.Path).Scan(&generation, &sourceGeneration); err != nil {
		return err
	}
	if generation != f.FileGeneration || sourceGeneration != f.SourceGeneration {
		return fmt.Errorf("replacement file generation changed")
	}
	for _, epID := range selected {
		if _, err = tx.ExecContext(ctx, `DELETE FROM media_file_episodes WHERE media_file_id=? AND episode_id=?`, f.MediaFileID, epID); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE replacement_operation_files SET removal_state='removed' WHERE operation_id=? AND media_file_id=? AND file_generation=?`, opID, f.MediaFileID, f.FileGeneration)
	if err != nil {
		return err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return e
		}
		return ErrNotFound
	}
	return tx.Commit()
}

func (d *DB) DeleteReplacementFileRecord(ctx context.Context, opID string, f ReplacementFile) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var generation, sourceGeneration string
	if err = tx.QueryRowContext(ctx, `SELECT file_generation,source_generation FROM media_files WHERE id=? AND path=?`, f.MediaFileID, f.Path).Scan(&generation, &sourceGeneration); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	} else {
		if generation != f.FileGeneration || sourceGeneration != f.SourceGeneration {
			return fmt.Errorf("replacement file generation changed")
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM media_files WHERE id=? AND path=? AND file_generation=?`, f.MediaFileID, f.Path, f.FileGeneration); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE replacement_operation_files SET removal_state='removed' WHERE operation_id=? AND media_file_id=? AND file_generation=?`, opID, f.MediaFileID, f.FileGeneration)
	if err != nil {
		return err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return e
		}
		return ErrNotFound
	}
	return tx.Commit()
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
