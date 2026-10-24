package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// ScannedFile is one media file found under a manual-import path, with the
// guesses Monarr would make about it. The UI shows this so a manual import
// is a preview-then-confirm, not a leap of faith.
type ScannedFile struct {
	Path             string `json:"path"`
	Name             string `json:"name"`
	Size             int64  `json:"size"`
	Kind             string `json:"kind"` // "video" | "book"
	Quality          string `json:"quality"`
	Season           int    `json:"season"`   // -1 when not a series file
	Episodes         []int  `json:"episodes"` // empty for movies/books
	SnapshotIdentity string `json:"snapshotIdentity,omitempty"`
}

// ScanImportPath lists the media files Monarr can see under a path — the
// preview for a manual import, and the direct answer to "why didn't my
// download import": if this returns an error or nothing, that's why.
func (s *Service) ScanImportPath(ctx context.Context, path string) ([]ScannedFile, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("no path given")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("path not accessible: %w", err)
	}

	var paths []string
	isMedia := func(p string) bool { return filename.IsVideo(p) || filename.IsBook(p) }
	if !info.IsDir() {
		if isMedia(path) {
			paths = []string{path}
		}
	} else {
		_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isMedia(p) {
				return nil
			}
			if strings.Contains(strings.ToLower(filepath.Base(p)), "sample") {
				return nil
			}
			paths = append(paths, p)
			return nil
		})
	}

	out := make([]ScannedFile, 0, len(paths))
	for _, p := range paths {
		base := filepath.Base(p)
		identityInfo, statErr := os.Lstat(p)
		if statErr != nil || identityInfo.Mode()&os.ModeSymlink != 0 || !identityInfo.Mode().IsRegular() {
			continue
		}
		sf := ScannedFile{Path: p, Name: base, Size: sizeOf(p), Season: -1, SnapshotIdentity: completedReviewFileIdentity(p, identityInfo)}
		if filename.IsBook(p) {
			sf.Kind = "book"
			if ext := filename.BookQualitySource(p); ext != "" {
				sf.Quality = ext
			}
		} else {
			sf.Kind = "video"
			parsed := parser.Parse(base)
			sf.Quality = parsed.Quality.Display()
			if fx, ok := filename.Extract(base); ok {
				sf.Season, sf.Episodes = fx.Season, fx.Episodes
			} else if len(parsed.Episodes) > 0 {
				sf.Season, sf.Episodes = parsed.Season, parsed.Episodes
			}
		}
		out = append(out, sf)
	}
	return out, nil
}

// ManualImportRequest points Monarr at a path and a target: import whatever
// media is there into this item (and copy). DownloadID ties it to a stuck
// queue row so that row is marked imported (or failed) by the attempt.
type ManualImportRequest struct {
	Path                    string
	Paths                   []string // exact files selected from the scan; nil = whole Path
	MediaItemID             int64
	CopyID                  int64
	DownloadID              int64 // 0 = not tied to a queue row
	ReviewLegacy            bool  // explicit per-file approval for a fenced pre-delivery payload
	ReviewLegacyFingerprint string
	ReviewLegacyFiles       []CompletedFileSelection
}

func legacyReviewTargetIdentity(ctx context.Context, db *sqlite.DB, itemID, copyID int64) (string, error) {
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return "", err
	}
	path := item.Path
	profileID := item.QualityProfileID
	bookType := item.BookType
	rootFolderID := item.RootFolderID
	monitored := item.Monitored
	if copyID != 0 {
		copy, err := db.GetMediaCopy(ctx, itemID, copyID)
		if err != nil {
			return "", err
		}
		if copy.Path != "" {
			path = copy.Path
		}
		profileID = copy.QualityProfileID
		bookType = copy.BookType
		rootFolderID = copy.RootFolderID
		monitored = copy.Monitored
	}
	value := fmt.Sprintf("legacy-review-target-v2\n%d\n%d\n%s\n%s\n%d\n%d\n%s\n%t\n%s\n%d\n%s\n%d\n%s\n",
		itemID, copyID, item.Kind, filepath.Clean(path), profileID, rootFolderID, bookType, monitored,
		item.Source, item.IDs.TMDB, item.IDs.IMDB, item.IDs.TVDB, item.IDs.ISBN13+"\n"+item.IDs.OLID+"\n"+item.IDs.ASIN)
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:]), nil
}

// ManualImportDefaultPath returns the completed-download folder Monarr sees.
// A configured remote-path mapping is authoritative; otherwise a recent
// completed payload teaches us the folder. The standard deployment path is
// only the final fallback.
func (s *Service) ManualImportDefaultPath(ctx context.Context) string {
	if clients, err := s.db.ListDownloadClients(ctx); err == nil {
		for _, client := range clients {
			if !client.Enabled {
				continue
			}
			for _, mapping := range client.PathMappings {
				if strings.TrimSpace(mapping.Local) != "" {
					return filepath.Clean(mapping.Local)
				}
			}
		}
	}
	if rows, err := s.db.ListRecentDownloads(ctx); err == nil {
		for _, dl := range rows {
			p := dl.ImportPath
			if p == "" {
				p = dl.SavePath
			}
			if p != "" {
				return filepath.Dir(filepath.Clean(p))
			}
		}
	}
	return "/pool/downloads"
}

// ManualImport imports the media at a path into a chosen item/copy — the
// escape hatch when automation can't resolve the payload. When tied to a
// download row it records the outcome on that row's handoff trace.
func (s *Service) ManualImport(ctx context.Context, req ManualImportRequest) (ImportResult, error) {
	dl := sqlite.Download{
		MediaItemID: req.MediaItemID, CopyID: req.CopyID,
		ReleaseTitle: filepath.Base(req.Path), ImportPath: req.Path,
	}
	if req.DownloadID != 0 {
		existing, err := s.db.GetDownload(ctx, req.DownloadID)
		if err != nil {
			return ImportResult{}, err
		}
		dl = existing
		if req.MediaItemID != 0 {
			dl.MediaItemID = req.MediaItemID
		}
		dl.CopyID = req.CopyID
		dl.ImportPath = req.Path
	}
	if dl.MediaItemID == 0 {
		return ImportResult{}, fmt.Errorf("no target item chosen")
	}

	// manual=true: the user pointed at this folder and pressed Import. The
	// profile gates AUTOMATION; telling a person "does not improve on" after
	// they explicitly asked is the same mistake as gating a manual grab.
	result, err := s.importDownloadFiles(ctx, dl, req.Path, req.Paths, true)
	files := result.Imported
	if req.DownloadID != 0 {
		row, gerr := s.db.GetDownload(ctx, req.DownloadID)
		if gerr == nil {
			row.ImportPath = req.Path
			if err != nil {
				s.advance(ctx, &row, "failed", row.Progress, err.Error(), stepFailed,
					"manual import failed: "+err.Error())
			} else {
				s.advance(ctx, &row, "imported", 1, "", stepImported,
					fmt.Sprintf("manually imported %d file(s) from %s", files, req.Path))
			}
		}
	}
	if err != nil {
		return result, err
	}
	s.InvalidateWanted()
	return result, nil
}

// QueueManualImport validates a manual selection, records it as a durable
// Activity job, and returns as soon as the bounded importer accepts it. The
// HTTP request must never stay open while an 18 GB file is copied.
func (s *Service) QueueManualImport(ctx context.Context, req ManualImportRequest) (int64, error) {
	if strings.TrimSpace(req.Path) == "" || req.MediaItemID == 0 {
		return 0, fmt.Errorf("path and target item are required")
	}
	id := req.DownloadID
	if req.ReviewLegacy {
		if id != 0 {
			return 0, fmt.Errorf("legacy review approval must create a separate manual import record")
		}
		if req.ReviewLegacyFingerprint == "" || len(req.ReviewLegacyFiles) == 0 || len(req.ReviewLegacyFiles) != len(req.Paths) {
			return 0, fmt.Errorf("legacy review requires the scanned inventory fingerprint and exact selected file identities")
		}
		if err := s.ScanCompleted(ctx); err != nil {
			return 0, fmt.Errorf("refresh legacy review inventory: %w", err)
		}
	}
	if id != 0 {
		unlock := s.lockDownload(id)
		defer unlock()
	}
	item, err := s.db.GetMediaItemFull(ctx, req.MediaItemID)
	if err != nil {
		return 0, err
	}
	isMedia := filename.IsVideo
	if item.Kind == "book" {
		isMedia = filename.IsBook
	}
	var admittedPaths []string
	if req.Paths == nil {
		files, scanErr := collectFiles(req.Path, isMedia)
		if scanErr != nil {
			return 0, scanErr
		}
		if len(files) == 0 {
			return 0, fmt.Errorf("no media files in %s", req.Path)
		}
		admittedPaths = files
	} else {
		if len(req.Paths) == 0 {
			return 0, fmt.Errorf("no files selected")
		}
		admittedPaths, err = selectedFiles(req.Path, req.Paths, isMedia)
		if err != nil {
			return 0, err
		}
	}
	admittedGenerations := make(map[string]string, len(admittedPaths))
	for _, path := range admittedPaths {
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			if statErr != nil {
				return 0, statErr
			}
			return 0, fmt.Errorf("selected path is not a regular file: %s", path)
		}
		generation := deliveryFileGeneration(info)
		if generation == "" {
			return 0, fmt.Errorf("selected file identity is unavailable: %s", path)
		}
		admittedGenerations[path] = generation
	}
	if req.ReviewLegacy {
		if len(req.Paths) == 0 {
			return 0, fmt.Errorf("legacy review requires an explicit file selection")
		}
		for i, path := range req.Paths {
			if i >= len(req.ReviewLegacyFiles) || path != req.ReviewLegacyFiles[i].Path {
				return 0, fmt.Errorf("legacy review file identities must match the exact selected paths")
			}
		}
		legacy, reviewErr := s.db.LegacyCleanupReviewPath(ctx, req.Path)
		if reviewErr != nil {
			return 0, reviewErr
		}
		if !legacy {
			return 0, fmt.Errorf("payload is not in the legacy review cohort")
		}
		if err := s.validateLegacyReviewSnapshot(ctx, req.Path, req.ReviewLegacyFingerprint, req.ReviewLegacyFiles); err != nil {
			return 0, err
		}
	}

	// Serialize final payload-cohort validation and durable admission with
	// cleanup/deletion. Import itself runs only after both shared locks release.
	dispatchCtx := ctx
	s.importTargetMu.Lock()
	decisionCtx, releaseStorage := s.lockStorageDecision(ctx)
	locked := true
	unlockAdmission := func() {
		if locked {
			locked = false
			releaseStorage()
			s.importTargetMu.Unlock()
		}
	}
	defer unlockAdmission()
	ctx = decisionCtx
	// Recheck the exact file selection inside the same target/storage authority
	// section as the durable row and handoff write. A concurrent Resolve delete
	// may win while this request waits for those locks.
	if _, selectionErr := selectedFiles(req.Path, admittedPaths, isMedia); selectionErr != nil {
		unlockAdmission()
		return 0, selectionErr
	}
	for path, expectedGeneration := range admittedGenerations {
		info, statErr := os.Lstat(path)
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || deliveryFileGeneration(info) != expectedGeneration {
			unlockAdmission()
			if statErr != nil {
				return 0, statErr
			}
			return 0, fmt.Errorf("selected file changed before import admission: %s", path)
		}
	}
	if req.ReviewLegacy {
		legacy, reviewErr := s.db.LegacyCleanupReviewPath(ctx, req.Path)
		if reviewErr != nil || !legacy {
			unlockAdmission()
			if reviewErr != nil {
				return 0, reviewErr
			}
			return 0, fmt.Errorf("payload is no longer in the legacy review cohort")
		}
		for _, selected := range req.ReviewLegacyFiles {
			info, statErr := os.Lstat(selected.Path)
			if statErr != nil || info.Mode()&os.ModeSymlink != 0 || completedReviewFileIdentity(selected.Path, info) != selected.SnapshotIdentity {
				unlockAdmission()
				return 0, fmt.Errorf("legacy review file changed before admission: %s", selected.Path)
			}
		}
	} else {
		legacy, reviewErr := s.db.LegacyCleanupReviewPath(ctx, req.Path)
		if reviewErr != nil || legacy {
			unlockAdmission()
			if reviewErr != nil {
				return 0, reviewErr
			}
			return 0, fmt.Errorf("this payload is fenced for explicit legacy review; select exact files from its current inventory")
		}
	}
	var dl sqlite.Download
	if id == 0 {
		dl = sqlite.Download{
			MediaItemID: req.MediaItemID, CopyID: req.CopyID,
			ReleaseTitle: filepath.Base(filepath.Clean(req.Path)),
			Protocol:     "manual", State: "downloaded", ImportPath: req.Path,
		}
	} else {
		dl, err = s.db.GetDownload(ctx, id)
		if err != nil {
			return 0, err
		}
		if s.ImportRunning(id) {
			return 0, fmt.Errorf("import is already running")
		}
	}
	dl.MediaItemID = req.MediaItemID
	dl.CopyID = req.CopyID
	dl.ImportPath = req.Path
	var targetIdentity string
	if req.ReviewLegacy {
		targetIdentity, err = legacyReviewTargetIdentity(ctx, s.db, req.MediaItemID, req.CopyID)
		if err != nil {
			return 0, err
		}
	}
	dl.Error = ""
	dl.State = "downloaded"
	selection := fmt.Sprintf("%d selected file(s)", len(admittedPaths))
	if req.Paths == nil {
		selection = "all media files"
	}
	step := stepManualQueued
	detailPrefix := ""
	if req.ReviewLegacy {
		step = stepLegacyReviewImportQueued
		detailPrefix = "explicit legacy review: "
	}
	dl.Handoff = append(dl.Handoff, sqlite.HandoffEntry{
		Step: step, At: time.Now().UnixMilli(),
		Detail:            detailPrefix + selection + " waiting for an import worker",
		Paths:             append([]string(nil), admittedPaths...),
		ReviewFingerprint: req.ReviewLegacyFingerprint,
		ReviewPath:        req.Path,
		ReviewFiles:       handoffReviewFiles(req.ReviewLegacyFiles),
		TargetMediaItemID: req.MediaItemID, TargetCopyID: req.CopyID, TargetIdentity: targetIdentity,
	})
	dl, err = s.db.CommitManualImportAdmission(ctx, dl)
	if err != nil {
		return 0, err
	}
	id = dl.ID
	unlockAdmission()
	ctx = dispatchCtx
	if !s.enqueueImport(ctx, dl, ports.ClientConfig{}) {
		s.failImport(ctx, &dl, "import queue is full; try again shortly")
		return 0, fmt.Errorf("import queue is full; try again shortly")
	}
	return id, nil
}

func handoffReviewFiles(files []CompletedFileSelection) []sqlite.HandoffFileSnapshot {
	out := make([]sqlite.HandoffFileSnapshot, 0, len(files))
	for _, file := range files {
		out = append(out, sqlite.HandoffFileSnapshot{Path: file.Path, SnapshotIdentity: file.SnapshotIdentity})
	}
	return out
}

func (s *Service) validateLegacyReviewSnapshot(ctx context.Context, path, fingerprint string, files []CompletedFileSelection) error {
	inventory, err := s.CompletedInventory(ctx)
	if err != nil {
		return err
	}
	canonical := canonicalCompletedPath(path)
	var entry *CompletedEntry
	for i := range inventory.Roots {
		for j := range inventory.Roots[i].Entries {
			if canonicalCompletedPath(inventory.Roots[i].Entries[j].Path) == canonical {
				entry = &inventory.Roots[i].Entries[j]
				break
			}
		}
	}
	if entry == nil || (!entry.LegacyReview && !hasLegacyReviewReceipt(entry.Receipts)) {
		return fmt.Errorf("payload is no longer in the legacy review inventory")
	}
	if fingerprint == "" || entry.Fingerprint != fingerprint {
		return fmt.Errorf("legacy payload inventory changed after review; refresh and review it again")
	}
	current := make(map[string]string, len(entry.ReviewFiles))
	for _, file := range entry.ReviewFiles {
		current[file.Path] = file.SnapshotIdentity
	}
	seen := map[string]bool{}
	for _, selected := range files {
		if selected.Path == "" || selected.SnapshotIdentity == "" || seen[selected.Path] || current[selected.Path] != selected.SnapshotIdentity || !within(canonical, selected.Path) {
			return fmt.Errorf("selected legacy file identity is absent or changed; refresh and review it again")
		}
		seen[selected.Path] = true
		info, err := os.Lstat(selected.Path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || completedReviewFileIdentity(selected.Path, info) != selected.SnapshotIdentity {
			return fmt.Errorf("selected legacy file changed after review; refresh and review it again")
		}
	}
	return nil
}

func hasLegacyReviewReceipt(receipts []sqlite.CompletedReceipt) bool {
	for _, receipt := range receipts {
		if receipt.LegacyReview {
			return true
		}
	}
	return false
}
