package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/quality"
)

var ErrPresenceRootChanged = errors.New("library volume identity changed; owner review required")

type PresenceObservation struct {
	ItemID, CopyID, ItemRevision, SharedRevision, RootID, RootGeneration int64
	RootPath, RootIdentity, ScopePath                                    string
	CheckedAt                                                            time.Time
	PresentFileIDs                                                       []int64
}

func (d *DB) PresenceRevision(ctx context.Context, id int64) (int64, int64, error) {
	var item, shared int64
	err := d.R.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision FROM lifecycle_item_revision WHERE media_item_id=?),0),revision FROM lifecycle_shared_revision WHERE id=1`, id).Scan(&item, &shared)
	return item, shared, err
}
func (d *DB) InvalidatePresence(ctx context.Context, id int64) error {
	_, err := d.W.ExecContext(ctx, `UPDATE library_presence_observations SET valid=0 WHERE media_item_id=?`, id)
	return err
}
func (d *DB) PresenceRootBinding(ctx context.Context, id int64, path string) (string, int64, error) {
	var identity string
	var generation int64
	err := d.R.QueryRowContext(ctx, `SELECT identity,generation FROM library_presence_roots WHERE root_id=? AND root_path=?`, id, path).Scan(&identity, &generation)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	return identity, generation, err
}
func (d *DB) PresenceRootGeneration(ctx context.Context, id int64, path, identity string) (int64, error) {
	expected, generation, err := d.PresenceRootBinding(ctx, id, path)
	if err != nil {
		return 0, err
	}
	if generation > 0 && expected != identity {
		return 0, ErrPresenceRootChanged
	}
	return generation, nil
}
func (d *DB) CheckPresenceRoot(ctx context.Context, id int64, path, identity string) error {
	_, err := d.PresenceRootGeneration(ctx, id, path, identity)
	return err
}

func (d *DB) SavePresence(ctx context.Context, o PresenceObservation) error {
	if o.RootIdentity == "" {
		return ErrPresenceRootChanged
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var item, shared int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision FROM lifecycle_item_revision WHERE media_item_id=?),0),revision FROM lifecycle_shared_revision WHERE id=1`, o.ItemID).Scan(&item, &shared); err != nil {
		return err
	}
	if item != o.ItemRevision || shared != o.SharedRevision {
		return ErrScanChanged
	}
	var root string
	if err = tx.QueryRowContext(ctx, `SELECT path FROM root_folders WHERE id=?`, o.RootID).Scan(&root); err != nil {
		return err
	}
	if root != o.RootPath {
		return ErrScanChanged
	}
	var identity string
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT identity,generation FROM library_presence_roots WHERE root_id=? AND root_path=?`, o.RootID, o.RootPath).Scan(&identity, &generation)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if generation != o.RootGeneration || generation > 0 && identity != o.RootIdentity {
		return ErrPresenceRootChanged
	}
	var fingerprint string
	if err = tx.QueryRowContext(ctx, `SELECT `+presenceFingerprintSQL+` FROM media_items m WHERE m.id=?`, o.ItemID).Scan(&fingerprint); err != nil {
		return err
	}
	ids := o.PresentFileIDs
	if ids == nil {
		ids = []int64{}
	}
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	if generation == 0 {
		// Only pre-upgrade records may bootstrap trust, never files a scan of
		// this untrusted volume inserted. New roots are explicitly pinned on add.
		var positive bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM library_presence_bootstrap_files b JOIN media_files f ON f.id=b.file_id JOIN json_each(?) x ON x.value=f.id WHERE b.media_item_id=? AND b.copy_id=? AND f.media_item_id=b.media_item_id AND COALESCE(f.copy_id,0)=b.copy_id AND f.path=b.path AND f.size=b.size AND f.file_generation=b.file_generation)`, string(data), o.ItemID, o.CopyID).Scan(&positive); err != nil {
			return err
		}
		if !positive {
			return fmt.Errorf("library volume needs affirmative legacy evidence or owner verification")
		}
		generation = 1
		if _, err = tx.ExecContext(ctx, `INSERT INTO library_presence_roots(root_id,root_path,identity,generation) VALUES(?,?,?,?)`, o.RootID, o.RootPath, o.RootIdentity, generation); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO library_presence_observations(media_item_id,copy_id,item_revision,presence_fingerprint,shared_revision,root_id,root_path,root_identity,root_generation,scope_path,checked_at,valid,present_file_ids) VALUES(?,?,?,?,?,?,?,?,?,?,?,1,?) ON CONFLICT(media_item_id,copy_id) DO UPDATE SET item_revision=excluded.item_revision,presence_fingerprint=excluded.presence_fingerprint,shared_revision=excluded.shared_revision,root_id=excluded.root_id,root_path=excluded.root_path,root_identity=excluded.root_identity,root_generation=excluded.root_generation,scope_path=excluded.scope_path,checked_at=excluded.checked_at,valid=1,present_file_ids=excluded.present_file_ids`, o.ItemID, o.CopyID, o.ItemRevision, fingerprint, o.SharedRevision, o.RootID, o.RootPath, o.RootIdentity, generation, o.ScopePath, o.CheckedAt.UnixMilli(), string(data))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Adding a root is an explicit owner configuration action on this volume.
func (d *DB) AddRootFolderWithPresence(ctx context.Context, path string, kind domain.RootKind, identity string) (domain.RootFolder, error) {
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return domain.RootFolder{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now()
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO root_folders(path,kind,added_at) VALUES(?,?,?) RETURNING id`, path, string(kind), now.UnixMilli()).Scan(&id)
	if err != nil {
		if isConstraint(err) {
			return domain.RootFolder{}, ErrDuplicate
		}
		return domain.RootFolder{}, err
	}
	if identity != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO library_presence_roots(root_id,root_path,identity,generation) VALUES(?,?,?,1)`, id, path, identity); err != nil {
			return domain.RootFolder{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return domain.RootFolder{}, err
	}
	return domain.RootFolder{ID: id, Path: path, Kind: kind, AddedAt: now}, nil
}
func (d *DB) SetScanFileQuality(ctx context.Context, id, itemID, size int64, generation string, q quality.Quality) error {
	_, err := d.W.ExecContext(ctx, `UPDATE media_files SET quality=?,quality_provenance='filename',quality_confidence='none' WHERE id=? AND media_item_id=? AND size=? AND file_generation=? AND quality_provenance IN ('','unknown','filename')`, q.String(), id, itemID, size, generation)
	return err
}
