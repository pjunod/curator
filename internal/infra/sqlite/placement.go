package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	sqlitegen "github.com/pjunod/monarr/internal/infra/sqlite/gen"
)

// Placement is an intent persisted before a library rename. Its metadata is
// sufficient to reconcile a published file after a crash without recopying it.
type Placement struct {
	DownloadID              int64                `json:"download_id,omitempty"`
	SourceGeneration        string               `json:"source_generation,omitempty"`
	SourceTransfer          string               `json:"source_transfer,omitempty"`
	SourcePayloadGeneration string               `json:"source_payload_generation,omitempty"`
	SourceFileGeneration    string               `json:"source_file_generation,omitempty"`
	SourceGUID              string               `json:"source_guid,omitempty"`
	SourceIndexerID         int64                `json:"source_indexer_id,omitempty"`
	ReplacementOperationID  string               `json:"replacement_operation_id,omitempty"`
	CleanupSuperseded       bool                 `json:"cleanup_superseded,omitempty"`
	SupersededDigests       map[string]string    `json:"superseded_digests,omitempty"`
	Superseded              []domain.MediaFile   `json:"superseded,omitempty"`
	ID                      string               `json:"id"`
	Source                  string               `json:"source"`
	Target                  string               `json:"target"`
	Temporary               string               `json:"temporary"`
	Backup                  string               `json:"backup"`
	SHA256                  string               `json:"sha256"`
	PreviousSHA256          string               `json:"previous_sha256"`
	TargetObserved          bool                 `json:"target_observed,omitempty"`
	TargetFileGeneration    string               `json:"target_file_generation,omitempty"`
	TargetAfterBackupGen    string               `json:"target_after_backup_generation,omitempty"`
	TemporaryFileGeneration string               `json:"temporary_file_generation,omitempty"`
	TemporaryObjectIdentity string               `json:"temporary_object_identity,omitempty"`
	PublishedFileGeneration string               `json:"published_file_generation,omitempty"`
	StorageRoot             string               `json:"storage_root,omitempty"`
	StorageIdentity         string               `json:"storage_identity,omitempty"`
	StorageRootIdentity     string               `json:"storage_root_identity,omitempty"`
	TargetParentIdentity    string               `json:"target_parent_identity,omitempty"`
	Size                    int64                `json:"size"`
	ItemID                  int64                `json:"item_id"`
	CopyID                  int64                `json:"copy_id"`
	EpisodeIDs              []int64              `json:"episode_ids"`
	Quality                 quality.Quality      `json:"quality"`
	Info                    mediainfo.Info       `json:"info"`
	Provenance              mediainfo.Provenance `json:"provenance"`
	Confidence              mediainfo.Confidence `json:"confidence"`
	Release                 string               `json:"release"`
	Indexer                 string               `json:"indexer"`
	RecoveryImport          string               `json:"recovery_import,omitempty"`
	RecoveryFile            string               `json:"recovery_file,omitempty"`
	State                   string               `json:"state"`
}

var ErrPlacementGenerationChanged = errors.New("placement receipt no longer identifies the current library generation")

func (d *DB) PreparePlacement(ctx context.Context, p Placement) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = d.W.ExecContext(ctx, `INSERT INTO import_placements(id,target,state,data,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, p.ID, p.Target, p.State, string(raw), time.Now().UnixMilli())
	return err
}

// UpdatePreparedPlacement persists the filesystem identities collected during
// unlocked staging. These are immutable authority snapshots for the later
// short rename/metadata critical section.
func (d *DB) UpdatePreparedPlacement(ctx context.Context, p Placement) error {
	if p.ID == "" || p.State != "prepared" {
		return fmt.Errorf("prepared placement identity required")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	result, err := d.W.ExecContext(ctx, `UPDATE import_placements SET data=?,updated_at=? WHERE id=? AND state='prepared'`, string(raw), time.Now().UnixMilli(), p.ID)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil {
		return err
	} else if rows != 1 {
		return ErrPlacementGenerationChanged
	}
	return nil
}

func (d *DB) GetPlacement(ctx context.Context, id string) (Placement, error) {
	var raw string
	err := d.R.QueryRowContext(ctx, `SELECT data FROM import_placements WHERE id=?`, id).Scan(&raw)
	var p Placement
	if err != nil {
		return p, err
	}
	err = json.Unmarshal([]byte(raw), &p)
	return p, err
}
func (d *DB) PendingPlacements(ctx context.Context) ([]Placement, error) {
	return d.unresolvedPlacements(ctx, true)
}

// UnresolvedPlacementReferences is complete: destructive checks cannot use
// the bounded recovery-work batch as absence evidence.
func (d *DB) UnresolvedPlacementReferences(ctx context.Context) ([]Placement, error) {
	return d.unresolvedPlacements(ctx, false)
}
func (d *DB) unresolvedPlacements(ctx context.Context, batch bool) ([]Placement, error) {
	query := `SELECT data FROM import_placements WHERE state IN ('prepared','committed') ORDER BY updated_at`
	if batch {
		query += ` LIMIT 100`
	}
	rows, err := d.R.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Placement
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		var p Placement
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CommitPlacement couples library metadata, episode links and the per-file
// recovery result. The outbox is assembled exclusively from these receipts.
func (d *DB) CommitPlacement(ctx context.Context, p Placement) (int64, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	if err = tx.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE id=?`, p.ID).Scan(&state); err != nil {
		return 0, err
	}
	if state == "committed" || state == "cleaned" {
		var fid int64
		var generation, sourceGeneration, sourceTransfer, sourcePayloadGeneration, operationID, sourceFileGeneration, path, digest string
		err = tx.QueryRowContext(ctx, `SELECT media_file_id,file_generation,source_generation,source_transfer,source_payload_generation,
			replacement_operation_id,source_file_generation,path,sha256 FROM media_file_placements WHERE placement_id=?`, p.ID).Scan(
			&fid, &generation, &sourceGeneration, &sourceTransfer, &sourcePayloadGeneration, &operationID, &sourceFileGeneration, &path, &digest)
		if err != nil {
			return 0, err
		}
		if sourceGeneration != p.SourceGeneration || sourceTransfer != p.SourceTransfer || sourcePayloadGeneration != p.SourcePayloadGeneration ||
			operationID != p.ReplacementOperationID || sourceFileGeneration != p.SourceFileGeneration || path != p.Target || digest != p.SHA256 {
			return 0, ErrPlacementGenerationChanged
		}
		var currentID int64
		var currentGeneration string
		err = tx.QueryRowContext(ctx, `SELECT id,file_generation FROM media_files WHERE id=? AND path=? AND media_item_id=? AND COALESCE(copy_id,0)=?`, fid, p.Target, p.ItemID, p.CopyID).Scan(&currentID, &currentGeneration)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, ErrPlacementGenerationChanged
			}
			return 0, err
		}
		if currentID != fid || currentGeneration != generation {
			return 0, ErrPlacementGenerationChanged
		}
		return fid, tx.Commit()
	}
	if p.RecoveryImport != "" {
		if err = checkRecoveryRevision(ctx, tx, p.RecoveryImport); err != nil {
			return 0, err
		}
	}
	if p.ReplacementOperationID != "" {
		var intent, scope, rawEpisodes string
		e := tx.QueryRowContext(ctx, `SELECT intent,scope,episode_ids FROM replacement_operations WHERE operation_id=?`, p.ReplacementOperationID).Scan(&intent, &scope, &rawEpisodes)
		if e != nil {
			return 0, e
		}
		var allowedEpisodes []int64
		if e = json.Unmarshal([]byte(rawEpisodes), &allowedEpisodes); e != nil {
			return 0, e
		}
		allowed := map[int64]bool{}
		for _, id := range allowedEpisodes {
			allowed[id] = true
		}
		if scope == "episodes" || len(allowedEpisodes) > 0 {
			if len(p.EpisodeIDs) == 0 {
				return 0, fmt.Errorf("replacement placement has no selected episode scope")
			}
			for _, id := range p.EpisodeIDs {
				if !allowed[id] {
					return 0, fmt.Errorf("replacement placement exceeds selected episode scope")
				}
			}
		}
		rows, e := tx.QueryContext(ctx, `SELECT media_file_id,file_generation,path,episode_ids,disposition,removal_state FROM replacement_operation_files WHERE operation_id=?`, p.ReplacementOperationID)
		if e != nil {
			return 0, e
		}
		matched := 0
		coveredEpisodes := map[int64]bool{}
		for rows.Next() {
			var fileID int64
			var generation, path, rawFileEpisodes, disposition, removal string
			if e = rows.Scan(&fileID, &generation, &path, &rawFileEpisodes, &disposition, &removal); e != nil {
				_ = rows.Close()
				return 0, e
			}
			var fileEpisodes []int64
			if e = json.Unmarshal([]byte(rawFileEpisodes), &fileEpisodes); e != nil {
				_ = rows.Close()
				return 0, e
			}
			relevant := len(p.EpisodeIDs) == 0 && len(fileEpisodes) == 0
			for _, target := range p.EpisodeIDs {
				for _, source := range fileEpisodes {
					if target == source {
						relevant = true
						coveredEpisodes[target] = true
					}
				}
			}
			if !relevant {
				continue
			}
			matched++
			if intent == "replace_bad" && disposition == "selected" && removal != "removed" {
				_ = rows.Close()
				return 0, fmt.Errorf("bad replacement source has not completed its approved cleanup")
			}
			if disposition == "preserved" {
				continue
			}
			var currentID int64
			var currentGeneration string
			e = tx.QueryRowContext(ctx, `SELECT id,file_generation FROM media_files WHERE id=? AND path=?`, fileID, path).Scan(&currentID, &currentGeneration)
			if removal == "removed" {
				if e == nil && currentID == fileID && currentGeneration == generation {
					_ = rows.Close()
					return 0, fmt.Errorf("replacement source file reappeared after removal: %s", path)
				}
				if e != nil && !errors.Is(e, sql.ErrNoRows) {
					_ = rows.Close()
					return 0, e
				}
			} else if intent == "replace_bad" {
				if e != nil {
					_ = rows.Close()
					return 0, fmt.Errorf("replacement source file disappeared before commit: %w", e)
				}
				if currentID != fileID || currentGeneration != generation {
					_ = rows.Close()
					return 0, fmt.Errorf("replacement source generation changed before commit")
				}
			}
		}
		if e = rows.Err(); e != nil {
			_ = rows.Close()
			return 0, e
		}
		_ = rows.Close()
		if matched == 0 {
			return 0, fmt.Errorf("replacement placement is outside the snapshotted source scope")
		}
		for _, episodeID := range p.EpisodeIDs {
			if !coveredEpisodes[episodeID] {
				return 0, fmt.Errorf("replacement placement episode %d is outside the snapshotted source scope", episodeID)
			}
		}
	}
	q := d.Write.WithTx(tx)
	fid, err := q.UpsertMediaFile(ctx, sqlitegen.UpsertMediaFileParams{MediaItemID: sql.NullInt64{Int64: p.ItemID, Valid: p.ItemID != 0}, CopyID: sql.NullInt64{Int64: p.CopyID, Valid: p.CopyID != 0}, Path: p.Target, Size: p.Size, AddedAt: time.Now().UnixMilli()})
	if err != nil {
		return 0, err
	}
	if err = q.UpdateMediaFileCopy(ctx, sqlitegen.UpdateMediaFileCopyParams{ID: fid, CopyID: sql.NullInt64{Int64: p.CopyID, Valid: p.CopyID != 0}}); err != nil {
		return 0, err
	}
	if err = q.ClearFileEpisodeLinks(ctx, fid); err != nil {
		return 0, err
	}
	for _, ep := range p.EpisodeIDs {
		if err = q.LinkFileEpisode(ctx, sqlitegen.LinkFileEpisodeParams{MediaFileID: fid, EpisodeID: ep}); err != nil {
			return 0, err
		}
	}
	if err = q.SetMediaFileSource(ctx, sqlitegen.SetMediaFileSourceParams{ID: fid, SourceRelease: p.Release, SourceIndexer: p.Indexer}); err != nil {
		return 0, err
	}
	fileGeneration := p.ID
	result, err := tx.ExecContext(ctx, `UPDATE media_files SET file_generation=?,source_generation=?,source_transfer=?,source_payload_generation=?,source_file_generation=?,source_guid=?,source_indexer_id=? WHERE id=?`, fileGeneration, p.SourceGeneration, p.SourceTransfer, p.SourcePayloadGeneration, p.SourceFileGeneration, p.SourceGUID, p.SourceIndexerID, fid)
	if err != nil {
		return 0, err
	}
	if n, e := result.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return 0, e
		}
		return 0, ErrNotFound
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO media_file_placements(placement_id,media_file_id,file_generation,source_generation,source_transfer,source_payload_generation,source_guid,source_indexer_id,replacement_operation_id,source_file_generation,path,sha256,committed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.ID, fid, fileGeneration, p.SourceGeneration, p.SourceTransfer, p.SourcePayloadGeneration, p.SourceGUID, p.SourceIndexerID, p.ReplacementOperationID, p.SourceFileGeneration, p.Target, p.SHA256, time.Now().UnixMilli()); err != nil {
		return 0, err
	}
	if err = q.SetFileQualityWithProvenance(ctx, sqlitegen.SetFileQualityWithProvenanceParams{ID: fid, Quality: p.Quality.String(), QualityProvenance: string(p.Provenance), QualityConfidence: string(p.Confidence)}); err != nil {
		return 0, err
	}
	info, err := json.Marshal(p.Info)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE media_files SET media_info=?,audio_languages=?,probed_at=? WHERE id=?`, string(info), audioLanguagesColumn(p.Info), time.Now().UnixMilli(), fid); err != nil {
		return 0, err
	}
	p.State = "committed"
	raw, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE import_placements SET state='committed',data=?,updated_at=? WHERE id=?`, string(raw), time.Now().UnixMilli(), p.ID); err != nil {
		return 0, err
	}
	if p.RecoveryImport != "" {
		receipt, err := json.Marshal(map[string]any{"id": p.RecoveryFile, "bytes": p.Size, "sha256": p.SHA256, "result": "imported"})
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO recovery_file_results(import_id,file_id,result) VALUES(?,?,?) ON CONFLICT(import_id,file_id) DO UPDATE SET result=excluded.result`, p.RecoveryImport, p.RecoveryFile, string(receipt)); err != nil {
			return 0, err
		}
	}
	if p.RecoveryImport != "" {
		if err = advanceRecoveryRevision(ctx, tx, p.RecoveryImport); err != nil {
			return 0, err
		}
	}
	return fid, tx.Commit()
}

func (d *DB) LibraryRevision(ctx context.Context) (int64, error) {
	var revision int64
	err := d.R.QueryRowContext(ctx, `SELECT revision FROM lifecycle_library_revision WHERE id=1`).Scan(&revision)
	return revision, err
}
func checkRecoveryRevision(ctx context.Context, tx *sql.Tx, id string) error {
	var matches bool
	if err := tx.QueryRowContext(ctx, `SELECT revision=json_extract(data,'$.accepted_revision') FROM lifecycle_library_revision,recovery_imports WHERE lifecycle_library_revision.id=1 AND recovery_imports.id=?`, id).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return fmt.Errorf("library changed during recovery; a fresh preview is required")
	}
	return nil
}
func advanceRecoveryRevision(ctx context.Context, tx *sql.Tx, id string) error {
	_, err := tx.ExecContext(ctx, `UPDATE recovery_imports SET data=json_set(data,'$.accepted_revision',(SELECT revision FROM lifecycle_library_revision WHERE id=1)) WHERE id=?`, id)
	return err
}

// DeleteRecoverySuperseded preserves the revision lease while retiring an old
// file row. Filesystem removal has already succeeded; a failed CAS keeps the
// old row visible for reconciliation instead of overwriting another edit.
func (d *DB) DeleteRecoverySuperseded(ctx context.Context, fileID int64, importID string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = checkRecoveryRevision(ctx, tx, importID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM media_files WHERE id=?`, fileID); err != nil {
		return err
	}
	if err = advanceRecoveryRevision(ctx, tx, importID); err != nil {
		return err
	}
	return tx.Commit()
}
