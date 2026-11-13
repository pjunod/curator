package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

var ErrScanChanged = errors.New("library changed while scanning; retain records and retry")

type ScanRootBinding struct {
	RootID         int64
	Path, Identity string
	Generation     int64
}
type ScannedLibraryFile struct {
	Path         string
	Size, CopyID int64
	EpisodeIDs   []int64
}
type ScanResult struct {
	FileIDs         map[string]int64
	FileGenerations map[string]string
	Linked, Removed int
}

// ApplyLibraryScan atomically reconciles exactly the title snapshot that was
// walked. An import, target edit or root rebind during IO rejects the whole write.
func (d *DB) ApplyLibraryScan(ctx context.Context, item domain.MediaItem, revision, shared int64, bindings []ScanRootBinding, prune map[int64]bool, files []ScannedLibraryFile) (ScanResult, error) {
	result := ScanResult{FileIDs: map[string]int64{}, FileGenerations: map[string]string{}}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	var current, settings int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision FROM lifecycle_item_revision WHERE media_item_id=?),0),revision FROM lifecycle_shared_revision WHERE id=1`, item.ID).Scan(&current, &settings); err != nil {
		return result, err
	}
	if current != revision || settings != shared {
		return result, ErrScanChanged
	}
	var busy bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM rematch_operations WHERE (source_item_id=? OR target_item_id=?) AND state NOT IN ('complete','cancelled','failed')) OR EXISTS(SELECT 1 FROM offload_operations WHERE media_item_id=? AND state NOT IN ('complete','cancelled','failed')) OR EXISTS(SELECT 1 FROM replacement_operations WHERE media_item_id=? AND state NOT IN ('complete','cancelled','failed'))`, item.ID, item.ID, item.ID, item.ID).Scan(&busy); err != nil {
		return result, err
	}
	if busy {
		return result, ErrScanChanged
	}
	for _, b := range bindings {
		if b.RootID == 0 || b.Identity == "" {
			continue
		}
		var path string
		if err = tx.QueryRowContext(ctx, `SELECT path FROM root_folders WHERE id=?`, b.RootID).Scan(&path); err != nil {
			return result, err
		}
		if path != b.Path {
			return result, ErrScanChanged
		}
		var identity string
		var generation int64
		err = tx.QueryRowContext(ctx, `SELECT identity,generation FROM library_presence_roots WHERE root_id=? AND root_path=?`, b.RootID, b.Path).Scan(&identity, &generation)
		if err != nil && err != sql.ErrNoRows {
			return result, err
		}
		if b.Generation == 0 {
			if err != sql.ErrNoRows {
				return result, ErrScanChanged
			}
		} else if err != nil || identity != b.Identity || generation != b.Generation {
			return result, ErrScanChanged
		}
	}
	old := map[string]domain.MediaFile{}
	for _, f := range item.Files {
		old[f.Path] = f
	}
	for _, f := range files {
		var id int64
		err = tx.QueryRowContext(ctx, `INSERT INTO media_files(media_item_id,copy_id,path,size,added_at) VALUES(?,NULLIF(?,0),?,?,?) ON CONFLICT(path) DO UPDATE SET media_item_id=excluded.media_item_id,size=excluded.size WHERE media_files.media_item_id=excluded.media_item_id OR media_files.media_item_id IS NULL RETURNING id`, item.ID, f.CopyID, f.Path, f.Size, time.Now().UnixMilli()).Scan(&id)
		if err == sql.ErrNoRows {
			return result, ErrScanChanged
		}
		if err != nil {
			return result, err
		}
		result.FileIDs[f.Path] = id
		var generation string
		if err = tx.QueryRowContext(ctx, `SELECT file_generation FROM media_files WHERE id=?`, id).Scan(&generation); err != nil {
			return result, err
		}
		result.FileGenerations[f.Path] = generation
		if _, found := old[f.Path]; !found {
			result.Linked++
		}
		if item.Kind == domain.KindBook {
			if _, err = tx.ExecContext(ctx, `UPDATE media_files SET copy_id=NULLIF(?,0) WHERE id=? AND media_item_id=?`, f.CopyID, id, item.ID); err != nil {
				return result, err
			}
		}
		if len(f.EpisodeIDs) > 0 {
			// Imported links are explicit acquisition/user decisions. Filename hints
			// can fill unlinked files, but cannot overwrite an imported mapping.
			prior, known := old[f.Path]
			if !known || prior.SourceGeneration == "" || len(prior.EpisodeIDs) == 0 {
				if _, err = tx.ExecContext(ctx, `DELETE FROM media_file_episodes WHERE media_file_id=?`, id); err != nil {
					return result, err
				}
				for _, episode := range f.EpisodeIDs {
					if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO media_file_episodes(media_file_id,episode_id) SELECT ?,id FROM episodes WHERE id=? AND media_item_id=?`, id, episode, item.ID); err != nil {
						return result, err
					}
				}
			}
		}
	}
	for path, f := range old {
		if _, exists := result.FileIDs[path]; exists || !prune[f.CopyID] {
			continue
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM media_files WHERE id=? AND media_item_id=? AND COALESCE(copy_id,0)=? AND path=? AND size=? AND file_generation=?`, f.ID, item.ID, f.CopyID, f.Path, f.Size, f.FileGeneration)
		if err != nil {
			return result, err
		}
		count, err := res.RowsAffected()
		if err != nil {
			return result, err
		}
		if count != 1 {
			return result, fmt.Errorf("%w: file identity", ErrScanChanged)
		}
		result.Removed++
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}
