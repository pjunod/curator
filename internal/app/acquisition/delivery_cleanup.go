package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/probe"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// recordDeliveryImportOutcomes records placement and conservative redundancy
// evidence even if a later file stopped the pack import. Any manifest entry
// without an outcome remains protected as recoverable.
func (s *Service) recordDeliveryImportOutcomes(ctx context.Context, r sqlite.DeliveryReceipt, verified []verifiedDeliveryFile, result ImportResult) error {
	bySource := make(map[string]verifiedDeliveryFile, len(verified))
	for _, f := range verified {
		bySource[filepath.Clean(f.absolute)] = f
	}
	seen := map[string]bool{}
	var errs []error
	for _, outcome := range result.Files {
		f, ok := bySource[filepath.Clean(outcome.SourcePath)]
		if !ok {
			errs = append(errs, fmt.Errorf("import outcome source is outside accepted manifest: %q", outcome.SourcePath))
			continue
		}
		seen[f.relative] = true
		disposition, reason, libraryPath := "unknown_recoverable", outcome.Reason, ""
		if outcome.Imported {
			disposition, libraryPath = "placed", outcome.Path
			if !s.deliveryLibraryPlacementMatches(ctx, r, f, libraryPath, outcome.FileGeneration) {
				disposition, reason = "unknown_recoverable", "placement could not be verified in the associated media copy"
				libraryPath = ""
			}
		} else if outcome.ReplacementDisposition == "excess" {
			if s.replacementOperationCommitted(ctx, outcome.ReplacementOperationID) {
				disposition, reason = "disposable_selected_scope", "replacement operation committed its selected placements; this out-of-scope payload is disposable"
			} else {
				disposition, reason = "unknown_recoverable", "replacement operation has not committed selected placements; excess payload is retained"
			}
		} else if targetPath, _, ok := s.deliveryRedundantLibraryProof(ctx, r, f, ""); ok {
			disposition, reason, libraryPath = "redundant", "same-copy usable file at "+targetPath+" already satisfies the quality profile", targetPath
		}
		if err := s.db.UpdateDeliveryFileDisposition(ctx, r.ID, f.relative, disposition, reason, libraryPath); err != nil {
			errs = append(errs, err)
			continue
		}
		if disposition == "placed" || disposition == "redundant" || disposition == "disposable_selected_scope" {
			if err := s.db.SetDeliveryFileCleanup(ctx, r.ID, f.relative, "pending", ""); err != nil {
				errs = append(errs, err)
			}
		}
		sourceGeneration := outcome.SourceGeneration
		if sourceGeneration == "" {
			sourceGeneration = r.GenerationKey
		}
		sourcePayloadGeneration := outcome.SourcePayloadGeneration
		if sourcePayloadGeneration == "" {
			sourcePayloadGeneration = r.Delivery.PublicationGeneration
		}
		libraryIdentity := ""
		if libraryPath != "" {
			if info, statErr := os.Lstat(libraryPath); statErr == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
				libraryIdentity = deliveryFileGeneration(info)
			}
		}
		if err := s.db.RecordDeliveryFileImport(ctx, sqlite.DeliveryFileImport{
			ReceiptID: r.ID, Path: f.relative, SourceGeneration: sourceGeneration,
			SourceTransfer: r.Delivery.RequestToken, SourcePayloadGeneration: sourcePayloadGeneration,
			ReplacementOperationID: outcome.ReplacementOperationID,
			ReplacementDisposition: outcome.ReplacementDisposition, PlacementID: outcome.PlacementID,
			LibraryFileGeneration: outcome.FileGeneration, LibraryFileIdentity: libraryIdentity, LibraryPath: outcome.Path,
		}); err != nil {
			errs = append(errs, err)
		}
	}
	ledgerRows, ledgerErr := s.db.DeliveryFileOutcomes(ctx, r.ID)
	if ledgerErr != nil {
		errs = append(errs, ledgerErr)
	}
	ledgerByPath := make(map[string]sqlite.DeliveryFileOutcome, len(ledgerRows))
	for _, row := range ledgerRows {
		ledgerByPath[row.Path] = row
	}
	for _, f := range verified {
		if seen[f.relative] {
			continue
		}
		row := ledgerByPath[f.relative]
		disposition, reason := "unknown_recoverable", "manifest file was not attempted by the importer"
		excessOperationID := replacementExcessSidecarOperation(f, verified, result)
		if knownDeliverySidecar(f.relative) {
			disposition, reason = "known_incidental", "recognized non-media sidecar retained outside media selection"
			if excessOperationID != "" {
				reason = "sidecar is associated only with an out-of-scope media file; retained until its selected replacement commits"
			}
		} else if row.Disposition == "unselected_recoverable" {
			continue
		}
		if row.Disposition != "pending" && row.Disposition != "unselected_recoverable" &&
			(excessOperationID == "" || (row.Disposition != "known_incidental" && row.Disposition != "unknown_recoverable")) {
			continue
		}
		if row.Disposition == "pending" || row.Disposition == "unselected_recoverable" ||
			(excessOperationID != "" && row.Disposition == "unknown_recoverable") {
			if err := s.db.UpdateDeliveryFileDisposition(ctx, r.ID, f.relative, disposition, reason, ""); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if excessOperationID != "" {
			if err := s.db.RecordDeliveryFileImport(ctx, sqlite.DeliveryFileImport{
				ReceiptID: r.ID, Path: f.relative, SourceGeneration: r.GenerationKey,
				SourceTransfer: r.Delivery.RequestToken, SourcePayloadGeneration: r.Delivery.PublicationGeneration,
				ReplacementOperationID: excessOperationID, ReplacementDisposition: "excess",
			}); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// replacementExcessSidecarOperation returns replacement authority only when a
// non-generic sidecar matches exactly one media member in the immutable
// publication, and that media member was positively classified as excess by
// this same import attempt. Unknown, mixed, and ambiguous associations remain
// ordinary reviewable sidecars.
func replacementExcessSidecarOperation(sidecar verifiedDeliveryFile, verified []verifiedDeliveryFile, result ImportResult) string {
	if !knownDeliverySidecar(sidecar.relative) || deliveryGenericArtName(filepath.Base(sidecar.relative)) {
		return ""
	}
	var matchedMedia verifiedDeliveryFile
	for _, candidate := range verified {
		if (!filename.IsVideo(candidate.relative) && !filename.IsBook(candidate.relative)) ||
			filepath.Dir(candidate.relative) != filepath.Dir(sidecar.relative) {
			continue
		}
		mediaStem := strings.TrimSuffix(filepath.Base(candidate.relative), filepath.Ext(candidate.relative))
		sidecarStem := strings.TrimSuffix(filepath.Base(sidecar.relative), filepath.Ext(sidecar.relative))
		matched, _ := deliverySidecarStemMatch(mediaStem, sidecarStem, sidecar.relative)
		if !matched {
			continue
		}
		if matchedMedia.relative != "" {
			return ""
		}
		matchedMedia = candidate
	}
	if matchedMedia.relative == "" {
		return ""
	}
	var operationID string
	for _, outcome := range result.Files {
		if filepath.Clean(outcome.SourcePath) != filepath.Clean(matchedMedia.absolute) ||
			outcome.ReplacementDisposition != "excess" || outcome.ReplacementOperationID == "" {
			continue
		}
		if operationID != "" && operationID != outcome.ReplacementOperationID {
			return ""
		}
		operationID = outcome.ReplacementOperationID
	}
	return operationID
}

func (s *Service) replacementOperationCommitted(ctx context.Context, operationID string) bool {
	if operationID == "" {
		return false
	}
	op, err := s.db.GetReplacementOperation(ctx, operationID)
	if err != nil || op.State != "complete" {
		return false
	}
	covered, err := s.replacementHasCurrentCommittedPlacement(ctx, op)
	return err == nil && covered
}

func knownDeliverySidecar(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".srt", ".ass", ".ssa", ".sub", ".idx", ".nfo", ".jpg", ".jpeg", ".png", ".webp", ".txt", ".xml":
		return true
	default:
		return false
	}
}

// importDeliverySidecars associates caption files with a media file from the
// same publication directory and places them beside the committed media path.
// It uses a bounded streaming copy and no-overwrite link publication so a
// retry cannot replace an unrelated library sidecar.
func (s *Service) importDeliverySidecars(ctx context.Context, r sqlite.DeliveryReceipt, files []verifiedDeliveryFile, result ImportResult) (ImportResult, error) {
	placed := make([]FileOutcome, 0)
	replacementOperationID := ""
	if r.DownloadID != 0 {
		if download, err := s.db.GetDownload(ctx, r.DownloadID); err == nil {
			replacementOperationID = download.ReplacementOperationID
		}
	}
	mediaTargets := make(map[string]string)
	for _, outcome := range result.Files {
		if outcome.Imported && outcome.Path != "" && (filename.IsVideo(outcome.SourcePath) || filename.IsBook(outcome.SourcePath)) {
			if relative := verifiedSourceRelative(files, outcome.SourcePath); relative != "" {
				mediaTargets[relative] = outcome.Path
			}
		}
	}
	// A media file placed on an earlier partial attempt remains an association
	// candidate even if its publication bytes have since been cleaned. The
	// durable ledger is the authority for that mapping.
	if rows, err := s.db.DeliveryFileOutcomes(ctx, r.ID); err != nil {
		return result, err
	} else {
		for _, row := range rows {
			if row.Disposition == "placed" && row.LibraryPath != "" &&
				(filename.IsVideo(row.Path) || filename.IsBook(row.Path)) {
				mediaTargets[row.Path] = row.LibraryPath
			}
		}
	}
	for _, sidecar := range files {
		if !knownDeliverySidecar(sidecar.relative) {
			continue
		}
		var mediaPath, suffix string
		matches := 0
		genericTargets := map[string]bool{}
		for media, targetPath := range mediaTargets {
			if media == "" || filepath.Dir(media) != filepath.Dir(sidecar.relative) {
				continue
			}
			mediaStem := strings.TrimSuffix(filepath.Base(media), filepath.Ext(media))
			sidecarStem := strings.TrimSuffix(filepath.Base(sidecar.relative), filepath.Ext(sidecar.relative))
			matched, candidateSuffix := deliverySidecarStemMatch(mediaStem, sidecarStem, sidecar.relative)
			if !matched {
				continue
			}
			if deliveryGenericArtName(filepath.Base(sidecar.relative)) {
				genericTargets[filepath.Dir(targetPath)] = true
				mediaPath = targetPath
				suffix = candidateSuffix
				continue
			}
			matches++
			mediaPath, suffix = targetPath, candidateSuffix
		}
		if deliveryGenericArtName(filepath.Base(sidecar.relative)) && len(genericTargets) == 1 {
			matches = 1
		}
		if matches == 0 {
			continue
		}
		if matches > 1 {
			return result, fmt.Errorf("caption %q matches multiple imported media files", sidecar.relative)
		}
		ext := filepath.Ext(sidecar.relative)
		name := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath)) + suffix + ext
		if deliveryGenericArtName(filepath.Base(sidecar.relative)) {
			name = filepath.Base(sidecar.relative)
		}
		target := filepath.Join(filepath.Dir(mediaPath), name)
		if err := s.copyAcceptedSidecar(ctx, r, sidecar, mediaPath, target); err != nil {
			return result, fmt.Errorf("place caption %q: %w", sidecar.relative, err)
		}
		generation := ""
		if info, err := os.Lstat(target); err == nil {
			generation = deliveryFileGeneration(info)
		}
		placed = append(placed, FileOutcome{Name: filepath.Base(sidecar.relative), SourcePath: sidecar.absolute,
			Imported: true, Path: target, SourceGeneration: r.GenerationKey,
			SourceFileGeneration: sidecar.generation, ReplacementOperationID: replacementOperationID,
			FileGeneration: generation})
	}
	result.Files = append(result.Files, placed...)
	result.Imported += len(placed)
	return result, nil
}

func verifiedSourceRelative(files []verifiedDeliveryFile, absolute string) string {
	for _, f := range files {
		if filepath.Clean(f.absolute) == filepath.Clean(absolute) {
			return f.relative
		}
	}
	return ""
}

func deliverySidecarStemMatch(mediaStem, sidecarStem, relative string) (bool, string) {
	if strings.EqualFold(sidecarStem, mediaStem) {
		return true, ""
	}
	if strings.HasPrefix(strings.ToLower(sidecarStem), strings.ToLower(mediaStem)+".") {
		return true, sidecarStem[len(mediaStem):]
	}
	if deliveryGenericArtName(filepath.Base(relative)) && deliveryImageSidecar(relative) {
		return true, ""
	}
	return false, ""
}

func deliveryImageSidecar(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return true
	}
	return false
}

func deliveryGenericArtName(name string) bool {
	switch strings.ToLower(strings.TrimSuffix(name, filepath.Ext(name))) {
	case "cover", "folder", "poster", "background", "fanart":
		return true
	}
	return false
}

func (s *Service) copyAcceptedSidecar(ctx context.Context, r sqlite.DeliveryReceipt, source verifiedDeliveryFile, mediaPath, target string) error {
	if err := rejectSymlinks(target, true); err != nil {
		return err
	}
	dirInfo, err := os.Lstat(filepath.Dir(target))
	if err != nil {
		return err
	}
	if !dirInfo.IsDir() {
		return fmt.Errorf("caption target parent is not a directory")
	}
	mediaInfo, err := os.Lstat(mediaPath)
	if err != nil || !mediaInfo.Mode().IsRegular() || mediaInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("associated library media is unavailable")
	}
	if digest, size, err := fileDigest(ctx, target); err == nil {
		if digest == source.digest && size == source.size {
			return nil
		}
		return fmt.Errorf("library caption already exists with different content")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	root, err := os.OpenRoot(source.root)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	payload, err := openDeliveryDirectoryNoSymlinks(root, source.relativeRoot)
	if err != nil {
		return err
	}
	defer func() { _ = payload.Close() }()
	parent, input, named, err := openDeliveryFileNoSymlinks(payload, filepath.FromSlash(source.relative))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	defer func() { _ = input.Close() }()
	before, err := input.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(named, before) || deliveryFileGeneration(before) != source.generation || before.Size() != source.size {
		return fmt.Errorf("caption source identity changed")
	}
	actual, size, err := fileDigest(ctx, source.absolute)
	if err != nil || size != source.size || actual != source.digest {
		if err != nil {
			return err
		}
		return fmt.Errorf("caption source does not match accepted manifest")
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	stageName := filepath.Join(filepath.Dir(target), ".monarr-caption-"+shortDeliveryStageID(target)+".stage")
	stage, err := os.OpenFile(stageName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		// Deterministic staging lets a retry recover a process crash between
		// writing and publishing the sidecar, without leaving random temp files.
		if digest, size, verifyErr := fileDigest(ctx, stageName); verifyErr == nil && size == source.size && digest == source.digest {
			return s.publishAcceptedSidecar(ctx, r, source, mediaPath, target, stageName, dirInfo, mediaInfo)
		}
		if removeErr := os.Remove(stageName); removeErr != nil {
			return errors.Join(err, removeErr)
		}
		stage, err = os.OpenFile(stageName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := copyHashContext(ctx, io.MultiWriter(stage, h), input)
	if copyErr != nil {
		_ = stage.Close()
		return copyErr
	}
	if n != source.size || hex.EncodeToString(h.Sum(nil)) != source.digest {
		_ = stage.Close()
		return fmt.Errorf("caption source changed during copy")
	}
	if err := stage.Sync(); err != nil {
		_ = stage.Close()
		return err
	}
	if err := stage.Close(); err != nil {
		return err
	}
	return s.publishAcceptedSidecar(ctx, r, source, mediaPath, target, stageName, dirInfo, mediaInfo)
}

func shortDeliveryStageID(value string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(value)))
	return hex.EncodeToString(sum[:12])
}

func (s *Service) publishAcceptedSidecar(ctx context.Context, r sqlite.DeliveryReceipt, source verifiedDeliveryFile, mediaPath, target, stageName string, expectedDir, expectedMedia os.FileInfo) error {
	s.importTargetMu.Lock()
	defer s.importTargetMu.Unlock()
	if err := rejectSymlinks(target, true); err != nil {
		return err
	}
	dirInfo, err := os.Lstat(filepath.Dir(target))
	if err != nil || !dirInfo.IsDir() || !os.SameFile(expectedDir, dirInfo) {
		return fmt.Errorf("caption target directory changed before publish")
	}
	mediaInfo, err := os.Lstat(mediaPath)
	if err != nil || !os.SameFile(expectedMedia, mediaInfo) || deliveryFileGeneration(expectedMedia) == "" || deliveryFileGeneration(expectedMedia) != deliveryFileGeneration(mediaInfo) {
		return fmt.Errorf("associated library media changed before publish")
	}
	owned := false
	rows, err := s.db.ListFilesForItem(ctx, r.MediaItemID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.MediaItemID == r.MediaItemID && row.CopyID == r.CopyID && filepath.Clean(row.Path) == filepath.Clean(mediaPath) {
			owned = true
			break
		}
	}
	if !owned {
		return fmt.Errorf("associated library media is no longer owned by this copy")
	}
	if digest, size, err := fileDigest(ctx, target); err == nil {
		if digest == source.digest && size == source.size {
			if err := os.Remove(stageName); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return syncDeliveryDirectory(filepath.Dir(target))
		}
		return fmt.Errorf("library caption already exists with different content")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Link(stageName, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			digest, size, verifyErr := fileDigest(ctx, target)
			if verifyErr == nil && size == source.size && digest == source.digest {
				if removeErr := os.Remove(stageName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return removeErr
				}
				return syncDeliveryDirectory(filepath.Dir(target))
			}
		}
		return err
	}
	if err := os.Remove(stageName); err != nil {
		return err
	}
	return syncDeliveryDirectory(filepath.Dir(target))
}

func syncDeliveryDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	return errors.Join(syncErr, closeErr)
}

// openDeliveryDirectoryNoSymlinks walks each directory entry with Lstat
// before opening it. os.Root confines traversal but intentionally follows
// in-root symlinks, so cleanup and accepted-manifest reads reject those too.
func openDeliveryDirectoryNoSymlinks(base *os.Root, rel string) (*os.Root, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("directory escapes verified root")
	}
	current, err := base.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if clean == "." {
		return current, nil
	}
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			_ = current.Close()
			return nil, fmt.Errorf("unsafe directory component")
		}
		info, err := current.Lstat(part)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			_ = current.Close()
			return nil, fmt.Errorf("symlink or non-directory in accepted path")
		}
		next, err := current.OpenRoot(part)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		if err := current.Close(); err != nil {
			_ = next.Close()
			return nil, err
		}
		current = next
	}
	return current, nil
}

func openDeliveryFileNoSymlinks(base *os.Root, rel string) (*os.Root, *os.File, os.FileInfo, error) {
	clean := filepath.Clean(rel)
	parentRel, name := filepath.Dir(clean), filepath.Base(clean)
	parent, err := openDeliveryDirectoryNoSymlinks(base, parentRel)
	if err != nil {
		return nil, nil, nil, err
	}
	named, err := parent.Lstat(name)
	if err != nil {
		_ = parent.Close()
		return nil, nil, nil, err
	}
	if named.Mode()&os.ModeSymlink != 0 || !named.Mode().IsRegular() {
		_ = parent.Close()
		return nil, nil, nil, fmt.Errorf("accepted manifest path is not a regular non-symlink file")
	}
	file, err := parent.Open(name)
	if err != nil {
		_ = parent.Close()
		return nil, nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(named, opened) {
		_ = file.Close()
		_ = parent.Close()
		if err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, nil, fmt.Errorf("accepted manifest file changed while opening")
	}
	return parent, file, named, nil
}

func (s *Service) deliveryLibraryPlacementMatches(ctx context.Context, r sqlite.DeliveryReceipt, f verifiedDeliveryFile, libraryPath, expectedGeneration string) bool {
	_, _, ok := s.deliveryLibraryPlacementProof(ctx, r, f, libraryPath, expectedGeneration, "")
	return ok
}

func (s *Service) deliveryLibraryGeneration(ctx context.Context, receiptID int64, sourcePath, libraryPath string) (string, string, error) {
	imports, err := s.db.DeliveryFileImports(ctx, receiptID)
	if err != nil {
		return "", "", err
	}
	var generation, identity string
	for _, imported := range imports {
		if imported.Path == sourcePath && filepath.Clean(imported.LibraryPath) == filepath.Clean(libraryPath) && imported.LibraryFileGeneration != "" {
			generation = imported.LibraryFileGeneration
			identity = imported.LibraryFileIdentity
		}
	}
	if generation == "" {
		return "", "", fmt.Errorf("accepted library file generation is unavailable")
	}
	return generation, identity, nil
}

func (s *Service) deliveryLibraryPlacementProof(ctx context.Context, r sqlite.DeliveryReceipt, f verifiedDeliveryFile, libraryPath, expectedGeneration, expectedIdentity string) (string, string, bool) {
	if knownDeliverySidecar(libraryPath) {
		// Sidecars have no media_files generation. On the first import the
		// FileOutcome generation is the physical file observation; later
		// cleanup retries use the separately persisted LibraryFileIdentity.
		if expectedIdentity == "" {
			expectedIdentity = expectedGeneration
		}
		return s.deliverySidecarLibraryProof(ctx, r, f, libraryPath, expectedIdentity)
	}
	if libraryPath == "" || expectedGeneration == "" {
		return "", "", false
	}
	files, err := s.db.ListFilesForItem(ctx, r.MediaItemID)
	if err != nil {
		return "", "", false
	}
	for _, existing := range files {
		if filepath.Clean(existing.Path) != filepath.Clean(libraryPath) || existing.MediaItemID != r.MediaItemID || existing.CopyID != r.CopyID {
			continue
		}
		before, err := os.Lstat(existing.Path)
		if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
			return "", "", false
		}
		digest, size, err := fileDigest(ctx, existing.Path)
		after, statErr := os.Lstat(existing.Path)
		if err == nil && statErr == nil && os.SameFile(before, after) && deliveryFileGeneration(before) != "" && deliveryFileGeneration(before) == deliveryFileGeneration(after) && existing.FileGeneration == expectedGeneration && (expectedIdentity == "" || deliveryFileGeneration(after) == expectedIdentity) && size == f.size && digest == f.digest {
			return existing.Path, deliveryFileGeneration(after), true
		}
		return "", "", false
	}
	return "", "", false
}

func (s *Service) deliverySidecarLibraryProof(ctx context.Context, r sqlite.DeliveryReceipt, f verifiedDeliveryFile, libraryPath, expectedGeneration string) (string, string, bool) {
	if expectedGeneration == "" {
		return "", "", false
	}
	files, err := s.db.ListFilesForItem(ctx, r.MediaItemID)
	if err != nil {
		return "", "", false
	}
	sidecarDir := filepath.Clean(filepath.Dir(libraryPath))
	sidecarStem := strings.TrimSuffix(filepath.Base(libraryPath), filepath.Ext(libraryPath))
	for _, media := range files {
		if media.MediaItemID != r.MediaItemID || media.CopyID != r.CopyID || filepath.Clean(filepath.Dir(media.Path)) != sidecarDir || (!filename.IsVideo(media.Path) && !filename.IsBook(media.Path)) {
			continue
		}
		mediaStem := strings.TrimSuffix(filepath.Base(media.Path), filepath.Ext(media.Path))
		if matched, _ := deliverySidecarStemMatch(mediaStem, sidecarStem, libraryPath); !matched {
			continue
		}
		before, err := os.Lstat(libraryPath)
		if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
			return "", "", false
		}
		digest, size, err := fileDigest(ctx, libraryPath)
		after, statErr := os.Lstat(libraryPath)
		if err == nil && statErr == nil && os.SameFile(before, after) && deliveryFileGeneration(before) != "" && deliveryFileGeneration(before) == deliveryFileGeneration(after) && deliveryFileGeneration(after) == expectedGeneration && size == f.size && digest == f.digest {
			return libraryPath, deliveryFileGeneration(after), true
		}
		return "", "", false
	}
	return "", "", false
}

// Redundancy is proven from the exact associated copy, current bytes and
// episode coverage. Importer skip reasons and matching titles are not proof.
func (s *Service) deliveryRedundantLibraryProof(ctx context.Context, r sqlite.DeliveryReceipt, f verifiedDeliveryFile, requiredPath string) (string, string, bool) {
	// Manual additional copies are independently requested media and must not
	// be disposed as redundant, even if their bytes match a primary copy.
	ledger, ledgerErr := s.db.DeliveryFileOutcomeByPath(ctx, r.ID, f.relative)
	if ledgerErr != nil || ledger.ManualAdditional {
		return "", "", false
	}
	item, err := s.db.GetMediaItemFull(ctx, r.MediaItemID)
	if err != nil {
		return "", "", false
	}
	profileID := item.QualityProfileID
	if r.CopyID != 0 {
		copy, err := s.db.GetMediaCopy(ctx, r.MediaItemID, r.CopyID)
		if err != nil || copy.MediaItemID != r.MediaItemID {
			return "", "", false
		}
		profileID = copy.QualityProfileID
	}
	profile, err := s.db.GetProfile(ctx, profileID)
	if err != nil {
		return "", "", false
	}
	candidate := quality.Quality{Source: quality.Source(filename.BookQualitySource(f.relative))}
	if !quality.IsBookFormat(candidate.Source) {
		candidate = parser.Parse(filepath.Base(f.relative)).Quality
	}
	if candidate.Source == quality.SourceUnknown || (candidate.Resolution == 0 && !quality.IsBookFormat(candidate.Source)) {
		download, err := s.db.GetDownload(ctx, r.DownloadID)
		if err == nil && download.Quality.Source != quality.SourceUnknown {
			candidate = download.Quality
		}
	}
	if candidate.Source == quality.SourceUnknown || !profile.Acceptable(candidate) {
		return "", "", false
	}
	mediaFiles, err := s.db.ListFilesForItem(ctx, r.MediaItemID)
	if err != nil {
		return "", "", false
	}
	expectedEpisodes := []int64(nil)
	if item.Kind == domain.KindSeries {
		parsed := parser.Parse(filepath.Base(f.relative))
		if parsed.Season < 0 || len(parsed.Episodes) == 0 {
			return "", "", false
		}
		for _, season := range item.Seasons {
			if season.Number != parsed.Season {
				continue
			}
			for _, number := range parsed.Episodes {
				found := false
				for _, episode := range season.Episodes {
					if episode.EpisodeNumber == number {
						expectedEpisodes = append(expectedEpisodes, episode.ID)
						found = true
						break
					}
				}
				if !found {
					return "", "", false
				}
			}
		}
		if len(expectedEpisodes) != len(parsed.Episodes) {
			return "", "", false
		}
		slices.Sort(expectedEpisodes)
	}
	for _, existing := range mediaFiles {
		if existing.MediaItemID != r.MediaItemID || existing.CopyID != r.CopyID || !existing.QualityKnown || existing.FileGeneration == "" || !existing.Provenance.Verified(existing.Confidence) || existing.Provenance == mediainfo.ProvenanceFailed || existing.Provenance == mediainfo.ProvenanceImplausible {
			continue
		}
		if requiredPath != "" && filepath.Clean(existing.Path) != filepath.Clean(requiredPath) {
			continue
		}
		if item.Kind == domain.KindSeries {
			got := append([]int64(nil), existing.EpisodeIDs...)
			slices.Sort(got)
			if !slices.Equal(got, expectedEpisodes) {
				continue
			}
		}
		// A report is an independent review signal, not quality or planner
		// evidence. Any open flag (or inability to read the flag state) blocks
		// destructive redundancy cleanup for this exact file generation.
		flagged, flagErr := s.db.HasOpenMediaFileReviewFlag(ctx, existing.ID, existing.FileGeneration)
		if flagErr != nil || flagged {
			continue
		}
		if replacementFileMarkedBad(ctx, s.db, existing.ID, existing.FileGeneration) {
			continue
		}
		before, err := os.Lstat(existing.Path)
		if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
			continue
		}
		// Exact bytes are stronger redundancy evidence than a second probe:
		// the existing file's associated quality is already verified, its
		// episode coverage was checked above, and the accepted manifest bytes
		// are identical. This also lets exact duplicate detection work with a
		// manifest whose filename is incomplete while never trusting a title
		// alone.
		existingDigest, existingSize, digestErr := fileDigest(ctx, existing.Path)
		afterDigest, afterDigestErr := os.Lstat(existing.Path)
		if digestErr == nil && afterDigestErr == nil && os.SameFile(before, afterDigest) &&
			deliveryFileGeneration(before) != "" && deliveryFileGeneration(before) == deliveryFileGeneration(afterDigest) &&
			existing.QualityKnown && existing.Provenance.Verified(existing.Confidence) &&
			profile.Acceptable(existing.Quality) && existingSize == f.size && existingDigest == f.digest {
			return existing.Path, deliveryFileGeneration(afterDigest), true
		}
		if quality.IsBookFormat(candidate.Source) {
			// Native books are not probed. Only exact-content duplication with a
			// trusted existing format can establish redundancy for them.
			digest, size, digestErr := fileDigest(ctx, existing.Path)
			after, statErr := os.Lstat(existing.Path)
			if digestErr == nil && digest == f.digest && size == f.size && statErr == nil && os.SameFile(before, after) && deliveryFileGeneration(before) != "" && deliveryFileGeneration(before) == deliveryFileGeneration(after) && !profile.Upgrade(candidate, nil, existing.Quality, true, quality.Audio{}) {
				return existing.Path, deliveryFileGeneration(after), true
			}
			continue
		}
		if filename.IsVideo(existing.Path) {
			candidateQuality, candidateAudio, sourceGeneration, ok := freshDeliveryVideoQuality(f.absolute, candidate, item.Runtime)
			if !ok || sourceGeneration != f.generation || !profile.LanguageAcceptable(candidateAudio.Languages) || !profile.LanguageMet(candidateAudio) {
				continue
			}
			currentQuality, currentAudio, _, ok := freshDeliveryVideoQuality(existing.Path, existing.Quality, item.Runtime)
			if !ok {
				continue
			}
			if profile.Upgrade(candidateQuality, candidateAudio.Languages, currentQuality, true, currentAudio) {
				continue
			}
			after, statErr := os.Lstat(existing.Path)
			if statErr == nil && os.SameFile(before, after) && deliveryFileGeneration(before) != "" && deliveryFileGeneration(before) == deliveryFileGeneration(after) {
				return existing.Path, deliveryFileGeneration(after), true
			}
		}
	}
	return "", "", false
}

func replacementFileMarkedBad(ctx context.Context, db *sqlite.DB, fileID int64, generation string) bool {
	var count int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM replacement_operation_files f
		JOIN replacement_operations o ON o.operation_id=f.operation_id
		WHERE f.media_file_id=? AND f.file_generation=? AND f.disposition='selected'
		AND f.removal_state!='removed' AND o.intent='replace_bad'`, fileID, generation).Scan(&count); err != nil {
		return true
	}
	return count != 0
}

func freshDeliveryVideoQuality(path string, hint quality.Quality, runtimeMinutes int) (quality.Quality, quality.Audio, string, bool) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return quality.Quality{}, quality.Audio{}, "", false
	}
	generation := deliveryFileGeneration(before)
	if generation == "" {
		return quality.Quality{}, quality.Audio{}, "", false
	}
	info, err := probe.NativeFile(path)
	if err != nil || !info.Measured() || info.Truncated() {
		return quality.Quality{}, quality.Audio{}, "", false
	}
	if _, implausible := mediainfo.DurationImplausible(info, runtimeMinutes); implausible {
		return quality.Quality{}, quality.Audio{}, "", false
	}
	measured, provenance, confidence := mediainfo.Resolve(info, hint, mediainfo.ProvenanceFilename)
	if measured.Source == quality.SourceUnknown || !provenance.Verified(confidence) || provenance == mediainfo.ProvenanceImplausible || provenance == mediainfo.ProvenanceFailed {
		return quality.Quality{}, quality.Audio{}, "", false
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, after) || generation != deliveryFileGeneration(after) {
		return quality.Quality{}, quality.Audio{}, "", false
	}
	languages, known := info.AudioLanguages()
	return measured, quality.Audio{Languages: languages, Known: known}, generation, true
}

func (s *Service) scheduleDeliveryFileCleanup(ctx context.Context, cfg ports.ClientConfig) {
	ids, err := s.db.DeliveryFileCleanupReceipts(ctx, cfg.ID, 16)
	if err != nil {
		s.log.Warn("delivery file cleanup list failed", "client", cfg.Name, "err", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		r, err := s.db.DeliveryReceiptByID(ctx, id)
		if err != nil {
			s.log.Warn("delivery cleanup receipt read failed", "receipt", id, "err", err)
			continue
		}
		s.queueDeliveryCleanup(ctx, cfg, r)
	}
}

func (s *Service) cleanupDeliveryFiles(ctx context.Context, r sqlite.DeliveryReceipt) error {
	// Per-file outcomes can be committed before the download/receipt terminal
	// stamp. Keep the accepted publication directory available until that stamp
	// is durable so a crash in this interval can replay from the manifest.
	if r.State != "imported" {
		return nil
	}
	legacy, err := s.db.LegacyCleanupReview(ctx, r.GenerationKey, r.MappedPath)
	if err != nil {
		return err
	}
	if legacy {
		return fmt.Errorf("completed-payload cleanup is blocked for the legacy review cohort")
	}
	rows, err := s.db.DeliveryFileOutcomes(ctx, r.ID)
	if err != nil {
		return err
	}
	imports, err := s.db.DeliveryFileImports(ctx, r.ID)
	if err != nil {
		return err
	}
	for _, imported := range imports {
		if imported.ReplacementDisposition != "excess" || imported.ReplacementOperationID == "" {
			continue
		}
		op, opErr := s.db.GetReplacementOperation(ctx, imported.ReplacementOperationID)
		if opErr != nil || op.State != "complete" {
			continue
		}
		operationCommitted := s.replacementOperationCommitted(ctx, imported.ReplacementOperationID)
		for i := range rows {
			row := &rows[i]
			if row.Path != imported.Path || row.CleanupState == "removed" || row.Disposition == "disposable_selected_scope" {
				continue
			}
			if row.CleanupNextAttemptAt.After(time.Now()) {
				continue
			}
			if !operationCommitted {
				if err := s.db.ScheduleProtectedDeliveryFileRetry(ctx, r.ID, row.Path,
					"replacement operation completed but current selected placement proof is unavailable; source remains protected"); err != nil {
					return err
				}
				row.CleanupState = "protected"
				continue
			}
			if err := s.db.UpdateDeliveryFileDisposition(ctx, r.ID, row.Path, "disposable_selected_scope",
				"replacement operation committed its selected placements; this out-of-scope payload is disposable", ""); err != nil {
				return err
			}
			if err := s.db.SetDeliveryFileCleanup(ctx, r.ID, row.Path, "pending", ""); err != nil {
				return err
			}
			row.Disposition, row.CleanupState = "disposable_selected_scope", "pending"
		}
	}
	for _, row := range rows {
		if row.CleanupState == "removed" || row.CleanupState == "protected" {
			continue
		}
		if row.CleanupNextAttemptAt.After(time.Now()) {
			continue
		}
		if row.Disposition != "placed" && row.Disposition != "redundant" && row.Disposition != "disposable_selected_scope" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		event, eventErr := s.db.BeginDeliveryAttempt(ctx, r.ID, "cleanup_file:"+row.Path)
		if eventErr != nil {
			return fmt.Errorf("record cleanup attempt for %s: %w", row.Path, eventErr)
		}
		if err := s.removeVerifiedDeliveryFile(ctx, r, row); err != nil {
			if finishErr := s.db.FinishDeliveryAttempt(ctx, event.ID, "failed", err.Error()); finishErr != nil {
				s.log.Error("delivery file cleanup attempt history could not be finished", "receipt", r.ID, "path", row.Path, "err", finishErr)
			}
			_ = s.db.SetDeliveryFileCleanup(ctx, r.ID, row.Path, "retry", err.Error())
			continue
		}
		if err := s.db.FinishDeliveryAttempt(ctx, event.ID, "succeeded", ""); err != nil {
			return err
		}
		if err := s.db.SetDeliveryFileCleanup(ctx, r.ID, row.Path, "removed", ""); err != nil {
			return err
		}
	}
	// Record a due directory-cleanup obligation before touching the publication
	// tree. If the process or database fails after removing the directory but
	// before recording success, the scheduler can safely replay against the
	// same receipt and accepted directory identity.
	if err := s.db.SetDeliveryDirectoryCleanup(ctx, r.ID, "pending", ""); err != nil {
		return err
	}
	directoryEvent, err := s.db.BeginDeliveryAttempt(ctx, r.ID, "cleanup_directory")
	if err != nil {
		return fmt.Errorf("record payload-directory cleanup attempt: %w", err)
	}
	directoryRemoved, err := s.removeEmptyDeliveryDirectories(ctx, r)
	if err != nil {
		_ = s.db.FinishDeliveryAttempt(ctx, directoryEvent.ID, "failed", err.Error())
		_ = s.db.SetDeliveryDirectoryCleanup(ctx, r.ID, "retry", err.Error())
		return err
	}
	if directoryRemoved {
		if err := s.db.FinishDeliveryAttempt(ctx, directoryEvent.ID, "succeeded", ""); err != nil {
			return err
		}
		if err := s.db.SetDeliveryDirectoryCleanup(ctx, r.ID, "removed", ""); err != nil {
			// The filesystem deletion is already complete. Preserve a due retry
			// obligation so a transient database failure cannot strand an empty
			// receipt forever outside DeliveryFileCleanupReceipts.
			retryErr := s.db.SetDeliveryDirectoryCleanup(ctx, r.ID, "retry", err.Error())
			return errors.Join(err, retryErr)
		}
		return nil
	}
	const detail = "publication still contains recoverable or unrelated files"
	if err := s.db.FinishDeliveryAttempt(ctx, directoryEvent.ID, "blocked", detail); err != nil {
		return err
	}
	return s.db.SetDeliveryDirectoryCleanup(ctx, r.ID, "protected", detail)
}

// DeleteSelectedDeliveryFiles is the explicit, reviewable escape hatch for a
// delivery payload. Each requested path must belong to the immutable manifest;
// the current scanned file identity authorizes this one discard, even if its
// bytes no longer match the published digest. Siblings remain untouched.
func (s *Service) DeleteSelectedDeliveryFiles(ctx context.Context, payloadPath, fingerprint string, selections []CompletedFileSelection) error {
	if len(selections) == 0 {
		return fmt.Errorf("select one or more manifest files to delete")
	}
	s.completedMu.Lock()
	err := s.deleteSelectedDeliveryFilesLocked(ctx, payloadPath, fingerprint, selections)
	s.completedMu.Unlock()
	if err != nil {
		return err
	}
	return s.ScanCompleted(ctx)
}

type selectedDeliveryDeletion struct {
	relative          string
	selection         CompletedFileSelection
	ledger            sqlite.DeliveryFileOutcome
	info              os.FileInfo
	currentSize       int64
	currentDigest     string
	currentGeneration string
}

func (s *Service) deleteSelectedDeliveryFilesLocked(ctx context.Context, payloadPath, fingerprint string, selections []CompletedFileSelection) error {
	if !filepath.IsAbs(payloadPath) || fingerprint == "" {
		return fmt.Errorf("select an unchanged delivery payload from the completed-storage inventory")
	}
	if !s.ownsDownloadPath(ctx, payloadPath) {
		return fmt.Errorf("select an entry inside Curator-owned download storage")
	}
	inventory, err := s.CompletedInventory(ctx)
	if err != nil {
		return err
	}
	var selectedEntry *CompletedEntry
	var storageRoot string
	for i := range inventory.Roots {
		root := &inventory.Roots[i]
		for j := range root.Entries {
			entry := &root.Entries[j]
			if entry.Path == payloadPath && entry.Fingerprint == fingerprint {
				selectedEntry, storageRoot = entry, root.Path
			}
		}
	}
	if selectedEntry == nil || selectedEntry.LegacyReview || !selectedEntry.DeliveryManaged {
		return fmt.Errorf("selected payload is not an unchanged delivery-managed entry")
	}
	for _, root := range inventory.Roots {
		if root.Path == storageRoot && root.Error != "" {
			return fmt.Errorf("inventory is incomplete; scan again before deleting selected files")
		}
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return err
	}
	receipts := make([]sqlite.DeliveryReceipt, 0, len(selectedEntry.Receipts))
	canonicalPayload := storagePath(payloadPath, roots)
	for _, association := range selectedEntry.Receipts {
		if association.DownloadID <= 0 || !association.DeliveryManaged {
			continue
		}
		beforeID := int64(0)
		for {
			page, next, pageErr := s.db.DeliveryReceiptPageForDownload(ctx, association.DownloadID, beforeID, 100)
			if pageErr != nil {
				return pageErr
			}
			for _, receipt := range page {
				if receipt.MediaItemID == association.MediaItemID && receipt.CopyID == association.CopyID &&
					storagePath(receipt.MappedPath, roots) == canonicalPayload {
					receipts = append(receipts, receipt)
				}
			}
			if next == 0 {
				break
			}
			beforeID = next
		}
	}
	if len(receipts) != 1 || (receipts[0].AckState != "accepted" && (receipts[0].AckState != "pending" || receipts[0].ConsumerReceiptID == "")) || receipts[0].State == "conflict" || receipts[0].State == "importing" {
		return fmt.Errorf("delivery has no locally prepared acceptance, is importing, or is ambiguous; selected-file deletion requires one stable manifest")
	}
	r := receipts[0]
	if r.DownloadID > 0 {
		// Import workers serialize on this per-download lock from queue pickup
		// through placement. Holding it across verification and unlink closes
		// the race where an import starts after the initial state check.
		unlockDownload := s.lockDownload(r.DownloadID)
		defer unlockDownload()
		current, err := s.db.DeliveryReceiptByID(ctx, r.ID)
		if err != nil || (current.AckState != "accepted" && (current.AckState != "pending" || current.ConsumerReceiptID == "")) || current.State == "conflict" || current.State == "importing" {
			return fmt.Errorf("delivery became active or ambiguous; selected-file deletion requires a locally prepared manifest")
		}
		r = current
		dl, err := s.db.GetDownload(ctx, r.DownloadID)
		if err != nil {
			return fmt.Errorf("delivery download state is unavailable; refusing deletion")
		}
		if dl.State == "importing" {
			return fmt.Errorf("delivery import is active; wait for it to finish before deleting selected files")
		}
	}
	root, payloadRelative, err := deliveryRootAndRelative(payloadPath, roots)
	if err != nil {
		return err
	}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = anchor.Close() }()
	payload, err := openDeliveryDirectoryNoSymlinks(anchor, payloadRelative)
	if err != nil {
		return err
	}
	defer func() { _ = payload.Close() }()
	payloadInfo, err := payload.Stat(".")
	if err != nil || !payloadInfo.IsDir() {
		return fmt.Errorf("delivery publication directory does not match its accepted generation")
	}
	identity := deliveryDirectoryObjectIdentity(payloadInfo)
	if identity == "" {
		return fmt.Errorf("publication directory identity is unavailable on this platform")
	}
	if r.PublicationDirectoryIdentity == "" {
		// Explicit review has selected this exact payload and each file will be
		// checked against the accepted manifest before the identity is pinned.
		if err := s.db.EnsureDeliveryDirectoryIdentity(ctx, r.ID, identity); err != nil {
			return err
		}
		r.PublicationDirectoryIdentity = identity
	} else if identity != r.PublicationDirectoryIdentity {
		return fmt.Errorf("delivery publication directory does not match its accepted generation")
	}
	manifest := make(map[string]ports.RunnerFile, len(r.Delivery.Files))
	for _, file := range r.Delivery.Files {
		manifest[file.Path] = file
	}
	chosen := map[string]bool{}
	verified := make([]selectedDeliveryDeletion, 0, len(selections))
	for _, selection := range selections {
		if !filepath.IsAbs(selection.Path) || selection.SnapshotIdentity == "" || !within(payloadPath, selection.Path) {
			return fmt.Errorf("selected path is outside the delivery payload or has no current snapshot")
		}
		rel, err := filepath.Rel(payloadPath, selection.Path)
		if err != nil || rel == "." || !filepath.IsLocal(rel) || !safeDeliveryRelativePath(filepath.ToSlash(rel)) {
			return fmt.Errorf("selected path is not a safe relative manifest file")
		}
		rel = filepath.ToSlash(rel)
		if chosen[rel] {
			return fmt.Errorf("selected manifest file appears more than once: %s", rel)
		}
		chosen[rel] = true
		entry, exists := manifest[rel]
		if !exists {
			return fmt.Errorf("selected file is not in the accepted delivery manifest: %s", rel)
		}
		ledger, ledgerErr := s.db.DeliveryFileOutcomeByPath(ctx, r.ID, rel)
		if ledgerErr != nil && !errors.Is(ledgerErr, sqlite.ErrNotFound) {
			return ledgerErr
		}
		if ledgerErr == nil && ledger.FileGeneration == "" {
			return fmt.Errorf("selected file has no durable manifest provenance record: %s", rel)
		}
		parent, file, named, err := openDeliveryFileNoSymlinks(payload, filepath.FromSlash(rel))
		if err != nil {
			return fmt.Errorf("open selected file %s: %w", rel, err)
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			_ = parent.Close()
			return statErr
		}
		if !info.Mode().IsRegular() || !os.SameFile(named, info) || completedReviewFileIdentity(selection.Path, info) != selection.SnapshotIdentity {
			_ = file.Close()
			_ = parent.Close()
			return fmt.Errorf("selected file changed since the inventory scan: %s", rel)
		}
		digest, size, digestErr := digestOpenFile(ctx, file)
		after, afterErr := file.Stat()
		closeErr := file.Close()
		parentCloseErr := parent.Close()
		if digestErr != nil || afterErr != nil || closeErr != nil || parentCloseErr != nil {
			return errors.Join(digestErr, afterErr, closeErr, parentCloseErr)
		}
		generation := deliveryFileGeneration(after)
		if !os.SameFile(info, after) || generation == "" || completedReviewFileIdentity(selection.Path, after) != selection.SnapshotIdentity {
			return fmt.Errorf("selected file changed after review: %s", rel)
		}
		if ledgerErr != nil {
			ledger = sqlite.DeliveryFileOutcome{ReceiptID: r.ID, Path: rel, SizeBytes: entry.SizeBytes, SHA256: entry.SHA256, FileGeneration: generation}
			if err := s.db.EnsureExplicitDeliveryFileOutcome(ctx, r.ID, sqlite.DeliveryManifestFile{Path: rel, SizeBytes: entry.SizeBytes, SHA256: entry.SHA256, FileGeneration: generation}); err != nil {
				return err
			}
		}
		verified = append(verified, selectedDeliveryDeletion{relative: rel, selection: selection, ledger: ledger, info: info, currentSize: size, currentDigest: digest, currentGeneration: generation})
	}
	s.importTargetMu.Lock()
	defer s.importTargetMu.Unlock()
	decisionCtx, release := s.lockStorageDecision(ctx)
	defer release()
	// Import admission can race the hashing pass above. Recheck after taking
	// the same short lock used by placement so explicit deletion never removes
	// files from an active importer.
	currentReceipt, err := s.db.DeliveryReceiptByID(decisionCtx, r.ID)
	if err != nil || (currentReceipt.AckState != "accepted" && (currentReceipt.AckState != "pending" || currentReceipt.ConsumerReceiptID == "")) || currentReceipt.State == "conflict" || currentReceipt.State == "importing" {
		return fmt.Errorf("delivery became active or ambiguous before deletion; try again when it is idle")
	}
	if currentReceipt.DownloadID > 0 {
		dl, getErr := s.db.GetDownload(decisionCtx, currentReceipt.DownloadID)
		if getErr != nil || dl.State == "importing" {
			return fmt.Errorf("delivery import is active or unavailable; wait for it to finish before deleting selected files")
		}
	}
	if err := s.checkStorageIdentity(decisionCtx, payloadPath); err != nil {
		return err
	}
	for _, file := range verified {
		currentPayload, err := anchor.Lstat(payloadRelative)
		if err != nil || !currentPayload.IsDir() || currentPayload.Mode()&os.ModeSymlink != 0 ||
			!os.SameFile(payloadInfo, currentPayload) || deliveryDirectoryObjectIdentity(currentPayload) != r.PublicationDirectoryIdentity {
			return fmt.Errorf("delivery publication directory changed before deletion")
		}
		parentRelative := filepath.Dir(filepath.FromSlash(file.relative))
		parent, err := openDeliveryDirectoryNoSymlinks(payload, parentRelative)
		if err != nil {
			return err
		}
		name := filepath.Base(filepath.FromSlash(file.relative))
		current, err := parent.Lstat(name)
		if err != nil {
			_ = parent.Close()
			return err
		}
		if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(file.info, current) || completedReviewFileIdentity(file.selection.Path, current) != file.selection.SnapshotIdentity {
			_ = parent.Close()
			return fmt.Errorf("selected file changed before deletion: %s", file.relative)
		}
		if err := s.db.PrepareExplicitDeliveryFileDelete(decisionCtx, r.ID, file.relative); err != nil {
			_ = parent.Close()
			return err
		}
		if err := parent.Remove(name); err != nil {
			_ = parent.Close()
			return err
		}
		if err := parent.Close(); err != nil {
			return err
		}
		if err := syncPath(filepath.Dir(file.selection.Path)); err != nil {
			return err
		}
		if err := s.db.SetDeliveryFileExplicitlyRemoved(decisionCtx, r.ID, file.relative); err != nil {
			return err
		}
		if err := s.db.AddHistory(decisionCtx, "delivery_file_deleted", r.DownloadID, filepath.Base(file.selection.Path), map[string]any{
			"receiptId": r.ID, "deliveryId": r.Delivery.DeliveryID, "publicationGeneration": r.Delivery.PublicationGeneration,
			"manifestPath": file.relative, "manifestSHA256": file.ledger.SHA256, "manifestSizeBytes": file.ledger.SizeBytes,
			"reviewedSHA256": file.currentDigest, "reviewedSizeBytes": file.currentSize, "reviewedFileGeneration": file.currentGeneration,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) removeVerifiedDeliveryFile(ctx context.Context, r sqlite.DeliveryReceipt, row sqlite.DeliveryFileOutcome) error {
	if !safeDeliveryRelativePath(row.Path) {
		return fmt.Errorf("unsafe ledger path")
	}
	if row.FileGeneration == "" {
		return fmt.Errorf("payload file generation is unavailable; refusing cleanup")
	}
	currentReceipt, err := s.db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil || currentReceipt.GenerationKey != r.GenerationKey || currentReceipt.Delivery.RequestToken != r.Delivery.RequestToken ||
		currentReceipt.Delivery.PublicationGeneration != r.Delivery.PublicationGeneration || filepath.Clean(currentReceipt.MappedPath) != filepath.Clean(r.MappedPath) ||
		currentReceipt.PublicationDirectoryIdentity != r.PublicationDirectoryIdentity {
		return fmt.Errorf("delivery receipt or accepted publication identity changed before cleanup")
	}
	absolute := filepath.Join(r.MappedPath, filepath.FromSlash(row.Path))
	info, err := os.Lstat(absolute)
	if err != nil {
		// Unlink may have succeeded immediately before a crash prevented the
		// cleanup ledger update. The source is already absent, so replay has no
		// destructive work to authorize; the caller can safely mark this row
		// settled without relying on a path-only second delete.
		if errors.Is(err, os.ErrNotExist) {
			if r.PublicationDirectoryIdentity == "" {
				return fmt.Errorf("accepted publication directory identity is unavailable")
			}
			if err := s.checkStorageIdentity(ctx, r.MappedPath); err != nil {
				return err
			}
			payloadInfo, statErr := os.Lstat(r.MappedPath)
			if statErr != nil || !payloadInfo.IsDir() || deliveryDirectoryObjectIdentity(payloadInfo) != r.PublicationDirectoryIdentity {
				return fmt.Errorf("accepted publication directory changed before cleanup replay")
			}
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != row.SizeBytes || deliveryFileGeneration(info) != row.FileGeneration {
		return fmt.Errorf("completed payload file identity changed before cleanup proof")
	}
	f := verifiedDeliveryFile{relative: row.Path, absolute: absolute, size: row.SizeBytes, digest: row.SHA256, generation: row.FileGeneration, info: info}
	var libraryEvidencePath, libraryEvidenceGeneration string
	var selectedReplacement sqlite.ReplacementOperation
	var selectedReplacementProof map[int64]replacementPlacementSnapshot
	switch row.Disposition {
	case "placed":
		var ok bool
		expectedLibraryGeneration, expectedLibraryIdentity, err := s.deliveryLibraryGeneration(ctx, r.ID, row.Path, row.LibraryPath)
		if err != nil {
			return err
		}
		libraryEvidencePath, libraryEvidenceGeneration, ok = s.deliveryLibraryPlacementProof(ctx, r, f, row.LibraryPath, expectedLibraryGeneration, expectedLibraryIdentity)
		if !ok {
			return fmt.Errorf("verified library placement is no longer present")
		}
	case "disposable_selected_scope":
		var ok bool
		selectedReplacement, ok = s.replacementDisposableDeliveryOperation(ctx, r, row)
		if !ok {
			return fmt.Errorf("selected-scope excess no longer has exact committed replacement authority")
		}
		if covered, coverErr := s.db.ReplacementHasCommittedCoverage(ctx, selectedReplacement); coverErr != nil || !covered {
			return fmt.Errorf("selected-scope replacement no longer covers its approved target")
		}
		selectedReplacementProof, err = s.inspectReplacementCoverage(ctx, selectedReplacement)
		if err != nil {
			return fmt.Errorf("selected-scope replacement placement is no longer usable: %w", err)
		}
	default:
		var ok bool
		libraryEvidencePath, libraryEvidenceGeneration, ok = s.deliveryRedundantLibraryProof(ctx, r, f, row.LibraryPath)
		if !ok {
			return fmt.Errorf("same-copy redundancy proof no longer holds")
		}
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return err
	}
	root, rel, err := deliveryRootAndRelative(r.MappedPath, roots)
	if err != nil {
		return err
	}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = anchor.Close() }()
	payload, err := openDeliveryDirectoryNoSymlinks(anchor, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = payload.Close() }()
	parent, file, named, err := openDeliveryFileNoSymlinks(payload, filepath.FromSlash(row.Path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return syncPath(filepath.Dir(filepath.Join(r.MappedPath, filepath.FromSlash(row.Path))))
		}
		return err
	}
	defer func() { _ = parent.Close() }()
	before, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return err
	}
	if !before.Mode().IsRegular() || before.Size() != row.SizeBytes || !os.SameFile(named, before) || deliveryFileGeneration(before) != row.FileGeneration {
		_ = file.Close()
		return fmt.Errorf("completed payload file type or size changed")
	}
	digest, n, hashErr := digestOpenFile(ctx, file)
	after, statErr := file.Stat()
	closeErr := file.Close()
	if hashErr != nil {
		return hashErr
	}
	if statErr != nil {
		return statErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !os.SameFile(before, after) || deliveryFileGeneration(after) != row.FileGeneration || n != row.SizeBytes || digest != row.SHA256 {
		return fmt.Errorf("completed payload file identity or digest changed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Keep the expensive hashing above outside global import/storage locks. The
	// short critical section rechecks the current generation at the anchored
	// name immediately before unlink so a path swap cannot redirect cleanup.
	s.importTargetMu.Lock()
	defer s.importTargetMu.Unlock()
	decisionCtx, release := s.lockStorageDecision(ctx)
	defer release()
	legacy, err := s.db.LegacyCleanupReview(decisionCtx, r.GenerationKey, r.MappedPath)
	if err != nil {
		return err
	}
	if legacy {
		return fmt.Errorf("completed-payload cleanup is blocked for the legacy review cohort")
	}
	if err := s.checkStorageIdentity(decisionCtx, r.MappedPath); err != nil {
		return err
	}
	if row.Disposition == "disposable_selected_scope" {
		operation, ok := s.replacementDisposableDeliveryOperation(decisionCtx, r, row)
		if !ok || operation.ID != selectedReplacement.ID {
			return fmt.Errorf("selected-scope replacement authority changed before cleanup")
		}
		covered, coverErr := s.db.ReplacementHasCommittedCoverage(decisionCtx, operation)
		if coverErr != nil || !covered {
			return fmt.Errorf("selected-scope replacement coverage changed before cleanup")
		}
		proofCtx := context.WithValue(decisionCtx, replacementPlacementProofKey{}, selectedReplacementProof)
		if err := s.finalReplacementPlacementCheck(proofCtx, operation); err != nil {
			return fmt.Errorf("selected-scope replacement placement changed before cleanup: %w", err)
		}
	} else if !s.deliveryLibraryEvidenceOwned(decisionCtx, r, libraryEvidencePath, libraryEvidenceGeneration) {
		return fmt.Errorf("verified library ownership or file identity changed")
	}
	parentRel, name := filepath.Dir(filepath.FromSlash(row.Path)), filepath.Base(filepath.FromSlash(row.Path))
	finalParent, err := openDeliveryDirectoryNoSymlinks(payload, parentRel)
	if err != nil {
		return err
	}
	defer func() { _ = finalParent.Close() }()
	current, err := finalParent.Lstat(name)
	if err != nil {
		return err
	}
	if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(before, current) || deliveryFileGeneration(current) != row.FileGeneration {
		return fmt.Errorf("completed payload name changed before unlink")
	}
	if err := finalParent.Remove(name); err != nil {
		return err
	}
	return syncPath(filepath.Dir(filepath.Join(r.MappedPath, filepath.FromSlash(row.Path))))
}

func (s *Service) replacementDisposableDeliveryOperation(ctx context.Context, r sqlite.DeliveryReceipt, row sqlite.DeliveryFileOutcome) (sqlite.ReplacementOperation, bool) {
	if row.Disposition != "disposable_selected_scope" || row.CleanupState == "removed" {
		return sqlite.ReplacementOperation{}, false
	}
	imports, err := s.db.DeliveryFileImports(ctx, r.ID)
	if err != nil {
		return sqlite.ReplacementOperation{}, false
	}
	for _, imported := range imports {
		if imported.Path == row.Path && imported.ReplacementDisposition == "excess" &&
			imported.ReplacementOperationID != "" && imported.SourceTransfer == r.Delivery.RequestToken &&
			imported.SourcePayloadGeneration == r.Delivery.PublicationGeneration && imported.SourceGeneration == r.GenerationKey {
			op, getErr := s.db.GetReplacementOperation(ctx, imported.ReplacementOperationID)
			if getErr != nil || op.State != "complete" || op.MediaItemID != r.MediaItemID || op.CopyID != r.CopyID {
				continue
			}
			return op, true
		}
	}
	return sqlite.ReplacementOperation{}, false
}

func (s *Service) deliveryLibraryEvidenceOwned(ctx context.Context, r sqlite.DeliveryReceipt, path, generation string) bool {
	if path == "" || generation == "" {
		return false
	}
	files, err := s.db.ListFilesForItem(ctx, r.MediaItemID)
	if err != nil {
		return false
	}
	owned := false
	if knownDeliverySidecar(path) {
		sidecarDir := filepath.Clean(filepath.Dir(path))
		sidecarStem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		for _, file := range files {
			if file.MediaItemID != r.MediaItemID || file.CopyID != r.CopyID || filepath.Clean(filepath.Dir(file.Path)) != sidecarDir || (!filename.IsVideo(file.Path) && !filename.IsBook(file.Path)) {
				continue
			}
			mediaStem := strings.TrimSuffix(filepath.Base(file.Path), filepath.Ext(file.Path))
			if matched, _ := deliverySidecarStemMatch(mediaStem, sidecarStem, path); matched {
				owned = true
				break
			}
		}
	} else {
		for _, file := range files {
			if filepath.Clean(file.Path) == filepath.Clean(path) && file.MediaItemID == r.MediaItemID && file.CopyID == r.CopyID {
				owned = true
				break
			}
		}
	}
	if !owned {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && deliveryFileGeneration(info) == generation
}

func digestOpenFile(ctx context.Context, file *os.File) (string, int64, error) {
	h := sha256.New()
	buf := make([]byte, 256*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		n, err := file.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
			total += int64(n)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", total, err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), total, nil
}

func (s *Service) removeEmptyDeliveryDirectories(ctx context.Context, r sqlite.DeliveryReceipt) (bool, error) {
	if err := s.checkStorageIdentity(ctx, r.MappedPath); err != nil {
		return false, err
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return false, err
	}
	root, rel, err := deliveryRootAndRelative(r.MappedPath, roots)
	if err != nil {
		return false, err
	}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		return false, err
	}
	defer func() { _ = anchor.Close() }()
	payload, err := openDeliveryDirectoryNoSymlinks(anchor, rel)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	payloadInfo, err := payload.Stat(".")
	if err != nil {
		_ = payload.Close()
		return false, err
	}
	if deliveryFileGeneration(payloadInfo) == "" {
		_ = payload.Close()
		return false, fmt.Errorf("publication directory identity is unavailable")
	}
	if r.PublicationDirectoryIdentity == "" || deliveryDirectoryObjectIdentity(payloadInfo) != r.PublicationDirectoryIdentity {
		_ = payload.Close()
		return false, fmt.Errorf("publication directory is not the accepted generation; refusing empty-directory cleanup")
	}
	if err := removeEmptyDeliveryDirs(ctx, payload, "."); err != nil {
		_ = payload.Close()
		return false, err
	}
	entries, err := deliveryRootReadDir(payload, ".")
	if err != nil {
		_ = payload.Close()
		return false, err
	}
	if len(entries) == 0 {
		// Removing empty child directories changes the publication directory's
		// timestamps. Refresh from the still-open directory handle so our own
		// cleanup is distinguished from a path replacement, then bind the final
		// name to this post-cleanup observation.
		refreshed, refreshErr := payload.Stat(".")
		if refreshErr != nil {
			_ = payload.Close()
			return false, refreshErr
		}
		payloadGeneration := deliveryFileGeneration(refreshed)
		if payloadGeneration == "" || !os.SameFile(payloadInfo, refreshed) || deliveryDirectoryObjectIdentity(refreshed) != r.PublicationDirectoryIdentity {
			_ = payload.Close()
			return false, fmt.Errorf("publication directory identity changed during cleanup")
		}
		current, statErr := anchor.Lstat(rel)
		if errors.Is(statErr, os.ErrNotExist) {
			_ = payload.Close()
			return true, syncPath(filepath.Dir(r.MappedPath))
		}
		if statErr != nil {
			_ = payload.Close()
			return false, statErr
		}
		if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || deliveryFileGeneration(current) != payloadGeneration || !os.SameFile(refreshed, current) {
			_ = payload.Close()
			return false, fmt.Errorf("publication directory changed before removal")
		}
		if err := payload.Close(); err != nil {
			return false, err
		}
		if err := anchor.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		return true, syncPath(filepath.Dir(r.MappedPath))
	}
	if err := payload.Close(); err != nil {
		return false, err
	}
	return false, nil
}

func removeEmptyDeliveryDirs(ctx context.Context, root *os.Root, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := deliveryRootReadDir(root, dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		child := entry.Name()
		if dir != "." {
			child = filepath.Join(dir, entry.Name())
		}
		before, err := root.Lstat(child)
		if err != nil || !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
			continue
		}
		nested, err := openDeliveryDirectoryNoSymlinks(root, child)
		if err != nil {
			continue
		}
		if err := removeEmptyDeliveryDirs(ctx, nested, "."); err != nil {
			_ = nested.Close()
			return err
		}
		nestedInfo, statErr := nested.Stat(".")
		if err := nested.Close(); err != nil {
			return err
		}
		if statErr != nil {
			return statErr
		}
		current, statErr := root.Lstat(child)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(nestedInfo, current) ||
			deliveryDirectoryObjectIdentity(before) == "" || deliveryDirectoryObjectIdentity(before) != deliveryDirectoryObjectIdentity(current) {
			continue
		}
		if err := root.Remove(child); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
			return err
		}
		if err := syncRootDirectory(root, dir); err != nil {
			return err
		}
	}
	return nil
}

func syncRootDirectory(root *os.Root, path string) error {
	f, err := root.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func deliveryRootReadDir(root *os.Root, name string) ([]os.DirEntry, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	entries, readErr := f.ReadDir(-1)
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return entries, nil
}
