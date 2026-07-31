package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

// Cleaning up after an import.
//
// Monarr never did. Every grab left the payload sitting in the download
// client's completed folder forever, and because place() falls back to copying
// whenever the client's disk and the library are not the same filesystem — the
// normal arrangement, fast local disk for downloads and a big array for the
// library — that leftover is a second full copy in real bytes. It is invisible
// from inside monarr, it never triggers a disk-space warning that names it,
// and it grows by the size of everything you have ever grabbed.
//
// Two paths lead here, and they exist for different reasons. One removes a
// payload the moment its import succeeds, which is the behaviour that should
// always have been there. The other is a sweep, which exists because the first
// one only helps from now on: turning the setting on has to also collect the
// backlog it was not there to prevent.

// cleanupPerSweep bounds one pass. Asking a client to delete several hundred
// jobs in one burst is a good way to make it stop answering, and a backlog
// that takes a few hours to drain is not a problem — it took months to build.
const cleanupPerSweep = 25

// JobCleanup is the scheduled sweep's task name.
const JobCleanup = "downloads.cleanup"

// HistoryPayloadRemoved records reclaimed space, because "where did my disk go"
// and "why is my disk suddenly free" are both questions people ask later.
const HistoryPayloadRemoved = "payload_removed"

// removePayload asks the client to delete an imported download's data, and
// records that it did so.
//
// Deleting the client's copy is safe even when monarr hard-linked rather than
// copied: a hardlink is a second name for the same inode, so removing the
// client's name leaves the library's name — and the bytes — untouched. That is
// the whole point of preferring hardlinks, and it means this one code path is
// correct for both arrangements.
//
// Best-effort throughout. A client that is down, a job already gone from its
// history, a handle it no longer recognises: none of those are worth surfacing
// as a failure, because the import they follow already succeeded. The flag is
// only set when the client agreed, so anything that did not work is simply
// tried again on the next sweep.
func (s *Service) removePayload(ctx context.Context, dl downloadRef) bool {
	if dl.Handle == "" {
		return false
	}
	cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID)
	if err != nil {
		return false
	}
	if !cfg.RemoveCompleted {
		return false
	}
	if err := s.newClient(cfg).Remove(ctx, ports.Handle(dl.Handle), true); err != nil {
		s.log.Debug("cleanup: client would not remove the payload",
			"release", dl.ReleaseTitle, "client", clientLabel(cfg), "err", err)
		return false
	}
	if err := s.db.MarkPayloadRemoved(ctx, dl.ID); err != nil {
		// The bytes are gone either way; the flag not sticking only means the
		// next sweep asks again, which is harmless.
		s.log.Warn("cleanup: could not record the removal", "download", dl.ID, "err", err)
	}
	s.log.Info("cleanup: payload removed from the download client",
		"release", dl.ReleaseTitle, "client", clientLabel(cfg), "size", dl.Size)
	_ = s.db.AddHistory(ctx, HistoryPayloadRemoved, dl.MediaItemID, dl.ReleaseTitle,
		map[string]any{"client": clientLabel(cfg), "bytes": dl.Size})
	return true
}

// downloadRef is the handful of fields cleanup needs, so both callers can pass
// what they already have without either one re-reading the row.
type downloadRef struct {
	ID          int64
	MediaItemID int64
	ClientID    int64
	Handle      string
	// ImportPath is where monarr just imported from — the fallback target
	// when the client will not remove its own payload.
	ImportPath   string
	ReleaseTitle string
	Size         int64
}

// CleanupPayloads sweeps imported downloads whose payload is still on the
// client's disk and removes it. Returns how many it collected.
//
// This is what drains a backlog. The per-import removal only helps from the
// moment it is switched on, and somebody enabling it after a year of grabs
// wants the year of grabs gone too — the setting means "monarr should not be
// leaving these around", not "monarr should stop leaving NEW ones around".
//
// Ordered oldest first so the backlog goes in the order it accumulated, and
// capped per pass so a client is never handed hundreds of deletions at once.
func (s *Service) CleanupPayloads(ctx context.Context) error {
	rows, err := s.db.ImportedWithPayload(ctx, cleanupPerSweep)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	removed, bytes := 0, int64(0)
	for _, dl := range rows {
		ref := downloadRef{
			ID: dl.ID, MediaItemID: dl.MediaItemID, ClientID: dl.ClientID,
			Handle: dl.Handle, ImportPath: dl.ImportPath,
			ReleaseTitle: dl.ReleaseTitle, Size: dl.Size,
		}
		if s.removePayload(ctx, ref) {
			removed++
			bytes += dl.Size
			continue
		}
		// The backlog gets the same disk fallback the inline path gets:
		// most of a backlog IS the payloads a client declined to remove.
		before := dl.ImportPath
		s.removeImportedDir(ctx, ref)
		if before != "" {
			if _, err := os.Stat(before); os.IsNotExist(err) {
				removed++
				bytes += dl.Size
			}
		}
	}
	if removed > 0 {
		s.log.Info("cleanup: swept imported payloads",
			"removed", removed, "of", len(rows), "bytes", bytes)
	}
	return nil
}

// cleanupAfterImport removes one payload immediately after its import lands.
//
// Runs inline rather than waiting for the sweep because the disk pressure is
// immediate: on a library that grabs a dozen 60 GB remuxes overnight, an hourly
// sweep is several hundred gigabytes late.
func (s *Service) cleanupAfterImport(ctx context.Context, dl downloadRef) {
	if s.removePayload(ctx, dl) {
		return
	}
	// The client would not, or could not, remove it. Ask the disk directly.
	//
	// This is the other half of the terabyte: monarr imported, told the
	// client to delete the payload, the client's delete did not take, and
	// nothing ever looked again — 90 GB per grab, in the very folder monarr
	// had just read. The bytes are in the library now; the copy in the
	// completed folder is nobody's.
	s.removeImportedDir(ctx, dl)
}

// removeImportedDir deletes the completed download's own directory after a
// verified-complete import, when the download client did not.
//
// Guarded, hard, in both directions: it only ever removes the directory the
// client itself reported and monarr just imported FROM, it refuses anything
// at or above a configured root folder, and it refuses a path that is
// inside one — a library folder is not a payload, whatever a path mapping
// says. A wrong deletion here is somebody's media, so every doubt is a no.
func (s *Service) removeImportedDir(ctx context.Context, dl downloadRef) {
	dir := strings.TrimSpace(dl.ImportPath)
	if dir == "" || !filepath.IsAbs(dir) {
		return
	}
	dir = filepath.Clean(dir)
	if dir == "/" || filepath.Dir(dir) == dir {
		return
	}
	cfg, err := s.db.GetDownloadClient(ctx, dl.ClientID)
	if err != nil || !cfg.RemoveCompleted {
		// The operator asked monarr to leave payloads alone. That answer
		// covers the disk as much as it covers the client.
		return
	}
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return // cannot prove it is safe, so it is not
	}
	for _, r := range roots {
		root := filepath.Clean(r.Path)
		if root == "" || root == "/" {
			continue
		}
		if dir == root || within(root, dir) || within(dir, root) {
			s.log.Warn("cleanup: refusing to remove a payload dir inside a root folder",
				"dir", dir, "root", root)
			return
		}
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		s.log.Warn("cleanup: could not remove the completed download's folder",
			"dir", dir, "err", err)
		return
	}
	if err := s.db.MarkPayloadRemoved(ctx, dl.ID); err != nil {
		s.log.Warn("cleanup: could not record the removal", "download", dl.ID, "err", err)
	}
	s.log.Info("cleanup: removed the completed download's folder from disk",
		"release", dl.ReleaseTitle, "dir", dir, "size", dl.Size)
	_ = s.db.AddHistory(ctx, HistoryPayloadRemoved, dl.MediaItemID, dl.ReleaseTitle,
		map[string]any{"dir": dir, "bytes": dl.Size, "by": "monarr (the client did not)"})
}

// within reports whether child is under parent, on path boundaries — so
// /data/media never matches /data/media-old.
func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// CleanupInterval is how often the sweep runs. Hourly: a payload nobody
// cleaned up is not urgent by the time the sweep is the thing finding it, and
// the inline path already handles everything that just imported.
const CleanupInterval = time.Hour
