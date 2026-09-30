package acquisition

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

const (
	CompletedRootsSetting = "completed_roots"
	JobCompletedScan      = "downloads.inventory"
	CompletedScanInterval = 5 * time.Minute
)

// CompletedEntry accounts for every kind of directory entry, including loose
// archives, hidden files and symlinks. Bytes are logical file sizes, not a
// promise of reclaimable disk space (hardlinks and sparse files differ).
type CompletedEntry struct {
	Fingerprint string                    `json:"fingerprint"`
	Path        string                    `json:"path"`
	Bytes       int64                     `json:"bytes"`
	Files       int                       `json:"files"`
	Status      string                    `json:"status"`
	Reason      string                    `json:"reason"`
	Receipts    []sqlite.CompletedReceipt `json:"receipts"`
}

type CompletedRoot struct {
	RootIdentity   string           `json:"rootIdentity"`
	Path           string           `json:"path"`
	CheckedAt      time.Time        `json:"checkedAt"`
	LastCompleteAt time.Time        `json:"lastCompleteAt"`
	Error          string           `json:"error"`
	Entries        []CompletedEntry `json:"entries"`
	Bytes          int64            `json:"bytes"`
}

type CompletedInventory struct {
	Roots     []CompletedRoot `json:"roots"`
	Bytes     int64           `json:"bytes"`
	Attention int             `json:"attention"`
}

func (s *Service) SetCompletedRoots(ctx context.Context, value string) error {
	s.completedMu.Lock()
	defer s.completedMu.Unlock()
	ctx, releaseStorage := s.lockStorageDecision(ctx)
	defer releaseStorage()
	for _, p := range strings.Split(value, "\n") {
		p = strings.TrimSpace(p)
		if p != "" && (!filepath.IsAbs(p) || canonicalCompletedPath(filepath.Clean(p)) == string(filepath.Separator)) {
			return fmt.Errorf("completed folders must be absolute paths below the filesystem root: %q", p)
		}
	}
	if err := s.db.SetMeta(ctx, CompletedRootsSetting, value); err != nil {
		return err
	}
	// Explicitly saving roots approves their current mount identities. Keep
	// prior counts until a complete scan succeeds; never erase stale evidence.
	inventory, err := s.CompletedInventory(ctx)
	if err != nil {
		return err
	}
	for _, root := range inventory.Roots {
		root.RootIdentity = ""
		raw, err := json.Marshal(root)
		if err != nil {
			return err
		}
		if err := s.db.SaveCompletedScan(ctx, root.Path, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

// completedRoots uses explicit roots when configured, otherwise local mappings
// and persisted payload parents. No last-100-rows limit: an old receipt still explains bytes.
func (s *Service) completedRoots(ctx context.Context, receipts []sqlite.CompletedReceipt) ([]string, error) {
	value, err := s.db.GetMeta(ctx, CompletedRootsSetting)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	paths := strings.Split(value, "\n")
	// This is Curator's working tree in the deployed container, not Runner's
	// /processing tree. Account for all of it, including completed siblings.
	if strings.TrimSpace(value) == "" {
		if info, err := os.Stat("/working/monarr"); err == nil && info.IsDir() {
			// Persist discovery so an unavailable mount cannot silently change
			// the inventory scope or erase its last successful report.
			if err := s.db.SetMeta(ctx, CompletedRootsSetting, "/working/monarr"); err != nil {
				return nil, err
			}
			return []string{canonicalCompletedPath("/working/monarr")}, nil
		}
	}
	if strings.TrimSpace(value) == "" {
		clients, err := s.db.ListDownloadClients(ctx)
		if err != nil {
			return nil, err
		}
		for _, c := range clients {
			for _, m := range c.PathMappings {
				paths = append(paths, m.Local)
			}
		}
		for _, r := range receipts {
			if r.Protocol != "manual" && filepath.IsAbs(r.Path) {
				paths = append(paths, filepath.Dir(r.Path))
			}
		}
	}
	seen := map[string]bool{}
	var roots []string
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		if p == string(filepath.Separator) {
			continue
		}
		// Resolve root aliases only; descendants are never followed.
		p = canonicalCompletedPath(p)
		if !seen[p] {
			seen[p] = true
			roots = append(roots, p)
		}
	}
	// Prefer roots that cover other roots through filesystem aliases, then
	// collapse the aliases before applying ordinary ancestor deduplication.
	scores := map[string]int{}
	for _, root := range roots {
		for _, other := range roots {
			if within(root, storagePath(other, []string{root})) {
				scores[root]++
			}
		}
	}
	sort.Slice(roots, func(i, j int) bool {
		if scores[roots[i]] != scores[roots[j]] {
			return scores[roots[i]] > scores[roots[j]]
		}
		return roots[i] < roots[j]
	})
	out := []string{}
	for _, p := range roots {
		p = storagePath(p, out)
		covered := false
		for _, root := range out {
			if within(root, p) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = append(out, "/pool/downloads")
	}
	sort.Strings(out)
	return out, nil
}

// CompletedInventory reads cached reports only. A failed scan keeps the last
// complete inventory, visibly stale, instead of making inaccessible bytes vanish.
func (s *Service) CompletedInventory(ctx context.Context) (CompletedInventory, error) {
	out := CompletedInventory{Roots: []CompletedRoot{}}
	reports, err := s.db.CompletedScans(ctx)
	if err != nil {
		return out, err
	}
	for _, raw := range reports {
		var root CompletedRoot
		if err := json.Unmarshal([]byte(raw), &root); err != nil {
			return out, err
		}
		out.Roots = append(out.Roots, root)
		out.Bytes += root.Bytes
		for _, e := range root.Entries {
			if e.Status != "active" && e.Status != "retained" && e.Status != "storage" {
				out.Attention++
			}
		}
	}
	return out, nil
}

// ScanCompleted runs off the queue poll and importer workers. It is read-only:
// an unmatched folder is evidence of an accounting gap, never deletion authority.
func (s *Service) ScanCompleted(ctx context.Context) error {
	s.completedMu.Lock()
	defer s.completedMu.Unlock()
	receipts, err := s.db.CompletedReceipts(ctx)
	if err != nil {
		return err
	}
	roots, err := s.completedRoots(ctx, receipts)
	if err != nil {
		return err
	}
	library, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return err
	}
	prior, err := s.CompletedInventory(ctx)
	if err != nil {
		return err
	}
	previous := map[string]CompletedRoot{}
	for _, root := range prior.Roots {
		previous[root.Path] = root
	}
	clients, err := s.db.ListDownloadClients(ctx)
	if err != nil {
		return err
	}
	remove := map[int64]bool{}
	for _, c := range clients {
		remove[c.ID] = c.RemoveCompleted
	}
	// Canonicalize associations without following any descendant during walking.
	for i := range receipts {
		receipts[i].Path = storagePath(receipts[i].Path, roots)
	}
	var failures []error
	for _, path := range roots {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		root := CompletedRoot{Path: path, CheckedAt: time.Now().UTC(), Entries: []CompletedEntry{}}
		if info, err := os.Stat(path); err != nil {
			root.Error = err.Error()
		} else {
			root.RootIdentity = fmt.Sprint(recoveryDirectoryIdentity(info))
			if old, ok := previous[path]; ok && old.RootIdentity != "" && old.RootIdentity != root.RootIdentity {
				root.Error = "storage mount identity changed; restore the mount, or explicitly re-save the storage roots to accept its replacement"
			}
		}
		for _, lib := range library {
			p := storagePath(lib.Path, roots)
			if within(p, path) || within(path, p) {
				root.Error = "completed folder overlaps a library root; correct the path configuration"
				break
			}
		}
		if root.Error == "" {
			// Inventory runs in the scheduler, not an HTTP request. Slow network
			// storage must be allowed to finish; shutdown cancellation and the
			// entry limit still bound work without discarding progress after 30s.
			root.Entries, root.Bytes, err = scanCompletedRoot(ctx, path, receipts, remove)
			if err != nil {
				root.Error = err.Error()
			} else if info, err := os.Stat(path); err != nil || fmt.Sprint(recoveryDirectoryIdentity(info)) != root.RootIdentity {
				root.Error = "storage mount changed during the scan"
			}
		}
		if root.Error != "" {
			if old, ok := previous[path]; ok {
				root.Entries, root.Bytes, root.LastCompleteAt = old.Entries, old.Bytes, old.LastCompleteAt
				root.RootIdentity = old.RootIdentity
			}
			failures = append(failures, fmt.Errorf("%s: %s", path, root.Error))
		} else {
			root.LastCompleteAt = root.CheckedAt
		}
		raw, err := json.Marshal(root)
		if err != nil {
			return err
		}
		if err := s.db.SaveCompletedScan(ctx, path, string(raw)); err != nil {
			return err
		}
	}
	for _, old := range prior.Roots {
		selected := false
		for _, root := range roots {
			if root == old.Path {
				selected = true
				break
			}
		}
		if !selected {
			if err := s.db.ForgetCompletedScan(ctx, old.Path); err != nil {
				return err
			}
		}
	}
	return errors.Join(failures...)
}

func scanCompletedRoot(ctx context.Context, root string, receipts []sqlite.CompletedReceipt, remove map[int64]bool) ([]CompletedEntry, int64, error) {
	return scanCompletedSelection(ctx, root, "", receipts, remove)
}

// A selected scan uses the same root identity and fingerprint format as the
// inventory, but never descends into unrelated payloads during deletion.
func scanCompletedSelection(ctx context.Context, root, selected string, receipts []sqlite.CompletedReceipt, remove map[int64]bool) ([]CompletedEntry, int64, error) {
	entries := []CompletedEntry{}
	byPath := map[string]int{}
	fingerprints := map[string]hash.Hash{}
	var rootIdentity any
	count := 0
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if selected != "" && path != root && !within(selected, path) && !within(path, selected) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		count++
		if count > 100000 {
			return fmt.Errorf("scan incomplete: more than 100000 entries")
		}
		if path == root {
			info, err := d.Info()
			if err != nil {
				return err
			}
			rootIdentity = recoveryDirectoryIdentity(info)
			if !d.IsDir() {
				return fmt.Errorf("completed root is not a directory")
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		top := filepath.Join(root, parts[0])
		// completed is storage infrastructure when the working tree is the
		// configured root. Its children must each have their own explanation.
		if parts[0] == "completed" && len(parts) > 1 {
			top = filepath.Join(root, parts[0], parts[1])
		}
		i, ok := byPath[top]
		if !ok {
			i = len(entries)
			byPath[top] = i
			e := CompletedEntry{Path: top, Status: "untracked", Reason: "No download record; review or manually import these files", Receipts: []sqlite.CompletedReceipt{}}
			for _, r := range receipts {
				if within(top, r.Path) || within(r.Path, top) {
					e.Receipts = append(e.Receipts, r)
				}
			}
			classifyCompleted(&e, remove)
			if rel == "completed" && d.IsDir() {
				e.Receipts = []sqlite.CompletedReceipt{}
				e.Status, e.Reason = "storage", "Completed-download storage; contents accounted for individually"
			}
			entries = append(entries, e)
			h := sha256.New()
			_, _ = fmt.Fprintf(h, "%v\n", rootIdentity)
			fingerprints[top] = h
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(fingerprints[top], "%q %v %d %d %d\n", path, recoveryDirectoryIdentity(info), info.Size(), info.ModTime().UnixNano(), info.Mode())
		if d.Type()&os.ModeSymlink != 0 {
			entries[i].Status, entries[i].Reason = "symlink", "Contains a symbolic link; target excluded from measured bytes"
		} else if info.Mode().IsRegular() {
			entries[i].Bytes += info.Size()
			entries[i].Files++
			total += info.Size()
		} else if !d.IsDir() {
			entries[i].Status, entries[i].Reason = "untracked", "Contains a special file; review required"
		}
		return nil
	})
	for i := range entries {
		entries[i].Fingerprint = hex.EncodeToString(fingerprints[entries[i].Path].Sum(nil))
	}
	return entries, total, err
}

func classifyCompleted(e *CompletedEntry, remove map[int64]bool) {
	if len(e.Receipts) == 0 {
		return
	}
	if len(e.Receipts) > 1 {
		e.Status, e.Reason = "ambiguous", "Multiple download associations; review before cleanup"
		return
	}
	r := e.Receipts[0]
	if !r.Live {
		e.Status, e.Reason = "untracked", "Activity record was cleared; historical association retained for review"
		return
	}
	switch r.State {
	case "imported":
		if r.PayloadRemoved {
			e.Status, e.Reason = "untracked", "Previously marked removed; files exist at this path again and need review"
		} else if !remove[r.ClientID] {
			e.Status, e.Reason = "retained", "Import finished; client cleanup is disabled or client was removed"
		} else {
			e.Status, e.Reason = "cleanup_pending", "Import finished but files remain; cleanup needs attention"
		}
	case "failed":
		e.Status, e.Reason = "failed", "Download or import failed; files retained for recovery"
	case "downloaded", "awaiting_import":
		e.Status, e.Reason = "awaiting_import", "Payload is waiting for import"
	default:
		e.Status, e.Reason = "active", "Associated with an active download or import"
	}
}

func (s *Service) CompletedHealth(ctx context.Context) health.Result {
	inventory, err := s.CompletedInventory(ctx)
	if err != nil {
		return health.Warn("completed-folder inventory unavailable: %v", err)
	}
	if len(inventory.Roots) == 0 {
		return health.Warn("completed folders have not been scanned yet")
	}
	for _, root := range inventory.Roots {
		if root.Error != "" {
			return health.Warn("completed folder %s: %s", root.Path, root.Error)
		}
		if time.Since(root.LastCompleteAt) > 3*CompletedScanInterval {
			return health.Warn("completed-folder inventory is stale: %s", root.Path)
		}
	}
	if inventory.Attention > 0 {
		return health.Warn("%d completed-folder entries need attention; %d logical bytes accounted for — see Activity", inventory.Attention, inventory.Bytes)
	}
	return health.OK()
}

// Keep a missing root's cache key stable even when an ancestor is an alias
// (for example /var on macOS). This does not establish mount availability.
func canonicalCompletedPath(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	return filepath.Join(canonicalCompletedPath(parent), filepath.Base(path))
}

// storagePath recognizes the common bind-mount alias: the same completed
// directory is exposed as /downloads and under /working/monarr/completed.
// EvalSymlinks alone cannot recognize bind mounts; filesystem identity can.
func storagePath(path string, roots []string) string {
	path = canonicalCompletedPath(path)
	for parent := path; ; parent = filepath.Dir(parent) {
		info, err := os.Stat(parent)
		if err == nil {
			for _, root := range roots {
				for _, candidate := range []string{root, filepath.Join(root, "completed")} {
					other, err := os.Stat(candidate)
					if err == nil && storageSameFile(info, other) {
						rel, err := filepath.Rel(parent, path)
						if err == nil {
							return filepath.Join(candidate, rel)
						}
					}
				}
			}
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	return path
}

func (s *Service) ownsDownloadPath(ctx context.Context, path string) bool {
	value, err := s.db.GetMeta(ctx, CompletedRootsSetting)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false
	}
	paths := strings.Split(value, "\n")
	if strings.TrimSpace(value) == "" {
		paths = []string{"/working/monarr"}
	}
	roots := []string{}
	for _, root := range paths {
		root = strings.TrimSpace(root)
		if filepath.IsAbs(root) {
			roots = append(roots, canonicalCompletedPath(root))
		}
	}
	// Ownership of a directory entry follows its parent, not the target of
	// a final symlink. Rooted removal unlinks that link without following it.
	path = filepath.Join(storagePath(filepath.Dir(path), roots), filepath.Base(path))
	for _, root := range roots {
		if path != root && within(root, path) {
			return true
		}
	}
	return false
}

// Injectable identity comparison permits bind-alias regression cases on hosts
// that cannot create bind mounts; production always uses os.SameFile.
var storageSameFile = os.SameFile

func (s *Service) checkStorageIdentity(ctx context.Context, path string) error {
	inventory, err := s.CompletedInventory(ctx)
	if err != nil {
		return err
	}
	for _, root := range inventory.Roots {
		if !within(root.Path, path) {
			continue
		}
		if root.Error != "" {
			return fmt.Errorf("storage scan is incomplete: %s", root.Error)
		}
		if root.RootIdentity == "" {
			continue
		}
		info, err := os.Stat(root.Path)
		if err != nil {
			return err
		}
		if fmt.Sprint(recoveryDirectoryIdentity(info)) != root.RootIdentity {
			return fmt.Errorf("storage mount changed; scan again before deleting")
		}
	}
	return nil
}
