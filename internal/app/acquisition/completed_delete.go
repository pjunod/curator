package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DeleteCompletedEntry is an explicit operator decision about Curator-owned
// storage. A cached path alone is not authority: repeat ownership, live-work,
// mount identity and recursive metadata checks before the rooted deletion.
func (s *Service) DeleteCompletedEntry(ctx context.Context, path, fingerprint string) error {
	s.completedMu.Lock()
	defer s.completedMu.Unlock()
	ctx, releaseStorage := s.lockStorageDecision(ctx)
	defer releaseStorage()
	s.importTargetMu.Lock()
	defer s.importTargetMu.Unlock()
	if fingerprint == "" || !s.ownsDownloadPath(ctx, path) {
		return fmt.Errorf("select an entry inside Curator-owned download storage")
	}
	inventory, err := s.CompletedInventory(ctx)
	if err != nil {
		return err
	}
	var selected *CompletedRoot
	for i := range inventory.Roots {
		root := &inventory.Roots[i]
		for _, entry := range root.Entries {
			if entry.Path == path && entry.Fingerprint == fingerprint && entry.Status != "storage" {
				selected = root
			}
		}
	}
	if selected == nil || selected.Error != "" {
		return fmt.Errorf("inventory changed or is incomplete; scan again before deleting")
	}
	if err := s.checkStorageIdentity(ctx, path); err != nil {
		return err
	}
	roots := []string{selected.Path}
	library, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return err
	}
	recovery, err := s.RecoverySettings(ctx)
	if err != nil {
		return err
	}
	protected := []string{recovery.LocalRoot}
	for _, root := range library {
		protected = append(protected, root.Path)
	}
	for _, root := range protected {
		if root == "" {
			continue
		}
		root = storagePath(root, roots)
		if within(root, path) || within(path, root) {
			return fmt.Errorf("path overlaps protected library or recovery storage")
		}
	}
	active, err := s.db.ListDownloadReservations(ctx)
	if err != nil {
		return err
	}
	for _, dl := range active {
		if own, ok := ctx.Value(partialCleanupIDKey{}).(int64); ok && dl.ID == own && dl.State == "failed" && dl.CleanupPending && filepath.Clean(dl.ImportPath) == filepath.Clean(path) {
			continue
		}
		if dl.State == "planned" {
			continue
		}
		candidate := dl.ImportPath
		if candidate == "" {
			candidate = dl.SavePath
		}
		if candidate == "" {
			return fmt.Errorf("download %d has not reported its storage path yet; wait before deleting", dl.ID)
		}
		candidate = storagePath(candidate, roots)
		if within(path, candidate) || within(candidate, path) {
			return fmt.Errorf("download %d still owns this path; finish or cancel it first", dl.ID)
		}
	}
	placements, err := s.db.PendingPlacements(ctx)
	if err != nil {
		return err
	}
	for _, p := range placements {
		for _, ref := range []string{p.Source, p.Target, p.Temporary, p.Backup} {
			if ref != "" && (within(path, ref) || within(ref, path)) {
				return fmt.Errorf("pending placement retains this path")
			}
		}
	}
	if err := rejectSymlinks(selected.Path, false); err != nil {
		return err
	}
	anchor, err := os.OpenRoot(selected.Path)
	if err != nil {
		return err
	}
	defer func() { _ = anchor.Close() }()
	anchored, err := anchor.Stat(".")
	if err != nil {
		return err
	}
	scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	fresh, _, err := scanCompletedSelection(scanCtx, selected.Path, path, nil, nil)
	cancel()
	if err != nil {
		return err
	}
	current, err := os.Stat(selected.Path)
	if err != nil || !os.SameFile(anchored, current) {
		return fmt.Errorf("storage root changed; scan again")
	}
	var match *CompletedEntry
	for i := range fresh {
		if fresh[i].Path == path {
			match = &fresh[i]
		}
	}
	if match == nil || match.Fingerprint != fingerprint {
		return fmt.Errorf("files changed since the scan; scan again before deleting")
	}
	rel, err := filepath.Rel(selected.Path, path)
	if err != nil || !filepath.IsLocal(rel) || rel == "." || rel == "completed" {
		return fmt.Errorf("cannot delete a storage root")
	}
	if err := s.db.AddHistory(ctx, "download_files_delete_requested", 0, filepath.Base(path), map[string]any{"path": path, "bytes": match.Bytes}); err != nil {
		return err
	}
	if err := anchor.RemoveAll(rel); err != nil {
		selected.Error = "delete incomplete: " + err.Error()
		raw, _ := json.Marshal(selected)
		_ = s.db.SaveCompletedScan(ctx, selected.Path, string(raw))
		return err
	}
	// Keep the latest classified report and remove only the confirmed entry.
	remaining := selected.Entries[:0]
	selected.Bytes = 0
	for _, entry := range selected.Entries {
		if entry.Path != path {
			remaining = append(remaining, entry)
			selected.Bytes += entry.Bytes
		}
	}
	selected.Entries = remaining
	raw, err := json.Marshal(selected)
	if err != nil {
		return err
	}
	if err := s.db.SaveCompletedScan(ctx, selected.Path, string(raw)); err != nil {
		return err
	}
	return s.db.AddHistory(ctx, "download_files_deleted", 0, filepath.Base(path), map[string]any{"path": path, "bytes": match.Bytes})
}
