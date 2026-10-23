package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/decision"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

const replacementQuarantinePrefix = ".monarr-replacement-"

type replacementPlacementProofKey struct{}

type replacementPlacementSnapshot struct {
	PlacementID, FileGeneration, Path, SHA256, FilesystemGeneration, StorageIdentity string
	MediaFileID, CopyID                                                              int64
}

func (s *Service) startReplacementWorker(ctx context.Context) {
	s.importWG.Add(1)
	go func() {
		defer s.importWG.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		s.processReplacementFileObservations(ctx)
		s.processReplacementOperations(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.processReplacementFileObservations(ctx)
				s.processReplacementOperations(ctx)
			}
		}
	}()
}

// File observations are explicit, visible review work. Hashing happens here
// off the API request path; the resulting digest and filesystem identity bind
// the user's later destructive decision to the exact bytes they reviewed.
func (s *Service) processReplacementFileObservations(ctx context.Context) {
	observations, err := s.db.ListPendingReplacementFileObservations(ctx, 32)
	if err != nil {
		s.log.Warn("replacement observation list failed", "err", err)
		return
	}
	for _, observation := range observations {
		if ctx.Err() != nil {
			return
		}
		observation.State = "stale"
		finish := func(reason string) {
			observation.Error = reason
			if e := s.db.FinishReplacementFileObservation(ctx, observation); e != nil && !errors.Is(e, sqlite.ErrNotFound) {
				s.log.Warn("replacement observation update failed", "observation", observation.ID, "err", e)
			}
		}
		item, e := s.db.GetMediaItemFull(ctx, observation.MediaItemID)
		if e != nil {
			finish("media item is unavailable")
			continue
		}
		files, e := s.db.ListFilesForItem(ctx, observation.MediaItemID)
		if e != nil {
			finish("media file is unavailable")
			continue
		}
		var file *domain.MediaFile
		for i := range files {
			if files[i].ID == observation.MediaFileID {
				file = &files[i]
				break
			}
		}
		if file == nil || file.FileGeneration != observation.FileGeneration || filepath.Clean(file.Path) != filepath.Clean(observation.Path) {
			finish("selected file changed before observation")
			continue
		}
		root := item.Path
		if file.CopyID != 0 {
			copy, copyErr := s.db.GetMediaCopy(ctx, item.ID, file.CopyID)
			if copyErr != nil {
				finish("selected copy is unavailable")
				continue
			}
			if copy.Path != "" {
				root = copy.Path
			}
		}
		rel, e := filepath.Rel(filepath.Clean(root), filepath.Clean(file.Path))
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			finish("selected file is outside the copy folder")
			continue
		}
		storage, e := replacementStorageIdentity(root)
		if e != nil || storage != observation.StorageIdentity {
			finish("selected storage mount changed")
			continue
		}
		before, e := os.Lstat(file.Path)
		if errors.Is(e, os.ErrNotExist) {
			trustedStorage, trustErr := s.trustedReplacementStorageIdentity(ctx, root, file.ID, file.FileGeneration)
			if trustErr != nil {
				finish("storage identity unverified; restore expected storage")
				continue
			}
			if trustedStorage != observation.StorageIdentity || storage != trustedStorage {
				finish("expected storage identity changed; restore expected storage")
				continue
			}
			parentIdentity, absenceErr := observeMissingReplacementPath(root, file.Path, observation.StorageIdentity)
			if absenceErr != nil {
				finish("selected file or expected storage is unavailable: " + absenceErr.Error())
				continue
			}
			observation.State = "absent"
			observation.FileIdentity = parentIdentity
			observation.FilesystemGeneration = parentIdentity
			observation.Size = 0
			observation.SHA256 = ""
			observation.Error = "file is absent on the verified selected storage"
			if e = s.db.FinishReplacementFileObservation(ctx, observation); e != nil && !errors.Is(e, sqlite.ErrNotFound) {
				s.log.Warn("replacement absence observation commit failed", "observation", observation.ID, "err", e)
			}
			continue
		}
		if e = rejectSymlinks(file.Path, false); e != nil {
			finish("selected file path requires review")
			continue
		}
		if e != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
			finish("selected file is unavailable or not regular")
			continue
		}
		generation := deliveryFileGeneration(before)
		identity := deliveryFileStableIdentity(before)
		if generation == "" || identity == "" {
			finish("filesystem identity is unavailable on this platform")
			continue
		}
		digest, size, e := fileDigest(ctx, file.Path)
		if e != nil {
			finish("selected file could not be read safely: " + e.Error())
			continue
		}
		after, e := os.Lstat(file.Path)
		storageAfter, mountErr := replacementStorageIdentity(root)
		if e != nil || mountErr != nil || storageAfter != storage || !os.SameFile(before, after) || generation != deliveryFileGeneration(after) || identity != deliveryFileStableIdentity(after) || size != after.Size() {
			finish("selected file or storage mount changed during observation")
			continue
		}
		observation.State, observation.SHA256, observation.FileIdentity, observation.FilesystemGeneration, observation.Size = "ready", digest, identity, generation, size
		observation.Error = ""
		if e = s.db.PinReplacementStorageIdentity(ctx, filepath.Clean(root), storage, observation.ID); e != nil {
			message := "storage identity unverified; restore expected storage"
			if errors.Is(e, sqlite.ErrReplacementStorageIdentityConflict) {
				message = "expected storage identity changed; restore expected storage"
			}
			observation.State = "failed"
			finish(message)
			continue
		}
		if e = s.db.FinishReplacementFileObservation(ctx, observation); e != nil && !errors.Is(e, sqlite.ErrNotFound) {
			s.log.Warn("replacement observation commit failed", "observation", observation.ID, "err", e)
		}
	}
}

func (s *Service) processReplacementOperations(ctx context.Context) {
	operations, err := s.db.ListOpenReplacementOperations(ctx, 32)
	if err != nil {
		s.log.Warn("replacement worker: could not list operations", "err", err)
		return
	}
	for _, op := range operations {
		if ctx.Err() != nil {
			return
		}
		if err := s.processReplacementOperation(ctx, op); err != nil {
			s.log.Warn("replacement operation remains pending", "operation", op.ID, "err", err)
		}
	}
}

func (s *Service) processReplacementOperation(ctx context.Context, op sqlite.ReplacementOperation) error {
	if op.State == "accepted" || op.State == "removing" || op.State == "cleanup_blocked" {
		if op.Intent == "replace_bad" {
			if err := s.removeBadReplacementSources(ctx, op); err != nil {
				_ = s.db.TransitionReplacementOperation(ctx, op.ID, "cleanup_blocked", err.Error())
				return err
			}
		}
		if err := s.db.TransitionReplacementOperation(ctx, op.ID, "searching", ""); err != nil {
			return err
		}
		op.State = "searching"
	}
	covered, err := s.db.ReplacementHasCommittedCoverage(ctx, op)
	if err != nil {
		return err
	}
	if covered {
		proof, proofErr := s.inspectReplacementCoverage(ctx, op)
		if proofErr != nil {
			_ = s.db.TransitionReplacementOperation(ctx, op.ID, "cleanup_blocked", proofErr.Error())
			return proofErr
		}
		ctx = context.WithValue(ctx, replacementPlacementProofKey{}, proof)
		if err := s.removeBadReplacementSources(ctx, op); err != nil {
			_ = s.db.TransitionReplacementOperation(ctx, op.ID, "cleanup_blocked", err.Error())
			return err
		}
		return s.db.TransitionReplacementOperation(ctx, op.ID, "complete", "")
	}
	if op.State != "searching" && op.State != "waiting_for_release" && op.State != "replacing" {
		return nil
	}

	rows, err := s.db.ListReplacementDownloads(ctx, op.ID)
	if err != nil {
		return err
	}
	for _, dl := range rows {
		switch dl.State {
		case "grabbed", "downloading", "downloaded", "awaiting_import", "importing":
			return s.db.TransitionReplacementOperation(ctx, op.ID, "replacing", "replacement download is still active")
		case "failed":
			blocked, reason, cleanupErr := s.replacementAttemptCleanupBlock(ctx, dl)
			if cleanupErr != nil {
				return cleanupErr
			}
			if blocked {
				return s.db.TransitionReplacementOperation(ctx, op.ID, "cleanup_blocked", reason)
			}
		case "imported":
			// A successful first file in a multi-episode request does not finish
			// the whole operation. Exact committed placement receipts below
			// decide whether the remaining selected scope is still wanted.
		}
	}

	item, err := s.db.GetMediaItemFull(ctx, op.MediaItemID)
	if err != nil {
		return err
	}
	if len(op.EpisodeIDs) == 0 {
		return s.searchAndAdmitReplacement(ctx, op, item, -1, 0)
	}
	for _, episodeID := range op.EpisodeIDs {
		seasonNumber, episodeNumber, ok := episodeNumbers(item, episodeID)
		if !ok {
			return s.db.TransitionReplacementOperation(ctx, op.ID, "failed", fmt.Sprintf("selected episode %d no longer belongs to this item", episodeID))
		}
		if s.replacementEpisodeCovered(ctx, op, episodeID) {
			continue
		}
		return s.searchAndAdmitReplacement(ctx, op, item, seasonNumber, episodeNumber)
	}
	return s.db.TransitionReplacementOperation(ctx, op.ID, "complete", "")
}

// inspectReplacementCoverage hashes the current exact placement outside the
// storage decision lock. The resulting stat generation and receipt identity
// are rechecked under the final lock immediately before retiring old bytes.
func (s *Service) inspectReplacementCoverage(ctx context.Context, op sqlite.ReplacementOperation) (map[int64]replacementPlacementSnapshot, error) {
	placements, err := s.db.CurrentReplacementPlacements(ctx, op.ID)
	if err != nil {
		return nil, err
	}
	item, err := s.db.GetMediaItemFull(ctx, op.MediaItemID)
	if err != nil {
		return nil, err
	}
	root := item.Path
	if op.CopyID != 0 {
		copy, copyErr := s.db.GetMediaCopy(ctx, op.MediaItemID, op.CopyID)
		if copyErr != nil {
			return nil, copyErr
		}
		if copy.Path != "" {
			root = copy.Path
		}
	}
	storageIdentity, err := expectedStorageIdentity(root)
	if err != nil || storageIdentity == "" {
		return nil, fmt.Errorf("replacement target storage is unavailable")
	}
	wanted := map[int64]bool{}
	for _, episodeID := range op.EpisodeIDs {
		wanted[episodeID] = true
	}
	covered := map[int64]bool{}
	proof := map[int64]replacementPlacementSnapshot{}
	for _, placement := range placements {
		if placement.MediaItemID != op.MediaItemID || placement.CopyID != op.CopyID || placement.PlacementID == "" || placement.FileGeneration == "" || placement.Path == "" || placement.SHA256 == "" {
			continue
		}
		relevant := len(wanted) == 0
		for _, episodeID := range placement.EpisodeIDs {
			if wanted[episodeID] {
				relevant = true
				covered[episodeID] = true
			}
		}
		if !relevant {
			continue
		}
		rel, relErr := filepath.Rel(filepath.Clean(root), filepath.Clean(placement.Path))
		if relErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("replacement placement escaped the selected copy root")
		}
		if pathErr := rejectSymlinks(placement.Path, false); pathErr != nil {
			return nil, fmt.Errorf("replacement placement path requires review: %w", pathErr)
		}
		flagged, flagErr := s.db.HasOpenMediaFileReviewFlag(ctx, placement.MediaFileID, placement.FileGeneration)
		if flagErr != nil {
			return nil, flagErr
		}
		if flagged {
			return nil, fmt.Errorf("replacement placement %s has an open library review flag", placement.Path)
		}
		info, statErr := os.Lstat(placement.Path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("replacement placement is unavailable for selected scope: %s", placement.Path)
		}
		digest, size, digestErr := fileDigest(ctx, placement.Path)
		after, afterErr := os.Lstat(placement.Path)
		if digestErr != nil || afterErr != nil || digest != placement.SHA256 || size != info.Size() || !os.SameFile(info, after) || deliveryFileGeneration(info) != deliveryFileGeneration(after) {
			return nil, fmt.Errorf("replacement placement changed after commit: %s", placement.Path)
		}
		proof[placement.MediaFileID] = replacementPlacementSnapshot{PlacementID: placement.PlacementID, FileGeneration: placement.FileGeneration,
			Path: placement.Path, SHA256: placement.SHA256, FilesystemGeneration: deliveryFileGeneration(after), StorageIdentity: storageIdentity, MediaFileID: placement.MediaFileID, CopyID: placement.CopyID}
	}
	for episodeID := range wanted {
		if !covered[episodeID] {
			return nil, fmt.Errorf("replacement placement no longer covers selected episode %d", episodeID)
		}
	}
	if len(proof) == 0 {
		return nil, fmt.Errorf("replacement placement is no longer present")
	}
	return proof, nil
}

func (s *Service) replacementHasCurrentCommittedPlacement(ctx context.Context, op sqlite.ReplacementOperation) (bool, error) {
	if covered, err := s.db.ReplacementHasCommittedCoverage(ctx, op); err != nil || !covered {
		return false, err
	}
	_, err := s.inspectReplacementCoverage(ctx, op)
	return err == nil, err
}

func (s *Service) finalReplacementPlacementCheck(ctx context.Context, op sqlite.ReplacementOperation) error {
	proof, ok := ctx.Value(replacementPlacementProofKey{}).(map[int64]replacementPlacementSnapshot)
	if !ok || len(proof) == 0 {
		return fmt.Errorf("current replacement placement proof is missing")
	}
	item, err := s.db.GetMediaItemFull(ctx, op.MediaItemID)
	if err != nil {
		return err
	}
	root := item.Path
	if op.CopyID != 0 {
		copy, copyErr := s.db.GetMediaCopy(ctx, op.MediaItemID, op.CopyID)
		if copyErr != nil {
			return copyErr
		}
		if copy.Path != "" {
			root = copy.Path
		}
	}
	storageIdentity, err := expectedStorageIdentity(root)
	if err != nil {
		return fmt.Errorf("replacement target storage changed before cleanup")
	}
	placements, err := s.db.CurrentReplacementPlacements(ctx, op.ID)
	if err != nil {
		return err
	}
	current := map[int64]sqlite.ReplacementPlacement{}
	for _, placement := range placements {
		current[placement.MediaFileID] = placement
	}
	for fileID, expected := range proof {
		placement, exists := current[fileID]
		if !exists || placement.PlacementID != expected.PlacementID || placement.FileGeneration != expected.FileGeneration || placement.Path != expected.Path || placement.SHA256 != expected.SHA256 || placement.CopyID != expected.CopyID || expected.StorageIdentity != storageIdentity {
			return fmt.Errorf("replacement placement identity changed before old-file cleanup")
		}
		if err := rejectSymlinks(expected.Path, false); err != nil {
			return fmt.Errorf("replacement placement path changed before cleanup: %w", err)
		}
		flagged, flagErr := s.db.HasOpenMediaFileReviewFlag(ctx, fileID, expected.FileGeneration)
		if flagErr != nil || flagged {
			return fmt.Errorf("replacement placement is under review before old-file cleanup")
		}
		info, statErr := os.Lstat(expected.Path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || deliveryFileGeneration(info) != expected.FilesystemGeneration {
			return fmt.Errorf("replacement placement bytes changed before old-file cleanup")
		}
	}
	return nil
}

// replacementAttemptCleanupBlock reconciles the mutable download flag with the
// durable Runner delivery decision. An explicit Keep is a settled exception:
// it retains the failed bytes while allowing the replacement to proceed.
// Without a matching delivery receipt, cleanup remains unresolved and fails
// closed. A removed payload and committed usable replacement are handled by
// their own durable receipts before this gate.
func (s *Service) replacementAttemptCleanupBlock(ctx context.Context, dl sqlite.Download) (bool, string, error) {
	if !dl.CleanupPending && (!dl.DeliveryExpected || dl.PayloadRemoved) {
		return false, "", nil
	}
	if dl.Transfer == "" {
		return true, "failed replacement attempt has no exact Runner transfer token", nil
	}
	cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID)
	if err != nil {
		return true, "could not verify Runner cleanup or Keep decision: " + err.Error(), nil
	}
	client, ok := s.newClient(cfg).(ports.RequestAdmissionProtocol)
	if !ok {
		return true, "failed replacement attempt has no authoritative Runner admission endpoint", nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	admission, found, err := client.GetRequestAdmission(lookupCtx, dl.Transfer)
	cancel()
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("admission receipt is unavailable")
		}
		return true, "could not verify current Runner cleanup or Keep decision: " + err.Error(), nil
	}
	if admission.ProtocolVersion != 1 || admission.RequestToken != dl.Transfer || admission.JobID == "" ||
		(admission.RunnerInstanceID != dl.RunnerInstanceID && dl.RunnerInstanceID != "") ||
		(admission.Generation != dl.RunnerAdmissionGeneration && dl.RunnerAdmissionGeneration != "") {
		return true, "Runner admission no longer matches the pinned failed attempt", nil
	}
	if err = s.db.BindRunnerAdmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, admission.RunnerInstanceID, admission.Generation); err != nil {
		return true, "Runner admission identity changed; failed attempt remains protected", nil
	}
	if admission.JobID != dl.Handle {
		return true, "Runner admission job handle changed; failed attempt remains protected", nil
	}
	// Keep is a live Runner decision. The immutable publication receipt may
	// capture an earlier value, so only this exact admission response can grant
	// the exception that retains the old bytes while allowing replacement.
	if admission.KeepSourceOnFailure || admission.CustodyState == "kept_by_user" {
		return false, "", nil
	}
	if admission.CustodyState == "curator" {
		return true, "Runner transferred the failed payload to Curator; selected import and cleanup must settle first", nil
	}
	if runnerFailureNeedsCleanupBeforeReplacement(admission) {
		return true, "failed replacement attempt still has required payload cleanup pending", nil
	}
	return false, "", nil
}

func (s *Service) removeBadReplacementSources(ctx context.Context, op sqlite.ReplacementOperation) error {
	files, err := s.db.ListReplacementFiles(ctx, op.ID)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f.RemovalState == "removed" {
			continue
		}
		selectedEpisodes := intersection(f.EpisodeIDs, op.EpisodeIDs)
		if f.Disposition == "preserved" {
			if f.RemovalState == "pending" && len(selectedEpisodes) > 0 {
				if err = s.db.DetachReplacementEpisodes(ctx, op.ID, f, selectedEpisodes); err != nil {
					return fmt.Errorf("detach selected episode links for %s: %w", f.Path, err)
				}
			} else {
				if err = s.db.SetReplacementFileRemovalState(ctx, op.ID, f, "removed"); err != nil {
					return err
				}
			}
			continue
		}
		if err = s.db.SetReplacementFileRemovalState(ctx, op.ID, f, "pending"); err != nil {
			return err
		}
		if f.SHA256 == "" && f.FileIdentity != "" && f.StorageIdentity != "" {
			item, itemErr := s.db.GetMediaItemFull(ctx, op.MediaItemID)
			if itemErr != nil {
				return itemErr
			}
			rootPath := item.Path
			if op.CopyID != 0 {
				copy, copyErr := s.db.GetMediaCopy(ctx, op.MediaItemID, op.CopyID)
				if copyErr != nil {
					return copyErr
				}
				if copy.Path != "" {
					rootPath = copy.Path
				}
			}
			if err = s.deleteMissingBadReplacementRecord(ctx, op, f, rootPath, f.StorageIdentity); err != nil {
				return err
			}
			continue
		}
		if f.FileIdentity == "" || f.StorageIdentity == "" || f.SHA256 == "" {
			return fmt.Errorf("reviewed file identity is incomplete for %s", f.Path)
		}
		err = s.removeBadReplacementFile(ctx, op, f)
		if err != nil {
			return err
		}
	}
	return nil
}

// removeBadReplacementFile hashes through an anchored file descriptor before
// acquiring the shared storage locks. The final critical section only checks
// the same file generation at its anchored name, quarantines it atomically,
// and unlinks that exact object. A changed file is left in quarantine for
// review; it is never overwritten or deleted on a path-only match.
func (s *Service) removeBadReplacementFile(ctx context.Context, op sqlite.ReplacementOperation, f sqlite.ReplacementFile) error {
	item, err := s.db.GetMediaItemFull(ctx, op.MediaItemID)
	if err != nil {
		return err
	}
	rootPath := item.Path
	if op.CopyID != 0 {
		copy, e := s.db.GetMediaCopy(ctx, op.MediaItemID, op.CopyID)
		if e != nil {
			return e
		}
		if copy.Path != "" {
			rootPath = copy.Path
		}
	}
	rel, err := filepath.Rel(filepath.Clean(rootPath), filepath.Clean(f.Path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !filepath.IsLocal(rel) {
		return fmt.Errorf("replacement source escaped the selected copy folder")
	}
	storage, err := expectedStorageIdentity(rootPath)
	if err != nil || storage != f.StorageIdentity {
		return fmt.Errorf("selected storage mount changed before removal")
	}
	currentFile, err := s.db.GetMediaFileCustodyByPath(ctx, f.Path)
	if err != nil || currentFile.ID != f.MediaFileID || currentFile.FileGeneration != f.FileGeneration || currentFile.SourceGeneration != f.SourceGeneration {
		return fmt.Errorf("selected library file generation changed before removal")
	}
	baseName := filepath.Base(rel)
	parentRel := filepath.Dir(rel)
	quarantineName := replacementQuarantinePrefix + op.ID + "-" + fmt.Sprint(f.MediaFileID)
	anchor, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer func() { _ = anchor.Close() }()
	parent, err := openDeliveryDirectoryNoSymlinks(anchor, parentRel)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()

	// A prior run may have durably renamed the reviewed file and stopped before
	// unlinking it. Rehash the quarantine outside the global storage lock.
	if _, qErr := parent.Lstat(quarantineName); qErr == nil {
		qParent, qFile, qNamed, e := openDeliveryFileNoSymlinks(anchor, filepath.Join(parentRel, quarantineName))
		if e != nil {
			return e
		}
		qInfo, e := qFile.Stat()
		if e != nil || !qInfo.Mode().IsRegular() || deliveryFileStableIdentity(qInfo) != f.FileIdentity {
			_ = qFile.Close()
			_ = qParent.Close()
			return fmt.Errorf("replacement quarantine identity changed")
		}
		qDigest, qSize, digestErr := digestOpenFile(ctx, qFile)
		qAfter, statErr := qFile.Stat()
		closeErr := qFile.Close()
		parentCloseErr := qParent.Close()
		if err = errors.Join(digestErr, statErr, closeErr, parentCloseErr); err != nil {
			return err
		}
		if !os.SameFile(qNamed, qAfter) || deliveryFileGeneration(qAfter) != deliveryFileGeneration(qInfo) || qDigest != f.SHA256 || qSize != f.Size {
			return fmt.Errorf("replacement quarantine no longer matches reviewed bytes")
		}
		parentInfo, e := parent.Stat(".")
		if e != nil {
			return e
		}
		return s.removeVerifiedReplacementQuarantine(ctx, op, f, rootPath, storage, parentRel, baseName, quarantineName, parentInfo, qInfo)
	} else if !errors.Is(qErr, os.ErrNotExist) {
		return qErr
	}

	sourceParent, file, named, err := openDeliveryFileNoSymlinks(anchor, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.deleteMissingBadReplacementRecord(ctx, op, f, rootPath, storage)
		}
		return fmt.Errorf("open reviewed replacement source: %w", err)
	}
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || !os.SameFile(named, before) || deliveryFileStableIdentity(before) != f.FileIdentity || before.Size() != f.Size {
		_ = file.Close()
		_ = sourceParent.Close()
		return fmt.Errorf("replacement source changed since review: %s", f.Path)
	}
	digest, size, digestErr := digestOpenFile(ctx, file)
	after, statErr := file.Stat()
	closeErr := file.Close()
	parentCloseErr := sourceParent.Close()
	if err = errors.Join(digestErr, statErr, closeErr, parentCloseErr); err != nil {
		return err
	}
	if digest != f.SHA256 || size != f.Size || !os.SameFile(before, after) || deliveryFileGeneration(before) != deliveryFileGeneration(after) {
		return fmt.Errorf("replacement source bytes changed since review: %s", f.Path)
	}
	return s.quarantineAndRemoveReplacementSource(ctx, op, f, rootPath, storage, parentRel, baseName, quarantineName, before)
}

func (s *Service) replacementFinalCheck(ctx context.Context, op sqlite.ReplacementOperation, f sqlite.ReplacementFile, rootPath, storage string) error {
	if _, proofExists := ctx.Value(replacementPlacementProofKey{}).(map[int64]replacementPlacementSnapshot); proofExists {
		if err := s.finalReplacementPlacementCheck(ctx, op); err != nil {
			return err
		}
	}
	current, err := s.db.GetMediaFileCustodyByPath(ctx, f.Path)
	if err != nil || current.ID != f.MediaFileID || current.MediaItemID != op.MediaItemID || current.FileGeneration != f.FileGeneration || current.SourceGeneration != f.SourceGeneration {
		return fmt.Errorf("selected library file generation changed before removal")
	}
	storageNow, err := expectedStorageIdentity(rootPath)
	if err != nil || storageNow != storage || storageNow != f.StorageIdentity {
		return fmt.Errorf("selected storage mount changed before removal")
	}
	pinned, err := s.db.GetReplacementStorageIdentity(ctx, filepath.Clean(rootPath))
	if err != nil || pinned != storageNow {
		return fmt.Errorf("storage identity unverified; restore expected storage")
	}
	return nil
}

func (s *Service) quarantineAndRemoveReplacementSource(ctx context.Context, op sqlite.ReplacementOperation, f sqlite.ReplacementFile, rootPath, storage, parentRel, name, quarantineName string, reviewed os.FileInfo) error {
	s.importTargetMu.Lock()
	decisionCtx, release := s.lockStorageDecision(ctx)
	locked := true
	unlock := func() {
		if locked {
			locked = false
			release()
			s.importTargetMu.Unlock()
		}
	}
	defer unlock()
	if err := s.replacementFinalCheck(decisionCtx, op, f, rootPath, storage); err != nil {
		return err
	}
	anchor, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer func() { _ = anchor.Close() }()
	parent, err := openDeliveryDirectoryNoSymlinks(anchor, parentRel)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	current, err := parent.Lstat(name)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(reviewed, current) || deliveryFileGeneration(current) != deliveryFileGeneration(reviewed) || deliveryFileStableIdentity(current) != f.FileIdentity {
		return fmt.Errorf("replacement source changed before quarantine: %s", f.Path)
	}
	if _, err := parent.Lstat(quarantineName); err == nil {
		return fmt.Errorf("replacement quarantine path already exists beside %s", f.Path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Link is exclusive: unlike rename, it cannot overwrite a quarantine name
	// created by a concurrent process. Recovery handles the brief two-name
	// state if the process stops before unlinking the original name.
	if err := parent.Link(name, quarantineName); err != nil {
		return fmt.Errorf("quarantine bad source %s: %w", f.Path, err)
	}
	if err := syncRootDirectory(parent, "."); err != nil {
		return err
	}
	original, originalErr := parent.Lstat(name)
	quarantined, quarantineErr := parent.Lstat(quarantineName)
	if originalErr != nil || quarantineErr != nil || !sameReplacementContentObservation(reviewed, original) ||
		!sameReplacementContentObservation(reviewed, quarantined) {
		return fmt.Errorf("replacement source changed during quarantine; bytes retained for review")
	}
	if err := parent.Remove(name); err != nil {
		return fmt.Errorf("remove original bad source name %s: %w", f.Path, err)
	}
	if err := syncRootDirectory(parent, "."); err != nil {
		return err
	}
	quarantined, err = parent.Lstat(quarantineName)
	if err != nil || !sameReplacementContentObservation(reviewed, quarantined) {
		return fmt.Errorf("replacement source changed after quarantine; bytes retained for review")
	}
	quarantineSnapshot := quarantined
	unlock()
	if err := verifyReplacementQuarantineDigest(ctx, rootPath, parentRel, quarantineName, f, quarantineSnapshot); err != nil {
		return err
	}
	return s.finalizeReplacementQuarantine(ctx, op, f, rootPath, storage, parentRel, name, quarantineName, quarantineSnapshot)
}

func (s *Service) removeVerifiedReplacementQuarantine(ctx context.Context, op sqlite.ReplacementOperation, f sqlite.ReplacementFile, rootPath, storage, parentRel, sourceName, quarantineName string, reviewedParent, reviewed os.FileInfo) error {
	s.importTargetMu.Lock()
	decisionCtx, release := s.lockStorageDecision(ctx)
	locked := true
	unlock := func() {
		if locked {
			locked = false
			release()
			s.importTargetMu.Unlock()
		}
	}
	defer unlock()
	if err := s.replacementFinalCheck(decisionCtx, op, f, rootPath, storage); err != nil {
		return err
	}
	anchor, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer func() { _ = anchor.Close() }()
	parent, err := openDeliveryDirectoryNoSymlinks(anchor, parentRel)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	currentParent, err := parent.Stat(".")
	if err != nil || !os.SameFile(reviewedParent, currentParent) {
		return fmt.Errorf("replacement quarantine directory changed before unlink")
	}
	current, err := parent.Lstat(quarantineName)
	if err != nil || !sameReplacementContentObservation(reviewed, current) {
		return fmt.Errorf("replacement quarantine changed before unlink")
	}
	// A crash after the exclusive hard link can leave both names. Remove the
	// original only when it is the same inode, size, and content timestamp.
	if source, sourceErr := parent.Lstat(sourceName); sourceErr == nil {
		if !sameReplacementContentObservation(reviewed, source) {
			return fmt.Errorf("replacement source path changed while quarantine recovery was pending")
		}
		if err := parent.Remove(sourceName); err != nil {
			return err
		}
		if err := syncRootDirectory(parent, "."); err != nil {
			return err
		}
		current, err = parent.Lstat(quarantineName)
		if err != nil || !sameReplacementContentObservation(reviewed, current) {
			return fmt.Errorf("replacement quarantine changed during recovery")
		}
	} else if !errors.Is(sourceErr, os.ErrNotExist) {
		return sourceErr
	}
	quarantineSnapshot := current
	unlock()
	if err := verifyReplacementQuarantineDigest(ctx, rootPath, parentRel, quarantineName, f, quarantineSnapshot); err != nil {
		return err
	}
	return s.finalizeReplacementQuarantine(ctx, op, f, rootPath, storage, parentRel, sourceName, quarantineName, quarantineSnapshot)
}

func (s *Service) deleteMissingBadReplacementRecord(ctx context.Context, op sqlite.ReplacementOperation, f sqlite.ReplacementFile, rootPath, storage string) error {
	s.importTargetMu.Lock()
	defer s.importTargetMu.Unlock()
	decisionCtx, release := s.lockStorageDecision(ctx)
	defer release()
	if err := s.replacementFinalCheck(decisionCtx, op, f, rootPath, storage); err != nil {
		return err
	}
	rel, err := filepath.Rel(filepath.Clean(rootPath), filepath.Clean(f.Path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !filepath.IsLocal(rel) {
		return fmt.Errorf("replacement source path is outside its selected root")
	}
	anchor, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer func() { _ = anchor.Close() }()
	parentRel, sourceName := filepath.Dir(rel), filepath.Base(rel)
	parent, err := openDeliveryDirectoryNoSymlinks(anchor, parentRel)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	quarantineName := replacementQuarantinePrefix + op.ID + "-" + fmt.Sprint(f.MediaFileID)
	if _, err := parent.Lstat(sourceName); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replacement source reappeared before library record cleanup")
	}
	if _, err := parent.Lstat(quarantineName); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replacement quarantine appeared before library record cleanup")
	}
	parentInfo, err := parent.Stat(".")
	if err != nil || (f.SHA256 == "" && deliveryDirectoryObjectIdentity(parentInfo) != f.FileIdentity) {
		return fmt.Errorf("replacement parent directory changed since absence review")
	}
	return s.db.DeleteReplacementFileRecord(decisionCtx, op.ID, f)
}

func sameReplacementContentObservation(expected, current os.FileInfo) bool {
	return expected != nil && current != nil && expected.Mode().IsRegular() && current.Mode().IsRegular() &&
		deliveryFileStableIdentity(expected) != "" && deliveryFileStableIdentity(expected) == deliveryFileStableIdentity(current) &&
		expected.Size() == current.Size() && expected.ModTime().Equal(current.ModTime())
}

func verifyReplacementQuarantineDigest(ctx context.Context, rootPath, parentRel, name string, f sqlite.ReplacementFile, expected os.FileInfo) error {
	anchor, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer func() { _ = anchor.Close() }()
	parent, file, named, err := openDeliveryFileNoSymlinks(anchor, filepath.Join(parentRel, name))
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil || !sameReplacementContentObservation(expected, info) || !os.SameFile(named, info) {
		_ = file.Close()
		_ = parent.Close()
		return fmt.Errorf("replacement quarantine identity changed before verification")
	}
	digest, size, digestErr := digestOpenFile(ctx, file)
	after, statErr := file.Stat()
	closeErr := file.Close()
	parentCloseErr := parent.Close()
	if err := errors.Join(digestErr, statErr, closeErr, parentCloseErr); err != nil {
		return err
	}
	if !sameReplacementContentObservation(expected, after) || digest != f.SHA256 || size != f.Size {
		return fmt.Errorf("replacement quarantine bytes changed after review")
	}
	return nil
}

func (s *Service) finalizeReplacementQuarantine(ctx context.Context, op sqlite.ReplacementOperation, f sqlite.ReplacementFile, rootPath, storage, parentRel, sourceName, quarantineName string, expected os.FileInfo) error {
	s.importTargetMu.Lock()
	defer s.importTargetMu.Unlock()
	decisionCtx, release := s.lockStorageDecision(ctx)
	defer release()
	if err := s.replacementFinalCheck(decisionCtx, op, f, rootPath, storage); err != nil {
		return err
	}
	anchor, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer func() { _ = anchor.Close() }()
	parent, err := openDeliveryDirectoryNoSymlinks(anchor, parentRel)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if _, err := parent.Lstat(sourceName); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replacement source path reappeared before final unlink")
	}
	current, err := parent.Lstat(quarantineName)
	if err != nil || !sameReplacementContentObservation(expected, current) {
		return fmt.Errorf("replacement quarantine changed before final unlink")
	}
	if err := parent.Remove(quarantineName); err != nil {
		return err
	}
	if err := syncRootDirectory(parent, "."); err != nil {
		return err
	}
	return s.db.DeleteReplacementFileRecord(decisionCtx, op.ID, f)
}

func intersection(fileEpisodeIDs, selected []int64) []int64 {
	if len(selected) == 0 {
		return append([]int64(nil), fileEpisodeIDs...)
	}
	wanted := map[int64]bool{}
	for _, id := range selected {
		wanted[id] = true
	}
	var out []int64
	for _, id := range fileEpisodeIDs {
		if wanted[id] {
			out = append(out, id)
		}
	}
	return out
}

func episodeNumbers(item domain.MediaItem, episodeID int64) (int, int, bool) {
	for _, season := range item.Seasons {
		for _, episode := range season.Episodes {
			if episode.ID == episodeID {
				return season.Number, episode.EpisodeNumber, true
			}
		}
	}
	return 0, 0, false
}

func (s *Service) replacementEpisodeCovered(ctx context.Context, op sqlite.ReplacementOperation, episodeID int64) bool {
	return s.db.ReplacementHasEpisodeCoverage(ctx, op.ID, episodeID)
}

func (s *Service) searchAndAdmitReplacement(ctx context.Context, op sqlite.ReplacementOperation, item domain.MediaItem, season, episode int) error {
	candidates, status, err := s.SearchCopyDetailed(ctx, op.MediaItemID, op.CopyID, season, episode)
	if err != nil {
		_ = s.db.TransitionReplacementOperation(ctx, op.ID, "waiting_for_release", err.Error())
		return err
	}
	rows, rowErr := s.db.ListReplacementDownloads(ctx, op.ID)
	if rowErr != nil {
		return rowErr
	}
	failed := make([]sqlite.Download, 0)
	for _, row := range rows {
		if row.State == "failed" && !row.CleanupPending && (!row.DeliveryExpected || row.PayloadRemoved) {
			failed = append(failed, row)
		}
	}
	try := func(candidates []Candidate) (bool, error) {
		for _, candidate := range candidates {
			if !candidate.Accepted || (op.Intent == "replace_quality" && !candidate.IsUpgrade) || sameSourceRelease(op, candidate) || sameFailedReplacement(failed, candidate) {
				continue
			}
			evidence := candidate.Match
			_, err = s.Grab(ctx, GrabRequest{MediaItemID: op.MediaItemID, CopyID: op.CopyID,
				Season: season, Episode: episode, Title: candidate.Release.Title, DownloadURL: candidate.Release.DownloadURL,
				Indexer: candidate.Release.Indexer, SourceGUID: candidate.Release.GUID, IndexerID: candidate.Release.IndexerID,
				Protocol: candidate.Release.Protocol, Size: candidate.Release.Size, CandidateToken: candidate.Token,
				MatchEvidence: &evidence, ReplacementOperationID: op.ID})
			if err != nil {
				continue
			}
			return true, s.db.TransitionReplacementOperation(ctx, op.ID, "replacing", "")
		}
		return false, nil
	}
	if admitted, e := try(candidates); e != nil {
		return e
	} else if admitted {
		return nil
	}
	// Episode searches run first. Only then fall back to a season pack; the
	// importer remains pinned to the operation's selected episode allowlist.
	if episode > 0 {
		packCandidates, packStatus, packErr := s.SearchCopyDetailed(ctx, op.MediaItemID, op.CopyID, season, 0)
		if packErr == nil {
			status.Partial = status.Partial || packStatus.Partial
			packCandidates, packErr = s.regradeReplacementPacks(ctx, op, item, season, episode, packCandidates)
		}
		if packErr == nil {
			if admitted, e := try(packCandidates); e != nil {
				return e
			} else if admitted {
				return nil
			}
		} else {
			status.Partial = true
		}
	}
	message := "no acceptable replacement release is currently available"
	if status.Partial {
		message += "; one or more indexer searches failed"
	}
	return s.db.TransitionReplacementOperation(ctx, op.ID, "waiting_for_release", message)
}

// SearchCopyDetailed grades a season pack against all episode targets. A
// scoped replacement must grade it against the one selected episode and the
// selected copy's profile, or a good sibling episode can veto a usable pack.
func (s *Service) regradeReplacementPacks(ctx context.Context, op sqlite.ReplacementOperation, item domain.MediaItem, season, episode int, candidates []Candidate) ([]Candidate, error) {
	var cp *domain.MediaCopy
	if op.CopyID != 0 {
		copy, err := s.db.GetMediaCopy(ctx, item.ID, op.CopyID)
		if err != nil {
			return nil, err
		}
		cp = &copy
	}
	want, err := s.targetCopy(ctx, item, season, episode, cp)
	if err != nil {
		return nil, err
	}
	profile, err := s.db.GetProfile(ctx, want.ProfileID())
	if err != nil {
		return nil, err
	}
	identity, err := s.allIdentity(ctx, time.Now())
	if err != nil {
		return nil, err
	}
	for i := range candidates {
		candidate := &candidates[i]
		blacklistReason := candidate.BlacklistReason
		parsed := parser.Parse(candidate.Release.Title)
		matched := releaseMatch(candidate.Release, parsed, want, identity)
		candidate.Match = matchEvidence(parsed, want, candidate.Release, matched)
		candidate.Accepted = false
		candidate.IsUpgrade = false
		candidate.Rejections = nil
		if !matched.Matched {
			candidate.Rejections = []decision.Rejection{{Code: matched.Code, Reason: matched.Reason}}
		} else {
			verdict := decision.Decide(releaseOf(parsed), want, profile)
			candidate.Accepted, candidate.IsUpgrade, candidate.Rejections = verdict.Accepted, verdict.IsUpgrade, verdict.Rejections
		}
		if blacklistReason != "" {
			candidate.Accepted = false
			candidate.Rejections = append(candidate.Rejections, decision.Rejection{Code: "blacklisted", Reason: blacklistReason})
		}
		candidate.Token = s.issueCandidateToken(op.MediaItemID, op.CopyID, season, episode, candidate.Release, candidate.Match)
	}
	return candidates, nil
}

func sameFailedReplacement(failed []sqlite.Download, candidate Candidate) bool {
	for _, row := range failed {
		if row.SourceGUID != "" && candidate.Release.GUID != "" && row.SourceIndexerID != 0 && candidate.Release.IndexerID != 0 &&
			strings.EqualFold(row.SourceGUID, candidate.Release.GUID) && row.SourceIndexerID == candidate.Release.IndexerID {
			return true
		}
		if strings.TrimSpace(row.Indexer) != "" && strings.TrimSpace(candidate.Release.Indexer) != "" &&
			strings.EqualFold(strings.TrimSpace(row.Indexer), strings.TrimSpace(candidate.Release.Indexer)) && normalizeRelease(row.ReleaseTitle) != "" &&
			normalizeRelease(row.ReleaseTitle) == normalizeRelease(candidate.Release.Title) {
			return true
		}
	}
	return false
}

func sameSourceRelease(op sqlite.ReplacementOperation, candidate Candidate) bool {
	if op.SourceGUID != "" && candidate.Release.GUID != "" && op.SourceIndexerID != 0 && candidate.Release.IndexerID != 0 &&
		strings.EqualFold(op.SourceGUID, candidate.Release.GUID) && op.SourceIndexerID == candidate.Release.IndexerID {
		return true
	}
	return strings.TrimSpace(op.SourceIndexer) != "" && strings.TrimSpace(candidate.Release.Indexer) != "" &&
		strings.EqualFold(strings.TrimSpace(op.SourceIndexer), strings.TrimSpace(candidate.Release.Indexer)) && normalizeRelease(op.SourceRelease) != "" &&
		normalizeRelease(op.SourceRelease) == normalizeRelease(candidate.Release.Title)
}
