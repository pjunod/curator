package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// ---- what may be a publication's directory ---------------------------------

// A publication is one payload directory. A completed root, the "completed"
// storage container under a working-tree root, or any directory above them is
// never a payload: a publication there "contains" every payload beside it, so
// Download storage explained every folder as inside it and refused their
// whole-folder deletes (field data 2026-11-12: publication 344 at
// /working/monarr/completed). Migration 0040 had fenced every download's
// import_path, and a pre-delivery download whose single-file payload sat
// directly in the completed directory reported that directory as its import
// path; migration 0073 then made each fenced path a historical publication.

// payloadContainer reports a path that is a completed root, a root's
// completed storage container, or a directory above either.
func payloadContainer(path string, roots []string) bool {
	path = filepath.Clean(path)
	if path == string(filepath.Separator) {
		return true
	}
	for _, root := range roots {
		root = filepath.Clean(root)
		if within(path, root) || path == filepath.Join(root, "completed") {
			return true
		}
	}
	return false
}

// isPayloadContainer is payloadContainer over the configured roots. A root
// list that cannot be read refuses nothing (the import's own root check
// holds what is outside them).
func (s *Service) isPayloadContainer(ctx context.Context, path string) bool {
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return false
	}
	return payloadContainer(path, roots)
}

// payloadDirs lists the payload directories of one completed root: its
// children, and for a working-tree root the children of its completed
// container instead of the container itself (as the inventory lists them).
func payloadDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name())
		if e.Name() != "completed" {
			out = append(out, path)
			continue
		}
		inner, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, c := range inner {
			if c.IsDir() {
				out = append(out, filepath.Join(path, c.Name()))
			}
		}
	}
	return out, nil
}

// ---- record-only publications end without a person --------------------------

// closeRecordOnlyPublications ends the publications that are only a record of
// a path (no download row, no manifest, no file: the migration-0040 review
// cohort and directories the cutover froze), when the evidence allows:
//
//   - the path was never a payload directory (payloadContainer): the record
//     is invalid, and it is closed so it stops claiming the folders under it;
//   - the directory is gone while the storage that held it is present (its
//     completed root verified by storage identity, or, outside every root,
//     its parent directory present): there is nothing left to review.
//
// Closing deletes nothing. A record whose directory is still there keeps
// waiting for its review batch (ADR 0026, addendum 10), and one whose storage
// cannot be verified now is checked again on the next pass.
func (s *Service) closeRecordOnlyPublications(ctx context.Context) (int, error) {
	pubs, err := s.db.RecordOnlyPublications(ctx)
	if err != nil || len(pubs) == 0 {
		return 0, err
	}
	roots, err := s.completedRoots(ctx, nil)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, p := range pubs {
		if ctx.Err() != nil {
			return closed, ctx.Err()
		}
		reason, ok := s.recordOnlyEnd(ctx, p, roots)
		if !ok {
			continue
		}
		done, err := s.db.ClosePublicationRecord(ctx, p.ID, reason)
		if err != nil {
			return closed, err
		}
		if !done {
			continue
		}
		closed++
		_ = s.db.AddHistory(ctx, "publication_closed", 0, filepath.Base(p.Path), map[string]any{"publication": p.Path, "id": p.ID, "cohort": p.Cohort, "reason": reason})
		s.log.Info("publication record closed", "publication", p.ID, "path", p.Path, "reason", reason)
	}
	if closed > 0 {
		s.log.Info("record-only publications closed", "closed", closed, "of", len(pubs))
	}
	return closed, nil
}

// recordOnlyEnd decides whether a record-only publication ends now, and why.
func (s *Service) recordOnlyEnd(ctx context.Context, p sqlite.Publication, roots []string) (string, bool) {
	if payloadContainer(p.Path, roots) {
		return fmt.Sprintf("not a payload directory: %s is a completed root or its storage container (recorded from a pre-delivery download's save directory); closed, nothing deleted", p.Path), true
	}
	_, err := os.Lstat(p.Path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return "", false // still there (its review decides it), or unreadable now
	}
	inside := false
	for _, root := range roots {
		if within(root, p.Path) {
			inside = true
		}
	}
	if inside {
		// An unmounted share is an empty directory, not a payload that is
		// gone: only a root whose storage identity still holds proves it.
		if err := s.checkStorageIdentity(ctx, p.Path); err != nil {
			return "", false
		}
	} else if info, err := os.Stat(filepath.Dir(p.Path)); err != nil || !info.IsDir() {
		return "", false
	}
	return fmt.Sprintf("its directory is gone (checked %s); closed, nothing deleted", time.Now().UTC().Format(time.RFC3339)), true
}
