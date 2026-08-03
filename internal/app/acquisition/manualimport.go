package acquisition

import (
	"context"
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
	Path     string `json:"path"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Kind     string `json:"kind"` // "video" | "book"
	Quality  string `json:"quality"`
	Season   int    `json:"season"`   // -1 when not a series file
	Episodes []int  `json:"episodes"` // empty for movies/books
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
		sf := ScannedFile{Path: p, Name: base, Size: sizeOf(p), Season: -1}
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
	Path        string
	Paths       []string // exact files selected from the scan; nil = whole Path
	MediaItemID int64
	CopyID      int64
	DownloadID  int64 // 0 = not tied to a queue row
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
	item, err := s.db.GetMediaItemFull(ctx, req.MediaItemID)
	if err != nil {
		return 0, err
	}
	isMedia := filename.IsVideo
	if item.Kind == "book" {
		isMedia = filename.IsBook
	}
	if req.Paths == nil {
		files, scanErr := collectFiles(req.Path, isMedia)
		if scanErr != nil {
			return 0, scanErr
		}
		if len(files) == 0 {
			return 0, fmt.Errorf("no media files in %s", req.Path)
		}
	} else {
		if len(req.Paths) == 0 {
			return 0, fmt.Errorf("no files selected")
		}
		if _, err := selectedFiles(req.Path, req.Paths, isMedia); err != nil {
			return 0, err
		}
	}

	id := req.DownloadID
	if id == 0 {
		id, err = s.db.InsertDownload(ctx, sqlite.Download{
			MediaItemID:  req.MediaItemID,
			CopyID:       req.CopyID,
			ReleaseTitle: filepath.Base(filepath.Clean(req.Path)),
			Protocol:     "manual",
			State:        "downloaded",
		})
		if err != nil {
			return 0, err
		}
	}

	unlock := s.lockDownload(id)
	defer unlock()
	dl, err := s.db.GetDownload(ctx, id)
	if err != nil {
		return 0, err
	}
	if s.ImportRunning(id) {
		return 0, fmt.Errorf("import is already running")
	}
	dl.MediaItemID = req.MediaItemID
	dl.CopyID = req.CopyID
	dl.ImportPath = req.Path
	if err := s.db.UpdateDownloadTarget(ctx, dl); err != nil {
		return 0, err
	}
	dl.Error = ""
	dl.State = "downloaded"
	selection := fmt.Sprintf("%d selected file(s)", len(req.Paths))
	if req.Paths == nil {
		selection = "all media files"
	}
	dl.Handoff = append(dl.Handoff, sqlite.HandoffEntry{
		Step: stepManualQueued, At: time.Now().UnixMilli(),
		Detail: selection + " waiting for an import worker",
		Paths:  append([]string(nil), req.Paths...),
	})
	if err := s.db.UpdateDownloadHandoff(ctx, dl); err != nil {
		return 0, err
	}
	if !s.enqueueImport(ctx, dl, ports.ClientConfig{}) {
		s.failImport(ctx, &dl, "import queue is full; try again shortly")
		return 0, fmt.Errorf("import queue is full; try again shortly")
	}
	return id, nil
}
