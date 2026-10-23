package acquisition

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

type ReplacementObservedFile struct {
	ID             int64  `json:"id"`
	FileGeneration string `json:"fileGeneration"`
	ObservationID  string `json:"observationId"`
}

// ReplacementRequest is the explicit user authorization. The pointer boolean
// is required so omission cannot silently choose a blacklist policy.
type ReplacementRequest struct {
	RequestID              string                    `json:"requestId"`
	SourceGeneration       string                    `json:"sourceGeneration"`
	SourceTransfer         string                    `json:"sourceTransfer,omitempty"`
	Scope                  string                    `json:"scope"`
	MediaItemID            int64                     `json:"mediaItemId"`
	SourceFileID           int64                     `json:"sourceFileId"`
	CopyID                 int64                     `json:"copyId"`
	EpisodeIDs             []int64                   `json:"episodeIds,omitempty"`
	Intent                 string                    `json:"intent"`
	BlacklistSourceRelease *bool                     `json:"blacklistSourceRelease"`
	ReasonCode             string                    `json:"reasonCode"`
	Reason                 string                    `json:"reason"`
	ObservedFiles          []ReplacementObservedFile `json:"observedFiles"`
}

var ErrReplacementStale = fmt.Errorf("replacement target changed; refresh review")
var ErrReplacementInvalid = fmt.Errorf("invalid replacement request")

// Injectable for focused mount-change tests; production uses platform mount
// identity checks from expectedStorageIdentity.
var replacementStorageIdentity = expectedStorageIdentity

func replacementInvalid(message string) error {
	return fmt.Errorf("%w: %s", ErrReplacementInvalid, message)
}

func (s *Service) CreateReplacement(ctx context.Context, req ReplacementRequest) (sqlite.ReplacementOperation, bool, error) {
	if strings.TrimSpace(req.RequestID) == "" || req.MediaItemID <= 0 || req.SourceFileID <= 0 ||
		(req.Scope != "episodes" && req.Scope != "download") || (req.Intent != "replace_bad" && req.Intent != "replace_quality") ||
		req.BlacklistSourceRelease == nil || strings.TrimSpace(req.ReasonCode) == "" || strings.TrimSpace(req.Reason) == "" || len(req.ObservedFiles) == 0 {
		return sqlite.ReplacementOperation{}, false, replacementInvalid("requestId, scope, item/copy, intent, blacklist choice, reason, and observedFiles are required; sourceGeneration is optional for legacy files")
	}
	if req.Scope == "episodes" && len(req.EpisodeIDs) == 0 || req.Scope == "download" && len(req.EpisodeIDs) != 0 {
		return sqlite.ReplacementOperation{}, false, replacementInvalid("episode scope requires episodeIds; download scope must omit episodeIds")
	}
	if len(req.EpisodeIDs) > 0 {
		sort.Slice(req.EpisodeIDs, func(i, j int) bool { return req.EpisodeIDs[i] < req.EpisodeIDs[j] })
		for i := 1; i < len(req.EpisodeIDs); i++ {
			if req.EpisodeIDs[i] == req.EpisodeIDs[i-1] {
				return sqlite.ReplacementOperation{}, false, replacementInvalid("episodeIds contains duplicates")
			}
		}
	}
	sort.Slice(req.ObservedFiles, func(i, j int) bool { return req.ObservedFiles[i].ID < req.ObservedFiles[j].ID })
	for i := 1; i < len(req.ObservedFiles); i++ {
		if req.ObservedFiles[i].ID == req.ObservedFiles[i-1].ID {
			return sqlite.ReplacementOperation{}, false, replacementInvalid("observedFiles contains duplicate ids")
		}
	}
	fingerprintPayload, err := json.Marshal(req)
	if err != nil {
		return sqlite.ReplacementOperation{}, false, err
	}
	fingerprint := sha256.Sum256(fingerprintPayload)
	fingerprintHex := hex.EncodeToString(fingerprint[:])
	if prior, e := s.db.GetReplacementOperationByRequestID(ctx, req.RequestID); e == nil {
		if prior.Fingerprint != fingerprintHex {
			return sqlite.ReplacementOperation{}, false, sqlite.ErrReplacementRequestConflict
		}
		return prior, true, nil
	} else if e != sqlite.ErrNotFound {
		return sqlite.ReplacementOperation{}, false, e
	}

	item, err := s.db.GetMediaItemFull(ctx, req.MediaItemID)
	if err != nil {
		return sqlite.ReplacementOperation{}, false, err
	}
	validEpisodes := map[int64]bool{}
	for _, season := range item.Seasons {
		for _, ep := range season.Episodes {
			validEpisodes[ep.ID] = true
		}
	}
	selected := map[int64]bool{}
	for _, epID := range req.EpisodeIDs {
		if !validEpisodes[epID] {
			return sqlite.ReplacementOperation{}, false, replacementInvalid(fmt.Sprintf("episode %d does not belong to item %d", epID, item.ID))
		}
		selected[epID] = true
	}
	files, err := s.db.ListFilesForItem(ctx, req.MediaItemID)
	if err != nil {
		return sqlite.ReplacementOperation{}, false, err
	}
	byID := make(map[int64]domain.MediaFile, len(files))
	for _, f := range files {
		byID[f.ID] = f
	}
	root := item.Path
	if req.CopyID != 0 {
		copy, e := s.db.GetMediaCopy(ctx, req.MediaItemID, req.CopyID)
		if e != nil {
			return sqlite.ReplacementOperation{}, false, e
		}
		if copy.Path != "" {
			root = copy.Path
		}
	}
	if root == "" {
		return sqlite.ReplacementOperation{}, false, ErrNoLibraryFolder
	}
	observed := make(map[int64]bool, len(req.ObservedFiles))
	var opFiles []sqlite.ReplacementFile
	covered := map[int64]bool{}
	var sourceRelease, sourceIndexer, sourceGUID, sourceGeneration, sourceTransfer string
	var sourceIndexerID int64
	sourceIdentitySet := false
	anchorFile, anchorExists := byID[req.SourceFileID]
	if !anchorExists || anchorFile.CopyID != req.CopyID || anchorFile.FileGeneration == "" {
		return sqlite.ReplacementOperation{}, false, ErrReplacementStale
	}
	if req.SourceGeneration != "" && anchorFile.SourceGeneration != req.SourceGeneration || req.SourceTransfer != "" && anchorFile.SourceTransfer != req.SourceTransfer {
		return sqlite.ReplacementOperation{}, false, ErrReplacementStale
	}
	for _, expected := range req.ObservedFiles {
		f, ok := byID[expected.ID]
		if !ok || observed[f.ID] || f.CopyID != req.CopyID || f.FileGeneration == "" || f.FileGeneration != expected.FileGeneration ||
			(sameReplacementSource(anchorFile, f) && req.SourceGeneration != "" && f.SourceGeneration != req.SourceGeneration) ||
			(sameReplacementSource(anchorFile, f) && req.SourceTransfer != "" && req.SourceTransfer != f.SourceTransfer) {
			return sqlite.ReplacementOperation{}, false, ErrReplacementStale
		}
		observed[f.ID] = true
		rel, e := filepath.Rel(filepath.Clean(root), filepath.Clean(f.Path))
		if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return sqlite.ReplacementOperation{}, false, replacementInvalid("file is outside the selected copy folder")
		}
		if err := rejectSymlinks(f.Path, true); err != nil {
			return sqlite.ReplacementOperation{}, false, ErrReplacementStale
		}
		observation, e := s.db.GetReplacementFileObservation(ctx, expected.ObservationID)
		if e != nil || ((observation.State != "ready" && req.Intent != "replace_bad") || (observation.State != "ready" && observation.State != "absent")) || observation.MediaItemID != req.MediaItemID || observation.MediaFileID != f.ID || observation.FileGeneration != f.FileGeneration || observation.SourceGeneration != f.SourceGeneration || filepath.Clean(observation.Path) != filepath.Clean(f.Path) || observation.FileIdentity == "" || observation.FilesystemGeneration == "" || observation.StorageIdentity == "" {
			return sqlite.ReplacementOperation{}, false, ErrReplacementStale
		}
		digest := observation.SHA256
		trustedStorage, trustErr := s.trustedReplacementStorageIdentity(ctx, root, f.ID, f.FileGeneration)
		if trustErr != nil || trustedStorage != observation.StorageIdentity {
			return sqlite.ReplacementOperation{}, false, ErrReplacementStale
		}
		if observation.State == "absent" {
			parentIdentity, absenceErr := observeMissingReplacementPath(root, f.Path, observation.StorageIdentity)
			if absenceErr != nil || parentIdentity != observation.FileIdentity || observation.SHA256 != "" || observation.Size != 0 {
				return sqlite.ReplacementOperation{}, false, ErrReplacementStale
			}
		} else {
			if observation.SHA256 == "" || validateReplacementObservation(root, f.Path, observation) != nil {
				return sqlite.ReplacementOperation{}, false, ErrReplacementStale
			}
		}
		containsSelected, containsUnselected := req.Scope == "download", false
		for _, epID := range f.EpisodeIDs {
			if req.Scope == "download" || selected[epID] {
				containsSelected = true
				covered[epID] = true
			} else if req.Scope == "episodes" {
				containsUnselected = true
			}
		}
		if req.Scope == "episodes" && !containsSelected {
			return sqlite.ReplacementOperation{}, false, replacementInvalid("observed file does not cover a selected episode")
		}
		if f.ID == req.SourceFileID {
			sourceRelease, sourceIndexer, sourceGUID, sourceIndexerID = f.SourceRelease, f.SourceIndexer, f.SourceGUID, f.SourceIndexerID
			sourceGeneration, sourceTransfer = f.SourceGeneration, f.SourceTransfer
			sourceIdentitySet = true
		}
		isTargetSource := sameReplacementSource(anchorFile, f)
		if isTargetSource && sourceIdentitySet && (sourceRelease != f.SourceRelease || sourceIndexer != f.SourceIndexer || sourceGUID != f.SourceGUID || sourceIndexerID != f.SourceIndexerID || sourceGeneration != f.SourceGeneration || sourceTransfer != f.SourceTransfer) {
			return sqlite.ReplacementOperation{}, false, replacementInvalid("observed files do not share one source release identity")
		}
		disposition := "selected"
		if containsUnselected || !isTargetSource {
			disposition = "preserved"
		}
		state := "protected"
		if req.Intent == "replace_bad" && (disposition == "selected" || (isTargetSource && containsSelected && containsUnselected)) {
			state = "pending"
		} else if req.Intent == "replace_quality" && isTargetSource && containsSelected && containsUnselected {
			// Keep the shared file for its healthy sibling, but detach the
			// selected episode only after the replacement placement commits.
			state = "pending"
		}
		opFiles = append(opFiles, sqlite.ReplacementFile{MediaFileID: f.ID, FileGeneration: f.FileGeneration, SourceGeneration: f.SourceGeneration, ObservationID: observation.ID, FileIdentity: observation.FileIdentity, StorageIdentity: observation.StorageIdentity, Size: observation.Size, Path: f.Path, SHA256: digest, EpisodeIDs: append([]int64(nil), f.EpisodeIDs...), Disposition: disposition, RemovalState: state})
	}
	// Bind the action to the complete current membership of the selected
	// source scope. An omitted duplicate file must not survive a bad-source
	// action, while files outside the reviewed source remain protected and
	// force a refreshed review instead of being silently swept in.
	for _, current := range files {
		if current.CopyID != req.CopyID {
			continue
		}
		sameSource := sameReplacementSource(anchorFile, current)
		intersectsSelected := false
		if req.Scope == "episodes" {
			for _, episodeID := range current.EpisodeIDs {
				if selected[episodeID] {
					intersectsSelected = true
					break
				}
			}
		}
		required := (req.Scope == "download" && sameSource) || (req.Scope == "episodes" && intersectsSelected)
		if required && !observed[current.ID] {
			return sqlite.ReplacementOperation{}, false, fmt.Errorf("%w: replacement review omitted current source file %d; refresh the selected scope", ErrReplacementStale, current.ID)
		}
	}
	if req.Scope == "episodes" {
		for epID := range selected {
			if !covered[epID] {
				return sqlite.ReplacementOperation{}, false, fmt.Errorf("selected episode %d is not covered by the observed source files", epID)
			}
		}
	}
	if req.Scope == "download" {
		for epID := range covered {
			selected[epID] = true
		}
	}
	if *req.BlacklistSourceRelease && (sourceGUID == "" || sourceIndexerID == 0) {
		return sqlite.ReplacementOperation{}, false, replacementInvalid("this source has no stable indexer GUID; it can be excluded for this request but cannot be safely globally blacklisted")
	}
	var opID [16]byte
	if _, err := rand.Read(opID[:]); err != nil {
		return sqlite.ReplacementOperation{}, false, err
	}
	state := "accepted"
	selectedEpisodeIDs := make([]int64, 0, len(selected))
	for id := range selected {
		selectedEpisodeIDs = append(selectedEpisodeIDs, id)
	}
	sort.Slice(selectedEpisodeIDs, func(i, j int) bool { return selectedEpisodeIDs[i] < selectedEpisodeIDs[j] })
	op := sqlite.ReplacementOperation{ID: hex.EncodeToString(opID[:]), RequestID: req.RequestID, Fingerprint: fingerprintHex, State: state,
		Intent: req.Intent, Scope: req.Scope, MediaItemID: req.MediaItemID, CopyID: req.CopyID, SourceGeneration: sourceGeneration,
		SourceTransfer: sourceTransfer, SourceRelease: sourceRelease, SourceIndexer: sourceIndexer, SourceGUID: sourceGUID,
		SourceIndexerID: sourceIndexerID, BlacklistSourceRelease: *req.BlacklistSourceRelease, ReasonCode: req.ReasonCode, Reason: req.Reason,
		EpisodeIDs: selectedEpisodeIDs, CreatedAt: time.Now()}
	// Recheck only metadata under the short shared storage/import-target
	// authority window. The observation worker performed the expensive hash;
	// this catches a same-path replacement between review and click without
	// making the API request hash a large media file.
	s.importTargetMu.Lock()
	decisionCtx, release := s.lockStorageDecision(ctx)
	defer func() {
		release()
		s.importTargetMu.Unlock()
	}()
	for _, f := range opFiles {
		observation, e := s.db.GetReplacementFileObservation(decisionCtx, f.ObservationID)
		if e != nil {
			return sqlite.ReplacementOperation{}, false, ErrReplacementStale
		}
		if observation.State == "absent" {
			trustedStorage, trustErr := s.trustedReplacementStorageIdentity(decisionCtx, root, f.MediaFileID, f.FileGeneration)
			parentIdentity, absenceErr := observeMissingReplacementPath(root, f.Path, trustedStorage)
			if trustErr != nil || absenceErr != nil || trustedStorage != observation.StorageIdentity || parentIdentity != observation.FileIdentity {
				return sqlite.ReplacementOperation{}, false, ErrReplacementStale
			}
		} else if validateReplacementObservation(root, f.Path, observation) != nil {
			return sqlite.ReplacementOperation{}, false, ErrReplacementStale
		}
	}
	return s.db.CreateReplacementOperation(decisionCtx, op, opFiles)
}

func validateReplacementObservation(root, path string, observation sqlite.ReplacementFileObservation) error {
	storage, err := replacementStorageIdentity(root)
	if err != nil || storage != observation.StorageIdentity {
		return ErrReplacementStale
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() != observation.Size || deliveryFileGeneration(info) != observation.FilesystemGeneration ||
		deliveryFileStableIdentity(info) != observation.FileIdentity {
		return ErrReplacementStale
	}
	return nil
}

func sameReplacementSource(anchor, candidate domain.MediaFile) bool {
	if anchor.SourceGeneration == "" && anchor.SourceTransfer == "" {
		return candidate.ID == anchor.ID
	}
	return candidate.SourceGeneration == anchor.SourceGeneration && candidate.SourceTransfer == anchor.SourceTransfer
}

func (s *Service) GetReplacement(ctx context.Context, id string) (sqlite.ReplacementOperation, []sqlite.ReplacementFile, error) {
	op, err := s.db.GetReplacementOperation(ctx, id)
	if err != nil {
		return op, nil, err
	}
	files, err := s.db.ListReplacementFiles(ctx, id)
	return op, files, err
}

func (s *Service) ReplacementFiles(ctx context.Context, id string) ([]sqlite.ReplacementFile, error) {
	return s.db.ListReplacementFiles(ctx, id)
}

func (s *Service) ListLibraryLifecycleReview(ctx context.Context, itemID int64) ([]sqlite.ReplacementOperation, []sqlite.LibraryReviewFlag, error) {
	if itemID <= 0 {
		return nil, nil, replacementInvalid("media item id must be positive")
	}
	if _, err := s.db.GetMediaItemFull(ctx, itemID); err != nil {
		return nil, nil, err
	}
	operations, err := s.db.ListReplacementOperationsForItem(ctx, itemID, 50)
	if err != nil {
		return nil, nil, err
	}
	flags, err := s.db.ListOpenMediaFileReviewFlags(ctx, itemID)
	return operations, flags, err
}

func (s *Service) ResolveLibraryReviewFlag(ctx context.Context, itemID, flagID int64, fileGeneration string) error {
	if itemID <= 0 || flagID <= 0 || strings.TrimSpace(fileGeneration) == "" {
		return replacementInvalid("item, review flag, and file generation are required")
	}
	return s.db.ResolveMediaFileReviewFlag(ctx, itemID, flagID, fileGeneration)
}

func (s *Service) ReportLibraryFile(ctx context.Context, itemID, fileID int64, fileGeneration, reasonCode, reason string, evidence json.RawMessage) error {
	rec, err := s.db.GetFileQuality(ctx, fileID)
	if err != nil {
		return err
	}
	if rec.MediaItemID != itemID {
		return ErrNotFound
	}
	files, err := s.db.ListFilesForItem(ctx, itemID)
	if err != nil {
		return err
	}
	matched := false
	for _, file := range files {
		if file.ID == fileID && file.FileGeneration == fileGeneration {
			matched = true
			break
		}
	}
	if !matched {
		return ErrReplacementStale
	}
	if len(evidence) == 0 {
		evidence = json.RawMessage(`{}`)
	}
	if !json.Valid(evidence) {
		return replacementInvalid("evidence must be valid JSON")
	}
	return s.db.CreateLibraryReviewFlag(ctx, itemID, fileID, fileGeneration, reasonCode, reason, string(evidence))
}

func (s *Service) StartReplacementFileObservation(ctx context.Context, itemID, fileID int64, fileGeneration, requestID string) (sqlite.ReplacementFileObservation, bool, error) {
	if strings.TrimSpace(requestID) == "" || strings.TrimSpace(fileGeneration) == "" {
		return sqlite.ReplacementFileObservation{}, false, replacementInvalid("requestId and fileGeneration are required")
	}
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return sqlite.ReplacementFileObservation{}, false, err
	}
	files, err := s.db.ListFilesForItem(ctx, itemID)
	if err != nil {
		return sqlite.ReplacementFileObservation{}, false, err
	}
	var selected *domain.MediaFile
	for i := range files {
		if files[i].ID == fileID {
			selected = &files[i]
			break
		}
	}
	if selected == nil || selected.FileGeneration != fileGeneration {
		return sqlite.ReplacementFileObservation{}, false, ErrReplacementStale
	}
	root := item.Path
	if selected.CopyID != 0 {
		cp, e := s.db.GetMediaCopy(ctx, itemID, selected.CopyID)
		if e != nil {
			return sqlite.ReplacementFileObservation{}, false, e
		}
		if cp.Path != "" {
			root = cp.Path
		}
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(selected.Path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return sqlite.ReplacementFileObservation{}, false, replacementInvalid("file is outside the selected copy folder")
	}
	if err = rejectSymlinks(selected.Path, false); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return sqlite.ReplacementFileObservation{}, false, ErrReplacementStale
		}
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return sqlite.ReplacementFileObservation{}, false, err
	}
	storageIdentity, err := replacementStorageIdentity(root)
	if err != nil {
		return sqlite.ReplacementFileObservation{}, false, err
	}
	return s.db.StartReplacementFileObservation(ctx, sqlite.ReplacementFileObservation{ID: hex.EncodeToString(random[:]), RequestID: requestID, MediaItemID: itemID, MediaFileID: fileID, FileGeneration: fileGeneration, StorageIdentity: storageIdentity})
}

func (s *Service) GetReplacementFileObservation(ctx context.Context, id string) (sqlite.ReplacementFileObservation, error) {
	return s.db.GetReplacementFileObservation(ctx, id)
}

// observeMissingReplacementPath proves only that the exact filename is absent
// under an accessible, non-symlink parent on the same mounted storage. It
// never treats an unavailable root or parent as absence.
func observeMissingReplacementPath(root, path, expectedStorage string) (string, error) {
	storage, err := replacementStorageIdentity(root)
	if err != nil || storage == "" || expectedStorage == "" || storage != expectedStorage {
		return "", fmt.Errorf("replacement storage identity is unavailable or changed")
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("missing replacement path escaped its root")
	}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		return "", err
	}
	defer func() { _ = anchor.Close() }()
	parent, err := openDeliveryDirectoryNoSymlinks(anchor, filepath.Dir(rel))
	if err != nil {
		return "", err
	}
	defer func() { _ = parent.Close() }()
	info, err := parent.Stat(".")
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("replacement file parent is unavailable")
	}
	identity := deliveryDirectoryObjectIdentity(info)
	if identity == "" {
		return "", fmt.Errorf("replacement file parent identity is unavailable")
	}
	if _, err = parent.Lstat(filepath.Base(rel)); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return "", fmt.Errorf("replacement file is present")
		}
		return "", err
	}
	storageAfter, err := replacementStorageIdentity(root)
	if err != nil || storageAfter != storage {
		return "", fmt.Errorf("replacement storage changed during absence review")
	}
	return identity, nil
}

// trustedReplacementStorageIdentity returns a durable root pin, or recovers a
// pin only from a previous ready observation of this exact file generation.
// It never adopts an identity observed while the file is absent.
func (s *Service) trustedReplacementStorageIdentity(ctx context.Context, root string, fileID int64, fileGeneration string) (string, error) {
	root = filepath.Clean(root)
	identity, err := s.db.GetReplacementStorageIdentity(ctx, root)
	if err == nil {
		return identity, nil
	}
	if !errors.Is(err, sqlite.ErrNotFound) {
		return "", err
	}
	identity, observationID, err := s.db.PriorVerifiedReplacementStorageIdentity(ctx, fileID, fileGeneration)
	if err != nil {
		return "", err
	}
	if err = s.db.PinReplacementStorageIdentity(ctx, root, identity, observationID); err != nil {
		return "", err
	}
	return identity, nil
}
