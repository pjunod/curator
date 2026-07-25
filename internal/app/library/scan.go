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

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/filename"
	"github.com/monarr-media/monarr/internal/domain/parser"
	"github.com/monarr-media/monarr/internal/domain/quality"
)

const scanReportKey = "last_scan_report"

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

// MissingItem is a library item whose folder is not on disk.
//
// Carries the id, not just the path, because a list of paths is only ever
// something to read. With the id the UI can offer the two things a user
// actually wants — drop the entry, or point it somewhere real — and the most
// common cause of these is an item added by title that was never attached to
// any folder at all.
type MissingItem struct {
	ID    int64            `json:"id"`
	Kind  domain.MediaKind `json:"kind"`
	Title string           `json:"title"`
	Path  string           `json:"path"`
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
	for _, item := range items {
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
		if _, err := os.Stat(item.Path); err != nil {
			report.MissingPaths = append(report.MissingPaths, item.Path)
			report.MissingItems = append(report.MissingItems, MissingItem{
				ID: item.ID, Kind: item.Kind, Title: item.Title, Path: item.Path,
			})
			continue
		}
		linked, removed, err := s.scanItem(ctx, item, copies)
		if err != nil {
			s.log.Warn("scan: item failed", "title", item.Title, "err", err)
			continue
		}
		report.ItemsScanned++
		report.FilesLinked += linked
		report.FilesRemoved += removed
	}

	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return report, err
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

// scanItem syncs one item's folders (the item's own plus each copy's own
// folder): every video file on disk gets a row (episode-linked for series
// via the filename extractor), and rows whose files vanished are pruned.
// Files found in a copy's separate folder are attributed to that copy;
// files in a shared folder keep whatever attribution their row already has
// (the upsert never overwrites copy_id).
func (s *Service) scanItem(ctx context.Context, item domain.MediaItem, copies []domain.MediaCopy) (linked, removed int, err error) {
	isMedia := filename.IsVideo
	if item.Kind == domain.KindBook {
		isMedia = filename.IsBook
	}
	type foundFile struct {
		size   int64
		copyID int64
	}
	onDisk := map[string]foundFile{} // path -> size + copy attribution
	walk := func(root string, copyID int64) error {
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isMedia(path) {
				return err
			}
			info, ierr := d.Info()
			if ierr != nil {
				return nil // vanished mid-walk; skip
			}
			onDisk[path] = foundFile{size: info.Size(), copyID: copyID}
			return nil
		})
	}
	if walkErr := walk(item.Path, 0); walkErr != nil {
		return 0, 0, walkErr
	}
	for _, cp := range copies {
		if cp.Path == "" || cp.Path == item.Path {
			continue
		}
		if _, statErr := os.Stat(cp.Path); statErr != nil {
			continue // copy folder not created yet — nothing to scan
		}
		if walkErr := walk(cp.Path, cp.ID); walkErr != nil {
			return 0, 0, walkErr
		}
	}

	existing, err := s.db.ListFilesForItem(ctx, item.ID)
	if err != nil {
		return 0, 0, err
	}
	existingByPath := map[string]domain.MediaFile{}
	for _, f := range existing {
		existingByPath[f.Path] = f
	}

	for path, ff := range onDisk {
		fileID, err := s.db.UpsertFile(ctx, item.ID, ff.copyID, path, ff.size)
		if err != nil {
			return linked, removed, err
		}
		// Record quality parsed from the file name so upgrade decisions
		// work for adopted libraries too. Book files are graded by their
		// extension — the format IS the quality (ADR 0006).
		if src := filename.BookQualitySource(path); item.Kind == domain.KindBook && src != "" {
			_ = s.db.SetFileQuality(ctx, fileID, quality.Quality{Source: quality.Source(src)})
		} else if q := parser.Parse(filepath.Base(path)).Quality; q.Resolution != 0 || q.Source != quality.SourceUnknown {
			_ = s.db.SetFileQuality(ctx, fileID, q)
		}
		if item.Kind == domain.KindSeries {
			if eps, ok := filename.Extract(path); ok {
				var ids []int64
				for _, epNum := range eps.Episodes {
					epID, err := s.db.GetEpisodeID(ctx, item.ID, eps.Season, epNum)
					if err != nil {
						if errors.Is(err, ErrNotFound) {
							continue // file references an episode we don't know
						}
						return linked, removed, err
					}
					ids = append(ids, epID)
				}
				if len(ids) > 0 {
					if err := s.db.ReplaceFileEpisodeLinks(ctx, fileID, ids); err != nil {
						return linked, removed, err
					}
				}
			}
		}
		if _, existed := existingByPath[path]; !existed {
			linked++
		}
	}

	for path, f := range existingByPath {
		if _, still := onDisk[path]; !still {
			if err := s.db.DeleteFile(ctx, f.ID); err != nil {
				return linked, removed, err
			}
			removed++
		}
	}
	return linked, removed, nil
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
	r.pruneResolvedMissing(ctx, s)
	return r, true, nil
}

// pruneResolvedMissing drops missing-folder entries that no longer apply:
// the item was removed, or it now points at a folder that exists.
func (r *Report) pruneResolvedMissing(ctx context.Context, s *Service) {
	if len(r.MissingItems) == 0 {
		return
	}
	keptItems := make([]MissingItem, 0, len(r.MissingItems))
	keptPaths := make([]string, 0, len(r.MissingItems))
	for _, m := range r.MissingItems {
		item, err := s.db.GetMediaItemFull(ctx, m.ID)
		if err != nil {
			continue // entry removed — nothing missing any more
		}
		if item.Path == "" {
			continue // no folder claimed, so none can be missing
		}
		if _, statErr := os.Stat(item.Path); statErr == nil {
			continue // it was pointed at something real
		}
		m.Path = item.Path // report where it points now, not where it did
		m.Title = item.Title
		keptItems = append(keptItems, m)
		keptPaths = append(keptPaths, item.Path)
	}
	r.MissingItems = keptItems
	r.MissingPaths = keptPaths
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
