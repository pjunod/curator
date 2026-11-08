package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Per-file, evidence-bound cleanup of a publication (pipeline contract v2,
// section 3.4; plan M4.1). This is the one way Curator deletes bytes from a
// Runner publication: after an import (on the cleanup lane), from the hourly
// sweep, and when a person discards a file in Resolve or Storage. A file
// leaves only when its recorded outcome is evidence for it:
//
//   - placed: the placement recorded the manifest's digest, and the library
//     file still has that digest now (same inode, or re-hashed);
//   - discarded (superseded, duplicate, rejected).
//
// unresolved and pending files are never touched. Before each unlink the
// publication generation (the manifest in the directory), the file's identity
// (size and digest against the manifest, then an unchanged inode generation
// since the hash) and the storage identity of the completed root are checked
// again; a root whose identity changed is an unmounted mount point and
// nothing under it is deleted. The directory goes when nothing is left in it
// but the manifest. Size alone is never evidence.

// publicationSweepBatch bounds one hourly pass.
const publicationSweepBatch = 50

// publicationCleanupResult says what one pass did, for logs and tests.
type publicationCleanupResult struct {
	Removed          int      // files unlinked by this pass
	Kept             []string // "<path>: <why>" for files whose evidence did not hold
	DirectoryRemoved bool
	Skipped          string // why the pass did not run at all
}

// cleanupPublication runs the per-file cleanup of one publication, holding
// its download row's lock (the importer holds the same lock while it places).
func (s *Service) cleanupPublication(ctx context.Context, publicationID int64) (publicationCleanupResult, error) {
	pub, err := s.db.GetPublication(ctx, publicationID)
	if err != nil {
		return publicationCleanupResult{}, err
	}
	if pub.DownloadID != 0 {
		unlock := s.lockDownload(pub.DownloadID)
		defer unlock()
	}
	return s.cleanupPublicationLocked(ctx, publicationID)
}

// afterPublicationImported starts the cleanup an import has just made due.
// The caller holds the download lock, so the inline fallback runs here.
func (s *Service) afterPublicationImported(ctx context.Context, publicationID int64) {
	if s.queuePublicationCleanup(publicationID) {
		return
	}
	if _, err := s.cleanupPublicationLocked(context.WithoutCancel(ctx), publicationID); err != nil {
		s.log.Info("publication cleanup incomplete; the hourly sweep retries", "publication", publicationID, "err", err)
	}
}

// sweepPublications is the hourly pass: the same rule, for publications
// whose import finished and that still have evidenced files on disk (or an
// empty directory left behind).
func (s *Service) sweepPublications(ctx context.Context) error {
	pubs, err := s.db.PublicationsToSweep(ctx, publicationSweepBatch)
	if err != nil {
		return err
	}
	removed, dirs := 0, 0
	for _, pub := range pubs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		res, err := s.cleanupPublication(ctx, pub.ID)
		if err != nil {
			s.log.Info("publication cleanup stopped", "publication", pub.ID, "path", pub.Path, "err", err)
		}
		removed += res.Removed
		if res.DirectoryRemoved {
			dirs++
		}
	}
	if removed > 0 || dirs > 0 {
		s.log.Info("cleanup: swept publications", "files_removed", removed, "directories_removed", dirs, "of", len(pubs))
	}
	return nil
}

// cleanupPublicationLocked is the implementation; the caller holds the
// download lock (or the publication has no row).
func (s *Service) cleanupPublicationLocked(ctx context.Context, publicationID int64) (publicationCleanupResult, error) {
	var res publicationCleanupResult
	pub, err := s.db.GetPublication(ctx, publicationID)
	if err != nil {
		return res, err
	}
	switch pub.State {
	case "removed":
		res.Skipped = "already removed"
		return res, nil
	case "discovered", "importing":
		res.Skipped = "the import owns this publication"
		return res, nil
	}
	if pub.Cohort == "historical" && pub.CleanupApprovedAt.IsZero() {
		res.Skipped = ErrHistoricalCohort.Error()
		return res, nil
	}
	root := filepath.Clean(pub.Path)
	if !filepath.IsAbs(root) || filepath.Dir(root) == root {
		return res, s.stopCleanup(ctx, pub, fmt.Errorf("publication path %q is not an absolute directory", pub.Path))
	}
	// Storage first: an unmounted share is an empty directory, not a payload
	// that is gone, and nothing below a root whose identity changed is read.
	if err := s.checkStorageIdentity(ctx, root); err != nil {
		return res, s.stopCleanup(ctx, pub, fmt.Errorf("storage identity check failed; nothing deleted: %w", err))
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return res, s.finishRemovedPublication(ctx, pub, 0, 0, "the publication directory was already gone")
	}
	if err != nil {
		return res, s.stopCleanup(ctx, pub, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return res, s.stopCleanup(ctx, pub, fmt.Errorf("publication path is not a plain directory"))
	}
	manifest, err := s.cleanupManifest(pub)
	if err != nil {
		return res, s.stopCleanup(ctx, pub, err)
	}
	files, err := s.db.ListPublicationFiles(ctx, pub.ID)
	if err != nil {
		return res, err
	}
	var item int64
	if pub.DownloadID != 0 {
		if dl, err := s.db.GetDownload(ctx, pub.DownloadID); err == nil {
			item = dl.MediaItemID
		}
	}
	var bytes int64
	for _, f := range files {
		if !f.RemovedAt.IsZero() || (f.Outcome != "placed" && f.Outcome != "discarded") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return res, err
		}
		kept, err := s.cleanupPublicationFile(ctx, pub, manifest, f, item)
		switch {
		case err != nil:
			// Transient (unreadable, changed while hashed, storage moved):
			// kept as it is, tried again by the next pass.
			res.Kept = append(res.Kept, f.Path+": "+err.Error())
		case kept != "":
			res.Kept = append(res.Kept, f.Path+": "+kept)
		default:
			res.Removed++
			bytes += f.Bytes
		}
	}
	if len(res.Kept) > 0 {
		summary := fmt.Sprintf("cleanup kept %d file(s): %s", len(res.Kept), strings.Join(firstN(res.Kept, 3), "; "))
		_ = s.db.SetPublicationError(ctx, pub.ID, summary)
		s.log.Info("publication cleanup kept files", "publication", pub.ID, "kept", res.Kept)
	}
	files, err = s.db.ListPublicationFiles(ctx, pub.ID)
	if err != nil {
		return res, err
	}
	for _, f := range files {
		if f.RemovedAt.IsZero() {
			// An unresolved, pending or kept file is still here: so is the
			// directory.
			if res.Removed > 0 {
				s.recordPublicationFilesRemoved(ctx, pub, res.Removed, bytes)
			}
			return res, nil
		}
	}
	removedDir, err := s.removeEmptyPublicationDirectory(ctx, pub, item)
	if err != nil {
		if res.Removed > 0 {
			s.recordPublicationFilesRemoved(ctx, pub, res.Removed, bytes)
		}
		return res, s.stopCleanup(ctx, pub, err)
	}
	res.DirectoryRemoved = removedDir
	return res, s.finishRemovedPublication(ctx, pub, res.Removed, bytes, "every file resolved and removed")
}

// cleanupManifest re-reads what the directory says it is now and requires the
// publication's generation. A directory whose manifest names another
// generation was replaced; nothing in it is ours to delete.
func (s *Service) cleanupManifest(pub sqlite.Publication) (map[string]ports.ManifestFile, error) {
	var m ports.Manifest
	raw, err := os.ReadFile(filepath.Join(pub.Path, ports.ManifestFileName))
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("%s is unreadable: %w", ports.ManifestFileName, err)
		}
		if pub.Generation != "" && m.Generation != pub.Generation {
			return nil, fmt.Errorf("the directory's manifest names generation %q, not this publication's %q; nothing deleted", m.Generation, pub.Generation)
		}
	case errors.Is(err, os.ErrNotExist):
		if pub.ManifestSource == "file" {
			return nil, fmt.Errorf("the directory's %s is missing; nothing deleted", ports.ManifestFileName)
		}
		// A v1 delivery record (or no manifest at all, for a directory the
		// cutover froze): the stored manifest is the evidence.
		if pub.ManifestJSON != "" {
			_ = json.Unmarshal([]byte(pub.ManifestJSON), &m)
		}
		if pub.Generation != "" && m.Generation != "" && m.Generation != pub.Generation {
			return nil, fmt.Errorf("the stored manifest names generation %q, not %q; nothing deleted", m.Generation, pub.Generation)
		}
	default:
		return nil, err
	}
	out := make(map[string]ports.ManifestFile, len(m.Files))
	for _, f := range m.Files {
		out[f.Path] = f
	}
	return out, nil
}

// cleanupPublicationFile verifies one file's evidence and unlinks it. kept
// names a definitive reason the file stays (it is then unresolved, shown in
// Resolve, and not re-read by later passes); err is a transient one.
func (s *Service) cleanupPublicationFile(ctx context.Context, pub sqlite.Publication, manifest map[string]ports.ManifestFile, f sqlite.PublicationFile, item int64) (kept string, err error) {
	demote := func(reason string) (string, error) {
		if err := s.db.DemotePublicationFile(ctx, pub.ID, f.Path, f.Outcome, reason); err != nil {
			return "", err
		}
		return reason, nil
	}
	mf, ok := manifest[f.Path]
	if !ok || !safeDeliveryRelativePath(f.Path) {
		return demote("kept: the publication's manifest no longer lists this file")
	}
	if mf.SHA256 == "" || f.SHA256 == "" || mf.SHA256 != f.SHA256 || mf.Bytes != f.Bytes {
		return demote("kept: the manifest records no matching digest for this file (size alone is not evidence)")
	}
	if f.Outcome == "placed" && (f.PlacedSHA256 != f.SHA256 || f.LibraryPath == "" || !filepath.IsAbs(f.LibraryPath)) {
		return demote("kept: the placement recorded no matching digest or library path")
	}
	base, err := os.OpenRoot(pub.Path)
	if err != nil {
		return "", err
	}
	defer func() { _ = base.Close() }()
	rel := filepath.FromSlash(f.Path)
	parent, file, named, err := openDeliveryFileNoSymlinks(base, rel)
	if errors.Is(err, os.ErrNotExist) {
		// The storage under the root is verified and the file is not there:
		// whoever removed it, it is gone.
		return "", s.db.MarkPublicationFileRemoved(ctx, pub.ID, f.Path, f.Outcome)
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = parent.Close() }()
	generation := deliveryFileGeneration(named)
	if named.Size() != f.Bytes {
		_ = file.Close()
		return demote(fmt.Sprintf("kept: the file is %d bytes, the manifest says %d", named.Size(), f.Bytes))
	}
	digest, n, err := digestOpenFile(ctx, file)
	_ = file.Close()
	if err != nil {
		return "", err
	}
	if n != f.Bytes || digest != f.SHA256 {
		return demote("kept: the file's content differs from its manifest digest")
	}
	var libGeneration string
	if f.Outcome == "placed" {
		lib, err := os.Lstat(f.LibraryPath)
		if errors.Is(err, os.ErrNotExist) {
			return demote("kept: the library file it was placed as (" + f.LibraryPath + ") is missing")
		}
		if err != nil {
			return "", err
		}
		if !os.SameFile(lib, named) {
			libDigest, libSize, gen, err := fileDigestSnapshot(ctx, f.LibraryPath)
			if err != nil {
				return "", err
			}
			if libSize != f.Bytes || libDigest != f.SHA256 {
				return demote("kept: the library file at " + f.LibraryPath + " differs from the manifest digest; this copy may be the only good one")
			}
			libGeneration = gen
		}
	}
	// The destructive step: serialized with imports into the same item and
	// with storage decisions, every identity re-checked, then one unlink.
	ctx, releaseTarget := s.lockImportTarget(ctx, item)
	defer releaseTarget()
	ctx, releaseStorage := s.lockStorageDecision(ctx)
	defer releaseStorage()
	if err := s.checkStorageIdentity(ctx, pub.Path); err != nil {
		return "", fmt.Errorf("storage identity check failed; nothing deleted: %w", err)
	}
	if _, err := s.cleanupManifest(pub); err != nil {
		return "", err
	}
	name := filepath.Base(rel)
	now, err := parent.Lstat(name)
	if err != nil {
		return "", err
	}
	if now.Mode()&os.ModeSymlink != 0 || !now.Mode().IsRegular() || deliveryFileGeneration(now) != generation || generation == "" {
		return "", fmt.Errorf("the file changed after it was verified")
	}
	if libGeneration != "" {
		lib, err := os.Lstat(f.LibraryPath)
		if err != nil || deliveryFileGeneration(lib) != libGeneration {
			return "", fmt.Errorf("the library file changed after it was verified")
		}
	}
	if err := parent.Remove(name); err != nil {
		return "", err
	}
	_ = syncRootDirectory(parent, ".")
	// The download lock held by every caller keeps Resolve from changing the
	// outcome between the verification and this record.
	return "", s.db.MarkPublicationFileRemoved(ctx, pub.ID, f.Path, f.Outcome)
}

// removeEmptyPublicationDirectory removes the publication's empty
// subdirectories, then its manifest and the directory itself, when nothing
// else is left in it. Anything Curator cannot account for keeps it.
func (s *Service) removeEmptyPublicationDirectory(ctx context.Context, pub sqlite.Publication, item int64) (bool, error) {
	base, err := os.OpenRoot(pub.Path)
	if err != nil {
		return false, err
	}
	defer func() { _ = base.Close() }()
	var others []string
	var walk func(dir string) (bool, error)
	walk = func(dir string) (bool, error) {
		f, err := base.Open(dir)
		if err != nil {
			return false, err
		}
		entries, err := f.ReadDir(-1)
		_ = f.Close()
		if err != nil {
			return false, err
		}
		empty := true
		for _, e := range entries {
			rel := filepath.Join(dir, e.Name())
			if dir == "." && e.Name() == ports.ManifestFileName && e.Type().IsRegular() {
				continue
			}
			if e.IsDir() && e.Type()&os.ModeSymlink == 0 {
				sub, err := walk(rel)
				if err != nil {
					return false, err
				}
				if sub {
					if err := base.Remove(rel); err != nil {
						return false, err
					}
					continue
				}
			} else {
				others = append(others, rel)
			}
			empty = false
		}
		return empty, nil
	}
	empty, err := walk(".")
	if err != nil {
		return false, err
	}
	if !empty {
		return false, fmt.Errorf("the directory holds %d entr(ies) no manifest file accounts for (%s); kept", len(others), strings.Join(firstN(others, 3), ", "))
	}
	ctx, releaseTarget := s.lockImportTarget(ctx, item)
	defer releaseTarget()
	ctx, releaseStorage := s.lockStorageDecision(ctx)
	defer releaseStorage()
	if err := s.checkStorageIdentity(ctx, pub.Path); err != nil {
		return false, fmt.Errorf("storage identity check failed; nothing deleted: %w", err)
	}
	if _, err := s.cleanupManifest(pub); err != nil {
		return false, err
	}
	if err := base.Remove(ports.ManifestFileName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	// rmdir, never a recursive delete: a file that appeared since the walk
	// keeps the directory.
	if err := os.Remove(pub.Path); err != nil {
		return false, err
	}
	if parent, err := os.OpenRoot(filepath.Dir(pub.Path)); err == nil {
		_ = syncRootDirectory(parent, ".")
		_ = parent.Close()
	}
	return true, nil
}

// stopCleanup records why a pass deleted nothing more and returns the error.
func (s *Service) stopCleanup(ctx context.Context, pub sqlite.Publication, err error) error {
	_ = s.db.SetPublicationError(ctx, pub.ID, "cleanup stopped: "+err.Error())
	return err
}

func (s *Service) recordPublicationFilesRemoved(ctx context.Context, pub sqlite.Publication, files int, bytes int64) {
	mediaItemID, title := s.publicationOwner(ctx, pub)
	_ = s.db.AddHistory(ctx, HistoryPayloadRemoved, mediaItemID, title,
		map[string]any{"publication": pub.Path, "files": files, "bytes": bytes, "by": "per-file cleanup"})
}

// finishRemovedPublication marks a publication removed once its directory is
// gone, with one history row for what this pass removed.
func (s *Service) finishRemovedPublication(ctx context.Context, pub sqlite.Publication, files int, bytes int64, why string) error {
	if err := s.db.MarkPublicationRemoved(ctx, pub.ID); err != nil {
		return err
	}
	mediaItemID, title := s.publicationOwner(ctx, pub)
	_ = s.db.AddHistory(ctx, HistoryPayloadRemoved, mediaItemID, title,
		map[string]any{"publication": pub.Path, "files": files, "bytes": bytes, "by": "per-file cleanup", "directory": "removed", "why": why})
	s.log.Info("cleanup: publication removed", "publication", pub.ID, "path", pub.Path, "files", files, "bytes", bytes, "why", why)
	return nil
}

func (s *Service) publicationOwner(ctx context.Context, pub sqlite.Publication) (int64, string) {
	if pub.DownloadID != 0 {
		if dl, err := s.db.GetDownload(ctx, pub.DownloadID); err == nil {
			return dl.MediaItemID, dl.ReleaseTitle
		}
	}
	return 0, filepath.Base(pub.Path)
}

func firstN(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return items[:n]
}

// ApprovePublicationCleanup is the M7 approval of a review batch: the named
// historical publications become subject to the per-file rule, and each is
// cleaned up now. Live publications need no approval and are not changed.
func (s *Service) ApprovePublicationCleanup(ctx context.Context, ids []int64) (int64, error) {
	n, err := s.db.ApprovePublicationCleanup(ctx, ids)
	if err != nil {
		return 0, err
	}
	_ = s.db.AddHistory(ctx, "publication_cleanup_approved", 0, fmt.Sprintf("%d publication(s)", n), map[string]any{"ids": ids, "approved": n})
	for _, id := range ids {
		if !s.queuePublicationCleanup(id) {
			if _, err := s.cleanupPublication(ctx, id); err != nil {
				s.log.Info("approved publication cleanup incomplete; the hourly sweep retries", "publication", id, "err", err)
			}
		}
	}
	return n, nil
}

// DiscardPublicationFiles is the operator's "delete this leftover" on a
// publication-managed directory in Storage: each selected file must be an
// unresolved (or never imported) file of the publication claiming the
// directory; it becomes discarded{rejected} and leaves through the same
// per-file cleanup, with the same checks. A historical publication waits for
// its review batch, as automatic deletion does.
func (s *Service) DiscardPublicationFiles(ctx context.Context, dir string, selections []CompletedFileSelection) error {
	ctx = withForegroundVerification(ctx)
	if len(selections) == 0 {
		return fmt.Errorf("select one or more files to delete")
	}
	pub, ok, err := s.db.PublicationByPath(ctx, filepath.Clean(dir))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no publication claims %s", dir)
	}
	if pub.Cohort == "historical" && pub.CleanupApprovedAt.IsZero() {
		return ErrHistoricalCohort
	}
	if pub.DownloadID != 0 {
		unlock := s.lockDownload(pub.DownloadID)
		defer unlock()
	}
	files, err := s.db.ListPublicationFiles(ctx, pub.ID)
	if err != nil {
		return err
	}
	byPath := map[string]sqlite.PublicationFile{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	var chosen []sqlite.PublicationFile
	for _, sel := range selections {
		rel, err := filepath.Rel(pub.Path, filepath.Clean(sel.Path))
		if err != nil || !filepath.IsLocal(rel) {
			return fmt.Errorf("%s is not inside the publication", sel.Path)
		}
		f, ok := byPath[filepath.ToSlash(rel)]
		if !ok {
			return fmt.Errorf("%s is not a manifest file of this publication; nothing deleted", rel)
		}
		if f.Outcome != "unresolved" && f.Outcome != "pending" && f.Outcome != "discarded" {
			return fmt.Errorf("%s is %s; only unresolved files can be discarded", rel, f.Outcome)
		}
		chosen = append(chosen, f)
	}
	for _, f := range chosen {
		if f.Outcome == "discarded" {
			continue
		}
		f.Outcome, f.DiscardReason, f.Reason = "discarded", "rejected", "discarded by an operator in Storage"
		if err := s.db.SetPublicationFileOutcome(ctx, f); err != nil {
			return err
		}
	}
	mediaItemID, title := s.publicationOwner(ctx, pub)
	_ = s.db.AddHistory(ctx, "publication_file_resolved", mediaItemID, title,
		map[string]any{"publication": pub.Path, "files": len(chosen), "action": "discard"})
	res, err := s.cleanupPublicationLocked(ctx, pub.ID)
	if err != nil {
		return err
	}
	if len(res.Kept) > 0 {
		return fmt.Errorf("discarded, but kept: %s", strings.Join(res.Kept, "; "))
	}
	if res.Skipped != "" {
		return fmt.Errorf("discarded; deletion waits: %s", res.Skipped)
	}
	return s.ScanCompleted(ctx)
}
