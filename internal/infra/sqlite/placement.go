package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	sqlitegen "github.com/pjunod/monarr/internal/infra/sqlite/gen"
)

// Placement is an intent persisted before a library rename. Its metadata is
// sufficient to reconcile a published file after a crash without recopying it.
type Placement struct {
	ID             string               `json:"id"`
	Source         string               `json:"source"`
	Target         string               `json:"target"`
	Temporary      string               `json:"temporary"`
	Backup         string               `json:"backup"`
	SHA256         string               `json:"sha256"`
	PreviousSHA256 string               `json:"previous_sha256"`
	Size           int64                `json:"size"`
	ItemID         int64                `json:"item_id"`
	CopyID         int64                `json:"copy_id"`
	EpisodeIDs     []int64              `json:"episode_ids"`
	Quality        quality.Quality      `json:"quality"`
	Info           mediainfo.Info       `json:"info"`
	Provenance     mediainfo.Provenance `json:"provenance"`
	Confidence     mediainfo.Confidence `json:"confidence"`
	Release        string               `json:"release"`
	Indexer        string               `json:"indexer"`
	RecoveryImport string               `json:"recovery_import,omitempty"`
	RecoveryFile   string               `json:"recovery_file,omitempty"`
	State          string               `json:"state"`
}

func (d *DB) PreparePlacement(ctx context.Context, p Placement) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = d.W.ExecContext(ctx, `INSERT INTO import_placements(id,target,state,data,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, p.ID, p.Target, p.State, string(raw), time.Now().UnixMilli())
	return err
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
	rows, err := d.R.QueryContext(ctx, `SELECT data FROM import_placements WHERE state<>'committed' ORDER BY updated_at LIMIT 100`)
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
	if err = q.SetFileQualityWithProvenance(ctx, sqlitegen.SetFileQualityWithProvenanceParams{ID: fid, Quality: p.Quality.String(), QualityProvenance: string(p.Provenance), QualityConfidence: string(p.Confidence)}); err != nil {
		return 0, err
	}
	info, err := json.Marshal(p.Info)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE media_files SET media_info=?,probed_at=? WHERE id=?`, string(info), time.Now().UnixMilli(), fid); err != nil {
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
	return fid, tx.Commit()
}
