package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DeleteCompletedEntry is an explicit operator decision about Curator-owned
// storage. A cached path alone is not authority: repeat ownership, live-work,
// mount identity and recursive metadata checks before the rooted deletion.
func (s *Service) DeleteCompletedEntry(ctx context.Context, path, fingerprint string) error {
	// An operator is waiting on this request: its verification reads are
	// foreground and are not held to the background budget.
	ctx = withForegroundVerification(ctx)
	s.completedMu.Lock()
	defer s.completedMu.Unlock()
	// Path-addressed: the payload's owners are not known yet, so this takes
	// the whole library, waiting briefly rather than hanging the request
	// behind a long placement.
	// The hourly partial-payload sweep holds completedMu and a download lock
	// here, so it must not sit polling: it tries briefly and comes back next
	// hour. An operator's request waits a little longer.
	wait := 15 * time.Second
	if ctx.Value(partialCleanupIDKey{}) != nil {
		wait = 250 * time.Millisecond
	}
	ctx, releaseTargets, err := s.lockAllImportTargets(ctx, wait)
	if err != nil {
		return err
	}
	defer releaseTargets()
	ctx, releaseStorage := s.lockStorageDecision(ctx)
	defer releaseStorage()
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
	for _, entry := range selected.Entries {
		if entry.Path == path && entry.DeliveryManaged {
			return fmt.Errorf("runner payloads require explicit selected-file deletion against the accepted manifest")
		}
		if entry.Path == path && entry.LegacyReview {
			return fmt.Errorf("pre-delivery backlog requires exact file review; whole-payload deletion is unavailable")
		}
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
	partialOwner, isPartialCleanup := ctx.Value(partialCleanupIDKey{}).(partialCleanupOwner)
	if isPartialCleanup {
		dl, err := s.db.GetDownload(ctx, partialOwner.DownloadID)
		if err != nil || dl.GenerationKey != partialOwner.GenerationKey || dl.DeliveryExpected {
			return fmt.Errorf("delivery-managed payloads require manifest-ledger cleanup")
		}
	}
	for _, dl := range active {
		if isPartialCleanup && dl.ID == partialOwner.DownloadID &&
			dl.GenerationKey == partialOwner.GenerationKey && dl.State == "failed" &&
			dl.CleanupPending && filepath.Join(canonicalCompletedPath(filepath.Dir(dl.ImportPath)), filepath.Base(dl.ImportPath)) == filepath.Clean(path) {
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
	if err := s.rejectReferencedPayload(ctx, path, roots); err != nil {
		return err
	}
	placements, err := s.db.UnresolvedPlacementReferences(ctx)
	if err != nil {
		return err
	}
	for _, p := range placements {
		for _, ref := range []string{p.Source, p.Target, p.Temporary, p.Backup} {
			if ref != "" {
				ref = storagePath(ref, roots)
			}
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
	legacyPaths, err := s.db.LegacyCleanupReviewPaths(ctx)
	if err != nil {
		return err
	}
	for _, legacyPath := range legacyPaths {
		legacyPath = storagePath(legacyPath, roots)
		if within(path, legacyPath) || within(legacyPath, path) {
			return fmt.Errorf("payload is in the legacy review cohort; review it before deleting")
		}
	}
	if isPartialCleanup {
		// The automatic partial path is authorized only for the exact failed
		// generation that still owns this payload. Repeat the cohort check at
		// the destructive boundary so stale selection results cannot bypass it.
		current, err := s.db.GetDownload(ctx, partialOwner.DownloadID)
		if err != nil || current.GenerationKey == "" || current.GenerationKey != partialOwner.GenerationKey ||
			current.State != "failed" || !current.CleanupPending || current.ImportPath == "" ||
			storagePath(current.ImportPath, roots) != storagePath(path, roots) {
			return fmt.Errorf("partial cleanup generation changed; review the payload before deleting")
		}
		if s.legacyCleanupReview(ctx, downloadRef{
			ID: current.ID, GenerationKey: current.GenerationKey, ImportPath: current.ImportPath,
		}) {
			return fmt.Errorf("payload is in the legacy review cohort; review it before deleting")
		}
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

// DeleteSelectedLegacyFiles is an explicit, per-file approval path for the
// migration-era review cohort. It relies on a fresh inventory identity and
// the retained legacy path fence; it does not invent or infer a Runner
// manifest for historical files.
func (s *Service) DeleteSelectedLegacyFiles(ctx context.Context, path, fingerprint string, selections []CompletedFileSelection) error {
	// An operator is waiting on this request: its verification reads are
	// foreground and are not held to the background budget.
	ctx = withForegroundVerification(ctx)
	if len(selections) == 0 || !filepath.IsAbs(path) || fingerprint == "" {
		return fmt.Errorf("select exact files from a legacy review entry")
	}
	s.completedMu.Lock()
	defer s.completedMu.Unlock()
	// Path-addressed: the payload's owners are not known yet, so this takes
	// the whole library, waiting briefly rather than hanging the request
	// behind a long placement.
	ctx, releaseTargets, err := s.lockAllImportTargets(ctx, 15*time.Second)
	if err != nil {
		return err
	}
	defer releaseTargets()
	ctx, releaseStorage := s.lockStorageDecision(ctx)
	defer releaseStorage()
	if !s.ownsDownloadPath(ctx, path) {
		return fmt.Errorf("select an entry inside Curator-owned download storage")
	}
	// Refresh association and storage facts while holding the same mutation
	// locks as import and cleanup. Cached inventory alone is not deletion authority.
	if err := s.scanCompletedLocked(ctx); err != nil {
		return err
	}
	inventory, err := s.CompletedInventory(ctx)
	if err != nil {
		return err
	}
	var selected *CompletedEntry
	var rootPath string
	for _, root := range inventory.Roots {
		for i := range root.Entries {
			entry := &root.Entries[i]
			if entry.Path == path && entry.Fingerprint == fingerprint && entry.LegacyReview && !entry.DeliveryManaged {
				selected, rootPath = entry, root.Path
			}
		}
	}
	if selected == nil {
		return fmt.Errorf("selected entry is not an unchanged legacy review payload")
	}
	for _, root := range inventory.Roots {
		if root.Path == rootPath && root.Error != "" {
			return fmt.Errorf("inventory is incomplete; scan again before deleting selected files")
		}
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return err
	}
	root, payloadRelative, err := deliveryRootAndRelative(path, roots)
	if err != nil {
		return err
	}
	if err := s.checkStorageIdentity(ctx, path); err != nil {
		return err
	}
	active, err := s.db.ListDownloadReservations(ctx)
	if err != nil {
		return err
	}
	reviewedPath := storagePath(path, roots)
	for _, dl := range active {
		candidate := dl.ImportPath
		if candidate == "" {
			candidate = dl.SavePath
		}
		if candidate == "" {
			continue
		}
		candidate = storagePath(candidate, roots)
		if candidate != "" && (within(reviewedPath, candidate) || within(candidate, reviewedPath)) && dl.State != "failed" && dl.State != "imported" {
			return fmt.Errorf("download %d still owns this path; finish or cancel it first", dl.ID)
		}
	}
	placements, err := s.db.UnresolvedPlacementReferences(ctx)
	if err != nil {
		return err
	}
	for _, p := range placements {
		for _, candidate := range []string{p.Source, p.Target, p.Temporary, p.Backup} {
			if candidate == "" {
				continue
			}
			candidate = storagePath(candidate, roots)
			if within(reviewedPath, candidate) || within(candidate, reviewedPath) {
				return fmt.Errorf("pending placement retains this path")
			}
		}
	}
	anchor, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = anchor.Close() }()
	payload, err := openDeliveryDirectoryNoSymlinks(anchor, payloadRelative)
	if err != nil {
		return err
	}
	defer func() { _ = payload.Close() }()
	payloadInfo, err := payload.Stat(".")
	if err != nil || !payloadInfo.IsDir() {
		return fmt.Errorf("legacy payload directory changed")
	}
	manifest := make(map[string]CompletedReviewFile, len(selected.ReviewFiles))
	for _, file := range selected.ReviewFiles {
		// Review inventory paths are absolute, while selections are converted
		// to payload-relative names before they are opened through the anchor.
		// Store both in the same normalized namespace so the exact snapshot
		// check below does not reject every valid selection.
		rel, err := filepath.Rel(path, file.Path)
		if err != nil || !filepath.IsLocal(rel) || rel == "." {
			continue
		}
		manifest[filepath.ToSlash(rel)] = file
	}
	type chosenFile struct {
		rel       string
		selection CompletedFileSelection
		info      os.FileInfo
	}
	chosen := make([]chosenFile, 0, len(selections))
	seen := map[string]bool{}
	for _, selection := range selections {
		if !filepath.IsAbs(selection.Path) || !within(path, selection.Path) || selection.SnapshotIdentity == "" {
			return fmt.Errorf("selected file is outside the reviewed legacy payload")
		}
		rel, err := filepath.Rel(path, selection.Path)
		if err != nil || !filepath.IsLocal(rel) || rel == "." || !safeDeliveryRelativePath(filepath.ToSlash(rel)) {
			return fmt.Errorf("selected path is not a safe reviewed file")
		}
		rel = filepath.ToSlash(rel)
		if seen[rel] {
			return fmt.Errorf("selected file appears more than once")
		}
		seen[rel] = true
		reviewed, ok := manifest[rel]
		if !ok || reviewed.SnapshotIdentity == "" || reviewed.SnapshotIdentity != selection.SnapshotIdentity {
			return fmt.Errorf("selected file was not in the reviewed inventory: %s", rel)
		}
		legacy, err := s.legacyReviewFileFenced(ctx, selection.Path, roots)
		if err != nil || !legacy {
			return fmt.Errorf("legacy review fence is unavailable for %s; refusing deletion", rel)
		}
		if err := s.rejectReferencedPayload(ctx, selection.Path, roots); err != nil {
			return err
		}
		parent, file, named, err := openDeliveryFileNoSymlinks(payload, filepath.FromSlash(rel))
		if err != nil {
			return err
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		parentErr := parent.Close()
		if statErr != nil || closeErr != nil || parentErr != nil {
			return errors.Join(statErr, closeErr, parentErr)
		}
		if !info.Mode().IsRegular() || !os.SameFile(named, info) || completedReviewFileIdentity(selection.Path, info) != selection.SnapshotIdentity {
			return fmt.Errorf("selected file changed since the inventory scan: %s", rel)
		}
		chosen = append(chosen, chosenFile{rel: rel, selection: selection, info: info})
	}
	for _, file := range chosen {
		if err := s.checkStorageIdentity(ctx, path); err != nil {
			return err
		}
		currentPayload, err := anchor.Lstat(payloadRelative)
		if err != nil || !currentPayload.IsDir() || currentPayload.Mode()&os.ModeSymlink != 0 || !os.SameFile(payloadInfo, currentPayload) {
			return fmt.Errorf("legacy payload directory changed before deletion")
		}
		parentRel := filepath.Dir(filepath.FromSlash(file.rel))
		parent, err := openDeliveryDirectoryNoSymlinks(payload, parentRel)
		if err != nil {
			return err
		}
		name := filepath.Base(filepath.FromSlash(file.rel))
		current, err := parent.Lstat(name)
		if err != nil {
			_ = parent.Close()
			return err
		}
		if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() || !os.SameFile(file.info, current) || completedReviewFileIdentity(file.selection.Path, current) != file.selection.SnapshotIdentity {
			_ = parent.Close()
			return fmt.Errorf("selected legacy file changed before deletion: %s", file.rel)
		}
		legacy, fenceErr := s.legacyReviewFileFenced(ctx, file.selection.Path, roots)
		if fenceErr != nil || !legacy {
			_ = parent.Close()
			return fmt.Errorf("legacy review fence changed before deletion")
		}
		if err := s.rejectReferencedPayload(ctx, file.selection.Path, roots); err != nil {
			_ = parent.Close()
			return err
		}
		if err := parent.Remove(name); err != nil {
			_ = parent.Close()
			return err
		}
		if err := parent.Close(); err != nil {
			return err
		}
		if err := syncPath(filepath.Dir(file.selection.Path)); err != nil {
			return err
		}
		if err := s.db.AddHistory(ctx, "legacy_review_file_deleted", 0, filepath.Base(file.selection.Path), map[string]any{"path": file.selection.Path, "snapshotIdentity": file.selection.SnapshotIdentity, "reviewedPath": path}); err != nil {
			return err
		}
	}
	return nil
}

// File associations protect payload bytes even when a legacy file was recorded
// outside a registered library root. Compare filesystem aliases consistently.
func (s *Service) rejectReferencedPayload(ctx context.Context, path string, roots []string) error {
	rows, err := s.db.R.QueryContext(ctx, `SELECT path FROM media_files`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ref string
		if err = rows.Scan(&ref); err != nil {
			return err
		}
		ref = storagePath(ref, roots)
		if within(path, ref) || within(ref, path) {
			return fmt.Errorf("library file retains this payload")
		}
	}
	return rows.Err()
}

// legacyReviewFileFenced decides one selected file from durable records,
// not the cached inventory. Inventory entries are top-level folders and may
// mix a fenced historical payload with ordinary downloads beside it, so the
// entry's classification (and a fence anywhere above or below it) is no
// authority for every file in it. The most specific record containing the
// file decides: a historical review path (including storage-change fences)
// or a legacy-review receipt fences it; any other receipt (an ordinary
// download, failed or imported) does not. A file no record contains is not
// fenced.
func (s *Service) legacyReviewFileFenced(ctx context.Context, file string, roots []string) (bool, error) {
	target := storagePath(file, roots)
	best, fenced := -1, false
	consider := func(recordPath string, legacy bool) {
		recordPath = storagePath(recordPath, roots)
		if recordPath == "" || !within(recordPath, target) {
			return
		}
		depth := len(filepath.Clean(recordPath))
		switch {
		case depth > best:
			best, fenced = depth, legacy
		case depth == best && !legacy:
			fenced = false
		}
	}
	paths, err := s.db.LegacyCleanupReviewPaths(ctx)
	if err != nil {
		return false, err
	}
	for _, legacyPath := range paths {
		consider(legacyPath, true)
	}
	receipts, err := s.db.CompletedReceipts(ctx)
	if err != nil {
		return false, err
	}
	for _, receipt := range receipts {
		consider(receipt.Path, receipt.LegacyReview)
	}
	return best >= 0 && fenced, nil
}
