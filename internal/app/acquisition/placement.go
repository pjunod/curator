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
type expectedSourceDigestsKey struct{}
type reviewedSourceSnapshotsKey struct{}
type acceptedSourceGenerationsKey struct{}
type acceptedPublicationGenerationKey struct{}
type independentPlacementCopyKey struct{}

// withIndependentPlacementCopy keeps accepted Runner publications immutable.
// Hard-linking would change the source inode's ctime when the library link is
// created, invalidating the manifest file generation on retry/cleanup.
func withIndependentPlacementCopy(ctx context.Context) context.Context {
	return context.WithValue(ctx, independentPlacementCopyKey{}, true)
}

func requiresIndependentPlacementCopy(ctx context.Context) bool {
	copy, _ := ctx.Value(independentPlacementCopyKey{}).(bool)
	return copy
}

// withAcceptedSourceGenerations carries the exact immutable manifest file
// generation through the shared importer. Keys are canonical absolute paths.
func withAcceptedSourceGenerations(ctx context.Context, generations map[string]string) context.Context {
	canonical := make(map[string]string, len(generations))
	for path, generation := range generations {
		canonical[filepath.Clean(path)] = generation
	}
	return context.WithValue(ctx, acceptedSourceGenerationsKey{}, canonical)
}

func acceptedSourceGeneration(ctx context.Context, path string) string {
	if generations, ok := ctx.Value(acceptedSourceGenerationsKey{}).(map[string]string); ok {
		return generations[filepath.Clean(path)]
	}
	return ""
}

// withAcceptedPublicationGeneration carries the Runner publication identity
// separately from per-file stat generations and the Curator download key.
func withAcceptedPublicationGeneration(ctx context.Context, generation string) context.Context {
	if strings.TrimSpace(generation) == "" {
		return ctx
	}
	return context.WithValue(ctx, acceptedPublicationGenerationKey{}, generation)
}

func acceptedPublicationGeneration(ctx context.Context) string {
	generation, _ := ctx.Value(acceptedPublicationGenerationKey{}).(string)
	return generation
}

// withExpectedSourceDigests binds accepted manifest content to the existing
// placement commit path. commitPlacement checks the digest it reads before it
// creates a placement journal or changes any library bytes.
func withExpectedSourceDigests(ctx context.Context, digests map[string]string) context.Context {
	canonical := make(map[string]string, len(digests))
	for path, digest := range digests {
		canonical[filepath.Clean(path)] = digest
	}
	return context.WithValue(ctx, expectedSourceDigestsKey{}, canonical)
}

func withReviewedSourceSnapshots(ctx context.Context, snapshots map[string]string) context.Context {
	canonical := make(map[string]string, len(snapshots))
	for path, identity := range snapshots {
		canonical[filepath.Clean(path)] = identity
	}
	return context.WithValue(ctx, reviewedSourceSnapshotsKey{}, canonical)
}

func validateReviewedSourceSnapshot(ctx context.Context, path string) error {
	snapshots, ok := ctx.Value(reviewedSourceSnapshotsKey{}).(map[string]string)
	if !ok {
		return nil
	}
	expected, ok := snapshots[filepath.Clean(path)]
	if !ok {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || completedReviewFileIdentity(path, info) != expected {
		return fmt.Errorf("source file changed after legacy review; review the file again: %s", path)
	}
	return nil
}

type recoveryPlacementContext struct {
	Destinations   map[string]string
	Progress       func(string, int64, int64)
	Validate       func() error
	ObserveFinal   func() (string, error)
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
	digest, size, _, err := fileDigestSnapshot(ctx, path)
	return digest, size, err
}

func fileDigestSnapshot(ctx context.Context, path string) (string, int64, string, error) {
	if err := rejectSymlinks(path, false); err != nil {
		return "", 0, "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, "", err
	}
	defer func() { _ = f.Close() }()
	before, err := f.Stat()
	if err != nil {
		return "", 0, "", err
	}
	if !before.Mode().IsRegular() {
		return "", 0, "", fmt.Errorf("not a regular file: %s", path)
	}
	beforeGeneration := deliveryFileGeneration(before)
	h := sha256.New()
	n, err := io.Copy(h, &contextReader{ctx: ctx, r: f})
	if err != nil {
		return "", 0, "", err
	}
	after, err := f.Stat()
	if err != nil {
		return "", 0, "", err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return "", 0, "", err
	}
	afterGeneration := deliveryFileGeneration(after)
	currentGeneration := deliveryFileGeneration(current)
	if !os.SameFile(before, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) ||
		(beforeGeneration != "" && (beforeGeneration != afterGeneration || afterGeneration != currentGeneration)) || n != before.Size() {
		return "", 0, "", fmt.Errorf("file changed during verification: %s", path)
	}
	return hex.EncodeToString(h.Sum(nil)), n, currentGeneration, nil
}
func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}

// manualPlacementDestination keeps a same-quality manual import beside existing
// bytes. The digest suffix is stable across retries and never authorizes overwrite.
func manualPlacementDestination(ctx context.Context, src, dest string, manual, replace bool) (string, error) {
	if !manual || replace {
		return dest, nil
	}
	incoming, _, err := fileDigest(ctx, src)
	if err != nil {
		return "", err
	}
	ext := filepath.Ext(dest)
	base := strings.TrimSuffix(dest, ext)
	for n := 0; n < 1000; n++ {
		current, _, err := fileDigest(ctx, dest)
		if errors.Is(err, os.ErrNotExist) || (err == nil && current == incoming) {
			return dest, nil
		}
		if err != nil {
			return "", err
		}
		dest = fmt.Sprintf("%s [manual-%s-%d]%s", base, incoming[:12], n, ext)
	}
	return "", fmt.Errorf("manual destination collision limit reached")
}

// replacementPlacementDestination keeps a selected source beside any current
// library path. Replacement cleanup later removes only the operation's exact
// observed generations; writing beside the target also protects shared or
// newer files that happen to render to the same name.
func replacementPlacementDestination(ctx context.Context, src, dest string, scope importScope) (string, error) {
	if scope.ReplacementOperationID == "" {
		return dest, nil
	}
	if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
		return dest, nil
	} else if err != nil {
		return "", err
	}
	if err := rejectSymlinks(dest, true); err != nil {
		return "", err
	}
	incoming, _, err := fileDigest(ctx, src)
	if err != nil {
		return "", err
	}
	shortOperation := scope.ReplacementOperationID
	if len(shortOperation) > 8 {
		shortOperation = shortOperation[:8]
	}
	ext := filepath.Ext(dest)
	base := strings.TrimSuffix(dest, ext)
	base = fmt.Sprintf("%s [replacement-%s-%s]", base, shortOperation, incoming[:12])
	for attempt := 0; attempt < 1000; attempt++ {
		candidate := base + ext
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%d%s", base, attempt, ext)
		}
		if err := rejectSymlinks(filepath.Dir(candidate), false); err != nil {
			return "", err
		}
		if err := rejectSymlinks(candidate, true); err != nil {
			return "", err
		}
		current, _, statErr := fileDigest(ctx, candidate)
		if errors.Is(statErr, os.ErrNotExist) {
			return candidate, nil
		}
		if statErr != nil {
			return "", statErr
		}
		if current == incoming {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("replacement destination collision limit reached")
}

func (s *Service) commitPlacement(ctx context.Context, item domain.MediaItem, scope importScope, src, dest string, q quality.Quality, eps []int64, replace bool) (int64, error) {
	if err := validateReviewedSourceSnapshot(ctx, src); err != nil {
		return 0, err
	}
	hash, size, err := fileDigest(ctx, src)
	if err != nil {
		return 0, err
	}
	if expected, ok := ctx.Value(expectedSourceDigestsKey{}).(map[string]string); ok {
		if digest, constrained := expected[filepath.Clean(src)]; constrained && digest != hash {
			return 0, fmt.Errorf("source file no longer matches its accepted delivery manifest: %s", src)
		}
	}
	if err := validateReviewedSourceSnapshot(ctx, src); err != nil {
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
		previous, _, targetGeneration, e := fileDigestSnapshot(ctx, dest)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return 0, e
		}
		targetObserved := e == nil
		if e = os.MkdirAll(filepath.Dir(dest), dirMode()); e != nil {
			return 0, e
		}
		storageIdentity, parentIdentity, e := placementParentIdentity(dest)
		if e != nil {
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
		p = sqlite.Placement{DownloadID: scope.DownloadID, ReplacementOperationID: scope.ReplacementOperationID,
			SourceGeneration: scope.SourceGeneration, SourceTransfer: scope.Transfer,
			SourceFileGeneration:    acceptedSourceGeneration(ctx, src),
			SourcePayloadGeneration: acceptedPublicationGeneration(ctx), SourceGUID: scope.SourceGUID, SourceIndexerID: scope.SourceIndexerID,
			ID: id, Source: src, Target: dest,
			Temporary: filepath.Join(filepath.Dir(dest), ".monarr-stage-"+id), Backup: filepath.Join(filepath.Dir(dest), ".monarr-prior-"+id),
			SHA256: hash, PreviousSHA256: previous, TargetObserved: targetObserved, TargetFileGeneration: targetGeneration,
			StorageRoot: filepath.Dir(dest), StorageIdentity: storageIdentity, StorageRootIdentity: parentIdentity,
			TargetParentIdentity: parentIdentity, Size: size, ItemID: item.ID, CopyID: scope.CopyID,
			EpisodeIDs: eps, Quality: measured, Info: info, Provenance: prov, Confidence: conf,
			Release: scope.Release, Indexer: scope.Indexer, State: "prepared"}
		p.CleanupSuperseded = replace && item.Kind == domain.KindBook && !scope.DeferCleanup && scope.ReplacementOperationID == ""
		p.SupersededDigests = map[string]string{}
		if replace {
			files, e := s.db.ListFilesForItem(ctx, item.ID)
			if e != nil {
				return 0, e
			}
			var approvedReplacementFiles map[int64]string
			if scope.ReplacementOperationID != "" {
				approvedReplacementFiles = map[int64]string{}
				snapshots, snapshotErr := s.db.ListReplacementFiles(ctx, scope.ReplacementOperationID)
				if snapshotErr != nil {
					return 0, snapshotErr
				}
				for _, snapshot := range snapshots {
					if snapshot.Disposition == "selected" {
						approvedReplacementFiles[snapshot.MediaFileID] = snapshot.FileGeneration
					}
				}
			}
			for _, old := range files {
				if old.CopyID != scope.CopyID {
					continue
				}
				if approvedReplacementFiles != nil && approvedReplacementFiles[old.ID] != old.FileGeneration {
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
		// A deterministic policy refusal has no publication custody. Measure
		// and validate before creating the journal; persist before any rename.
		if err = s.validateMeasuredPlacement(ctx, item, scope, p, replace); err != nil {
			return 0, err
		}
		if err = s.db.PreparePlacement(ctx, p); err != nil {
			return 0, err
		}
	}
	// Hashing, backup creation, and payload staging happen outside the global
	// storage authority lock. importTargetMu still reserves this destination.
	if err = s.preparePlacementPublication(ctx, &p); err != nil {
		return 0, err
	}
	if p.State == "prepared" {
		if err = s.validateMeasuredPlacement(ctx, item, scope, p, replace); err != nil {
			return 0, err
		}
	}
	var recoveryObservation string
	if recovery != nil && recovery.ObserveFinal != nil {
		recoveryObservation, err = recovery.ObserveFinal()
		if err != nil {
			return 0, err
		}
	}
	if recovery != nil && recovery.Validate != nil {
		if err = recovery.Validate(); err != nil {
			return 0, err
		}
	}
	// Serialize only final identity checks, publication, and the metadata commit.
	decisionCtx, releaseDecision := s.lockStorageDecision(ctx)
	ctx = decisionCtx
	if recovery != nil && recovery.ObserveFinal != nil {
		currentObservation, observeErr := recovery.ObserveFinal()
		if observeErr != nil || currentObservation != recoveryObservation {
			releaseDecision()
			if observeErr != nil {
				return 0, observeErr
			}
			return 0, fmt.Errorf("recovery target changed before placement publication")
		}
	}
	if err = s.publishPlacement(ctx, &p); err != nil {
		releaseDecision()
		return 0, err
	}
	fid, err := s.db.CommitPlacement(ctx, p)
	releaseDecision()
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
		if profile, perr := s.db.GetProfile(ctx, scope.ProfileID); perr == nil {
			s.recordLanguageMismatch(ctx, item.ID, dest, scope.Release, scope.Indexer, profile, p.Info)
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

func placementParentIdentity(target string) (string, string, error) {
	parent := filepath.Dir(target)
	if err := rejectSymlinks(parent, false); err != nil {
		return "", "", err
	}
	info, err := os.Stat(parent)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("placement parent is not a directory: %s", parent)
	}
	storage, err := expectedStorageIdentity(parent)
	if err != nil {
		return "", "", err
	}
	identity := deliveryDirectoryObjectIdentity(info)
	if identity == "" {
		return "", "", fmt.Errorf("placement parent identity is unavailable: %s", parent)
	}
	return storage, identity, nil
}

func (s *Service) preparePlacementPublication(ctx context.Context, p *sqlite.Placement) error {
	legacyIdentitySnapshot := legacyPlacementIdentitySnapshot(*p)
	if err := rejectSymlinks(filepath.Dir(p.Target), false); err != nil {
		return err
	}
	storage, parentIdentity, err := placementParentIdentity(p.Target)
	if err != nil {
		return err
	}
	if p.StorageIdentity != "" && p.StorageIdentity != storage {
		return fmt.Errorf("placement storage identity changed; review required")
	}
	if p.TargetParentIdentity != "" && p.TargetParentIdentity != parentIdentity {
		return fmt.Errorf("placement parent identity changed; review required")
	}
	if p.StorageIdentity == "" {
		p.StorageIdentity = storage
	}
	if p.StorageRoot == "" {
		p.StorageRoot = filepath.Dir(p.Target)
	}
	if p.StorageRootIdentity == "" {
		p.StorageRootIdentity = parentIdentity
	}
	if p.TargetParentIdentity == "" {
		p.TargetParentIdentity = parentIdentity
	}
	if p.State != "prepared" {
		digest, _, generation, err := fileDigestSnapshot(ctx, p.Target)
		if err != nil {
			return err
		}
		if digest != p.SHA256 || (p.PublishedFileGeneration != "" && generation != p.PublishedFileGeneration) {
			return fmt.Errorf("committed placement bytes or generation changed; a new placement is required")
		}
		if err = syncPath(p.Target); err != nil {
			return err
		}
		p.PublishedFileGeneration = generation
		return nil
	}

	// A crash after rename but before the DB transaction is reconciled only
	// after a full digest proves that the target contains this intent's bytes.
	if digest, _, generation, e := fileDigestSnapshot(ctx, p.Target); e == nil && digest == p.SHA256 {
		if p.PublishedFileGeneration != "" && p.PublishedFileGeneration != generation {
			return fmt.Errorf("published placement generation changed")
		}
		info, statErr := os.Lstat(p.Target)
		if statErr != nil {
			return statErr
		}
		if !publishedPlacementMatchesIntent(*p, info, generation, legacyIdentitySnapshot) {
			return fmt.Errorf("published target does not match this placement's recorded identity")
		}
		if e = syncPath(p.Target); e != nil {
			return e
		}
		p.PublishedFileGeneration = generation
		return s.db.UpdatePreparedPlacement(ctx, *p)
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}

	currentDigest, _, currentGeneration, targetErr := fileDigestSnapshot(ctx, p.Target)
	if targetErr != nil && !errors.Is(targetErr, os.ErrNotExist) {
		return targetErr
	}
	if targetErr == nil {
		if currentDigest != p.PreviousSHA256 {
			return fmt.Errorf("target changed after placement preview: %s", p.Target)
		}
		if p.TargetFileGeneration == "" {
			// Old journals can be upgraded only after the digest proves the
			// exact previous bytes they recorded.
			p.TargetObserved = true
			p.TargetFileGeneration = currentGeneration
		} else if currentGeneration != p.TargetFileGeneration && currentGeneration != p.TargetAfterBackupGen {
			// An interrupted hard-link backup changes ctime on the target. It is
			// accepted only when the backup hashes to the reviewed bytes, is the
			// same inode, and its post-hash generation still matches the target.
			backupDigest, _, backupGeneration, backupErr := fileDigestSnapshot(ctx, p.Backup)
			if backupErr != nil || backupDigest != p.PreviousSHA256 {
				return fmt.Errorf("target generation changed after placement preview: %s", p.Target)
			}
			backupInfo, backupErr := os.Stat(p.Backup)
			targetInfo, targetErr := os.Stat(p.Target)
			if backupErr != nil || targetErr != nil || !os.SameFile(backupInfo, targetInfo) ||
				deliveryFileGeneration(targetInfo) != backupGeneration || deliveryFileGeneration(targetInfo) != currentGeneration {
				return fmt.Errorf("interrupted rollback link does not prove the reviewed target")
			}
			p.TargetAfterBackupGen = backupGeneration
		}
	} else if p.TargetObserved || p.PreviousSHA256 != "" {
		return fmt.Errorf("reviewed placement target disappeared before publication")
	}

	if stagedDigest, _, stagedGeneration, e := fileDigestSnapshot(ctx, p.Temporary); e == nil {
		if stagedDigest != p.SHA256 {
			return fmt.Errorf("staging identity mismatch; review %s", p.Temporary)
		}
		stagedInfo, statErr := os.Lstat(p.Temporary)
		if statErr != nil {
			return statErr
		}
		stagedObjectIdentity := deliveryFileStableIdentity(stagedInfo)
		if p.TemporaryFileGeneration != "" && p.TemporaryFileGeneration != stagedGeneration {
			return fmt.Errorf("staging generation changed; review %s", p.Temporary)
		}
		if p.TemporaryObjectIdentity != "" && p.TemporaryObjectIdentity != stagedObjectIdentity {
			return fmt.Errorf("staging object changed; review %s", p.Temporary)
		}
		p.TemporaryFileGeneration = stagedGeneration
		p.TemporaryObjectIdentity = stagedObjectIdentity
	} else if errors.Is(e, os.ErrNotExist) {
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
		if err = validateReviewedSourceSnapshot(ctx, p.Source); err != nil {
			return err
		}
		if err = placeFile(ctx, p.Source, p.Temporary, progress); err != nil {
			return err
		}
		if err = validateReviewedSourceSnapshot(ctx, p.Source); err != nil {
			return err
		}
		stagedDigest, _, p.TemporaryFileGeneration, err = fileDigestSnapshot(ctx, p.Temporary)
		if err != nil {
			return err
		}
		if stagedDigest != p.SHA256 {
			return fmt.Errorf("copy digest mismatch")
		}
		stagedInfo, statErr := os.Lstat(p.Temporary)
		if statErr != nil {
			return statErr
		}
		p.TemporaryObjectIdentity = deliveryFileStableIdentity(stagedInfo)
	} else {
		return e
	}
	if p.TemporaryFileGeneration == "" || p.TemporaryObjectIdentity == "" {
		return fmt.Errorf("staged file identity is unavailable")
	}
	if err = syncPath(p.Temporary); err != nil {
		return err
	}
	// Persist the staged directory entry and any newly created ancestors before
	// the later short, anchored publication section.
	if err = syncPublicationDirectory(p.Temporary); err != nil {
		return err
	}

	if p.TargetObserved {
		backupDigest, _, backupGeneration, backupErr := fileDigestSnapshot(ctx, p.Backup)
		if backupErr != nil && !errors.Is(backupErr, os.ErrNotExist) {
			return backupErr
		}
		if backupErr == nil {
			if backupDigest != p.PreviousSHA256 {
				return fmt.Errorf("rollback identity mismatch")
			}
			backupInfo, e := os.Stat(p.Backup)
			if e != nil {
				return e
			}
			targetInfo, e := os.Stat(p.Target)
			if e != nil {
				return e
			}
			if !os.SameFile(backupInfo, targetInfo) {
				return fmt.Errorf("rollback link no longer identifies the reviewed target")
			}
			targetAfterHash := deliveryFileGeneration(targetInfo)
			if targetAfterHash == "" || backupGeneration == "" || targetAfterHash != backupGeneration {
				return fmt.Errorf("placement target generation is unavailable")
			}
			if p.TargetAfterBackupGen != "" && p.TargetAfterBackupGen != targetAfterHash {
				return fmt.Errorf("rollback target changed after backup verification")
			}
			p.TargetAfterBackupGen = targetAfterHash
		}
	}
	if recovery, ok := ctx.Value(recoveryPlacementKey{}).(*recoveryPlacementContext); ok && recovery.Validate != nil {
		if err = recovery.Validate(); err != nil {
			return err
		}
	}
	return s.db.UpdatePreparedPlacement(ctx, *p)
}

// Legacy journals predate the per-object publication identity fields. They may
// be upgraded from a digest-proven published target only when none of those
// identity fields were ever recorded. Newer journals must bind publication to
// the staged object or to the exact reviewed target generation.
func legacyPlacementIdentitySnapshot(p sqlite.Placement) bool {
	return p.StorageIdentity == "" && p.TargetParentIdentity == "" &&
		p.TargetFileGeneration == "" && p.TemporaryFileGeneration == "" && p.TemporaryObjectIdentity == ""
}

func publishedPlacementMatchesIntent(p sqlite.Placement, info os.FileInfo, generation string, allowLegacy bool) bool {
	if info == nil || generation == "" || deliveryFileGeneration(info) != generation {
		return false
	}
	if p.PublishedFileGeneration != "" {
		return generation == p.PublishedFileGeneration
	}
	if p.TemporaryObjectIdentity != "" {
		return deliveryFileStableIdentity(info) == p.TemporaryObjectIdentity
	}
	if p.TemporaryFileGeneration != "" {
		return false
	}
	if p.TargetObserved {
		return generation == p.TargetFileGeneration || (p.TargetAfterBackupGen != "" && generation == p.TargetAfterBackupGen)
	}
	return allowLegacy
}

func (s *Service) publishPlacement(ctx context.Context, p *sqlite.Placement) error {
	storage, parentIdentity, err := placementParentIdentity(p.Target)
	if err != nil {
		return err
	}
	if storage != p.StorageIdentity || parentIdentity != p.TargetParentIdentity || parentIdentity != p.StorageRootIdentity {
		return fmt.Errorf("placement storage or parent identity changed; review required")
	}
	root, err := os.OpenRoot(filepath.Dir(p.Target))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	rootInfo, err := root.Stat(".")
	if err != nil || deliveryDirectoryObjectIdentity(rootInfo) != p.TargetParentIdentity {
		return fmt.Errorf("placement parent changed before publication")
	}
	pathParentInfo, err := os.Stat(filepath.Dir(p.Target))
	if err != nil || !os.SameFile(rootInfo, pathParentInfo) {
		return fmt.Errorf("placement parent path changed before publication")
	}
	targetName := filepath.Base(p.Target)
	temporaryName := filepath.Base(p.Temporary)
	backupName := filepath.Base(p.Backup)
	if p.PublishedFileGeneration != "" {
		info, e := root.Lstat(targetName)
		if e != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || deliveryFileGeneration(info) != p.PublishedFileGeneration {
			return fmt.Errorf("published placement generation changed")
		}
		return syncOpenedDirectory(root)
	}
	if p.State == "committed" {
		return fmt.Errorf("committed target changed; a new preview is required")
	}
	stagedInfo, err := root.Lstat(temporaryName)
	if err != nil || stagedInfo.Mode()&os.ModeSymlink != 0 || !stagedInfo.Mode().IsRegular() ||
		deliveryFileGeneration(stagedInfo) != p.TemporaryFileGeneration || deliveryFileStableIdentity(stagedInfo) != p.TemporaryObjectIdentity {
		return fmt.Errorf("staging generation changed before publication")
	}
	if p.TargetObserved {
		current, e := root.Lstat(targetName)
		if e != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() ||
			(deliveryFileGeneration(current) != p.TargetAfterBackupGen && deliveryFileGeneration(current) != p.TargetFileGeneration) {
			return fmt.Errorf("target generation changed before publication")
		}
		backup, backupErr := root.Lstat(backupName)
		if errors.Is(backupErr, os.ErrNotExist) {
			if deliveryFileGeneration(current) != p.TargetFileGeneration {
				return fmt.Errorf("original target changed before rollback link")
			}
			preLink := current
			if err = root.Link(targetName, backupName); err != nil {
				return err
			}
			backup, backupErr = root.Lstat(backupName)
			current, e = root.Lstat(targetName)
			if backupErr != nil || e != nil || !os.SameFile(current, backup) || !samePlacementContentObservation(preLink, current) || !samePlacementContentObservation(preLink, backup) {
				return fmt.Errorf("rollback link could not be bound to the reviewed target")
			}
			p.TargetAfterBackupGen = deliveryFileGeneration(current)
			if p.TargetAfterBackupGen == "" {
				return fmt.Errorf("placement target generation is unavailable")
			}
			if err = syncOpenedDirectory(root); err != nil {
				return err
			}
			if err = s.db.UpdatePreparedPlacement(ctx, *p); err != nil {
				return err
			}
		} else if backupErr != nil || !os.SameFile(current, backup) {
			return fmt.Errorf("rollback link changed before publication")
		}
	} else if _, e := root.Lstat(targetName); e == nil || !errors.Is(e, os.ErrNotExist) {
		return fmt.Errorf("unreviewed target appeared before publication")
	}
	if err = root.Rename(temporaryName, targetName); err != nil {
		return err
	}
	if err = syncOpenedDirectory(root); err != nil {
		return err
	}
	published, err := root.Lstat(targetName)
	if err != nil || published.Mode()&os.ModeSymlink != 0 || !published.Mode().IsRegular() {
		return fmt.Errorf("published placement cannot be identified")
	}
	pathParentInfo, err = os.Stat(filepath.Dir(p.Target))
	if err != nil || !os.SameFile(rootInfo, pathParentInfo) {
		return fmt.Errorf("placement parent path changed during publication")
	}
	if !samePlacementContentObservation(stagedInfo, published) {
		return fmt.Errorf("published file differs from the staged content observation")
	}
	p.PublishedFileGeneration = deliveryFileGeneration(published)
	if p.PublishedFileGeneration == "" {
		return fmt.Errorf("published placement generation is unavailable")
	}
	if deliveryFileStableIdentity(published) != p.TemporaryObjectIdentity {
		return fmt.Errorf("published object differs from the staged object")
	}
	return s.db.UpdatePreparedPlacement(ctx, *p)
}

// Publication changes directory entries and therefore may change ctime, but it
// must preserve the exact inode and content observation captured before the
// link/rename. The content digest was verified before entering storage authority.
func samePlacementContentObservation(before, after os.FileInfo) bool {
	return before != nil && after != nil && os.SameFile(before, after) && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func syncOpenedDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
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
		var recovery RecoveryImport
		var recoveryObservation string
		if p.RecoveryImport != "" {
			r, e := s.RecoveryImport(ctx, p.RecoveryImport)
			if e != nil {
				continue
			}
			recovery = r
			if r.State == "cancel_pending" || r.State == "cancelled" {
				if e = s.rollbackPlacement(ctx, p); e != nil {
					s.log.Warn("cancelled placement needs reconciliation", "id", p.ID, "err", e)
				}
				continue
			}
			recoveryObservation, e = s.recoveryFinalObservation(ctx, r)
			if e != nil {
				continue
			}
			if e = s.validateRecoveryTarget(ctx, r); e != nil {
				continue
			}
		}
		// Reconcile only already published bytes. An old unplaced intention cannot
		// replace a newer library file merely because the daemon restarted.
		got, _, generation, e := fileDigestSnapshot(ctx, p.Target)
		if e != nil || got != p.SHA256 {
			continue
		}
		info, statErr := os.Lstat(p.Target)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
			!publishedPlacementMatchesIntent(p, info, generation, legacyPlacementIdentitySnapshot(p)) {
			continue
		}
		if e = syncPath(p.Target); e != nil {
			continue
		}
		if p.PublishedFileGeneration == "" {
			p.PublishedFileGeneration = generation
			storage, parentIdentity, identityErr := placementParentIdentity(p.Target)
			if identityErr != nil {
				continue
			}
			if p.StorageIdentity == "" {
				p.StorageIdentity = storage
			}
			if p.StorageRoot == "" {
				p.StorageRoot = filepath.Dir(p.Target)
			}
			if p.StorageRootIdentity == "" {
				p.StorageRootIdentity = parentIdentity
			}
			if p.TargetParentIdentity == "" {
				p.TargetParentIdentity = parentIdentity
			}
			if e = s.db.UpdatePreparedPlacement(ctx, p); e != nil {
				continue
			}
		}
		decisionCtx, release := s.lockStorageDecision(ctx)
		if p.RecoveryImport != "" {
			currentObservation, observeErr := s.recoveryFinalObservation(decisionCtx, recovery)
			if observeErr != nil || currentObservation != recoveryObservation {
				release()
				continue
			}
		}
		current, statErr := os.Lstat(p.Target)
		storage, parentIdentity, identityErr := placementParentIdentity(p.Target)
		if statErr != nil || identityErr != nil || !current.Mode().IsRegular() || current.Mode()&os.ModeSymlink != 0 ||
			deliveryFileGeneration(current) != p.PublishedFileGeneration || storage != p.StorageIdentity ||
			parentIdentity != p.TargetParentIdentity || parentIdentity != p.StorageRootIdentity {
			release()
			continue
		}
		if e = syncPublicationDirectory(p.Target); e != nil {
			release()
			continue
		}
		_, e = s.db.CommitPlacement(decisionCtx, p)
		release()
		if e != nil {
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
	return syncPublicationDirectory(path)
}

func syncPublicationDirectory(path string) error {
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
