package sqlite

import (
	"context"
	"fmt"
	"time"
)

// FilePathRepair changes only a known file's location, keeping its identity,
// copy attribution, measured quality, release provenance, and episode links.
type FilePathRepair struct {
	ID               int64
	OldPath, NewPath string
}

// RepairItemFolder commits placement and matching file locations together.
// Compare the old paths inside the transaction so a stale form cannot undo
// a concurrent placement edit or move files that another operation changed.
func (d *DB) RepairItemFolder(ctx context.Context, id, rootID int64, oldPath, newPath string, files []FilePathRepair) error {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE media_items
		SET path = ?, root_folder_id = ?, updated_at = ? WHERE id = ? AND path = ?`,
		newPath, rootID, time.Now().UnixMilli(), id, oldPath)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("item location changed during repair; reload and try again")
	}
	for _, file := range files {
		result, err := tx.ExecContext(ctx, `UPDATE media_files SET path = ?
			WHERE id = ? AND media_item_id = ? AND path = ?`, file.NewPath, file.ID, id, file.OldPath)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("file location changed during repair; reload and try again")
		}
	}
	if _, err := d.Write.WithTx(tx).BumpIdentityRevision(ctx); err != nil {
		return err
	}
	return tx.Commit()
}
