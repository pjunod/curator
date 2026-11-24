package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

const scanReportKey = "last_scan_report"

var errScanIncomplete = errors.New("library scope scan incomplete; retained existing file records")

// ScanCompleted is published on the bus after every reconcile.
type ScanCompleted struct {
	ItemsScanned  int   `json:"itemsScanned"`
	FilesLinked   int   `json:"filesLinked"`
	FilesRemoved  int   `json:"filesRemoved"`
	UnmatchedDirs int   `json:"unmatchedDirs"`
	DurationMS    int64 `json:"durationMs"`
}

// EventType implements bus.Event.
func (ScanCompleted) EventType() string { return "library.scan.completed" }

// UnmatchedDir is a top-level directory in a root folder that no library
// item claims — an adoption candidate the UI offers to match.
type UnmatchedDir struct {
	RootFolderID int64  `json:"rootFolderId"`
	Path         string `json:"path"`
	Name         string `json:"name"`
}

// MissingItem is an unavailable folder for an item with previously recorded files.
// An uncreated destination awaiting its first download is not a folder problem.
type MissingItem struct {
	ID     int64            `json:"id"`
	Kind   domain.MediaKind `json:"kind"`
	Title  string           `json:"title"`
	Path   string           `json:"path"`
	Reason string           `json:"reason"`
}

// Report is the persisted result of the last reconcile.
type Report struct {
	ScannedAt    time.Time `json:"scannedAt"`
	RootsScanned int       `json:"rootsScanned"`
	ItemsScanned int       `json:"itemsScanned"`
	FilesLinked  int       `json:"filesLinked"`
	FilesRemoved int       `json:"filesRemoved"`
	// SkippedDirs and IgnoredDirs are counted rather than listed, so the
	// user can see the sweep excluded something without the excluded
	// things becoming a second list to read. Silent truncation reads as
	// "there was nothing there".
	SkippedDirs int `json:"skippedDirs"`
	IgnoredDirs int `json:"ignoredDirs"`
	// UnmatchedTotal is the true count; UnmatchedDirs may be a capped
	// prefix of it when the API is asked for a page rather than the lot.
	UnmatchedTotal int            `json:"unmatchedTotal"`
	UnmatchedDirs  []UnmatchedDir `json:"unmatchedDirs"`
	// MissingPaths is kept for readers that only want the paths;
	// MissingItems is the same set with the identity needed to act on it.
	MissingPaths []string      `json:"missingPaths"`
	MissingItems []MissingItem `json:"missingItems"`
}

// Scan reconciles the library against disk: walks every item's folder,
// upserting and episode-linking the files found and pruning records whose
// files vanished; then walks each root folder to surface unclaimed
// directories as adoption candidates. Disk is treated as a source of truth
// to reconcile against, never assumed exclusively owned (blueprint §5.1).
func (s *Service) Scan(ctx context.Context) (Report, error) {
	start := time.Now()
	report := Report{
		ScannedAt:     start,
		UnmatchedDirs: []UnmatchedDir{},
		MissingPaths:  []string{},
		MissingItems:  []MissingItem{},
	}

	items, err := s.db.ListMediaItems(ctx, "")
	if err != nil {
		return report, err
	}
	claimed := map[string]bool{}
	// A Re-match moving files holds both titles' leases, which a scan does
	// not take; it leaves those titles, and the folder the move chose, alone
	// until the move is over (ADR 0027), so a file mid-move is neither pruned
	// nor adopted twice. The next scan picks them up.
	moving, movingFolders, err := s.db.OpenRematchScope(ctx)
	if err != nil {
		return report, err
	}
	for _, f := range movingFolders {
		claimed[filepath.Clean(f)] = true
	}
	presenceRoots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return report, err
	}
	for _, item := range items {
		if err := s.db.InvalidatePresence(ctx, item.ID); err != nil {
			return report, err
		}
		// Copies with their own folders are claimed and scanned too.
		copies, err := s.db.ListMediaCopies(ctx, item.ID)
		if err != nil {
			return report, err
		}
		for _, cp := range copies {
			if cp.Path != "" {
				claimed[cp.Path] = true
			}
		}
		if item.Path == "" {
			continue
		}
		claimed[item.Path] = true
		// First, and re-read now: a move that started after the snapshot
		// above still owns the title, and a folder that looks missing
		// mid-move is not missing.
		if s.rematchOpen(ctx, moving, item.ID) {
			s.log.Info("scan: skipping a title a re-match is moving files for", "title", item.Title)
			continue
		}
		if item.RootFolderID != 0 {
			rootPath := ""
			for _, root := range presenceRoots {
				if root.ID == item.RootFolderID {
					rootPath = root.Path
					break
				}
			}
			if _, _, rootErr := s.checkPresenceRoot(ctx, item.RootFolderID, rootPath); rootErr != nil && !errors.Is(rootErr, errPresenceUnsupported) {
				s.log.Warn("scan: library root unavailable; keeping existing records", "title", item.Title, "err", rootErr)
				continue
			}
		}
		if reason := folderProblem(item.Path); reason != "" {
			missing, err := s.missingItem(ctx, item, reason)
			if err != nil {
				return report, err
			}
			if missing != nil {
				report.MissingPaths = append(report.MissingPaths, item.Path)
				report.MissingItems = append(report.MissingItems, *missing)
			}
			if reason == "missing" {
				if e := s.verifyPresence(ctx, item.ID, true); e != nil {
					s.log.Debug("scan: presence unknown", "title", item.Title, "err", e)
				}
			}
			continue
		}
		linked, removed, err := s.scanItem(ctx, item, copies)
		if errors.Is(err, errRematchStarted) {
			s.log.Info("scan: a re-match started on a title mid-scan; left alone", "title", item.Title)
			continue
		}
		if err != nil {
			s.log.Warn("scan: item failed", "title", item.Title, "err", err)
			continue
		}
		if e := s.VerifyPresence(ctx, item.ID); e != nil {
			s.log.Debug("scan: presence unknown", "title", item.Title, "err", e)
		}
		report.ItemsScanned++
		report.FilesLinked += linked
		report.FilesRemoved += removed
	}

	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return report, err
	}
	// A move that started during the item pass chose a folder the first
	// read did not know: re-read before the unmatched-folders pass so it is
	// not offered for adoption.
	if _, folders, err := s.db.OpenRematchScope(ctx); err == nil {
		for _, f := range folders {
			claimed[filepath.Clean(f)] = true
		}
	}
	// Two exclusion mechanisms, and they answer different questions
	// (ADR 0009 §4): patterns say "never look at things shaped like this",
	// dismissals say "I looked, it is not media, stop asking".
	skip := s.loadSkipMatcher(ctx)
	ignored := map[string]bool{}
	if rows, err := s.db.ListIgnoredPaths(ctx); err == nil {
		for _, ip := range rows {
			ignored[ip.Path] = true
		}
	} else {
		s.log.Warn("scan: could not read ignored paths", "err", err)
	}

	for _, root := range roots {
		entries, err := os.ReadDir(root.Path)
		if err != nil {
			s.log.Warn("scan: root folder unreadable", "path", root.Path, "err", err)
			continue
		}
		report.RootsScanned++
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if skip.skip(e.Name()) {
				report.SkippedDirs++
				continue
			}
			full := filepath.Join(root.Path, e.Name())
			if ignored[full] {
				report.IgnoredDirs++
				continue
			}
			if !claimed[full] {
				report.UnmatchedDirs = append(report.UnmatchedDirs, UnmatchedDir{
					RootFolderID: root.ID, Path: full, Name: e.Name(),
				})
			}
		}
	}

	// Set the true count at the source. It used to be filled in by the API
	// handler when serving, which left the *stored* report saying zero — and
	// anything that later adjusted the count had nothing to adjust.
	report.UnmatchedTotal = len(report.UnmatchedDirs)
	if raw, err := json.Marshal(report); err == nil {
		if err := s.db.SetMeta(ctx, scanReportKey, string(raw)); err != nil {
			s.log.Warn("scan: could not persist report", "err", err)
		}
	}

	elapsed := time.Since(start)
	s.log.Info("scan: complete",
		"items", report.ItemsScanned, "linked", report.FilesLinked,
		"removed", report.FilesRemoved, "unmatched", len(report.UnmatchedDirs),
		"duration", elapsed)
	s.publish(ScanCompleted{
		ItemsScanned:  report.ItemsScanned,
		FilesLinked:   report.FilesLinked,
		FilesRemoved:  report.FilesRemoved,
		UnmatchedDirs: len(report.UnmatchedDirs),
		DurationMS:    elapsed.Milliseconds(),
	})
	return report, nil
}

// errRematchStarted: a Re-match opened on the title while its scan ran.
var errRematchStarted = errors.New("a re-match started on this title")

// rematchOpen says a Re-match owns the title: by the scan's snapshot, or by
// the operations open right now.
func (s *Service) rematchOpen(ctx context.Context, snapshot map[int64]bool, itemID int64) bool {
	if snapshot[itemID] {
		return true
	}
	id, err := s.db.OpenRematchForItem(ctx, itemID)
	return err == nil && id != ""
}

// scanItem syncs one item's folders (the item's own plus each copy's own
// folder): every video file on disk gets a row (episode-linked for series
// via the filename extractor), and rows whose files vanished are pruned.
// Files found in a copy's separate folder are attributed to that copy;
// files in a shared folder keep whatever attribution their row already has
// (the upsert never overwrites copy_id).
func (s *Service) scanItem(ctx context.Context, selected domain.MediaItem, _ []domain.MediaCopy) (linked, removed int, err error) {
	if s.rematchOpen(ctx, nil, selected.ID) {
		return 0, 0, errRematchStarted
	}
	revision, shared, err := s.db.PresenceRevision(ctx, selected.ID)
	if err != nil {
		return 0, 0, err
	}
	item, err := s.db.GetMediaItemFull(ctx, selected.ID)
	if err != nil {
		return 0, 0, err
	}
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return 0, 0, err
	}
	paths := map[int64]string{}
	for _, root := range roots {
		paths[root.ID] = root.Path
	}
	type scope struct {
		id, root int64
		path     string
		before   os.FileInfo
		binding  sqlite.ScanRootBinding
	}
	scopes := []scope{{id: 0, root: item.RootFolderID, path: item.Path}}
	for _, cp := range item.Copies {
		root, path := cp.RootFolderID, cp.Path
		if root == 0 {
			root = item.RootFolderID
		}
		if path == "" {
			path = item.Path
		}
		scopes = append(scopes, scope{id: cp.ID, root: root, path: path})
	}
	prune := map[int64]bool{}
	bindings := []sqlite.ScanRootBinding{}
	for i := range scopes {
		sc := &scopes[i]
		if sc.path != "" {
			if err := validatePresenceScope(paths[sc.root], sc.path); err != nil {
				return 0, 0, err
			}
		}
		for _, file := range item.Files {
			if file.CopyID == sc.id && sc.path != "" && !presenceInside(sc.path, file.Path) {
				return 0, 0, errScanIncomplete
			}
		}
		if sc.root == 0 {
			prune[sc.id] = true
			continue
		} // legacy unassigned-root scanning stays supported
		before, identity, e := s.checkPresenceRoot(ctx, sc.root, paths[sc.root])
		if errors.Is(e, errPresenceUnsupported) {
			prune[sc.id] = true
			continue
		}
		if e != nil {
			return 0, 0, e
		}
		generation, e := s.db.PresenceRootGeneration(ctx, sc.root, paths[sc.root], identity)
		if e != nil {
			return 0, 0, e
		}
		sc.before = before
		sc.binding = sqlite.ScanRootBinding{RootID: sc.root, Path: paths[sc.root], Identity: identity, Generation: generation}
		bindings = append(bindings, sc.binding)
		prune[sc.id] = generation > 0
		if generation == 0 {
			offloaded := item.OffloadedAt > 0
			for _, season := range item.Seasons {
				for _, episode := range season.Episodes {
					offloaded = offloaded || episode.OffloadedAt > 0
				}
			}
			if offloaded {
				return 0, 0, fmt.Errorf("verify this library volume before indexing an offloaded title")
			}
		}
	}
	isMedia := filename.IsVideo
	if item.Kind == domain.KindBook {
		isMedia = filename.IsBook
	}
	old := map[string]domain.MediaFile{}
	for _, f := range item.Files {
		old[f.Path] = f
	}
	found := map[string]sqlite.ScannedLibraryFile{}
	episodeIDs := map[[2]int]int64{}
	for _, season := range item.Seasons {
		for _, ep := range season.Episodes {
			episodeIDs[[2]int{ep.SeasonNumber, ep.EpisodeNumber}] = ep.ID
		}
	}
	walked := map[string]bool{}
	for _, sc := range scopes {
		if sc.path == "" {
			prune[sc.id] = false
			continue
		}
		if walked[sc.path] {
			continue
		}
		walked[sc.path] = true
		if _, e := os.Stat(sc.path); os.IsNotExist(e) {
			continue
		} else if e != nil {
			return 0, 0, errScanIncomplete
		}
		e := filepath.WalkDir(sc.path, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return errScanIncomplete
			}
			if entry.IsDir() || !isMedia(path) {
				return nil
			}
			info, e := entry.Info()
			if e != nil {
				return errScanIncomplete
			}
			if !info.Mode().IsRegular() {
				return errScanIncomplete
			}
			copyID := sc.id
			if prior, exists := old[path]; exists {
				copyID = prior.CopyID
			}
			if item.Kind == domain.KindBook && sc.id == 0 {
				bookType := quality.BookTypeForSource(quality.Source(filename.BookQualitySource(path)))
				copyID = 0
				if bookType != item.BookType {
					for _, cp := range item.Copies {
						if cp.BookType == bookType {
							copyID = cp.ID
							break
						}
					}
				}
			}
			file := sqlite.ScannedLibraryFile{Path: path, Size: info.Size(), CopyID: copyID}
			if item.Kind == domain.KindSeries {
				if eps, ok := filename.Extract(path); ok {
					for _, num := range eps.Episodes {
						if id := episodeIDs[[2]int{eps.Season, num}]; id > 0 {
							file.EpisodeIDs = append(file.EpisodeIDs, id)
						}
					}
				}
			}
			found[path] = file
			return nil
		})
		if e != nil {
			return 0, 0, e
		}
	}
	for _, sc := range scopes {
		if sc.before != nil {
			after, e := os.Stat(sc.binding.Path)
			if e != nil || !os.SameFile(sc.before, after) {
				return 0, 0, errScanIncomplete
			}
			if e = s.db.CheckPresenceRoot(ctx, sc.root, sc.binding.Path, sc.binding.Identity); e != nil {
				return 0, 0, e
			}
		}
	}
	files := make([]sqlite.ScannedLibraryFile, 0, len(found))
	for _, f := range found {
		files = append(files, f)
	}
	probeState := map[string]sqlite.FileQuality{}
	if records, e := s.db.FileQualityRecords(ctx, item.ID); e == nil {
		for _, r := range records {
			probeState[r.Path] = r
		}
	}
	result, err := s.db.ApplyLibraryScan(ctx, item, revision, shared, bindings, prune, files)
	if err != nil {
		return 0, 0, err
	}
	// Probe work uses the reconciled current file. Filename guesses never
	// overwrite a newly imported/measured record if bytes changed meanwhile.

	for path, ff := range found {
		id := result.FileIDs[path]
		if item.Kind == domain.KindBook {
			if src := filename.BookQualitySource(path); src != "" {
				_ = s.db.SetScanFileQuality(ctx, id, item.ID, ff.Size, result.FileGenerations[path], quality.Quality{Source: quality.Source(src)})
			}
		} else {
			if q := parser.Parse(filepath.Base(path)).Quality; q.Resolution != 0 || q.Source != quality.SourceUnknown {
				_ = s.db.SetScanFileQuality(ctx, id, item.ID, ff.Size, result.FileGenerations[path], q)
			}
			if rec, ok := probeState[path]; !ok || rec.NeedsProbe(ff.Size) {
				if e := s.EnqueueProbe(ctx, id); e != nil {
					s.log.Warn("scan: probe could not be queued", "file", id, "err", e)
				}
			}
		}
	}
	return result.Linked, result.Removed, nil
}

// LastScanReport returns the persisted report from the most recent Scan.
// ok is false when no scan has run yet.
//
// The missing-folder list is re-verified on read. It is a claim about the
// filesystem that goes stale the moment the user acts on it — removing the
// entry, or adopting a folder for it — and a list that keeps naming items you
// have already dealt with is worse than no list. Re-checking is affordable
// here precisely because this list is small: it costs one stat per *missing*
// item, not per library item.
func (s *Service) LastScanReport(ctx context.Context) (Report, bool, error) {
	raw, err := s.db.GetMeta(ctx, scanReportKey)
	if err != nil {
		return Report{}, false, nil // no rows = never scanned; other errors also read as absent
	}
	var r Report
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Report{}, false, err
	}
	if err := r.pruneResolvedMissing(ctx, s); err != nil {
		return Report{}, false, err
	}
	return r, true, nil
}

// pruneResolvedMissing drops missing-folder entries that no longer apply:
// the item was removed, or it now points at a folder that exists.
func (r *Report) pruneResolvedMissing(ctx context.Context, s *Service) error {
	// Old reports may contain paths only. Resolve their identities too, so
	// upgrading removes the old false alarms without requiring another scan.
	if len(r.MissingItems) == 0 && len(r.MissingPaths) > 0 {
		items, err := s.db.ListMediaItems(ctx, "")
		if err != nil {
			return err
		}
		paths := map[string]bool{}
		for _, path := range r.MissingPaths {
			paths[path] = true
		}
		for _, item := range items {
			if paths[item.Path] {
				r.MissingItems = append(r.MissingItems, MissingItem{ID: item.ID})
			}
		}
	}
	keptItems := make([]MissingItem, 0, len(r.MissingItems))
	keptPaths := make([]string, 0, len(r.MissingItems))
	for _, m := range r.MissingItems {
		item, err := s.db.GetMediaItemFull(ctx, m.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		missing, err := s.missingItem(ctx, item, folderProblem(item.Path))
		if err != nil {
			return err
		}
		if missing != nil {
			keptItems = append(keptItems, *missing)
			keptPaths = append(keptPaths, missing.Path)
		}
	}
	r.MissingItems = keptItems
	r.MissingPaths = keptPaths
	return nil
}

// IgnoreDir dismisses an adoption candidate for good. Dismissals are keyed
// by exact path and survive rescans — without that, every non-media folder
// under a root is re-offered forever (ADR 0009 §4).
//
// The dismissal is also removed from the persisted report immediately, so
// the list the user is looking at reflects the click without waiting for
// the next scan.
func (s *Service) IgnoreDir(ctx context.Context, path, reason string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("ignored path must be absolute")
	}
	clean := filepath.Clean(path)
	if err := s.db.IgnorePath(ctx, clean, reason); err != nil {
		return err
	}
	report, ok, err := s.LastScanReport(ctx)
	if err != nil || !ok {
		return nil //nolint:nilerr // the dismissal itself succeeded
	}
	kept := report.UnmatchedDirs[:0]
	for _, d := range report.UnmatchedDirs {
		if d.Path == clean {
			report.IgnoredDirs++
			continue
		}
		kept = append(kept, d)
	}
	report.UnmatchedDirs = kept
	// Recompute rather than leave alone: UnmatchedTotal is what the UI counts
	// ("Review 4 folders…"), and dropping an entry from the list without
	// touching the total is how a dismissed folder stayed in the count while
	// being absent from the window it claimed to describe.
	report.UnmatchedTotal = len(kept)
	if raw, err := json.Marshal(report); err == nil {
		if err := s.db.SetMeta(ctx, scanReportKey, string(raw)); err != nil {
			s.log.Warn("scan: could not persist report after dismissal", "err", err)
		}
	}
	return nil
}

// ListIgnoredDirs returns every dismissal, so the UI can show and undo them.
// A dismissal nobody can find again is a trap rather than a feature.
func (s *Service) ListIgnoredDirs(ctx context.Context) ([]domain.IgnoredPath, error) {
	return s.db.ListIgnoredPaths(ctx)
}

// UnignoreDir undoes a dismissal. The path reappears on the next scan.
func (s *Service) UnignoreDir(ctx context.Context, path string) error {
	return s.db.UnignorePath(ctx, filepath.Clean(path))
}
