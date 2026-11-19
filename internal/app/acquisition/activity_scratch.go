package acquisition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// recoverActivityStageIdentity upgrades pre-object-identity scratch journals.
// It runs under the download and title locks after the writer has exited.
// The source must still be the journaled complete content, and every staged
// byte must be its prefix. A differently named/object/content file is never
// granted disposal authority simply because it lives in the same directory.
func (s *Service) recoverActivityStageIdentity(ctx context.Context, p *sqlite.Placement) error {
	if p.TemporaryObjectIdentity != "" {
		return nil
	}
	before, err := os.Lstat(p.Temporary)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() > p.Size || p.SHA256 == "" {
		return fmt.Errorf("temporary file cannot be matched to its recorded download")
	}
	digest, _, sourceGeneration, err := fileDigestSnapshot(ctx, p.Source)
	if err != nil {
		return fmt.Errorf("reconnect the original download source before retrying: %w", err)
	}
	if digest != p.SHA256 || (p.SourceFileGeneration != "" && sourceGeneration != p.SourceFileGeneration) {
		return fmt.Errorf("the original download changed; temporary file reconciliation stopped")
	}
	source, err := os.Open(p.Source)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	stage, err := os.Open(p.Temporary)
	if err != nil {
		return err
	}
	defer func() { _ = stage.Close() }()
	opened, err := stage.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return fmt.Errorf("temporary file changed while it was opened")
	}
	a, b := make([]byte, 128<<10), make([]byte, 128<<10)
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, readErr := stage.Read(a)
		if n > 0 {
			if _, err = io.ReadFull(source, b[:n]); err != nil || !bytes.Equal(a[:n], b[:n]) {
				return fmt.Errorf("temporary bytes do not belong to the recorded download")
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return readErr
			}
			break
		}
	}
	after, err := os.Lstat(p.Temporary)
	if err != nil || deliveryFileGeneration(before) != deliveryFileGeneration(after) {
		return fmt.Errorf("temporary file changed during reconciliation")
	}
	src, err := source.Stat()
	if err != nil || deliveryFileGeneration(src) != sourceGeneration {
		return fmt.Errorf("the download source changed during reconciliation")
	}
	p.TemporaryObjectIdentity = deliveryFileStableIdentity(after)
	if p.TemporaryObjectIdentity == "" {
		return fmt.Errorf("temporary file object identity is unavailable")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	res, err := s.db.W.ExecContext(ctx, `UPDATE import_placements SET data=? WHERE id=? AND state IN ('prepared','retained') AND json_extract(data,'$.source_generation')=? AND json_extract(data,'$.temporary')=?`, string(raw), p.ID, p.SourceGeneration, p.Temporary)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return sqlite.ErrPlacementGenerationChanged
	}
	return nil
}

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
		return fmt.Errorf("Library parent changed during recovery")
	}
	if _, err = root.Lstat(filepath.Base(p.Target)); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the installed Library file is preserved; its prior backup still needs reconciliation")
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
	if err = root.Link(filepath.Base(p.Backup), filepath.Base(p.Target)); err != nil {
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
