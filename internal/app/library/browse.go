package library

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DirEntry is one child directory of a browsed path.
type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Registered is true when this exact path is already a root folder, so
	// the picker can show it as taken rather than offering it twice.
	Registered bool `json:"registered"`
}

// BrowseResult is the answer to "what is under here".
type BrowseResult struct {
	Path   string     `json:"path"`
	Parent string     `json:"parent"` // "" at the filesystem root
	Dirs   []DirEntry `json:"dirs"`
}

// maxBrowseEntries caps one listing. A directory with more children than
// this is almost certainly a media folder rather than a place to put one,
// and an unbounded response is a denial-of-service against the browser.
const maxBrowseEntries = 1000

// Browse lists the immediate child directories of path, for the root-folder
// picker and the path typeahead (ADR 0009 §2a). Both need the same thing:
// the server can see the filesystem and the browser cannot.
//
// Deliberate constraints, because this is a directory-enumeration primitive
// exposed over HTTP:
//
//   - Directories only. Never file contents, never sizes — the caller is
//     choosing a folder, and anything more is surface for no benefit.
//   - One level per call. No recursion, ever: it keeps the response fast on
//     network mounts, where a recursive walk would hang the UI.
//   - Unreadable and missing are reported identically. Distinguishing them
//     is exactly what makes an enumeration primitive useful to someone
//     probing a host.
//
// Authentication is the caller's job (the API layer), and it is not
// optional — an unauthenticated version of this is a disclosure bug.
func (s *Service) Browse(ctx context.Context, path string) (BrowseResult, error) {
	if path == "" {
		path = string(filepath.Separator)
	}
	if !filepath.IsAbs(path) {
		return BrowseResult{}, fmt.Errorf("browse path must be absolute")
	}
	clean := filepath.Clean(path)

	// Symlinks are resolved before listing, so a link cannot be used to
	// present one path while enumerating another.
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return BrowseResult{}, errNotReadable(clean)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return BrowseResult{}, errNotReadable(clean)
	}
	entries, err := os.ReadDir(resolved)
	if err != nil {
		return BrowseResult{}, errNotReadable(clean)
	}

	registered := map[string]bool{}
	if roots, err := s.db.ListRootFolders(ctx); err == nil {
		for _, rf := range roots {
			registered[rf.Path] = true
		}
	}

	out := BrowseResult{Path: resolved, Dirs: []DirEntry{}}
	if parent := filepath.Dir(resolved); parent != resolved {
		out.Parent = parent
	}
	for _, e := range entries {
		if len(out.Dirs) >= maxBrowseEntries {
			break
		}
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(resolved, e.Name())
		out.Dirs = append(out.Dirs, DirEntry{
			Name: e.Name(), Path: full, Registered: registered[full],
		})
	}
	sort.Slice(out.Dirs, func(i, j int) bool {
		return strings.ToLower(out.Dirs[i].Name) < strings.ToLower(out.Dirs[j].Name)
	})
	return out, nil
}

// Suggest powers the typeahead: given whatever the user has typed so far,
// list the directories that could complete it. A trailing separator means
// "show me everything in here"; anything else is treated as a prefix to
// filter the parent's children by.
func (s *Service) Suggest(ctx context.Context, partial string) (BrowseResult, error) {
	if partial == "" || !filepath.IsAbs(partial) {
		partial = string(filepath.Separator)
	}
	dir, prefix := partial, ""
	if !strings.HasSuffix(partial, string(filepath.Separator)) {
		dir, prefix = filepath.Split(partial)
	}
	res, err := s.Browse(ctx, dir)
	if err != nil {
		return BrowseResult{}, err
	}
	if prefix == "" {
		return res, nil
	}
	lower := strings.ToLower(prefix)
	filtered := res.Dirs[:0]
	for _, d := range res.Dirs {
		if strings.HasPrefix(strings.ToLower(d.Name), lower) {
			filtered = append(filtered, d)
		}
	}
	res.Dirs = filtered
	return res, nil
}

// errNotReadable is deliberately uniform: "does not exist" and "exists but
// you may not read it" are the same answer, because the difference is the
// useful part to an attacker and noise to a user picking a folder.
func errNotReadable(path string) error {
	return fmt.Errorf("%s is not readable", path)
}
