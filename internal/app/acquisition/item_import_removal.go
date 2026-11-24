package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// Remove original (Import file… on the item page). A standalone manual
// import may take each source away once its library copy is in place: on
// one filesystem the library file is a hard link, so this is a rename;
// across volumes it is a verified copy and an unlink. The removal is
// journaled in the download's trace before the import completes (pending)
// and after it is done (result), so a restart in between finishes it, and
// what was kept, and why, is on the row.

const (
	stepOriginalsPending = "originals_removal_pending"
	stepOriginalsDone    = "originals_removal_done"
	// HistoryManualOriginalsRemoved records what Remove original did.
	HistoryManualOriginalsRemoved = "manual_import_originals_removed"
)

// removalIdentity is a source file's identity for Remove original: the
// object, its size and its modification time. It ignores the change time on
// purpose: hard-linking the source into the library changes that, and is
// exactly what the import does.
func removalIdentity(info os.FileInfo) string {
	return fmt.Sprintf("%s:%d:%d", deliveryFileStableIdentity(info), info.Size(), info.ModTime().UnixNano())
}

// removeOriginalRefusal says why a source may not be removed by Remove
// original: it is a file the library tracks, or it lies in a library item's
// folder. "" when it may.
func (s *Service) removeOriginalRefusal(ctx context.Context, path string, folders []string) (string, error) {
	path = filepath.Clean(path)
	if _, err := s.db.GetMediaFileCustodyByPath(ctx, path); err == nil {
		return path + " is a file in the library", nil
	} else if !errors.Is(err, sqlite.ErrNotFound) {
		return "", err
	}
	for _, folder := range folders {
		folder = filepath.Clean(folder)
		if path == folder || within(folder, path) {
			return path + " is inside the library folder " + folder, nil
		}
	}
	return "", nil
}

// recordPendingRemoval journals the removal a completed manual import owes,
// before the import is marked complete.
func (s *Service) recordPendingRemoval(ctx context.Context, dl *sqlite.Download, entry sqlite.HandoffEntry, result ImportResult) error {
	pending := sqlite.HandoffEntry{Step: stepOriginalsPending, At: time.Now().UnixMilli(),
		Detail: "Remove original: removing the originals once their library copies are confirmed"}
	for _, f := range result.Files {
		if !f.Imported || f.Path == "" || f.SourcePath == "" || filepath.Clean(f.Path) == filepath.Clean(f.SourcePath) {
			continue
		}
		pending.Removals = append(pending.Removals, sqlite.HandoffRemoval{
			Source: f.SourcePath, Library: f.Path, Identity: entry.SourceIdentities[filepath.Clean(f.SourcePath)],
		})
	}
	pending.Removals = append(pending.Removals, result.Sidecars...)
	if len(pending.Removals) == 0 {
		return nil
	}
	if err := s.db.AppendHandoffEntry(ctx, dl.ID, pending); err != nil {
		return err
	}
	dl.Handoff = append(dl.Handoff, pending)
	return nil
}

// pendingRemoval returns the newest Remove original a row still owes.
func pendingRemoval(handoff []sqlite.HandoffEntry) (sqlite.HandoffEntry, bool) {
	for i := len(handoff) - 1; i >= 0; i-- {
		switch handoff[i].Step {
		case stepOriginalsDone:
			return sqlite.HandoffEntry{}, false
		case stepOriginalsPending:
			return handoff[i], true
		}
	}
	return sqlite.HandoffEntry{}, false
}

// finishOriginalRemoval carries out a row's pending Remove original under
// the item's import lock and the storage-decision lock: each source goes
// only while it is still the file that was chosen and its library copy is
// the same object (a hard link) or the same bytes (a copy). Anything else is
// kept, with the reason on the row.
func (s *Service) finishOriginalRemoval(ctx context.Context, downloadID int64) {
	dl, err := s.db.GetDownload(ctx, downloadID)
	if err != nil {
		return
	}
	pending, ok := pendingRemoval(dl.Handoff)
	if !ok || dl.State != "imported" {
		return
	}
	lockCtx, releaseTarget := s.lockImportTarget(ctx, dl.MediaItemID)
	defer releaseTarget()
	lockCtx, releaseStorage := s.lockStorageDecision(lockCtx)
	defer releaseStorage()
	var removed, kept []string
	for _, r := range pending.Removals {
		if lockCtx.Err() != nil {
			kept = append(kept, r.Source+": stopped")
			continue
		}
		if why := s.removeOneOriginal(lockCtx, r); why != "" {
			kept = append(kept, r.Source+": "+why)
			continue
		}
		removed = append(removed, r.Source)
	}
	detail := fmt.Sprintf("Remove original: removed %d original(s)", len(removed))
	if len(kept) > 0 {
		detail += fmt.Sprintf("; original kept: %s", strings.Join(kept, "; "))
	}
	done := sqlite.HandoffEntry{Step: stepOriginalsDone, At: time.Now().UnixMilli(), Detail: detail}
	bg := context.WithoutCancel(ctx)
	if err := s.db.AppendHandoffEntry(bg, dl.ID, done); err != nil {
		s.log.Warn("manual import: Remove original result not recorded", "download", dl.ID, "err", err)
	}
	_ = s.db.AddHistory(bg, HistoryManualOriginalsRemoved, dl.MediaItemID, dl.ReleaseTitle,
		map[string]any{"removed": removed, "kept": kept})
	s.log.Info("manual import: Remove original", "download", dl.ID, "removed", len(removed), "kept", len(kept))
}

// removeOneOriginal removes one source, or says why it stays.
func (s *Service) removeOneOriginal(ctx context.Context, r sqlite.HandoffRemoval) string {
	src, err := os.Lstat(r.Source)
	if errors.Is(err, os.ErrNotExist) {
		return "" // already gone (a resumed removal)
	}
	if err != nil {
		return err.Error()
	}
	if !src.Mode().IsRegular() {
		return "it is no longer a regular file"
	}
	if r.Identity == "" || removalIdentity(src) != r.Identity {
		return "it changed after it was chosen"
	}
	lib, err := os.Lstat(r.Library)
	if err != nil || !lib.Mode().IsRegular() {
		return "its library copy is not in place"
	}
	if !os.SameFile(src, lib) {
		same, err := sameFileBytes(ctx, r.Source, r.Library)
		if err != nil {
			return "its library copy could not be compared: " + err.Error()
		}
		if !same {
			return "its library copy differs from it"
		}
	}
	if err := os.Remove(r.Source); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err.Error()
	}
	_ = syncPath(filepath.Dir(r.Source))
	return ""
}

// resumeOriginalRemovals finishes the Remove original a previous process
// journaled but did not get to.
func (s *Service) resumeOriginalRemovals(ctx context.Context) {
	rows, err := s.db.ListRecentDownloads(ctx)
	if err != nil {
		return
	}
	for _, dl := range rows {
		if ctx.Err() != nil {
			return
		}
		if dl.Protocol != "manual" || dl.State != "imported" {
			continue
		}
		if _, ok := pendingRemoval(dl.Handoff); ok {
			s.finishOriginalRemoval(ctx, dl.ID)
		}
	}
}

// removeOriginalAdmission refuses Remove original for any selected file that
// is part of a runner delivery, a library file, or inside a library item's
// folder, and otherwise returns each file's identity to seal with the
// admission. The caller holds the import-target and storage-decision locks.
func (s *Service) removeOriginalAdmission(ctx context.Context, paths []string) (map[string]string, error) {
	folders, err := s.db.LibraryFolders(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(paths))
	for _, path := range paths {
		if _, claimed, err := s.publicationClaiming(ctx, path); err != nil {
			return nil, err
		} else if claimed {
			return nil, fmt.Errorf("%s is part of a runner delivery; place it through Resolve, which cleans it up under its own rules", path)
		}
		why, err := s.removeOriginalRefusal(ctx, path, folders)
		if err != nil {
			return nil, err
		}
		if why != "" {
			return nil, fmt.Errorf("remove original refused: %s", why)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		out[filepath.Clean(path)] = removalIdentity(info)
	}
	return out, nil
}
