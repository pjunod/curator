package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

type PresenceRootState struct {
	RootID           int64  `json:"rootId"`
	Path             string `json:"path"`
	ExpectedIdentity string `json:"expectedIdentity"`
	Generation       int64  `json:"generation"`
	SharedRevision   int64  `json:"sharedRevision"`
	LibraryRevision  int64  `json:"libraryRevision"`
	FileRecords      int    `json:"fileRecords"`
	ActiveWork       int    `json:"activeWork"`
}
type presenceQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readPresenceRootState(ctx context.Context, q presenceQuerier, id int64) (PresenceRootState, error) {
	v := PresenceRootState{}
	err := q.QueryRowContext(ctx, `SELECT r.id,r.path,COALESCE(p.identity,''),COALESCE(p.generation,0),s.revision,l.revision,
 (SELECT COUNT(*) FROM media_files f JOIN media_items m ON m.id=f.media_item_id LEFT JOIN media_copies c ON c.id=f.copy_id WHERE COALESCE(NULLIF(c.root_folder_id,0),m.root_folder_id)=r.id),
 (SELECT COUNT(*) FROM downloads d JOIN media_items m ON m.id=d.media_item_id WHERE (m.root_folder_id=r.id OR EXISTS(SELECT 1 FROM media_copies c WHERE c.media_item_id=m.id AND c.root_folder_id=r.id)) AND ((d.superseded=0 AND d.state IN('planned','submitting','grabbed','downloading','held','downloaded','awaiting_import','importing') AND NOT EXISTS(SELECT 1 FROM import_attempt_controls c WHERE c.download_id=d.id AND c.generation_key=d.generation_key AND c.outcome='cancelled')) OR EXISTS(SELECT 1 FROM import_attempt_stages a WHERE a.download_id=d.id AND a.state='held') OR EXISTS(SELECT 1 FROM import_placements a WHERE json_extract(a.data,'$.download_id')=d.id AND a.state IN('prepared','retained')) OR EXISTS(SELECT 1 FROM import_publication_authorizations a WHERE a.download_id=d.id AND (a.outcome='pending' OR a.resolution='retained_for_review'))))+
 (SELECT COUNT(*) FROM import_placements a JOIN media_items m ON m.id=json_extract(a.data,'$.item_id') WHERE COALESCE(json_extract(a.data,'$.download_id'),0)=0 AND a.state IN('prepared','retained') AND (m.root_folder_id=r.id OR EXISTS(SELECT 1 FROM media_copies c WHERE c.media_item_id=m.id AND c.root_folder_id=r.id)))+
 (SELECT COUNT(*) FROM offload_operations o JOIN media_items m ON m.id=o.media_item_id WHERE (m.root_folder_id=r.id OR EXISTS(SELECT 1 FROM media_copies c WHERE c.media_item_id=m.id AND c.root_folder_id=r.id)) AND o.state NOT IN('complete','cancelled','failed'))+
 (SELECT COUNT(*) FROM rematch_operations o JOIN media_items m ON m.id IN(o.source_item_id,o.target_item_id) WHERE (m.root_folder_id=r.id OR EXISTS(SELECT 1 FROM media_copies c WHERE c.media_item_id=m.id AND c.root_folder_id=r.id)) AND o.state NOT IN('complete','cancelled','failed'))+
 (SELECT COUNT(*) FROM replacement_operations o JOIN media_items m ON m.id=o.media_item_id WHERE (m.root_folder_id=r.id OR EXISTS(SELECT 1 FROM media_copies c WHERE c.media_item_id=m.id AND c.root_folder_id=r.id)) AND o.state NOT IN('complete','cancelled','failed'))
 FROM root_folders r LEFT JOIN library_presence_roots p ON p.root_id=r.id AND p.root_path=r.path CROSS JOIN lifecycle_shared_revision s CROSS JOIN lifecycle_library_revision l WHERE r.id=? AND s.id=1 AND l.id=1`, id).Scan(&v.RootID, &v.Path, &v.ExpectedIdentity, &v.Generation, &v.SharedRevision, &v.LibraryRevision, &v.FileRecords, &v.ActiveWork)
	if err == sql.ErrNoRows {
		return v, ErrNotFound
	}
	return v, err
}
func (d *DB) PresenceRootState(ctx context.Context, id int64) (PresenceRootState, error) {
	return readPresenceRootState(ctx, d.R, id)
}
func (d *DB) BindPresenceRoot(ctx context.Context, expected PresenceRootState, identity string) error {
	if identity == "" {
		return fmt.Errorf("volume identity unavailable")
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := readPresenceRootState(ctx, tx, expected.RootID)
	if err != nil {
		return err
	}
	if current != expected {
		return ErrScanChanged
	}
	if current.ActiveWork > 0 {
		return fmt.Errorf("active library work still owns this volume")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO library_presence_roots(root_id,root_path,identity,generation) VALUES(?,?,?,?) ON CONFLICT(root_id,root_path) DO UPDATE SET identity=excluded.identity,generation=excluded.generation`, current.RootID, current.Path, identity, current.Generation+1)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE library_presence_observations SET valid=0 WHERE root_id=?`, current.RootID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE lifecycle_shared_revision SET revision=revision+1 WHERE id=1`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1`); err != nil {
		return err
	}
	return tx.Commit()
}
