package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RemoveFileRequest preserves the old remove endpoint's shape. Bad and quality
// replacement decisions use the reviewed replacement workflow; Search and
// Blocklist remain for wire compatibility and are rejected below.
type RemoveFileRequest struct {
	// FromDisk deletes the bytes, not just the row. Default false is the
	// cautious reading of an ambiguous request; the UI sets it explicitly.
	FromDisk bool
	// Deprecated: use the reviewed replacement workflow to blocklist a source.
	Blocklist bool
	// Deprecated: replacement acquisition requires an explicit reviewed scope.
	Search bool
	// Reason is what the user (or the UI) says about it, kept in history.
	Reason string
}

// RemoveFileResult reports what actually happened, so the UI can say so rather
// than assuming every part succeeded.
type RemoveFileResult struct {
	Path            string `json:"path"`
	DeletedFromDisk bool   `json:"deletedFromDisk"`
	Blocklisted     string `json:"blocklisted,omitempty"` // release title, empty if none
	Searched        bool   `json:"searched"`
	// Note carries the one thing that did not work out, in the user's terms:
	// most often "monarr did not grab this file, so there is nothing to
	// blocklist". Not an error — the deletion still happened.
	Note string `json:"note,omitempty"`
}

// RemoveFile removes one library association, and optionally its bytes. Bad or
// replacement decisions must use the reviewed replacement workflow.
//
// The row goes first and the bytes second, and the order matters: if the
// unlink fails (a read-only mount, a permission), the row is already gone and
// the next scan will find the file and re-adopt it — recoverable, and visible.
// The reverse order can leave a row pointing at nothing, which reads as "you
// have this file" forever.
func (s *Service) RemoveFile(ctx context.Context, itemID, fileID int64, req RemoveFileRequest) (RemoveFileResult, error) {
	if req.Search || req.Blocklist {
		return RemoveFileResult{}, ErrReplacementReviewRequired
	}
	rec, err := s.db.GetFileQuality(ctx, fileID)
	if err != nil {
		return RemoveFileResult{}, err
	}
	if rec.MediaItemID != itemID {
		// Not pedantry: the file id comes off a URL, and a mismatched pair
		// means somebody's stale tab is about to delete a stranger's file.
		return RemoveFileResult{}, fmt.Errorf("file %d does not belong to item %d: %w",
			fileID, itemID, ErrNotFound)
	}
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return RemoveFileResult{}, err
	}

	release, _ := s.db.FileSource(ctx, fileID)
	result := RemoveFileResult{Path: rec.Path}

	if err := s.db.DeleteFile(ctx, fileID); err != nil {
		return result, fmt.Errorf("remove file record: %w", err)
	}
	if req.FromDisk {
		switch err := os.Remove(rec.Path); {
		case err == nil:
			result.DeletedFromDisk = true
		case errors.Is(err, os.ErrNotExist):
			// Already gone. The row was the only thing left of it, and that
			// is now gone too, which is exactly what was asked for.
			result.DeletedFromDisk = true
		default:
			result.Note = fmt.Sprintf("removed from the library, but the file itself could not be deleted: %v", err)
			s.log.Warn("remove file: unlink failed", "path", rec.Path, "err", err)
		}
	}

	_ = s.db.AddHistory(ctx, HistoryFileRemoved, itemID, release, map[string]any{
		"file":        filepath.Base(rec.Path),
		"fromDisk":    result.DeletedFromDisk,
		"blocklisted": result.Blocklisted != "",
		"reason":      req.Reason,
	})
	s.log.Info("file removed by user", "item", item.Title, "file", filepath.Base(rec.Path),
		"fromDisk", result.DeletedFromDisk, "blocklisted", result.Blocklisted)

	// The item just lost a file, so invalidate its cached wanted state.
	s.InvalidateWanted()
	return result, nil
}

// HistoryFileRemoved records a user deleting a file by hand. It is in history
// rather than only in the log because "where did my file go" is a question
// asked days later, by which point the log has rotated.
const HistoryFileRemoved = "file_removed"
