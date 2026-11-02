package acquisition

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// assertImportSourceDisposable runs at actual automatic deletion admission,
// under the storage-decision authority. New attempts can use retained source
// bytes, but success never transfers custody from an older uncertain stage.
func (s *Service) assertImportSourceDisposable(ctx context.Context, path string) error {
	paths, err := s.db.ImportSourceCustodyPaths(ctx)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	if path == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("source path is unknown; retained import custody cannot be excluded")
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return fmt.Errorf("completed roots unavailable; retained import alias custody cannot be excluded")
	}
	for _, root := range roots {
		info, e := os.Stat(root)
		if e != nil {
			return e
		}
		if !info.IsDir() {
			return fmt.Errorf("completed root is not an available directory: %s", root)
		}
	}
	candidate := storagePath(path, roots)
	for _, source := range paths {
		if !filepath.IsAbs(source) {
			return fmt.Errorf("retained source identity is not an absolute path")
		}
		source = storagePath(source, roots)
		if within(candidate, source) || within(source, candidate) {
			return fmt.Errorf("source retained by an unresolved import stage or publication: %s", source)
		}
	}
	return nil
}
