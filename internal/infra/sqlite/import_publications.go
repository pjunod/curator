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

type ImportPublication struct {
	ImportAttemptKey
	ID          string `json:"id"`
	PlacementID string `json:"placementId,omitempty"`
	ReceiptID   int64  `json:"receiptId,omitempty"`
	Source      string `json:"source"`
	Target      string `json:"target"`
	Payload     string `json:"-"`
	Outcome     string `json:"outcome"`
	Resolution  string `json:"resolution,omitempty"`
}

func (d *DB) AssertPublicationTargetAvailable(ctx context.Context, target string) error {
	var retained int
	if err := d.W.QueryRowContext(ctx, `SELECT count(*) FROM import_publication_authorizations WHERE target=? AND resolution='retained_for_review'`, target).Scan(&retained); err != nil {
		return err
	}
	if retained > 0 {
		return ErrImportReviewRequired
	}
	return nil
}

func (d *DB) unresolvedSidecarReferences(ctx context.Context) ([]Placement, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT publication_id,download_id,generation_key,source,target,COALESCE(json_extract(payload,'$.stage'),'') FROM import_publication_authorizations WHERE placement_id IS NULL AND (outcome='pending' OR resolution IN ('retained_for_review','operator_adopted'))`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var refs []Placement
	for rows.Next() {
		var p Placement
		if err = rows.Scan(&p.ID, &p.DownloadID, &p.SourceGeneration, &p.Source, &p.Target, &p.Temporary); err != nil {
			return nil, err
		}
		p.State = "retained"
		refs = append(refs, p)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	stages, err := d.R.QueryContext(ctx, `SELECT download_id,generation_key,stage,source,target FROM import_attempt_stages WHERE state='held'`)
	if err != nil {
		return nil, err
	}
	defer stages.Close()
	for stages.Next() {
		var p Placement
		if err = stages.Scan(&p.DownloadID, &p.SourceGeneration, &p.Temporary, &p.Source, &p.Target); err != nil {
			return nil, err
		}
		p.State = "retained"
		refs = append(refs, p)
	}
	return refs, stages.Err()
}

func placementPublication(p Placement) (ImportPublication, error) {
	if p.ImportEpoch == nil || p.DownloadID == 0 {
		return ImportPublication{}, ErrImportAttemptChanged
	}
	data, err := json.Marshal(map[string]any{
		"source": p.Source, "target": p.Target, "sha256": p.SHA256, "size": p.Size,
		"temporary": p.Temporary, "temporaryIdentity": p.TemporaryObjectIdentity,
		"targetGeneration": p.TargetFileGeneration, "storage": p.StorageIdentity,
		"parent": p.TargetParentIdentity, "item": p.ItemID, "copy": p.CopyID,
		"sourceGeneration": p.SourceGeneration, "sourceTransfer": p.SourceTransfer,
		"payloadGeneration": p.SourcePayloadGeneration, "sourceFileGeneration": p.SourceFileGeneration,
		"replacement": p.ReplacementOperationID, "epoch": *p.ImportEpoch,
		"episodes": p.EpisodeIDs, "quality": p.Quality, "info": p.Info, "provenance": p.Provenance, "confidence": p.Confidence,
		"release": p.Release, "indexer": p.Indexer, "previousSHA256": p.PreviousSHA256, "backup": p.Backup,
		"sourceGUID": p.SourceGUID, "sourceIndexer": p.SourceIndexerID,
	})
	return ImportPublication{ImportAttemptKey: ImportAttemptKey{p.DownloadID, p.SourceGeneration, *p.ImportEpoch},
		ID: "placement:" + p.ID, PlacementID: p.ID, Source: p.Source, Target: p.Target, Payload: string(data)}, err
}

func authorizeImportPublication(ctx context.Context, tx *sql.Tx, p ImportPublication) error {
	a, err := guardImportAttempt(ctx, tx, p.ImportAttemptKey)
	if err != nil {
		return err
	}
	if a.Outcome != "" {
		return ErrImportCancelled
	}
	if !a.PlanSealed {
		return ErrImportPlanChanged
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM import_attempt_files WHERE download_id=? AND generation_key=? AND attempt_epoch=? AND source=? AND outcome='pending'`, p.DownloadID, p.GenerationKey, p.Epoch, p.Source).Scan(&pending); err != nil {
		return err
	}
	if pending != 1 {
		return ErrImportPlanChanged
	}
	var retained int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM import_publication_authorizations WHERE target=? AND resolution='retained_for_review'`, p.Target).Scan(&retained); err != nil {
		return err
	}
	if retained > 0 {
		return ErrImportReviewRequired
	}
	var placement, receipt any
	if p.PlacementID != "" {
		placement = p.PlacementID
	}
	if p.ReceiptID != 0 {
		receipt = p.ReceiptID
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO import_publication_authorizations(download_id,generation_key,attempt_epoch,publication_id,placement_id,receipt_id,source,target,payload,authorized_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, p.DownloadID, p.GenerationKey, p.Epoch, p.ID, placement, receipt, p.Source, p.Target, p.Payload, time.Now().UnixMilli()); err != nil {
		return err
	}
	var source, target, payload, outcome string
	var pid sql.NullString
	var rid sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT source,target,payload,outcome,placement_id,receipt_id FROM import_publication_authorizations WHERE download_id=? AND generation_key=? AND attempt_epoch=? AND publication_id=?`, p.DownloadID, p.GenerationKey, p.Epoch, p.ID).Scan(&source, &target, &payload, &outcome, &pid, &rid); err != nil {
		return err
	}
	if source != p.Source || target != p.Target || payload != p.Payload || pid.String != p.PlacementID || rid.Int64 != p.ReceiptID {
		return ErrImportAttemptChanged
	}
	if outcome != "pending" {
		return ErrImportAttemptChanged
	}
	return nil
}

func (d *DB) AuthorizeImportPlacement(ctx context.Context, p Placement) error {
	publication, err := placementPublication(p)
	if err != nil {
		return err
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var raw, state string
	if err = tx.QueryRowContext(ctx, `SELECT data,state FROM import_placements WHERE id=?`, p.ID).Scan(&raw, &state); err != nil {
		return err
	}
	var prepared Placement
	if err = json.Unmarshal([]byte(raw), &prepared); err != nil {
		return err
	}
	snapshot, err := placementPublication(prepared)
	if err != nil {
		return err
	}
	if state != "prepared" || snapshot.Payload != publication.Payload {
		return ErrPlacementGenerationChanged
	}
	if err = authorizeImportPublication(ctx, tx, publication); err != nil {
		return err
	}
	return tx.Commit()
}

func commitImportPublication(ctx context.Context, tx *sql.Tx, p ImportPublication) error {
	if _, err := guardImportAttempt(ctx, tx, p.ImportAttemptKey); err != nil {
		return err
	}
	var source, target, payload, outcome string
	if err := tx.QueryRowContext(ctx, `SELECT source,target,payload,outcome FROM import_publication_authorizations WHERE download_id=? AND generation_key=? AND attempt_epoch=? AND publication_id=?`, p.DownloadID, p.GenerationKey, p.Epoch, p.ID).Scan(&source, &target, &payload, &outcome); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrImportAttemptChanged
		}
		return err
	}
	if source != p.Source || target != p.Target || payload != p.Payload || (outcome != "pending" && outcome != "committed") {
		return ErrImportAttemptChanged
	}
	if _, err := tx.ExecContext(ctx, `UPDATE import_publication_authorizations SET outcome='committed',resolved_at=COALESCE(resolved_at,?) WHERE download_id=? AND generation_key=? AND attempt_epoch=? AND publication_id=? AND outcome IN ('pending','committed')`, time.Now().UnixMilli(), p.DownloadID, p.GenerationKey, p.Epoch, p.ID); err != nil {
		return err
	}
	return resolveImportFile(ctx, tx, p.ImportAttemptKey, p.Source, "committed", p.PlacementID)
}

// RetainUncertainImport is called with both local TryLocks held and explicit
// operator fencing acknowledgement. It revokes publication, not its history.
// No absence observation or inferred lock result grants cleanup authority.
func (d *DB) RetainUncertainImport(ctx context.Context, key ImportAttemptKey, ids []string) (ImportAttempt, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return ImportAttempt{}, err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := guardImportAttempt(ctx, tx, key)
	if err != nil {
		return a, err
	}
	if a.Outcome != "pending" || a.PendingPublications == 0 {
		return a, ErrImportPublicationPending
	}
	selected := map[string]bool{}
	for _, id := range ids {
		if selected[id] {
			return a, ErrImportAttemptChanged
		}
		selected[id] = true
	}
	rows, e := tx.QueryContext(ctx, `SELECT publication_id FROM import_publication_authorizations WHERE download_id=? AND generation_key=? AND outcome='pending'`, key.DownloadID, key.GenerationKey)
	if e != nil {
		return a, e
	}
	count := 0
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return a, e
		}
		if !selected[id] {
			rows.Close()
			return a, ErrImportAttemptChanged
		}
		count++
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return a, e
	}
	if count != len(selected) {
		return a, ErrImportAttemptChanged
	}
	now := time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `UPDATE import_publication_authorizations SET outcome='reconciled',resolved_at=?,resolution='retained_for_review' WHERE download_id=? AND generation_key=? AND outcome='pending'`, now, key.DownloadID, key.GenerationKey); err != nil {
		return a, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE import_attempt_controls SET outcome='cancelled',finalized_at=? WHERE download_id=? AND generation_key=? AND attempt_epoch=? AND outcome='pending'`, now, key.DownloadID, key.GenerationKey, key.Epoch); err != nil {
		return a, err
	}
	if err = writeImportState(ctx, tx, key, "downloaded", "import_cancelled", "cancelled after explicit fencing review; uncertain source/stage/publication custody retained"); err != nil {
		return a, err
	}
	a.Outcome = "cancelled"
	a.RetainedPublications += a.PendingPublications
	a.PendingPublications = 0
	return a, tx.Commit()
}

func (d *DB) ImportPublications(ctx context.Context, id int64, generation string) ([]ImportPublication, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT download_id,generation_key,attempt_epoch,publication_id,COALESCE(placement_id,''),COALESCE(receipt_id,0),source,target,outcome,resolution FROM import_publication_authorizations WHERE download_id=? AND generation_key=? ORDER BY attempt_epoch,publication_id`, id, generation)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var publications []ImportPublication
	for rows.Next() {
		var p ImportPublication
		if err = rows.Scan(&p.DownloadID, &p.GenerationKey, &p.Epoch, &p.ID, &p.PlacementID, &p.ReceiptID, &p.Source, &p.Target, &p.Outcome, &p.Resolution); err != nil {
			return nil, err
		}
		publications = append(publications, p)
	}
	return publications, rows.Err()
}

// ResolveRetainedImportTargets records an explicit Library review/disposition.
// The caller acknowledges each displayed publication ID. This does not perform
// filesystem cleanup and never reactivates a revoked authorization.
func (d *DB) ResolveRetainedImportTargets(ctx context.Context, key ImportAttemptKey, ids []string, disposition string) error {
	if disposition != "operator_adopted" && disposition != "operator_disposed" {
		return fmt.Errorf("explicit Library adoption or operator disposition required")
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, err := guardImportAttempt(ctx, tx, key)
	if err != nil {
		return err
	}
	if a.Outcome == "pending" || a.PendingPublications > 0 {
		return ErrImportPublicationPending
	}
	if len(ids) == 0 {
		return ErrImportReviewRequired
	}
	for _, id := range ids {
		if strings.HasPrefix(id, "stage:") || strings.HasPrefix(id, "stagepath:") {
			if disposition != "operator_disposed" {
				return ErrImportReviewRequired
			}
			var result sql.Result
			if stage, ok := strings.CutPrefix(id, "stagepath:"); ok {
				result, err = tx.ExecContext(ctx, `UPDATE import_attempt_stages SET state='operator_disposed' WHERE download_id=? AND generation_key=? AND stage=? AND state='held' AND NOT EXISTS(SELECT 1 FROM import_publication_authorizations a WHERE json_extract(a.payload,'$.stage')=? AND (a.outcome='pending' OR a.resolution IN ('retained_for_review','operator_adopted')))`, key.DownloadID, key.GenerationKey, stage, stage)
			} else {
				pid := strings.TrimPrefix(id, "stage:")
				result, err = tx.ExecContext(ctx, `UPDATE import_placements SET state='abandoned',data=json_set(data,'$.state','abandoned'),updated_at=? WHERE id=? AND json_extract(data,'$.download_id')=? AND json_extract(data,'$.source_generation')=? AND state IN ('prepared','retained') AND NOT EXISTS(SELECT 1 FROM import_publication_authorizations a WHERE a.placement_id=? AND (a.outcome='pending' OR a.resolution IN ('retained_for_review','operator_adopted')))`, time.Now().UnixMilli(), pid, key.DownloadID, key.GenerationKey, pid)
			}
			if err != nil {
				return err
			}
			n, e := result.RowsAffected()
			if e != nil {
				return e
			}
			if n != 1 {
				return ErrImportAttemptChanged
			}
			continue
		}
		var placement sql.NullString
		var target, mediaPath string
		if err = tx.QueryRowContext(ctx, `SELECT placement_id,target,COALESCE(json_extract(payload,'$.mediaPath'),target) FROM import_publication_authorizations WHERE download_id=? AND generation_key=? AND publication_id=? AND outcome='reconciled' AND resolution IN ('retained_for_review','operator_adopted')`, key.DownloadID, key.GenerationKey, id).Scan(&placement, &target, &mediaPath); err != nil {
			return err
		}
		if disposition == "operator_adopted" {
			var owned int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM media_files f JOIN downloads d ON d.media_item_id=f.media_item_id AND COALESCE(d.copy_id,0)=COALESCE(f.copy_id,0) WHERE d.id=? AND d.generation_key=? AND f.path=? AND f.file_generation!=''`, key.DownloadID, key.GenerationKey, mediaPath).Scan(&owned); err != nil {
				return err
			}
			if owned != 1 {
				return ErrImportReviewRequired
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE import_publication_authorizations SET resolution=? WHERE download_id=? AND generation_key=? AND publication_id=? AND outcome='reconciled' AND resolution IN ('retained_for_review','operator_adopted')`, disposition, key.DownloadID, key.GenerationKey, id); err != nil {
			return err
		}
		if disposition == "operator_disposed" {
			if _, err = tx.ExecContext(ctx, `UPDATE import_attempt_stages SET state='operator_disposed' WHERE stage=(SELECT json_extract(payload,'$.stage') FROM import_publication_authorizations WHERE download_id=? AND generation_key=? AND publication_id=?) AND state='held'`, key.DownloadID, key.GenerationKey, id); err != nil {
				return err
			}
		}
		if placement.Valid {
			state := "retained"
			if disposition == "operator_disposed" {
				state = "abandoned"
			}
			if _, err = tx.ExecContext(ctx, `UPDATE import_placements SET state=?,data=json_set(data,'$.state',?),updated_at=? WHERE id=? AND state IN ('prepared','retained')`, state, state, time.Now().UnixMilli(), placement.String); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// AuthorizeImportSidecar participates in the same cancellation ordering as a
// media rename. Receipt association is checked inside the write transaction.
func (d *DB) AuthorizeImportSidecar(ctx context.Context, p ImportPublication) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var matches int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM runner_delivery_receipts WHERE id=? AND download_id=? AND generation_key=? AND state!='conflict'`, p.ReceiptID, p.DownloadID, p.GenerationKey).Scan(&matches); err != nil {
		return err
	}
	if matches != 1 || p.PlacementID != "" {
		return ErrImportAttemptChanged
	}
	if err = authorizeImportPublication(ctx, tx, p); err != nil {
		return err
	}
	return tx.Commit()
}

// CommitImportSidecar couples the publication result with the delivery ledger;
// cancellation cannot finalize between the external link and its bookkeeping.
func (d *DB) CommitImportSidecar(ctx context.Context, p ImportPublication, v DeliveryFileImport) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if p.ReceiptID != v.ReceiptID || p.Target != v.LibraryPath {
		return ErrImportAttemptChanged
	}
	if err = commitImportPublication(ctx, tx, p); err != nil {
		return err
	}
	if err = recordDeliveryFileImport(ctx, tx, v); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE import_attempt_stages SET state='released' WHERE download_id=? AND generation_key=? AND attempt_epoch=? AND stage=json_extract(?,'$.stage') AND state='held'`, p.DownloadID, p.GenerationKey, p.Epoch, p.Payload); err != nil {
		return err
	}

	result, err := tx.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET disposition='placed',library_path=?,cleanup_state='pending',reason='',updated_at=? WHERE receipt_id=? AND path=?`, v.LibraryPath, time.Now().UnixMilli(), v.ReceiptID, v.Path)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrImportPlanChanged
	}
	return tx.Commit()
}

func (d *DB) RecordImportStage(ctx context.Context, key ImportAttemptKey, source, target, stage string) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	a, err := guardImportAttempt(ctx, tx, key)
	if err != nil {
		return err
	}
	if a.Outcome != "" {
		return ErrImportCancelled
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO import_attempt_stages(download_id,generation_key,attempt_epoch,stage,source,target) VALUES(?,?,?,?,?,?)`, key.DownloadID, key.GenerationKey, key.Epoch, stage, source, target)
	if err != nil {
		return err
	}
	return tx.Commit()
}
