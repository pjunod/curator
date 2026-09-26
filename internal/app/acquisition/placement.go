package acquisition

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

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
	Destinations   map[string]string
	Progress       func(string, int64, int64)
	Validate       func() error
	EpisodeTargets map[string]RecoveryEpisodeTarget
	ImportID       string
	Files          map[string]string
}

// rejectSymlinks checks every existing component, including the mount root.
// A missing final path is allowed only for a destination not yet published.
func rejectSymlinks(path string, missing bool) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("absolute path required: %s", path)
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "darwin" {
		for _, alias := range []string{"/var", "/tmp"} {
			if path == alias || strings.HasPrefix(path, alias+"/") {
				path = "/private" + path
				break
			}
		}
	}
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

func (s *Service) commitPlacement(ctx context.Context, item domain.MediaItem, scope importScope, src, dest string, q quality.Quality, eps []int64, replace bool) (int64, error) {
	hash, size, err := fileDigest(ctx, src)
	if err != nil {
		return 0, err
	}
	key := fmt.Sprintf("%d:%d:%s:%s:%s", item.ID, scope.CopyID, dest, hash, scope.Attempt)
	recovery, _ := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext)
	if recovery != nil {
		key += "/" + recovery.ImportID
	}
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	p, err := s.db.GetPlacement(ctx, id)
	// A committed ordinary attempt is idempotent while its library result
	// exists. A deliberate reimport after removal gets a new durable generation.
	for generation := 0; recovery == nil && err == nil && (p.State == "committed" || p.State == "cleaned"); generation++ {
		current, _, e := fileDigest(ctx, p.Target)
		var present int
		dbErr := s.db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE path=? AND media_item_id=?`, p.Target, p.ItemID).Scan(&present)
		if dbErr != nil {
			return 0, dbErr
		}
		if e == nil && current == p.SHA256 && present > 0 {
			break
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return 0, e
		}
		if generation >= 1000 {
			return 0, fmt.Errorf("placement generation limit reached")
		}
		id = fmt.Sprintf("%x", sha256.Sum256([]byte(key+"/"+id)))
		p, err = s.db.GetPlacement(ctx, id)
	}
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
		info := mediainfo.Info{}
		if item.Kind != domain.KindBook {
			if recovery != nil {
				info, _ = probe.NativeFile(src)
			} else {
				info, _ = probe.File(src)
			}
		}
		measured, prov, conf := q, mediainfo.ProvenanceRelease, mediainfo.ConfidenceNone
		if item.Kind == domain.KindBook {
			prov = mediainfo.ProvenanceFilename
		}
		if info.Measured() {
			measured, prov, conf = mediainfo.Resolve(info, q, mediainfo.ProvenanceRelease)
			if _, bad := mediainfo.DurationImplausible(info, item.Runtime); bad {
				measured = quality.Quality{Source: quality.SourceUnknown, Resolution: info.ResolutionTier()}
				prov = mediainfo.ProvenanceImplausible
				conf = mediainfo.ConfidenceNone
			}
		}
		p = sqlite.Placement{ID: id, Source: src, Target: dest, Temporary: filepath.Join(filepath.Dir(dest), ".monarr-stage-"+id), Backup: filepath.Join(filepath.Dir(dest), ".monarr-prior-"+id), SHA256: hash, PreviousSHA256: previous, Size: size, ItemID: item.ID, CopyID: scope.CopyID, EpisodeIDs: eps, Quality: measured, Info: info, Provenance: prov, Confidence: conf, Release: scope.Release, Indexer: scope.Indexer, State: "prepared"}
		p.CleanupSuperseded = replace && item.Kind == domain.KindBook && !scope.DeferCleanup
		p.SupersededDigests = map[string]string{}
		if replace {
			files, e := s.db.ListFilesForItem(ctx, item.ID)
			if e != nil {
				return 0, e
			}
			for _, old := range files {
				if old.CopyID != scope.CopyID {
					continue
				}
				matched := len(eps) == 0
				for _, ep := range old.EpisodeIDs {
					for _, wanted := range eps {
						if ep == wanted {
							matched = true
						}
					}
				}
				if matched {
					if p.CleanupSuperseded && old.Path != dest {
						digest, _, e := fileDigest(ctx, old.Path)
						if e != nil && !errors.Is(e, os.ErrNotExist) {
							return 0, e
						}
						p.SupersededDigests[old.Path] = digest
					}
					p.Superseded = append(p.Superseded, old)
				}
			}
		}
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
	if recovery != nil && recovery.Validate != nil {
		if err = recovery.Validate(); err != nil {
			return 0, err
		}
	}
	if err = s.publishPlacement(ctx, p); err != nil {
		return 0, err
	}
	if recovery != nil && recovery.Validate != nil {
		if err = recovery.Validate(); err != nil {
			return 0, err
		}
	}
	fid, err := s.db.CommitPlacement(ctx, p)
	if err != nil {
		return 0, err
	}
	if p.State != "committed" {
		if why, bad := mediainfo.DurationImplausible(p.Info, item.Runtime); bad {
			s.recordImplausible(ctx, item.ID, dest, scope.Release, p.Info, why)
		} else if p.Provenance == mediainfo.ProvenanceImplausible {
			why, _ := mediainfo.Implausible(p.Info)
			s.recordImplausible(ctx, item.ID, dest, scope.Release, p.Info, why)
		}
		if q.Resolution != 0 && p.Quality.Resolution != 0 && q.Resolution != p.Quality.Resolution {
			_ = s.db.AddHistory(ctx, HistoryQualityMismatch, item.ID, scope.Release, map[string]any{"claimed": q.String(), "measured": p.Quality.String(), "file": filepath.Base(dest), "facts": p.Info.Summary()})
		}
	}
	if e := s.cleanupPlacement(ctx, p); e != nil {
		s.log.Warn("placement cleanup remains pending", "id", p.ID, "err", e)
	}
	return fid, nil
}
func (s *Service) cleanupPlacement(ctx context.Context, p sqlite.Placement) error {
	if p.CleanupSuperseded {
		for _, old := range p.Superseded {
			if old.Path == p.Target {
				continue
			}
			got, _, err := fileDigest(ctx, old.Path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil {
				if got != p.SupersededDigests[old.Path] {
					return fmt.Errorf("superseded book changed: %s", old.Path)
				}
				if err = os.Remove(old.Path); err != nil {
					return err
				}
			}
			if err = syncPath(filepath.Dir(old.Path)); err != nil {
				return err
			}
			if err = s.db.DeleteFile(ctx, old.ID); err != nil {
				return err
			}
		}
	}
	for path, expected := range map[string]string{p.Backup: p.PreviousSHA256, p.Temporary: p.SHA256} {
		if expected == "" {
			continue
		}
		got, _, err := fileDigest(ctx, path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if got != expected {
			return fmt.Errorf("cleanup identity mismatch: %s", path)
		}
		if err = os.Remove(path); err != nil {
			return err
		}
		if err = syncPath(filepath.Dir(path)); err != nil {
			return err
		}
	}
	_, err := s.db.W.ExecContext(ctx, `UPDATE import_placements SET state='cleaned',updated_at=? WHERE id=? AND state='committed'`, time.Now().UnixMilli(), p.ID)
	return err
}

func (s *Service) publishPlacement(ctx context.Context, p sqlite.Placement) error {
	got, _, err := fileDigest(ctx, p.Target)
	if err == nil && got == p.SHA256 {
		return syncPublication(p.Target)
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
		progress := transfers.Progress(ctx)
		if recovery, ok := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext); ok && recovery.Progress != nil {
			previous := progress
			progress = func(done, total int64) {
				if previous != nil {
					previous(done, total)
				}
				recovery.Progress(p.Source, done, total)
			}
		}
		if err = placeFile(ctx, p.Source, p.Temporary, progress); err != nil {
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
	if recovery, ok := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext); ok && recovery.Validate != nil {
		if err := recovery.Validate(); err != nil {
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
	return syncPublication(p.Target)
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
		// Rotate incomplete work so one damaged placement cannot starve later rows.
		_, _ = s.db.W.ExecContext(ctx, `UPDATE import_placements SET updated_at=? WHERE id=?`, time.Now().UnixMilli(), p.ID)
		if p.State == "committed" {
			if e := s.cleanupPlacement(ctx, p); e != nil {
				s.log.Warn("placement cleanup pending", "id", p.ID, "err", e)
			}
			continue
		}
		if p.RecoveryImport != "" {
			r, e := s.RecoveryImport(ctx, p.RecoveryImport)
			if e != nil {
				continue
			}
			if r.State == "cancel_pending" || r.State == "cancelled" {
				if e = s.rollbackPlacement(ctx, p); e != nil {
					s.log.Warn("cancelled placement needs reconciliation", "id", p.ID, "err", e)
				}
				continue
			}
			if e = s.validateRecoveryTarget(ctx, r); e != nil {
				continue
			}
		}
		// Reconcile only already published bytes. An old unplaced intention cannot
		// replace a newer library file merely because the daemon restarted.
		got, _, e := fileDigest(ctx, p.Target)
		if e != nil || got != p.SHA256 {
			continue
		}
		if e = syncPublication(p.Target); e != nil {
			continue
		}
		if _, e = s.db.CommitPlacement(ctx, p); e != nil {
			s.log.Error("placement commit remains pending", "id", p.ID, "err", e)
		} else if e = s.cleanupPlacement(ctx, p); e != nil {
			s.log.Warn("placement cleanup pending", "id", p.ID, "err", e)
		}
	}
}

// A previous rename is not evidence that its fsync succeeded. Recovery repeats
// both file and directory synchronization, including newly created ancestors.
func syncPublication(path string) error {
	if err := syncPath(path); err != nil {
		return err
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		if err := syncPath(parent); err != nil {
			return err
		}
		if filepath.Dir(parent) == parent {
			return nil
		}
	}
}
func (s *Service) rollbackPlacement(ctx context.Context, p sqlite.Placement) error {
	got, _, err := fileDigest(ctx, p.Target)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if got == p.SHA256 || (errors.Is(err, os.ErrNotExist) && p.PreviousSHA256 != "") {
		if p.PreviousSHA256 == "" {
			if err = os.Remove(p.Target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		} else {
			backup, _, e := fileDigest(ctx, p.Backup)
			if e != nil {
				return e
			}
			if backup != p.PreviousSHA256 {
				return fmt.Errorf("rollback backup changed: %s", p.Backup)
			}
			if e = os.Rename(p.Backup, p.Target); e != nil {
				return e
			}
			if e = syncPublication(p.Target); e != nil {
				return e
			}
		}
	} else if got != p.PreviousSHA256 {
		return fmt.Errorf("cancelled target changed: %s", p.Target)
	}
	if _, err = os.Stat(filepath.Dir(p.Target)); err == nil {
		if err = syncPath(filepath.Dir(p.Target)); err != nil {
			return err
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = s.cleanupPlacement(ctx, sqlite.Placement{ID: p.ID, Target: p.Target, Backup: p.Backup, Temporary: p.Temporary, SHA256: p.SHA256, PreviousSHA256: p.PreviousSHA256}); err != nil {
		return err
	}
	_, err = s.db.W.ExecContext(ctx, `UPDATE import_placements SET state='rolled_back',updated_at=? WHERE id=? AND state='prepared'`, time.Now().UnixMilli(), p.ID)
	return err
}
func (s *Service) rollbackRecoveryPlacements(ctx context.Context, id string) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT data FROM import_placements WHERE state='prepared' AND json_extract(data,'$.recovery_import')=?`, id)
	if err != nil {
		return err
	}
	var placements []sqlite.Placement
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var p sqlite.Placement
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			break
		}
		placements = append(placements, p)
	}
	if err == nil {
		err = rows.Err()
	}
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, p := range placements {
		if err = s.rollbackPlacement(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) verifyRecoveryPublications(ctx context.Context, r RecoveryImport) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT data FROM import_placements WHERE state IN ('committed','cleaned') AND json_extract(data,'$.recovery_import')=?`, r.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	seen, files := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		var p sqlite.Placement
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			return err
		}
		path := strings.ToLower(filepath.Clean(p.Target))
		if seen[path] || files[p.RecoveryFile] {
			return fmt.Errorf("recovery placements overlap")
		}
		seen[path], files[p.RecoveryFile] = true, true
		got, size, e := fileDigest(ctx, p.Target)
		if e != nil {
			return e
		}
		if got != p.SHA256 || size != p.Size {
			return fmt.Errorf("published recovery changed before receipt: %s", p.Target)
		}
		if e = syncPublication(p.Target); e != nil {
			return e
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(files) != len(r.Recovery.Files) {
		return fmt.Errorf("not every staged file has a surviving publication")
	}
	return nil
}
