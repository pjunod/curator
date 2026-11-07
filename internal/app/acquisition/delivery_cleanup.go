package acquisition

import (
	"context"
	"crypto/rand"
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

// deliveryLibraryPlacementCheck proves a placement and, when it cannot, says
// whether the failure was the environment (the database or library storage
// could not be read: worth retrying) rather than a definitive change (the row
// or file is gone, or now holds another generation or other bytes). Cheap
// identity checks run before hashing, so a changed library file is never
// re-read in full to learn what its size or generation already shows.
func (s *Service) deliveryLibraryPlacementCheck(ctx context.Context, r sqlite.DeliveryReceipt, f verifiedDeliveryFile, libraryPath, expectedGeneration, expectedIdentity string) (path, generation string, ok, transient bool) {
	if knownDeliverySidecar(libraryPath) {
		// Sidecars have no media_files generation. On the first import the
		// FileOutcome generation is the physical file observation; later
		// cleanup retries use the separately persisted LibraryFileIdentity.
		if expectedIdentity == "" {
			expectedIdentity = expectedGeneration
		}
		path, generation, ok, transient = s.deliverySidecarLibraryProof(ctx, r, f, libraryPath, expectedIdentity)
		return path, generation, ok, transient
	}
	if libraryPath == "" || expectedGeneration == "" {
		return "", "", false, false
	}
	files, err := s.db.ListFilesForItem(ctx, r.MediaItemID)
	if err != nil {
		return "", "", false, true
	}
	for _, existing := range files {
		if filepath.Clean(existing.Path) != filepath.Clean(libraryPath) || existing.MediaItemID != r.MediaItemID || existing.CopyID != r.CopyID {
			continue
		}
		if existing.FileGeneration != expectedGeneration {
			return "", "", false, false
		}
		before, err := os.Lstat(existing.Path)
		if err != nil {
			return "", "", false, s.libraryStorageUnreadable(ctx, existing.Path)
		}
		if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != f.size ||
			deliveryFileGeneration(before) == "" || (expectedIdentity != "" && deliveryFileGeneration(before) != expectedIdentity) {
			return "", "", false, false
		}
		digest, size, err := fileDigest(ctx, existing.Path)
		if err != nil {
			return "", "", false, true
		}
		after, statErr := os.Lstat(existing.Path)
		if statErr != nil {
			return "", "", false, s.libraryStorageUnreadable(ctx, existing.Path)
		}
		if os.SameFile(before, after) && deliveryFileGeneration(before) == deliveryFileGeneration(after) && size == f.size && digest == f.digest {
			return existing.Path, deliveryFileGeneration(after), true, false
		}
		return "", "", false, false
	}
	return "", "", false, false
}

// libraryStorageUnreadable reports whether a library path could not be
// observed, as opposed to observed and found changed. Any error other than
// "absent" (permissions, I/O) is unreadable. An absent path is judged against
// the registered library root that holds it, not its own folder: a title or
// season folder the user deleted leaves a readable root and is a definitive
// loss, while a root that is missing, unreadable or empty is how an unmounted
// library looks, and is worth retrying. A path under no registered root is a
// definitive loss; an unreadable root list is transient.
func (s *Service) libraryStorageUnreadable(ctx context.Context, path string) bool {
	_, err := os.Lstat(path)
	if err == nil {
		return false
	}
	if !errors.Is(err, os.ErrNotExist) {
		return true
	}
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return true
	}
	root := ""
	for _, candidate := range roots {
		clean := filepath.Clean(candidate.Path)
		if candidate.Path != "" && within(clean, path) && len(clean) > len(root) {
			root = clean
		}
	}
	if root == "" {
		return false
	}
	dir, err := os.Open(root)
	if err != nil {
		return true
	}
	defer func() { _ = dir.Close() }()
	if _, err := dir.Readdirnames(1); err != nil {
		// io.EOF: an empty root under which a placement was recorded is an
		// unmounted mount point; any other error is unreadable storage.
		return true
	}
	return false
}

// deliverySidecarLibraryProof proves a sidecar placement next to its
// title's media file. Like deliveryLibraryPlacementCheck it reports whether a
// failure was the environment (database, digest or storage unreadable) rather
// than a definitive change.
func (s *Service) deliverySidecarLibraryProof(ctx context.Context, r sqlite.DeliveryReceipt, f verifiedDeliveryFile, libraryPath, expectedGeneration string) (path, generation string, ok, transient bool) {
	if expectedGeneration == "" {
		return "", "", false, false
	}
	files, err := s.db.ListFilesForItem(ctx, r.MediaItemID)
	if err != nil {
		return "", "", false, true
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
		if err != nil {
			return "", "", false, s.libraryStorageUnreadable(ctx, libraryPath)
		}
		if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != f.size ||
			deliveryFileGeneration(before) == "" || deliveryFileGeneration(before) != expectedGeneration {
			return "", "", false, false
		}
		digest, size, err := fileDigest(ctx, libraryPath)
		if err != nil {
			return "", "", false, true
		}
		after, statErr := os.Lstat(libraryPath)
		if statErr != nil {
			return "", "", false, s.libraryStorageUnreadable(ctx, libraryPath)
		}
		if os.SameFile(before, after) && deliveryFileGeneration(before) == deliveryFileGeneration(after) && size == f.size && digest == f.digest {
			return libraryPath, deliveryFileGeneration(after), true, false
		}
		return "", "", false, false
	}
	return "", "", false, false
}

// deliveryPayloadMediaCount is the number of media files a delivery holds, for
// the sole-file condition on borrowing a release's episodes. The importer
// counts what is on disk under the publication (collectFiles); the manifest
// is what Runner published. A payload counts as a sole file only when both
// say exactly one, so the proof can never borrow where the importer refused.
func deliveryPayloadMediaCount(r sqlite.DeliveryReceipt) int {
	manifest := 0
	for _, file := range r.Delivery.Files {
		if filename.IsVideo(file.Path) && !isReleaseSample(file.Path) {
			manifest++
		}
	}
	onDisk, err := collectFiles(r.MappedPath, filename.IsVideo)
	if err != nil {
		return 0
	}
	return max(manifest, len(onDisk))
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
	scope, ok := s.deliveryLibraryScope(ctx, r, f)
	if !ok {
		return "", "", false
	}
	if path, generation, ok := deliveryExactLibraryCopy(ctx, s.db, f, scope, requiredPath); ok {
		return path, generation, true
	}
	// Different bytes. Only a verified, unflagged library file that the
	// candidate does not improve on makes it redundant — and never on a
	// borrowed episode: that is the indexer's word, and a different file
	// judged "no better" could be a different episode, which cleanup deletes.
	if scope.identityBorrowed {
		return "", "", false
	}
	item := scope.item
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
	if quality.IsBookFormat(candidate.Source) {
		// Native books are not probed: only exact bytes (above) prove them.
		return "", "", false
	}
	candidate = parser.Parse(filepath.Base(f.relative)).Quality
	if candidate.Source == quality.SourceUnknown || candidate.Resolution == 0 {
		if scope.downloadErr == nil && scope.download.Quality.Source != quality.SourceUnknown {
			candidate = scope.download.Quality
		}
	}
	if candidate.Source == quality.SourceUnknown || !profile.Acceptable(candidate) {
		return "", "", false
	}
	for _, existing := range scope.files {
		if !existing.QualityKnown || !existing.Provenance.Verified(existing.Confidence) || existing.Provenance == mediainfo.ProvenanceFailed || existing.Provenance == mediainfo.ProvenanceImplausible {
			continue
		}
		if requiredPath != "" && filepath.Clean(existing.Path) != filepath.Clean(requiredPath) {
			continue
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
		if !filename.IsVideo(existing.Path) || !profile.Acceptable(existing.Quality) {
			continue
		}
		before, err := os.Lstat(existing.Path)
		if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
			continue
		}
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
	return "", "", false
}

// deliveryLibraryScope is what a payload file must match in the library: the
// receipt's item and copy, and — for a series — exactly the episodes the file
// is for, borrowed from the grabbed release under the importer's own
// conditions when the file name has none (obfuscated posts).
type deliveryLibraryScope struct {
	item             domain.MediaItem
	files            []domain.MediaFile
	expectedEpisodes []int64
	identityBorrowed bool
	download         sqlite.Download
	downloadErr      error
	// publication is this receipt's mapped publication. Nothing inside it is
	// library evidence — a library row that aliases the payload would
	// otherwise prove the payload redundant by itself, and cleanup would
	// delete the only copy.
	publication string
}

func (s *Service) deliveryLibraryScope(ctx context.Context, r sqlite.DeliveryReceipt, f verifiedDeliveryFile) (deliveryLibraryScope, bool) {
	var scope deliveryLibraryScope
	scope.publication = r.MappedPath
	item, err := s.db.GetMediaItemFull(ctx, r.MediaItemID)
	if err != nil {
		return scope, false
	}
	scope.item = item
	scope.download, scope.downloadErr = s.db.GetDownload(ctx, r.DownloadID)
	if item.Kind == domain.KindSeries {
		parsed := parser.Parse(filepath.Base(f.relative))
		if scope.downloadErr == nil {
			season, episodes := grabbedEpisodeTargets(scope.download.WantableIDs, item.ID)
			if borrowed, ok := releaseEpisodeIdentity(parsed, f.relative, scope.download.ReleaseTitle, deliveryPayloadMediaCount(r), season, episodes); ok {
				parsed, scope.identityBorrowed = borrowed, true
			}
		}
		if parsed.Season < 0 || len(parsed.Episodes) == 0 {
			return scope, false
		}
		for _, season := range item.Seasons {
			if season.Number != parsed.Season {
				continue
			}
			for _, number := range parsed.Episodes {
				found := false
				for _, episode := range season.Episodes {
					if episode.EpisodeNumber == number {
						scope.expectedEpisodes = append(scope.expectedEpisodes, episode.ID)
						found = true
						break
					}
				}
				if !found {
					return scope, false
				}
			}
		}
		if len(scope.expectedEpisodes) != len(parsed.Episodes) {
			return scope, false
		}
		slices.Sort(scope.expectedEpisodes)
	}
	files, err := s.db.ListFilesForItem(ctx, r.MediaItemID)
	if err != nil {
		return scope, false
	}
	for _, existing := range files {
		if existing.MediaItemID != r.MediaItemID || existing.CopyID != r.CopyID || existing.FileGeneration == "" {
			continue
		}
		if item.Kind == domain.KindSeries {
			got := append([]int64(nil), existing.EpisodeIDs...)
			slices.Sort(got)
			if !slices.Equal(got, scope.expectedEpisodes) {
				continue
			}
		}
		scope.files = append(scope.files, existing)
	}
	return scope, true
}

// deliveryExactLibraryCopy finds a library file in scope holding exactly the
// payload file's bytes, in the same format. Identical bytes make the payload
// copy redundant whatever the library file's standing — unverified quality,
// an open review flag, an implausible runtime, a profile that would reject
// it: deleting the payload loses nothing the library does
// not already hold. (Paul, 2026-11-01: six
// copies of a one-minute fake were imported over one another, and the five
// superseded payloads were retained forever because the surviving library
// copy was flagged.) The digest is bound to one stable file generation.
func deliveryExactLibraryCopy(ctx context.Context, db *sqlite.DB, f verifiedDeliveryFile, scope deliveryLibraryScope, requiredPath string) (string, string, bool) {
	for _, existing := range scope.files {
		if requiredPath != "" && filepath.Clean(existing.Path) != filepath.Clean(requiredPath) {
			continue
		}
		if withinPublication(existing.Path, scope.publication, f.absolute) {
			continue
		}
		// The same format, too: a book's format is its extension, so identical
		// bytes delivered as .epub over a library .pdf are a format change the
		// import must make, not a duplicate.
		if !strings.EqualFold(filepath.Ext(existing.Path), filepath.Ext(f.relative)) {
			continue
		}
		// An in-flight replace-bad operation owns the fate of the file it
		// selected and of the payloads that answer it; it is not this proof's
		// to settle.
		if replacementFileMarkedBad(ctx, db, existing.ID, existing.FileGeneration) {
			continue
		}
		before, err := os.Lstat(existing.Path)
		if err != nil || !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 || before.Size() != f.size {
			continue
		}
		digest, size, err := fileDigest(ctx, existing.Path)
		after, statErr := os.Lstat(existing.Path)
		if err == nil && statErr == nil && os.SameFile(before, after) && deliveryFileGeneration(before) != "" &&
			deliveryFileGeneration(before) == deliveryFileGeneration(after) && size == f.size && digest == f.digest {
			return existing.Path, deliveryFileGeneration(after), true
		}
	}
	return "", "", false
}

// withinPublication reports whether path could be the payload rather than
// a library copy of it: inside the accepted publication, or the payload
// file's own directory entry. It compares filesystem identity, not path
// text, so a bind-mounted alias of the completed folder ("/downloads" for
// "/working/monarr/completed") is still recognized, and so is a library
// folder bind-mounted onto a subfolder of the publication (the same-entry
// check). Any symlinked folder on the path is refused outright, as
// placement refuses it: a symlinked library folder into the publication
// would otherwise pass. Only the publication counts — a library configured
// inside a completed root is still a library — and a hard-linked placement
// is a different directory entry, not refused. Anything that cannot be
// examined is treated as inside: it cannot be evidence.
func withinPublication(path, publication, payloadFile string) bool {
	if publication == "" || rejectSymlinks(path, false) != nil {
		return true
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return true
	}
	if payloadFile != "" {
		payloadParent, err := os.Stat(filepath.Dir(payloadFile))
		if err != nil || (os.SameFile(parent, payloadParent) && filepath.Base(path) == filepath.Base(payloadFile)) {
			return true
		}
	}
	pub, err := os.Stat(publication)
	if err != nil {
		return true
	}
	if !pub.IsDir() {
		pubParent, err := os.Stat(filepath.Dir(publication))
		return err != nil || (os.SameFile(parent, pubParent) && filepath.Base(path) == filepath.Base(publication))
	}
	for dir := filepath.Dir(path); ; {
		if info, err := os.Stat(dir); err == nil && os.SameFile(info, pub) {
			return true
		}
		next := filepath.Dir(dir)
		if next == dir {
			return false
		}
		dir = next
	}
}

func replacementFileMarkedBad(ctx context.Context, db *sqlite.DB, fileID int64, generation string) bool {
	var count int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM replacement_operation_files f
		JOIN replacement_operations o ON o.operation_id=f.operation_id
		WHERE f.media_file_id=? AND f.file_generation=? AND f.disposition='selected'
		AND f.removal_state!='removed' AND o.intent='replace_bad'
		AND o.state IN ('accepted','removing','cleanup_blocked','searching','waiting_for_release','replacing')`, fileID, generation).Scan(&count); err != nil {
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

func (s *Service) resumeExplicitDeliveryDeleteBatches(ctx context.Context, r sqlite.DeliveryReceipt) (map[string]bool, error) {
	due, err := s.db.ExplicitDeliveryDeleteBatchesDue(ctx, r.ID, 16)
	if err != nil {
		return nil, err
	}
	pending, err := s.db.ExplicitDeliveryDeleteBatchesPending(ctx, r.ID, 10000)
	if err != nil {
		return nil, err
	}
	protected := make(map[string]bool)
	for _, batch := range pending {
		for _, file := range batch.Files {
			if !file.HistoryRecorded {
				protected[file.Path] = true
			}
		}
	}
	for _, batch := range due {
		for _, file := range batch.Files {
			if !file.HistoryRecorded {
				protected[file.Path] = true
			}
		}
		selections := make([]CompletedFileSelection, 0, len(batch.Files))
		for _, file := range batch.Files {
			selections = append(selections, CompletedFileSelection{
				Path:             file.ReviewedPath,
				SnapshotIdentity: file.SnapshotIdentity,
			})
		}
		if err := s.deleteSelectedDeliveryFilesForBatch(ctx, batch.ID, batch.PayloadPath, batch.InventoryFingerprint, selections); err != nil {
			return protected, err
		}
	}
	if len(pending) == 10000 {
		// Refuse generic cleanup rather than risk missing an explicit path
		// behind an unexpectedly large journal. Due batches above still get
		// their independent bounded worker opportunities.
		return protected, fmt.Errorf("explicit delivery deletion journal exceeds the safe pending-batch scan limit")
	}
	return protected, nil
}

func (s *Service) cleanupDeliveryFiles(ctx context.Context, r sqlite.DeliveryReceipt) error {
	explicitPaths, err := s.resumeExplicitDeliveryDeleteBatches(ctx, r)
	if err != nil {
		return err
	}
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
		if explicitPaths[row.Path] {
			continue
		}
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
			// A placement observed gone or changed is definitive: the payload is
			// kept for explicit review instead of re-proving (and re-hashing the
			// library) forever. A library that could not be read retries.
			state := "retry"
			if errors.Is(err, errDeliveryPlacementLost) {
				state = "protected"
			}
			_ = s.db.SetDeliveryFileCleanup(ctx, r.ID, row.Path, state, err.Error())
			continue
		}
		if err := s.db.FinishDeliveryAttempt(ctx, event.ID, "succeeded", ""); err != nil {
			return err
		}
		if err := s.db.SetDeliveryFileCleanup(ctx, r.ID, row.Path, "removed", ""); err != nil {
			return err
		}
	}
	// A reviewed batch owns the publication directory until every selected
	// path has its own durable completion/audit marker. This also protects the
	// crash window where generic cleanup already stamped an unlinked file as
	// removed but the explicit journal still needs to record its event.
	if len(explicitPaths) > 0 {
		return nil
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
	// An operator is waiting on this request: its verification reads are
	// foreground and are not held to the background budget.
	ctx = withForegroundVerification(ctx)
	if len(selections) == 0 {
		return fmt.Errorf("select one or more manifest files to delete")
	}
	batchID, batchIDErr := explicitDeliveryDeleteBatchID(payloadPath, fingerprint, selections)
	if batchIDErr != nil {
		return batchIDErr
	}
	if prior, err := s.db.ExplicitDeliveryDeleteBatch(ctx, batchID); err == nil && !prior.AbandonedAt.IsZero() {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return err
		}
		batchID = fmt.Sprintf("%s-%x", batchID, nonce)
	}
	return s.deleteSelectedDeliveryFilesForBatch(ctx, batchID, payloadPath, fingerprint, selections)
}

func (s *Service) deleteSelectedDeliveryFilesForBatch(ctx context.Context, batchID, payloadPath, fingerprint string, selections []CompletedFileSelection) error {
	s.completedMu.Lock()
	err := s.deleteSelectedDeliveryFilesLocked(ctx, batchID, payloadPath, fingerprint, selections)
	s.completedMu.Unlock()
	if err != nil {
		if _, batchErr := s.db.ExplicitDeliveryDeleteBatch(ctx, batchID); batchErr == nil {
			if retryErr := s.db.ScheduleExplicitDeliveryDeleteRetry(ctx, batchID, err.Error()); retryErr != nil {
				return errors.Join(err, retryErr)
			}
		}
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
	parentIdentity    string
	absent            bool
}

func persistedBatchFile(batch sqlite.ExplicitDeliveryDeleteBatch, path string) (sqlite.ExplicitDeliveryDeleteFile, bool) {
	for _, file := range batch.Files {
		if file.Path == path {
			return file, true
		}
	}
	return sqlite.ExplicitDeliveryDeleteFile{}, false
}

func rootStorageIdentity(roots []CompletedRoot, path string) string {
	for _, root := range roots {
		if canonicalCompletedPath(root.Path) == canonicalCompletedPath(path) {
			return root.StorageIdentity
		}
	}
	return ""
}

func syncDeliveryParent(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func explicitDeliveryDeleteBatchID(payloadPath, fingerprint string, selections []CompletedFileSelection) (string, error) {
	if len(selections) == 0 {
		return "", fmt.Errorf("select one or more manifest files to delete")
	}
	ordered := append([]CompletedFileSelection(nil), selections...)
	slices.SortFunc(ordered, func(a, b CompletedFileSelection) int {
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return strings.Compare(a.SnapshotIdentity, b.SnapshotIdentity)
	})
	h := sha256.New()
	_, _ = io.WriteString(h, canonicalCompletedPath(payloadPath)+"\x00"+fingerprint+"\x00")
	for _, selection := range ordered {
		if selection.Path == "" || selection.SnapshotIdentity == "" {
			return "", fmt.Errorf("each selected delivery file requires a reviewed snapshot")
		}
		_, _ = io.WriteString(h, canonicalCompletedPath(selection.Path)+"\x00"+selection.SnapshotIdentity+"\x00")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Service) deleteSelectedDeliveryFilesLocked(ctx context.Context, batchID, payloadPath, fingerprint string, selections []CompletedFileSelection) error {
	if !filepath.IsAbs(payloadPath) || fingerprint == "" {
		return fmt.Errorf("select an unchanged delivery payload from the completed-storage inventory")
	}
	existingBatch, batchErr := s.db.ExplicitDeliveryDeleteBatch(ctx, batchID)
	if batchErr != nil && !errors.Is(batchErr, sqlite.ErrNotFound) {
		return batchErr
	}
	hasBatch := batchErr == nil
	if hasBatch && !existingBatch.AbandonedAt.IsZero() {
		return fmt.Errorf("deletion request was cancelled; review the current files before another action")
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
			if canonicalCompletedPath(entry.Path) == canonicalCompletedPath(payloadPath) &&
				(entry.Fingerprint == fingerprint || hasBatch) {
				selectedEntry, storageRoot = entry, root.Path
			}
		}
	}
	if selectedEntry == nil && !hasBatch {
		return fmt.Errorf("selected payload is not an unchanged delivery-managed entry")
	}
	if selectedEntry != nil && (selectedEntry.LegacyReview || !selectedEntry.DeliveryManaged) {
		return fmt.Errorf("selected payload is not an unchanged delivery-managed entry")
	}
	if hasBatch && (canonicalCompletedPath(existingBatch.PayloadPath) != canonicalCompletedPath(payloadPath) ||
		existingBatch.InventoryFingerprint != fingerprint) {
		return fmt.Errorf("selected deletion does not match its original reviewed batch")
	}
	if hasBatch {
		persisted := make(map[string]string, len(existingBatch.Files))
		for _, file := range existingBatch.Files {
			persisted[file.Path] = file.SnapshotIdentity
		}
		if len(persisted) != len(selections) {
			return fmt.Errorf("selected deletion does not contain the complete reviewed batch")
		}
		for _, selection := range selections {
			rel, relErr := filepath.Rel(canonicalCompletedPath(payloadPath), canonicalCompletedPath(selection.Path))
			file, ok := persistedBatchFile(existingBatch, filepath.ToSlash(rel))
			if relErr != nil || !ok || persisted[filepath.ToSlash(rel)] != selection.SnapshotIdentity || canonicalCompletedPath(file.ReviewedPath) != canonicalCompletedPath(selection.Path) {
				return fmt.Errorf("selected deletion differs from its persisted reviewed batch")
			}
		}
	}
	for _, root := range inventory.Roots {
		if (storageRoot == "" && within(canonicalCompletedPath(root.Path), canonicalCompletedPath(payloadPath))) ||
			canonicalCompletedPath(root.Path) == canonicalCompletedPath(storageRoot) {
			if hasBatch && root.StorageIdentity != existingBatch.StorageIdentity {
				return fmt.Errorf("expected storage changed since the explicit deletion review")
			}
			if root.Error != "" {
				return fmt.Errorf("inventory is incomplete; scan again before deleting selected files")
			}
			if storageRoot == "" {
				storageRoot = root.Path
			}
		}
	}
	if storageRoot == "" {
		return fmt.Errorf("configured completed storage root is unavailable")
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return err
	}
	receipts := make([]sqlite.DeliveryReceipt, 0, 1)
	canonicalPayload := storagePath(payloadPath, roots)
	if hasBatch {
		receipt, getErr := s.db.DeliveryReceiptByID(ctx, existingBatch.ReceiptID)
		if getErr != nil {
			return fmt.Errorf("reviewed delivery receipt is unavailable")
		}
		if storagePath(receipt.MappedPath, roots) != canonicalPayload {
			return fmt.Errorf("reviewed receipt no longer maps to this payload")
		}
		receipts = append(receipts, receipt)
	} else {
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
	}
	if len(receipts) == 1 {
		if err := deliveryReceiptActionGuard(receipts[0]); err != nil {
			return err
		}
	}
	if len(receipts) != 1 || (receipts[0].AckState != "accepted" && (receipts[0].AckState != "pending" || receipts[0].ConsumerReceiptID == "")) || receipts[0].State == "conflict" || receipts[0].State == "importing" {
		return fmt.Errorf("delivery has no locally prepared acceptance, is importing, or is ambiguous; selected-file deletion requires one stable manifest")
	}
	r := receipts[0]
	if hasBatch && (r.GenerationKey != existingBatch.GenerationKey || r.Delivery.RequestToken != existingBatch.RequestToken ||
		r.Delivery.PublicationGeneration != existingBatch.PublicationGeneration ||
		r.PublicationDirectoryIdentity != existingBatch.PublicationDirectoryIdentity) {
		return fmt.Errorf("delivery identity differs from the persisted explicit deletion review")
	}
	if err := deliveryReceiptActionGuard(r); err != nil {
		return err
	}
	if r.DownloadID > 0 {
		// Import workers serialize on this per-download lock from queue pickup
		// through placement. Holding it across verification and unlink closes
		// the race where an import starts after the initial state check.
		unlockDownload := s.lockDownload(r.DownloadID)
		defer unlockDownload()
		current, err := s.deliveryReceiptForAction(ctx, r.ID)
		if err != nil {
			return err
		}
		if (current.AckState != "accepted" && (current.AckState != "pending" || current.ConsumerReceiptID == "")) || current.State == "conflict" || current.State == "importing" {
			return fmt.Errorf("delivery became active or ambiguous; selected-file deletion requires a locally prepared manifest")
		}
		r = current
		dl, err := s.db.GetDownload(ctx, r.DownloadID)
		if err != nil && (!hasBatch || !errors.Is(err, sqlite.ErrNotFound)) {
			return fmt.Errorf("delivery download state is unavailable; refusing deletion")
		}
		if err == nil && dl.State == "importing" {
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
		if hasBatch {
			return fmt.Errorf("reviewed publication identity is unavailable; refusing replay")
		}
		// Explicit review has selected this exact payload and each file will be
		// checked against the accepted manifest before the identity is pinned.
		if err := s.db.EnsureDeliveryDirectoryIdentity(ctx, r.ID, identity); err != nil {
			return err
		}
		r.PublicationDirectoryIdentity = identity
	} else if identity != r.PublicationDirectoryIdentity {
		return fmt.Errorf("delivery publication directory does not match its accepted generation")
	}
	if hasBatch && existingBatch.PublicationDirectoryIdentity != identity {
		return fmt.Errorf("publication directory differs from the reviewed deletion batch")
	}
	manifest := make(map[string]ports.RunnerFile, len(r.Delivery.Files))
	for _, file := range r.Delivery.Files {
		manifest[file.Path] = file
	}
	chosen := map[string]bool{}
	verified := make([]selectedDeliveryDeletion, 0, len(selections))
	for _, selection := range selections {
		if !filepath.IsAbs(selection.Path) || selection.SnapshotIdentity == "" || !within(canonicalCompletedPath(payloadPath), canonicalCompletedPath(selection.Path)) {
			return fmt.Errorf("selected path is outside the delivery payload or has no current snapshot")
		}
		rel, err := filepath.Rel(canonicalCompletedPath(payloadPath), canonicalCompletedPath(selection.Path))
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
		var parent *os.Root
		var file *os.File
		var named os.FileInfo
		parentRelative := filepath.Dir(filepath.FromSlash(rel))
		parent, err = openDeliveryDirectoryNoSymlinks(payload, parentRelative)
		if err != nil {
			return fmt.Errorf("open selected parent %s: %w", rel, err)
		}
		name := filepath.Base(filepath.FromSlash(rel))
		named, err = parent.Lstat(name)
		parentInfo, parentStatErr := parent.Stat(".")
		if parentStatErr != nil {
			_ = parent.Close()
			return parentStatErr
		}
		parentIdentity := deliveryDirectoryObjectIdentity(parentInfo)
		if errors.Is(err, os.ErrNotExist) && hasBatch {
			persistedFile, ok := persistedBatchFile(existingBatch, rel)
			if !ok || canonicalCompletedPath(persistedFile.ReviewedPath) != canonicalCompletedPath(selection.Path) || persistedFile.SnapshotIdentity != selection.SnapshotIdentity || persistedFile.ParentIdentity == "" || persistedFile.ParentIdentity != parentIdentity {
				_ = parent.Close()
				return fmt.Errorf("absent selected file is not bound to this reviewed parent and snapshot: %s", rel)
			}
			_ = parent.Close()
			if ledgerErr != nil {
				return fmt.Errorf("reviewed deletion lost its durable manifest file record: %s", rel)
			}
			verified = append(verified, selectedDeliveryDeletion{relative: rel, selection: selection, ledger: ledger, parentIdentity: parentIdentity, absent: true})
			continue
		}
		if err != nil {
			_ = parent.Close()
			return fmt.Errorf("open selected file %s: %w", rel, err)
		}
		file, err = parent.Open(name)
		if err != nil {
			_ = parent.Close()
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
		if hasBatch {
			persistedFile, ok := persistedBatchFile(existingBatch, rel)
			if !ok || canonicalCompletedPath(persistedFile.ReviewedPath) != canonicalCompletedPath(selection.Path) || persistedFile.SnapshotIdentity != selection.SnapshotIdentity || persistedFile.ParentIdentity != parentIdentity {
				return fmt.Errorf("selected file differs from its persisted reviewed batch: %s", rel)
			}
		}
		verified = append(verified, selectedDeliveryDeletion{relative: rel, selection: selection, ledger: ledger, info: info, currentSize: size, currentDigest: digest, currentGeneration: generation, parentIdentity: parentIdentity})
	}
	if hasBatch && rootStorageIdentity(inventory.Roots, storageRoot) != existingBatch.StorageIdentity {
		return fmt.Errorf("storage identity differs from the reviewed deletion batch")
	}
	targetCtx, releaseTarget := s.lockImportTarget(ctx, r.MediaItemID)
	defer releaseTarget()
	decisionCtx, release := s.lockStorageDecision(targetCtx)
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
		if (getErr != nil && (!hasBatch || !errors.Is(getErr, sqlite.ErrNotFound))) || (getErr == nil && dl.State == "importing") {
			return fmt.Errorf("delivery import is active or unavailable; wait for it to finish before deleting selected files")
		}
	}
	if err := s.checkStorageIdentity(decisionCtx, payloadPath); err != nil {
		return err
	}
	if !hasBatch {
		files := make([]sqlite.ExplicitDeliveryDeleteFile, 0, len(verified))
		for _, file := range verified {
			files = append(files, sqlite.ExplicitDeliveryDeleteFile{Path: file.relative, ReviewedPath: file.selection.Path, SnapshotIdentity: file.selection.SnapshotIdentity, ParentIdentity: file.parentIdentity})
		}
		batch := sqlite.ExplicitDeliveryDeleteBatch{
			ID: batchID, ReceiptID: r.ID, PayloadPath: canonicalCompletedPath(payloadPath),
			InventoryFingerprint: fingerprint, PublicationDirectoryIdentity: r.PublicationDirectoryIdentity,
			StorageIdentity: rootStorageIdentity(inventory.Roots, storageRoot), GenerationKey: r.GenerationKey,
			RequestToken: r.Delivery.RequestToken, PublicationGeneration: r.Delivery.PublicationGeneration, Files: files,
		}
		if batch.StorageIdentity == "" {
			return fmt.Errorf("verified storage identity is unavailable; refusing explicit deletion")
		}
		if err := s.db.PrepareExplicitDeliveryDeleteBatch(decisionCtx, batch); err != nil {
			return err
		}
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
		parentInfo, parentErr := parent.Stat(".")
		if parentErr != nil || deliveryDirectoryObjectIdentity(parentInfo) != file.parentIdentity {
			_ = parent.Close()
			return fmt.Errorf("selected file parent changed since review: %s", file.relative)
		}
		row, rowErr := s.db.DeliveryFileOutcomeByPath(decisionCtx, r.ID, file.relative)
		if rowErr != nil {
			_ = parent.Close()
			return rowErr
		}
		if file.absent {
			if !errors.Is(err, os.ErrNotExist) {
				_ = parent.Close()
				return fmt.Errorf("selected file reappeared after its reviewed deletion: %s", file.relative)
			}
			persistedFile, persisted := persistedBatchFile(existingBatch, file.relative)
			if !persisted {
				_ = parent.Close()
				return fmt.Errorf("selected file is absent without its exact durable deletion intent: %s", file.relative)
			}
			if persistedFile.HistoryRecorded {
				_ = parent.Close()
				continue
			}
			if err := syncDeliveryParent(parent); err != nil {
				_ = parent.Close()
				return err
			}
			if err := parent.Close(); err != nil {
				return err
			}
			// An attempt marker cannot prove that Remove succeeded. A crash after
			// unlink and a later external removal are indistinguishable here.
			eventType := "delivery_file_absence_confirmed"
			if err := s.db.CompleteExplicitDeliveryFileDelete(decisionCtx, batchID, r.ID, file.relative, eventType, r.MediaItemID, filepath.Base(file.selection.Path), map[string]any{
				"receiptId": r.ID, "downloadId": r.DownloadID, "deliveryId": r.Delivery.DeliveryID, "publicationGeneration": r.Delivery.PublicationGeneration,
				"manifestPath": file.relative, "manifestSHA256": file.ledger.SHA256, "manifestSizeBytes": file.ledger.SizeBytes,
				"reviewedFileGeneration": file.selection.SnapshotIdentity, "absenceConfirmedOnReplay": true, "priorUnlinkAttempt": persistedFile.UnlinkStarted,
			}); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			_ = parent.Close()
			return err
		}
		if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(file.info, current) || completedReviewFileIdentity(file.selection.Path, current) != file.selection.SnapshotIdentity {
			_ = parent.Close()
			return fmt.Errorf("selected file changed before deletion: %s", file.relative)
		}
		if row.CleanupState == "removed" {
			_ = parent.Close()
			return fmt.Errorf("selected file is marked removed but its reviewed bytes reappeared: %s", file.relative)
		}
		if err := s.db.BeginExplicitDeliveryFileUnlink(decisionCtx, batchID, file.relative); err != nil {
			_ = parent.Close()
			return err
		}
		if err := s.unlinkExplicitDeliveryFile(parent, name); err != nil {
			_ = parent.Close()
			return err
		}
		if err := syncDeliveryParent(parent); err != nil {
			_ = parent.Close()
			return err
		}
		if err := parent.Close(); err != nil {
			return err
		}
		if err := s.db.CompleteExplicitDeliveryFileDelete(decisionCtx, batchID, r.ID, file.relative, "delivery_file_deleted", r.MediaItemID, filepath.Base(file.selection.Path), map[string]any{
			"receiptId": r.ID, "downloadId": r.DownloadID, "deliveryId": r.Delivery.DeliveryID, "publicationGeneration": r.Delivery.PublicationGeneration,
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
		var transient bool
		libraryEvidencePath, libraryEvidenceGeneration, ok, transient = s.deliveryLibraryPlacementCheck(ctx, r, f, row.LibraryPath, expectedLibraryGeneration, expectedLibraryIdentity)
		if !ok && !row.ManualAdditional {
			// The placement was superseded — typically by a later copy of the
			// same release imported over it. If the library still holds these
			// exact bytes for the same episodes, this copy is redundant.
			if scope, scoped := s.deliveryLibraryScope(ctx, r, f); scoped {
				libraryEvidencePath, libraryEvidenceGeneration, ok = deliveryExactLibraryCopy(ctx, s.db, f, scope, "")
			}
		}
		if !ok && transient {
			return fmt.Errorf("library placement could not be read; cleanup will retry")
		}
		if !ok {
			return fmt.Errorf("%w; payload retained for explicit review", errDeliveryPlacementLost)
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
		if !ok && !row.ManualAdditional {
			// The recorded library file was replaced, possibly by an
			// identical copy under another name. Identical bytes anywhere in
			// scope still make this copy redundant.
			if scope, scoped := s.deliveryLibraryScope(ctx, r, f); scoped {
				libraryEvidencePath, libraryEvidenceGeneration, ok = deliveryExactLibraryCopy(ctx, s.db, f, scope, "")
			}
		}
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
	targetCtx, releaseTarget := s.lockImportTarget(ctx, r.MediaItemID)
	defer releaseTarget()
	decisionCtx, release := s.lockStorageDecision(targetCtx)
	defer release()
	if err := s.assertImportSourceDisposable(decisionCtx, absolute); err != nil {
		return err
	}
	legacy, err := s.db.LegacyCleanupReview(decisionCtx, r.GenerationKey, r.MappedPath)
	if err != nil {
		return err
	}
	if legacy {
		return fmt.Errorf("completed-payload cleanup is blocked for the legacy review cohort")
	}
	pendingExplicitDelete, err := s.db.HasPendingExplicitDeliveryDelete(decisionCtx, r.ID, row.Path)
	if err != nil {
		return err
	}
	if pendingExplicitDelete {
		return fmt.Errorf("an exact reviewed deletion batch owns this payload file")
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
	} else if !s.deliveryLibraryEvidenceOwned(decisionCtx, r, libraryEvidencePath, libraryEvidenceGeneration, absolute) {
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

func (s *Service) deliveryLibraryEvidenceOwned(ctx context.Context, r sqlite.DeliveryReceipt, path, generation, payloadFile string) bool {
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
				// A replace-bad selection made since the proof owns this
				// file's fate; leave the payload until it resolves.
				owned = !replacementFileMarkedBad(ctx, s.db, file.ID, file.FileGeneration)
				break
			}
		}
	}
	if !owned {
		return false
	}
	// Evidence inside the publication is the payload, not the library.
	if withinPublication(path, r.MappedPath, payloadFile) {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && deliveryFileGeneration(info) == generation
}

func digestOpenFile(ctx context.Context, file *os.File) (string, int64, error) {
	h := sha256.New()
	buf := make([]byte, 256*1024)
	var total int64
	src := newVerifyReader(ctx, file)
	for {
		if err := ctx.Err(); err != nil {
			return "", total, err
		}
		n, err := src.Read(buf)
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
	targetCtx, releaseTarget := s.lockImportTarget(ctx, r.MediaItemID)
	defer releaseTarget()
	decisionCtx, release := s.lockStorageDecision(targetCtx)
	defer release()
	pendingExplicitDelete, err := s.db.HasPendingExplicitDeliveryDelete(decisionCtx, r.ID, "")
	if err != nil {
		return false, err
	}
	if pendingExplicitDelete {
		return false, fmt.Errorf("an exact reviewed deletion batch still owns the publication directory")
	}
	if err := s.checkStorageIdentity(decisionCtx, r.MappedPath); err != nil {
		return false, err
	}
	roots, err := s.completedRoots(decisionCtx, nil)
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

var errDeliveryPlacementLost = errors.New("verified library placement is no longer present")

func (s *Service) unlinkExplicitDeliveryFile(parent *os.Root, name string) error {
	if s.explicitDeliveryUnlink != nil {
		return s.explicitDeliveryUnlink(parent, name)
	}
	return parent.Remove(name)
}
