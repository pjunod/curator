package acquisition

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pjunod/monarr/internal/app/transfers"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/probe"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

type targetHeldKey struct{}
type recoveryPlacementKey struct{}
type recoveryPlacementContext struct {
	ImportID string
	Files    map[string]string
}

// rejectSymlinks checks every existing component, including the mount root.
// A missing final path is allowed only for a destination not yet published.
func rejectSymlinks(path string, missing bool) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("absolute path required: %s", path)
	}
	path = filepath.Clean(path)
	cur := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(path, cur), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) && missing {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink requires review: %s", cur)
		}
	}
	return nil
}
func fileDigest(ctx context.Context, path string) (string, int64, error) {
	if err := rejectSymlinks(path, false); err != nil {
		return "", 0, err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !before.Mode().IsRegular() {
		return "", 0, fmt.Errorf("not a regular file: %s", path)
	}
	h := sha256.New()
	n, err := io.Copy(h, &contextReader{ctx: ctx, r: f})
	if err != nil {
		return "", 0, err
	}
	after, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !os.SameFile(before, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || n != before.Size() {
		return "", 0, fmt.Errorf("file changed during verification: %s", path)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

func (s *Service) commitPlacement(ctx context.Context, item domain.MediaItem, scope importScope, src, dest string, q quality.Quality, eps []int64) (int64, error) {
	hash, size, err := fileDigest(ctx, src)
	if err != nil {
		return 0, err
	}
	key := fmt.Sprintf("%d:%d:%s:%s", item.ID, scope.CopyID, dest, hash)
	recovery, _ := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext)
	if recovery != nil {
		key += "/" + recovery.ImportID
	}
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	p, err := s.db.GetPlacement(ctx, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		if err = rejectSymlinks(dest, true); err != nil {
			return 0, err
		}
		previous, _, e := fileDigest(ctx, dest)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return 0, e
		}
		info, _ := probe.File(src)
		measured, prov, conf := q, mediainfo.ProvenanceRelease, mediainfo.ConfidenceNone
		if info.Measured() {
			measured, prov, conf = mediainfo.Resolve(info, q, mediainfo.ProvenanceRelease)
			if _, bad := mediainfo.DurationImplausible(info, item.Runtime); bad {
				measured = quality.Quality{Source: quality.SourceUnknown, Resolution: info.ResolutionTier()}
				prov = mediainfo.ProvenanceImplausible
				conf = mediainfo.ConfidenceNone
			}
		}
		p = sqlite.Placement{ID: id, Source: src, Target: dest, Temporary: filepath.Join(filepath.Dir(dest), ".monarr-stage-"+id), Backup: filepath.Join(filepath.Dir(dest), ".monarr-prior-"+id), SHA256: hash, PreviousSHA256: previous, Size: size, ItemID: item.ID, CopyID: scope.CopyID, EpisodeIDs: eps, Quality: measured, Info: info, Provenance: prov, Confidence: conf, Release: scope.Release, Indexer: scope.Indexer, State: "prepared"}
		if recovery != nil {
			p.RecoveryImport = recovery.ImportID
			p.RecoveryFile = recovery.Files[src]
			if p.RecoveryFile == "" {
				return 0, fmt.Errorf("file is outside the recovery selection")
			}
		}
		if err = s.db.PreparePlacement(ctx, p); err != nil {
			return 0, err
		}
	}
	if err = s.publishPlacement(ctx, p); err != nil {
		return 0, err
	}
	fid, err := s.db.CommitPlacement(ctx, p)
	if err != nil {
		return 0, err
	}
	// Old bytes survive until metadata and the recovery receipt commit together.
	if p.PreviousSHA256 != "" {
		if got, _, e := fileDigest(ctx, p.Backup); e == nil && got == p.PreviousSHA256 {
			if e = os.Remove(p.Backup); e != nil {
				return fid, e
			}
			if e = syncPath(filepath.Dir(p.Backup)); e != nil {
				return fid, e
			}
		}
	}
	return fid, nil
}
func (s *Service) publishPlacement(ctx context.Context, p sqlite.Placement) error {
	got, _, err := fileDigest(ctx, p.Target)
	if err == nil && got == p.SHA256 {
		return nil
	} // reconcile publication before DB commit
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if got != p.PreviousSHA256 {
		return fmt.Errorf("target changed after placement preview: %s", p.Target)
	}
	if p.State == "committed" {
		return fmt.Errorf("committed target changed; a new preview is required")
	}
	if err = rejectSymlinks(filepath.Dir(p.Target), true); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p.Target), dirMode()); err != nil {
		return err
	}
	if staged, _, e := fileDigest(ctx, p.Temporary); e == nil {
		if staged != p.SHA256 {
			return fmt.Errorf("staging identity mismatch; review %s", p.Temporary)
		}
	} else if errors.Is(e, os.ErrNotExist) {
		// placeFile remains the common injection seam. Recovery copies are forced
		// independent by placeTemp's context check.
		if err = placeFile(ctx, p.Source, p.Temporary, transfers.Progress(ctx)); err != nil {
			return err
		}
	} else {
		return e
	}
	staged, _, err := fileDigest(ctx, p.Temporary)
	if err != nil {
		return err
	}
	if staged != p.SHA256 {
		return fmt.Errorf("copy digest mismatch")
	}
	if err = syncPath(p.Temporary); err != nil {
		return err
	}
	if p.PreviousSHA256 != "" {
		if backup, _, e := fileDigest(ctx, p.Backup); e == nil {
			if backup != p.PreviousSHA256 {
				return fmt.Errorf("rollback identity mismatch")
			}
		} else if errors.Is(e, os.ErrNotExist) {
			if e = os.Link(p.Target, p.Backup); e != nil {
				return e
			}
			if e = syncPath(p.Backup); e != nil {
				return e
			}
		} else {
			return e
		}
		if err = syncPath(filepath.Dir(p.Backup)); err != nil {
			return err
		}
	}
	current, _, e := fileDigest(ctx, p.Target)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if current != p.PreviousSHA256 {
		return fmt.Errorf("target changed before publication")
	}
	if err = rejectSymlinks(p.Target, true); err != nil {
		return err
	}
	if err = os.Rename(p.Temporary, p.Target); err != nil {
		return err
	}
	return syncPath(filepath.Dir(p.Target))
}
func (s *Service) reconcilePlacements(ctx context.Context) {
	s.importTargetMu.Lock()
	defer s.importTargetMu.Unlock()
	rows, err := s.db.PendingPlacements(ctx)
	if err != nil {
		s.log.Error("placement reconciliation unavailable", "err", err)
		return
	}
	for _, p := range rows {
		// Reconcile only already published bytes. An old unplaced intention cannot
		// replace a newer library file merely because the daemon restarted.
		got, _, e := fileDigest(ctx, p.Target)
		if e != nil || got != p.SHA256 {
			continue
		}
		if _, e = s.db.CommitPlacement(ctx, p); e != nil {
			s.log.Error("placement commit remains pending", "id", p.ID, "err", e)
		}
	}
}
