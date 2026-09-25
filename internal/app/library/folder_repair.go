package library

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func folderProblem(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "missing"
	}
	if err != nil {
		return "inaccessible"
	}
	if !info.IsDir() {
		return "not_directory"
	}
	f, err := os.Open(path)
	if err != nil {
		return "inaccessible"
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return "inaccessible"
	}
	return ""
}

func withinFolder(folder, path string) bool {
	if folder == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(folder, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Service) missingItem(ctx context.Context, item domain.MediaItem, reason string) (*MissingItem, error) {
	if item.Path == "" || reason == "" {
		return nil, nil
	}
	files, err := s.db.ListFilesForItem(ctx, item.ID)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		// A separate copy does not prove the primary destination ever held
		// files. Shared-folder book editions, however, do count.
		if file.CopyID == 0 || withinFolder(item.Path, file.Path) {
			return &MissingItem{ID: item.ID, Kind: item.Kind, Title: item.Title, Path: item.Path, Reason: reason}, nil
		}
	}
	return nil, nil
}

// RepairFolder reconnects an entry to a user-selected existing media folder.
// It never creates folders or moves files, and a failed validation leaves the
// entry and its file records untouched. expectedPath protects stale forms.
func (s *Service) RepairFolder(ctx context.Context, id int64, expectedPath, path string) error {
	item, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if item.Path != expectedPath {
		return fmt.Errorf("%w: this item's folder changed; check again before repairing", ErrInvalidInput)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: choose an absolute folder path", ErrInvalidInput)
	}
	path, err = filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("%w: cannot access the selected folder: %v", ErrInvalidInput, err)
	}
	if reason := folderProblem(path); reason != "" {
		return fmt.Errorf("%w: selected folder is %s", ErrInvalidInput, reason)
	}
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return err
	}
	var selectedRoot domain.RootFolder
	var relativePath string
	longest := 0
	for _, root := range roots {
		resolved, err := filepath.EvalSymlinks(root.Path)
		if err != nil {
			continue
		}
		if resolved == path {
			return fmt.Errorf("%w: choose this title's folder, not an entire library root", ErrInvalidInput)
		}
		if withinFolder(resolved, path) && len(resolved) > longest {
			longest = len(resolved)
			selectedRoot = root
			relativePath, _ = filepath.Rel(resolved, path)
		}
	}
	if selectedRoot.ID == 0 {
		return fmt.Errorf("%w: choose a folder inside a registered library root", ErrInvalidInput)
	}
	if !selectedRoot.Kind.Accepts(item.Kind) {
		return fmt.Errorf("%w: selected root does not hold %s", ErrInvalidInput, item.Kind)
	}
	items, err := s.db.ListMediaItems(ctx, "")
	if err != nil {
		return err
	}
	for _, other := range items {
		copies, err := s.db.ListMediaCopies(ctx, other.ID)
		if err != nil {
			return err
		}
		paths := []string{}
		if other.ID != id {
			paths = append(paths, other.Path)
		}
		for _, copy := range copies {
			paths = append(paths, copy.Path)
		}
		for _, claimed := range paths {
			if resolved, err := filepath.EvalSymlinks(claimed); err == nil {
				claimed = resolved
			}
			if withinFolder(claimed, path) || withinFolder(path, claimed) {
				return fmt.Errorf("%w: folder overlaps a location already assigned to %q", ErrInvalidInput, other.Title)
			}
		}
	}
	// Read the complete tree before changing placement. An empty directory
	// cannot repair lost media; unreadable descendants must not prune records.
	isMedia := filename.IsVideo
	if item.Kind == domain.KindBook {
		isMedia = filename.IsBook
	}
	found := false
	err = filepath.WalkDir(path, func(p string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && isMedia(p) {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			found = found || (info.Mode().IsRegular() && info.Size() > 0)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("%w: cannot read the selected folder: %v", ErrInvalidInput, err)
	}
	if !found {
		return fmt.Errorf("%w: selected folder contains no media files; restore the files or choose their actual folder", ErrInvalidInput)
	}
	// Use the registered root's spelling so the next scan claims the same
	// path even when the browser returned a symlink-resolved /private/var path.
	path = filepath.Join(selectedRoot.Path, relativePath)
	var repairs []sqlite.FilePathRepair
	for _, file := range item.Files {
		if !withinFolder(item.Path, file.Path) {
			continue
		}
		relative, err := filepath.Rel(item.Path, file.Path)
		if err != nil {
			return err
		}
		newPath := filepath.Join(path, relative)
		info, err := os.Stat(newPath)
		if err == nil && info.Mode().IsRegular() && info.Size() == file.Size {
			repairs = append(repairs, sqlite.FilePathRepair{ID: file.ID, OldPath: file.Path, NewPath: newPath})
		}
	}
	if err := s.db.RepairItemFolder(ctx, id, selectedRoot.ID, expectedPath, path, repairs); err != nil {
		return folderTakenErr(err, path)
	}
	item.Path, item.RootFolderID = path, selectedRoot.ID
	if _, _, err := s.scanItem(ctx, item, item.Copies); err != nil {
		return fmt.Errorf("folder saved, but scanning it failed: %w", err)
	}
	s.dropFromOutstanding(ctx, path)
	return nil
}
