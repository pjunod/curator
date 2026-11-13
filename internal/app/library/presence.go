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
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

var errPresenceUnsupported = errors.New("volume identity is unavailable on this platform")

func (s *Service) VerifyPresence(ctx context.Context, id int64) error {
	return s.verifyPresence(ctx, id, false)
}

// This pass reads a complete media manifest, without changing library records.
// Capture revisions BEFORE all filesystem reads; every proof write compares them.
func (s *Service) verifyPresence(ctx context.Context, id int64, primaryOnly bool) error {
	if err := s.db.InvalidatePresence(ctx, id); err != nil {
		return err
	}
	revision, shared, err := s.db.PresenceRevision(ctx, id)
	if err != nil {
		return err
	}
	item, err := s.db.GetMediaItemFull(ctx, id)
	if err != nil {
		return err
	}
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return err
	}
	rootPaths := map[int64]string{}
	for _, r := range roots {
		rootPaths[r.ID] = r.Path
	}
	type scope struct {
		id, root int64
		path     string
	}
	scopes := []scope{{0, item.RootFolderID, item.Path}}
	if !primaryOnly {
		for _, cp := range item.Copies {
			root, path := cp.RootFolderID, cp.Path
			if root == 0 {
				root = item.RootFolderID
			}
			if path == "" {
				path = item.Path
			}
			scopes = append(scopes, scope{cp.ID, root, path})
		}
	}
	known := map[string]domain.MediaFile{}
	for _, f := range item.Files {
		known[f.Path] = f
	}
	manifests := map[string]map[string]int64{}
	isMedia := filename.IsVideo
	if item.Kind == domain.KindBook {
		isMedia = filename.IsBook
	}
	var first error
	for _, sc := range scopes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		check := func() error {
			root := rootPaths[sc.root]
			if root == "" || sc.path == "" || !presenceInside(root, sc.path) {
				return fmt.Errorf("library scope is unassigned or outside its root")
			}
			if err := validatePresenceScope(root, sc.path); err != nil {
				return err
			}
			before, identity, err := s.checkPresenceRoot(ctx, sc.root, root)
			if err != nil {
				return err
			}
			generation, err := s.db.PresenceRootGeneration(ctx, sc.root, root, identity)
			if err != nil {
				return err
			}
			manifest, exists := manifests[sc.path]
			if !exists {
				manifest = map[string]int64{}
				info, statErr := os.Stat(sc.path)
				if statErr != nil && !os.IsNotExist(statErr) {
					return fmt.Errorf("library scope cannot be checked")
				}
				if statErr == nil {
					if !info.IsDir() {
						return fmt.Errorf("library scope is not a directory")
					}
					if err = filepath.WalkDir(sc.path, func(path string, e fs.DirEntry, walkErr error) error {
						if walkErr != nil {
							return walkErr
						}
						if e.Type()&os.ModeSymlink != 0 {
							return fmt.Errorf("symlink prevents complete library verification")
						}
						if e.IsDir() || !isMedia(path) {
							return nil
						}
						f, err := e.Info()
						if err != nil {
							return err
						}
						if !f.Mode().IsRegular() {
							return fmt.Errorf("media identity is not a regular file")
						}
						manifest[path] = f.Size()
						return nil
					}); err != nil {
						return fmt.Errorf("library manifest is incomplete: %w", err)
					}
				}
				manifests[sc.path] = manifest
			}
			// Shared folders compare the union of all recorded copies once. Attribution
			// remains the catalogue's copy ID, never the directory enumeration order.
			for path, size := range manifest {
				f, ok := known[path]
				if !ok || f.Size != size {
					return fmt.Errorf("library contains unindexed or changed media")
				}
			}
			present := []int64{}
			for _, f := range item.Files {
				if f.CopyID != sc.id {
					continue
				}
				if !presenceInside(sc.path, f.Path) {
					return fmt.Errorf("recorded file is outside its scope")
				}
				if size, ok := manifest[f.Path]; ok && size == f.Size {
					present = append(present, f.ID)
				}
			}
			after, err := os.Stat(root)
			if err != nil || !os.SameFile(before, after) {
				return fmt.Errorf("library volume changed during verification")
			}
			return s.db.SavePresence(ctx, sqlite.PresenceObservation{ItemID: id, CopyID: sc.id, ItemRevision: revision, SharedRevision: shared, RootID: sc.root, RootGeneration: generation, RootPath: root, RootIdentity: identity, ScopePath: sc.path, CheckedAt: time.Now(), PresentFileIDs: present})
		}
		if err := check(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
func presenceInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func (s *Service) checkPresenceRoot(ctx context.Context, id int64, root string) (os.FileInfo, string, error) {
	if root == "" {
		return nil, "", fmt.Errorf("library root is unassigned")
	}
	before, err := os.Stat(root)
	if err != nil || !before.IsDir() {
		return nil, "", fmt.Errorf("library root unavailable")
	}
	f, err := os.Open(root)
	if err != nil {
		return nil, "", fmt.Errorf("library root unreadable")
	}
	names, err := f.Readdirnames(1)
	_ = f.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, "", fmt.Errorf("library root unavailable or empty")
	}
	identity := presenceRootIdentity(before)
	if identity == "" {
		if len(names) == 0 {
			return nil, "", fmt.Errorf("unidentifiable library root is empty")
		}
		return nil, "", errPresenceUnsupported
	}
	generation, err := s.db.PresenceRootGeneration(ctx, id, root, identity)
	if err != nil {
		return nil, "", err
	}
	if len(names) == 0 && generation == 0 {
		return nil, "", fmt.Errorf("unverified library root is empty")
	}
	return before, identity, nil
}

// Do not interpret an unwalked symlink or an out-of-root scope as absence.
// Missing suffixes are allowed only after every existing ancestor is checked.
func validatePresenceScope(root, path string) error {
	if path == "" || root != "" && !presenceInside(root, path) {
		return fmt.Errorf("library scope is outside its root")
	}
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("library scope is not a directly walkable directory")
		}
		if current == filepath.Clean(root) || filepath.Dir(current) == current {
			break
		}
	}
	return nil
}
