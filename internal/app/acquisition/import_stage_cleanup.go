package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// StageCleanupReview binds explicit disposal to a cancelled attempt and a
// current zero-byte object. Nonempty or unidentifiable stages remain held.
type StageCleanupReview struct {
	ID          string `json:"id"`
	Stage       string `json:"stage"`
	Fingerprint string `json:"fingerprint"`
	Absent      bool   `json:"absent"`
}

var stageCleanupUnlinkHook = func() {}

var stageCleanupSyncDirectory = syncRootDirectory

func (s *Service) withStageCleanup(ctx context.Context, key sqlite.ImportAttemptKey, ids []string, fn func(context.Context, []sqlite.ImportStageCustody) error) error {
	if len(ids) == 0 || len(ids) > 100 {
		return fmt.Errorf("select between 1 and 100 stage IDs")
	}
	unlock, ok := s.tryLockDownload(key.DownloadID)
	if !ok {
		return sqlite.ErrImportPublicationPending
	}
	defer unlock()
	dl, err := s.db.GetDownload(ctx, key.DownloadID)
	if err != nil {
		return err
	}
	targetCtx, release, ok := s.tryLockImportTarget(ctx, dl.MediaItemID)
	if !ok || s.ImportRunning(key.DownloadID) {
		if ok {
			release()
		}
		return sqlite.ErrImportPublicationPending
	}
	defer release()
	ctx, releaseStorage := s.lockStorageDecision(targetCtx)
	defer releaseStorage()
	a, err := s.db.GetImportAttempt(ctx, key.DownloadID, key.GenerationKey)
	if err != nil {
		return err
	}
	if a.ImportAttemptKey != key || dl.GenerationKey != key.GenerationKey {
		return sqlite.ErrImportAttemptChanged
	}
	if a.Outcome != "cancelled" || a.PendingPublications > 0 || a.RetainedPublications > 0 {
		return fmt.Errorf("cancel the current attempt and resolve publication custody before cleaning stages")
	}
	custody, err := s.db.ImportStageCustody(ctx, key.DownloadID, key.GenerationKey)
	if err != nil {
		return err
	}
	selected := []sqlite.ImportStageCustody{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return fmt.Errorf("duplicate stage ID")
		}
		seen[id] = true
		found := false
		for _, c := range custody {
			if c.ID == id && strings.HasPrefix(id, "stage:") {
				selected = append(selected, c)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("selected stage has no identifiable placement or is still under publication custody")
		}
	}
	return fn(ctx, selected)
}

func (s *Service) stageCleanupSnapshot(ctx context.Context, key sqlite.ImportAttemptKey, c sqlite.ImportStageCustody) (StageCleanupReview, *os.Root, *os.File, string, error) {
	review := StageCleanupReview{ID: c.ID, Stage: c.Stage}
	fail := func(e error) (StageCleanupReview, *os.Root, *os.File, string, error) { return review, nil, nil, "", e }
	p, err := s.db.GetPlacement(ctx, strings.TrimPrefix(c.ID, "stage:"))
	if err != nil {
		return fail(err)
	}
	parent := filepath.Dir(p.Target)
	if p.DownloadID != key.DownloadID || p.SourceGeneration != key.GenerationKey || p.ImportEpoch == nil || *p.ImportEpoch > key.Epoch || p.Temporary != c.Stage || p.Temporary != filepath.Join(parent, ".monarr-stage-"+p.ID) || p.Temporary == p.Target || p.TemporaryObjectIdentity == "" || p.StorageIdentity == "" || p.TargetParentIdentity == "" {
		return fail(fmt.Errorf("stage allocation evidence is incomplete or changed"))
	}
	storage, parentID, err := placementParentIdentity(p.Target)
	if err != nil {
		return fail(err)
	}
	if storage != p.StorageIdentity || parentID != p.TargetParentIdentity {
		return fail(fmt.Errorf("stage storage or parent identity changed"))
	}
	if p.Backup != "" {
		if p.Backup != filepath.Join(parent, ".monarr-prior-"+p.ID) {
			return fail(fmt.Errorf("backup path is not the placement's own name"))
		}
		if _, e := os.Lstat(p.Backup); !errors.Is(e, os.ErrNotExist) {
			return fail(fmt.Errorf("prior-file backup remains or cannot be proven absent"))
		}
	}
	base, err := os.OpenRoot(parent)
	if err != nil {
		return fail(err)
	}
	closeFail := func(e error) (StageCleanupReview, *os.Root, *os.File, string, error) {
		_ = base.Close()
		return fail(e)
	}
	bound, err := base.Stat(".")
	if err != nil {
		return closeFail(err)
	}
	namedParent, err := os.Lstat(parent)
	if err != nil || !os.SameFile(bound, namedParent) {
		return closeFail(fmt.Errorf("stage parent changed while opening"))
	}
	name := filepath.Base(p.Temporary)
	info, err := base.Lstat(name)
	var file *os.File
	generation := ""
	if errors.Is(err, os.ErrNotExist) {
		review.Absent = true
	} else {
		if err != nil {
			return closeFail(err)
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != 0 || deliveryFileStableIdentity(info) != p.TemporaryObjectIdentity {
			return closeFail(fmt.Errorf("stage is not the recorded empty object; retained for review"))
		}
		file, err = base.Open(name)
		if err != nil {
			return closeFail(err)
		}
		opened, e := file.Stat()
		generation = deliveryFileGeneration(info)
		if e != nil || generation == "" || deliveryFileGeneration(opened) != generation {
			_ = file.Close()
			return closeFail(fmt.Errorf("stage changed while opening"))
		}
	}
	raw, _ := json.Marshal(struct {
		Key                                   sqlite.ImportAttemptKey
		ID, Path, Storage, Parent, Generation string
		Absent                                bool
	}{key, p.ID, p.Temporary, storage, parentID, generation, review.Absent})
	digest := sha256.Sum256(raw)
	review.Fingerprint = hex.EncodeToString(digest[:])
	return review, base, file, generation, nil
}

func (s *Service) PreviewImportStageCleanup(ctx context.Context, key sqlite.ImportAttemptKey, ids []string) ([]StageCleanupReview, error) {
	out := []StageCleanupReview{}
	err := s.withStageCleanup(ctx, key, ids, func(ctx context.Context, stages []sqlite.ImportStageCustody) error {
		for _, stage := range stages {
			review, base, file, _, err := s.stageCleanupSnapshot(ctx, key, stage)
			if err != nil {
				return err
			}
			if file != nil {
				_ = file.Close()
			}
			_ = base.Close()
			out = append(out, review)
		}
		return nil
	})
	return out, err
}

func (s *Service) CleanupImportStages(ctx context.Context, key sqlite.ImportAttemptKey, ids []string, fingerprints map[string]string, acknowledged bool) (sqlite.ImportAttempt, error) {
	if !acknowledged {
		return sqlite.ImportAttempt{}, fmt.Errorf("explicit stage disposal acknowledgement required")
	}
	err := s.withStageCleanup(ctx, key, ids, func(ctx context.Context, stages []sqlite.ImportStageCustody) error {
		for _, stage := range stages {
			if err := s.cleanupImportStage(ctx, key, stage, fingerprints[stage.ID]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return sqlite.ImportAttempt{}, err
	}
	return s.db.GetImportAttempt(ctx, key.DownloadID, key.GenerationKey)
}

func (s *Service) cleanupImportStage(ctx context.Context, key sqlite.ImportAttemptKey, stage sqlite.ImportStageCustody, fingerprint string) error {
	review, base, file, generation, err := s.stageCleanupSnapshot(ctx, key, stage)
	if err != nil {
		return err
	}
	defer func() { _ = base.Close() }()
	if file != nil {
		defer func() { _ = file.Close() }()
	}
	if fingerprint == "" || fingerprint != review.Fingerprint {
		return fmt.Errorf("stage cleanup evidence changed; preview again")
	}
	stageCleanupUnlinkHook()
	storage, parentID, err := placementParentIdentity(stage.Target)
	if err != nil {
		return err
	}
	p, err := s.db.GetPlacement(ctx, strings.TrimPrefix(stage.ID, "stage:"))
	if err != nil {
		return err
	}
	bound, err := base.Stat(".")
	if err != nil {
		return err
	}
	namedParent, err := os.Lstat(filepath.Dir(stage.Stage))
	if err != nil {
		return err
	}
	if storage != p.StorageIdentity || parentID != p.TargetParentIdentity || !os.SameFile(bound, namedParent) {
		return fmt.Errorf("stage storage or parent changed before cleanup")
	}
	// Recheck the backup through the same verified parent immediately before
	// disposal. A late backup keeps the placement's reconciliation custody.
	if p.Backup != "" {
		if _, err := base.Lstat(filepath.Base(p.Backup)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("prior-file backup appeared or cannot be proven absent")
		}
	}
	name := filepath.Base(stage.Stage)
	current, err := base.Lstat(name)
	if review.Absent {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("an entry appeared at the absent stage name")
		}
	} else {
		if err != nil {
			return err
		}
		opened, err := file.Stat()
		if err != nil {
			return err
		}
		if !current.Mode().IsRegular() || current.Size() != 0 || deliveryFileGeneration(current) != generation || deliveryFileGeneration(opened) != generation {
			return fmt.Errorf("stage changed before cleanup")
		}
		if err = base.Remove(name); err != nil {
			return err
		}
	}
	// Absence is not durable evidence until the directory sync succeeds too:
	// this may be a retry of an unlink whose previous fsync failed.
	if err = stageCleanupSyncDirectory(base, "."); err != nil {
		return err
	}
	// Only after the physical step. A crash here leaves held evidence; the next
	// explicitly reviewed absent-stage pass can finish the ledger without unlink.
	resolve := []string{stage.ID}
	custody, err := s.db.ImportStageCustody(ctx, key.DownloadID, key.GenerationKey)
	if err != nil {
		return err
	}
	for _, c := range custody {
		if strings.HasPrefix(c.ID, "stagepath:") && c.Stage == stage.Stage {
			resolve = append(resolve, c.ID)
		}
	}
	return s.db.ResolveRetainedImportTargets(ctx, key, resolve, "operator_disposed")
}
