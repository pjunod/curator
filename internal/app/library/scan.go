package library

import (
	"context"
	"encoding/json"
	"errors"
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

// Report is the persisted result of the last reconcile.
type Report struct {
	ScannedAt     time.Time      `json:"scannedAt"`
	RootsScanned  int            `json:"rootsScanned"`
	ItemsScanned  int            `json:"itemsScanned"`
	FilesLinked   int            `json:"filesLinked"`
	FilesRemoved  int            `json:"filesRemoved"`
	UnmatchedDirs []UnmatchedDir `json:"unmatchedDirs"`
	MissingPaths  []string       `json:"missingPaths"`
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
	}

	items, err := s.db.ListMediaItems(ctx, "")
	if err != nil {
		return report, err
	}
	claimed := map[string]bool{}
	for _, item := range items {
		if item.Path == "" {
			continue
		}
		claimed[item.Path] = true
		if _, err := os.Stat(item.Path); err != nil {
			report.MissingPaths = append(report.MissingPaths, item.Path)
			continue
		}
		linked, removed, err := s.scanItem(ctx, item)
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
			full := filepath.Join(root.Path, e.Name())
			if !claimed[full] {
				report.UnmatchedDirs = append(report.UnmatchedDirs, UnmatchedDir{
					RootFolderID: root.ID, Path: full, Name: e.Name(),
				})
			}
		}
	}

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

// scanItem syncs one item's folder: every video file on disk gets a row
// (episode-linked for series via the filename extractor), and rows whose
// files vanished are pruned.
func (s *Service) scanItem(ctx context.Context, item domain.MediaItem) (linked, removed int, err error) {
	isMedia := filename.IsVideo
	if item.Kind == domain.KindBook {
		isMedia = filename.IsBook
	}
	onDisk := map[string]int64{} // path -> size
	walkErr := filepath.WalkDir(item.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !isMedia(path) {
			return err
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil // vanished mid-walk; skip
		}
		onDisk[path] = info.Size()
		return nil
	})
	if walkErr != nil {
		return 0, 0, walkErr
	}

	existing, err := s.db.ListFilesForItem(ctx, item.ID)
	if err != nil {
		return 0, 0, err
	}
	existingByPath := map[string]domain.MediaFile{}
	for _, f := range existing {
		existingByPath[f.Path] = f
	}

	for path, size := range onDisk {
		fileID, err := s.db.UpsertFile(ctx, item.ID, path, size)
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
func (s *Service) LastScanReport(ctx context.Context) (Report, bool, error) {
	raw, err := s.db.GetMeta(ctx, scanReportKey)
	if err != nil {
		return Report{}, false, nil // no rows = never scanned; other errors also read as absent
	}
	var r Report
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Report{}, false, err
	}
	return r, true, nil
}
