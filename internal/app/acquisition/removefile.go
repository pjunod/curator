package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monarr-media/monarr/internal/domain"
)

// Removing one file, by hand.
//
// Everything else in this package moves files because a rule said so: an
// upgrade replaces what it beats, a failed download blocklists itself. This is
// the other case — a person looked at a specific file, decided it is bad, and
// wants it gone. Until now there was no way to say that. The Files table was
// read-only, DeleteMediaFile existed in the storage layer with no caller above
// it, and the only route to a blocklist ran through the download queue, which
// forgets a release the moment it imports.
//
// Two things are deliberately separate here. Deleting a file says "I do not
// want this copy". Blocklisting says "and never take this release again".
// They usually travel together and they are not the same statement: a file
// deleted to free space should not poison a release that was fine.

// RemoveFileRequest is one user decision about one file.
type RemoveFileRequest struct {
	// FromDisk deletes the bytes, not just the row. Default false is the
	// cautious reading of an ambiguous request; the UI sets it explicitly.
	FromDisk bool
	// Blocklist declares the release that produced this file bad, so
	// automation never grabs it again. No-op for a file monarr did not grab.
	Blocklist bool
	// Search hunts for a replacement once the file is gone. Only meaningful
	// with Blocklist or FromDisk, since otherwise nothing changed.
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

// RemoveFile deletes one library file and optionally declares its release bad.
//
// The row goes first and the bytes second, and the order matters: if the
// unlink fails (a read-only mount, a permission), the row is already gone and
// the next scan will find the file and re-adopt it — recoverable, and visible.
// The reverse order can leave a row pointing at nothing, which reads as "you
// have this file" forever.
func (s *Service) RemoveFile(ctx context.Context, itemID, fileID int64, req RemoveFileRequest) (RemoveFileResult, error) {
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

	release, indexer := s.db.FileSource(ctx, fileID)
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

	if req.Blocklist {
		switch {
		case release == "":
			result.Note = joinNote(result.Note,
				"monarr did not grab this file, so there is no release to blocklist")
		default:
			reason := req.Reason
			if reason == "" {
				reason = "marked bad by user"
			}
			if err := s.db.AddBlocklist(ctx, itemID, release, indexer, reason); err != nil {
				result.Note = joinNote(result.Note, fmt.Sprintf("could not blocklist %q: %v", release, err))
				s.log.Warn("remove file: blocklist failed", "release", release, "err", err)
			} else {
				result.Blocklisted = release
			}
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

	// The item just lost a file, so whatever the wanted index believed about
	// it is now wrong. Invalidate before searching, or the search re-derives
	// the same stale answer and finds nothing to do.
	s.InvalidateWanted()

	if req.Search {
		if err := s.searchReplacement(ctx, item, rec.CopyID); err != nil {
			result.Note = joinNote(result.Note, fmt.Sprintf("could not start a search: %v", err))
		} else {
			result.Searched = true
		}
	}
	return result, nil
}

// HistoryFileRemoved records a user deleting a file by hand. It is in history
// rather than only in the log because "where did my file go" is a question
// asked days later, by which point the log has rotated.
const HistoryFileRemoved = "file_removed"

// searchReplacement hunts for whatever the item is now missing.
//
// It re-derives the wantables from scratch rather than reusing the deleted
// file's, because a deletion can change the shape of what is wanted: removing
// the only file of a movie turns "upgrade" into "missing", and removing one
// episode of a season leaves the rest alone.
func (s *Service) searchReplacement(ctx context.Context, item domain.MediaItem, copyID int64) error {
	enabled, err := s.enabledIndexers(ctx)
	if err != nil {
		return err
	}
	if len(enabled) == 0 {
		return ErrNoIndexers
	}
	var cp *domain.MediaCopy
	if copyID != 0 {
		c, err := s.db.GetMediaCopy(ctx, item.ID, copyID)
		if err == nil {
			cp = &c
		}
	}
	target, err := s.targetCopy(ctx, item, 0, 0, cp)
	if err != nil {
		// A series needs a season to build a wantable, and "which season"
		// is not a question a file deletion answers. Fall back to the
		// item-wide sweep, which figures it out from the wanted index.
		return s.AutoSearchItem(ctx, item.ID)
	}
	return s.searchAndGrabBest(ctx, target, enabled)
}

func joinNote(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}
