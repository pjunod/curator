package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// restoreActivityPrior recovers a missing Library target from its journaled
// prior link. Link is exclusive, so a new/external target is never overwritten.
func (s *Service) restoreActivityPrior(ctx context.Context, p sqlite.Placement) error {
	parent := filepath.Dir(p.Target)
	if p.PreviousSHA256 == "" || p.Backup != filepath.Join(parent, ".monarr-prior-"+p.ID) {
		return fmt.Errorf("the prior Library backup still needs placement recovery")
	}
	storage, identity, err := placementParentIdentity(p.Target)
	if err != nil {
		return err
	}
	if storage != p.StorageIdentity || identity != p.TargetParentIdentity {
		return fmt.Errorf("reconnect the original Library drive before recovering its prior file")
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	bound, err := root.Stat(".")
	if err != nil || deliveryDirectoryObjectIdentity(bound) != p.TargetParentIdentity {
		return fmt.Errorf("library parent changed during recovery")
	}
	backup, err := root.Lstat(filepath.Base(p.Backup))
	if err != nil || !backup.Mode().IsRegular() || backup.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("the prior Library backup is unavailable or changed")
	}
	digest, _, err := fileDigest(ctx, p.Backup)
	if err != nil {
		return err
	}
	if digest != p.PreviousSHA256 {
		return fmt.Errorf("the prior Library backup no longer matches its recorded bytes")
	}
	again, err := root.Lstat(filepath.Base(p.Backup))
	if err != nil || deliveryFileGeneration(again) != deliveryFileGeneration(backup) {
		return fmt.Errorf("the prior Library backup changed during recovery")
	}
	target, targetErr := root.Lstat(filepath.Base(p.Target))
	if targetErr == nil {
		if !os.SameFile(target, backup) {
			return fmt.Errorf("the installed Library file is preserved; its prior backup still needs reconciliation")
		}
	} else if !errors.Is(targetErr, os.ErrNotExist) {
		return targetErr
	} else if err = root.Link(filepath.Base(p.Backup), filepath.Base(p.Target)); err != nil {
		return err
	}
	if err = syncRootDirectory(root, "."); err != nil {
		return err
	}
	if err = root.Remove(filepath.Base(p.Backup)); err != nil {
		return err
	}
	return syncRootDirectory(root, ".")
}

func (s *Service) verifyAbsentActivityStage(ctx context.Context, dl sqlite.Download, stage sqlite.ImportStageCustody) error {
	var raw string
	if err := s.db.R.QueryRowContext(ctx, `SELECT data FROM import_placements WHERE json_extract(data,'$.download_id')=? AND json_extract(data,'$.source_generation')=? AND json_extract(data,'$.temporary')=? ORDER BY updated_at DESC LIMIT 1`, dl.ID, dl.GenerationKey, stage.Stage).Scan(&raw); err != nil {
		return fmt.Errorf("the legacy stage has no recorded Library storage identity")
	}
	var p sqlite.Placement
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return err
	}
	ctx, release := s.lockImportTarget(ctx, p.ItemID)
	defer release()
	_, releaseStorage := s.lockStorageDecision(ctx)
	defer releaseStorage()
	storage, parent, err := placementParentIdentity(p.Target)
	if err != nil {
		return err
	}
	if storage != p.StorageIdentity || parent != p.TargetParentIdentity || p.StorageIdentity == "" || p.TargetParentIdentity == "" {
		return fmt.Errorf("reconnect the original Library drive before reconciling its temporary files")
	}
	root, err := os.OpenRoot(filepath.Dir(p.Target))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if _, err = root.Lstat(filepath.Base(stage.Stage)); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the temporary name is no longer absent")
	}
	return syncRootDirectory(root, ".")
}
