package acquisition

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

// Legacy whole-payload cleanup for clients that do not use the durable
// Runner delivery protocol. Delivery-managed generations are fenced below and
// cleaned from their accepted per-file manifest instead.

// cleanupPerSweep bounds one legacy-client cleanup pass. Asking a client to
// delete several hundred jobs in one burst can make it stop answering.
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
	if s.deliveryExpectedDownload(ctx, dl) {
		return false
	}
	if s.storageReviewHeld(ctx, dl) {
		return false
	}
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
	if dl.ImportPath != "" && !s.safePayloadPath(ctx, dl) {
		return false
	}
	if err := s.assertImportSourceDisposable(ctx, dl.ImportPath); err != nil {
		s.log.Debug("cleanup: import custody retained", "download", dl.ID, "err", err)
		return false
	}
	if err := s.newClient(cfg).Remove(ctx, ports.Handle(dl.Handle), true); err != nil {
		s.log.Debug("cleanup: client would not remove the payload",
			"release", dl.ReleaseTitle, "client", clientLabel(cfg), "err", err)
		return false
	}
	// A successful client response may only mean its history row is gone.
	// When Monarr knows the local path, disk disappearance is the receipt.
	if dl.ImportPath != "" && !payloadAbsent(dl.ImportPath) {
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
	ID            int64
	MediaItemID   int64
	ClientID      int64
	Handle        string
	GenerationKey string
	// ImportPath is where monarr just imported from — the fallback target
	// when the client will not remove its own payload.
	ImportPath   string
	ReleaseTitle string
	Size         int64
}

// CleanupPayloads retries eligible legacy imported downloads for clients
// whose whole-payload cleanup setting is enabled. Runner delivery generations
// are excluded: their manifest ledger owns per-file cleanup and preserves
// unresolved siblings. Attempts rotate so a blocked row cannot starve later
// payloads, and the batch is capped to avoid overwhelming a client.
func (s *Service) CleanupPayloads(ctx context.Context) error {
	// Records of a path that is gone, or that was never a payload's, end
	// first, so they claim nothing below.
	if _, err := s.closeRecordOnlyPublications(ctx); err != nil {
		s.log.Warn("cleanup: closing record-only publications failed", "err", err)
	}
	// Every waiting publication is offered (or ended) here too, so none
	// depends on its client's poll; and rows hidden for a pending salvage
	// are settled from Runner.
	if _, _, err := s.offerWaitingPublications(ctx); err != nil {
		s.log.Warn("cleanup: offering waiting publications failed", "err", err)
	}
	if _, err := s.exitOrphanedOverdue(ctx); err != nil {
		s.log.Warn("cleanup: ending overdue rows of gone clients failed", "err", err)
	}
	if _, err := s.settlePendingSalvages(ctx); err != nil {
		s.log.Warn("cleanup: settling pending salvages failed", "err", err)
	}
	// Adoption next: a committed directory no publication claims gets one
	// (attributed, or orphaned and discarded), and the sweep below retires
	// an orphan in the same pass.
	if _, _, err := s.adoptUnclaimedPublications(ctx); err != nil {
		s.log.Warn("cleanup: publication adoption failed", "err", err)
	}
	if err := s.sweepPublications(ctx); err != nil {
		s.log.Warn("cleanup: publication sweep failed", "err", err)
	}
	s.cleanupPartialPayloads(ctx)
	rows, err := s.db.ImportedCleanupCandidates(ctx, cleanupPerSweep)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	removed, bytes := 0, int64(0)
	for _, candidate := range rows {
		unlockDownload := s.lockDownload(candidate.ID)
		ctx, releaseStorage := s.lockStorageDecision(ctx)
		unlock := func() { releaseStorage(); unlockDownload() }
		dl, err := s.db.GetDownload(ctx, candidate.ID)
		if err != nil || candidate.GenerationKey == "" || dl.GenerationKey != candidate.GenerationKey ||
			dl.DeliveryExpected || dl.State != "imported" || dl.PayloadRemoved || !dl.AddedAt.Equal(candidate.AddedAt) ||
			s.publicationManaged(ctx, dl.ID, dl.ImportPath) {
			unlock()
			continue
		}
		if err := s.db.TouchPayloadCleanup(ctx, dl.ID); err != nil {
			unlock()
			return err
		}
		// Match runImport's fallback for older handoffs. A known source must
		// still pass local path guards and disappearance checks.
		if dl.ImportPath == "" {
			dl.ImportPath = dl.SavePath
		}
		if dl.ImportPath != "" && !s.safePayloadPath(ctx, downloadRef{ID: dl.ID, ImportPath: dl.ImportPath}) {
			unlock()
			continue
		}
		if err := s.assertImportSourceDisposable(ctx, dl.ImportPath); err != nil {
			unlock()
			continue
		}
		if dl.ImportPath != "" && payloadAbsent(dl.ImportPath) {
			if !dl.PayloadRemoved {
				_ = s.db.MarkPayloadRemoved(ctx, dl.ID)
			}
			unlock()
			continue
		}
		ref := downloadRef{
			ID: dl.ID, MediaItemID: dl.MediaItemID, ClientID: dl.ClientID,
			Handle: dl.Handle, GenerationKey: dl.GenerationKey, ImportPath: dl.ImportPath,
			ReleaseTitle: dl.ReleaseTitle, Size: dl.Size,
		}
		if s.removePayload(ctx, ref) {
			unlock()
			removed++
			bytes += dl.Size
			continue
		}
		// Legacy whole-payload cleanup gets the same guarded local fallback as
		// the inline path. Delivery-managed generations cannot reach this branch.
		s.removeImportedDir(ctx, ref)
		confirmed, err := s.db.GetDownload(ctx, dl.ID)
		unlock()
		if (err == nil && confirmed.PayloadRemoved) || (err != nil && payloadAbsent(dl.ImportPath)) {
			removed++
			bytes += dl.Size
		}
	}
	if removed > 0 {
		s.log.Info("cleanup: swept imported payloads",
			"removed", removed, "of", len(rows), "bytes", bytes)
	}
	return nil
}

// cleanupAfterImport requests legacy whole-payload cleanup after import. The
// durable Runner path is handled separately by the delivery file ledger.
func (s *Service) cleanupAfterImport(ctx context.Context, dl downloadRef) {
	ctx, releaseStorage := s.lockStorageDecision(ctx)
	defer releaseStorage()
	if s.removePayload(ctx, dl) {
		return
	}
	// The client would not, or could not, remove it. Ask the disk directly.
	//
	// A client may fail to delete its payload. The guarded fallback is limited
	// to legacy whole-payload cleanup; Runner publications use per-file proof.
	s.removeImportedDir(ctx, dl)
}

// removeImportedDir deletes a legacy completed download's own directory after
// a verified-complete import, when the client did not. Delivery-managed
// Runner generations are rejected at this boundary.
//
// Guarded, hard, in both directions: it only ever removes the directory the
// client itself reported and monarr just imported FROM, it refuses anything
// at or above a configured root folder, and it refuses a path that is
// inside one — a library folder is not a payload, whatever a path mapping
// says. A wrong deletion here is somebody's media, so every doubt is a no.
func (s *Service) removeImportedDir(ctx context.Context, dl downloadRef) {
	if s.deliveryExpectedDownload(ctx, dl) {
		return
	}
	if s.storageReviewHeld(ctx, dl) {
		return
	}
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
	// Runner owns its processing and recovery trees. Legacy Runner paths can be
	// deleted only in a configured Curator-owned working tree; delivery-managed
	// publications never reach this whole-directory fallback.
	if ports.IsNativeRunnerClient(cfg.Type) && !s.ownsDownloadPath(ctx, dir) {
		s.log.Warn("cleanup: Runner payload remains pending; retry through Runner", "download", dl.ID, "dir", dir)
		return
	}
	if !s.safePayloadPath(ctx, dl) {
		return
	}
	if payloadAbsent(dir) {
		_ = s.db.MarkPayloadRemoved(ctx, dl.ID)
		return
	}
	if _, err := os.Lstat(dir); err != nil {
		return
	}
	if err := s.assertImportSourceDisposable(ctx, dir); err != nil {
		s.log.Debug("cleanup: import custody retained", "download", dl.ID, "err", err)
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

// deliveryExpectedDownload fences legacy whole-directory cleanup from every
// delivery-managed generation. Those files are removed only from the accepted
// manifest ledger, which preserves unresolved siblings and Activity history.
func (s *Service) deliveryExpectedDownload(ctx context.Context, dl downloadRef) bool {
	// A publication's files leave only by their recorded per-file outcome
	// (pipeline v2, contract section 3.4), and the historical cohort not at
	// all until its review approves it: no whole-directory cleanup, ever.
	if s.publicationManaged(ctx, dl.ID, dl.ImportPath) {
		return true
	}
	if dl.ID <= 0 {
		return false
	}
	current, err := s.db.GetDownload(ctx, dl.ID)
	if err != nil {
		return true
	}
	if dl.GenerationKey != "" && current.GenerationKey != dl.GenerationKey {
		return true
	}
	return current.DeliveryExpected
}

// storageReviewHeld keeps a payload whose storage changed under it out of
// automatic cleanup until a person reviews it (the storage-change fences).
// The immutable generation key binds the check when integer IDs are reused;
// a ref without one cannot be told apart from a newer row, so it is held.
func (s *Service) storageReviewHeld(ctx context.Context, dl downloadRef) bool {
	if dl.ID <= 0 {
		return dl.Handle != "" || dl.ImportPath != ""
	}
	current, err := s.db.GetDownload(ctx, dl.ID)
	if err != nil {
		s.log.Warn("cleanup: could not resolve download generation", "download", dl.ID, "err", err)
		return true
	}
	if dl.GenerationKey == "" || current.GenerationKey == "" || dl.GenerationKey != current.GenerationKey {
		return true
	}
	for _, path := range []string{current.ImportPath, dl.ImportPath} {
		if path != "" && !filepath.IsAbs(path) {
			return true
		}
		if path == "" {
			continue
		}
		held, err := s.db.StorageChangeReview(ctx, current.GenerationKey, path)
		if err != nil {
			s.log.Warn("cleanup: could not verify storage review", "download", dl.ID, "err", err)
			return true
		}
		if held {
			return true
		}
	}
	return false
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

// Missing is meaningful only while the containing directory is reachable.
func payloadAbsent(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	_, err := os.Lstat(path)
	if !os.IsNotExist(err) {
		return false
	}
	parent, err := os.Stat(filepath.Dir(path))
	return err == nil && parent.IsDir()
}

// Apply path guards before either client or local deletion. A queue handle
// cannot authorize deleting a shared directory or a path reused by another job.
func (s *Service) safePayloadPath(ctx context.Context, dl downloadRef) bool {
	if err := s.payloadPathError(ctx, dl); err != nil {
		s.log.Warn("cleanup: imported payload retained", "download", dl.ID, "path", dl.ImportPath, "reason", err)
		return false
	}
	return true
}

func (s *Service) payloadPathError(ctx context.Context, dl downloadRef) error {
	dir := strings.TrimSpace(dl.ImportPath)
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == string(filepath.Separator) {
		return fmt.Errorf("payload path must be absolute and below the filesystem root")
	}
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("payload path is a symbolic link")
	}
	receipts, err := s.db.CompletedReceipts(ctx)
	if err != nil {
		return fmt.Errorf("read download ownership: %w", err)
	}
	completed, err := s.completedRoots(ctx, receipts)
	if err != nil {
		return fmt.Errorf("read download storage roots: %w", err)
	}
	real := storagePath(dir, completed)
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return fmt.Errorf("read library roots: %w", err)
	}
	for _, root := range roots {
		p := storagePath(root.Path, completed)
		if within(p, real) || within(real, p) {
			return fmt.Errorf("payload overlaps library root %s", root.Path)
		}
	}
	recovery, err := s.RecoverySettings(ctx)
	if err != nil {
		return fmt.Errorf("read recovery settings: %w", err)
	}
	if recovery.LocalRoot != "" {
		p := storagePath(recovery.LocalRoot, completed)
		if within(p, real) || within(real, p) {
			return fmt.Errorf("payload overlaps recovery root %s", recovery.LocalRoot)
		}
	}
	if err := s.checkStorageIdentity(ctx, real); err != nil {
		return err
	}
	for _, root := range completed {
		if within(real, root) || real == filepath.Join(root, "completed") {
			return fmt.Errorf("payload is a shared storage container")
		}
	}
	for _, receipt := range receipts {
		if receipt.DownloadID == dl.ID && receipt.Live {
			continue
		}
		p := storagePath(receipt.Path, completed)
		// Cleared history at a shared storage container identifies no
		// particular payload. Live broad paths still retain custody.
		if (!receipt.Live || (receipt.Dismissed && receipt.State == "imported")) && isCompletedStorageContainer(p, completed) {
			continue
		}
		if within(real, p) || within(p, real) {
			return fmt.Errorf("payload overlaps download %d (%s) at %s", receipt.DownloadID, receipt.Title, receipt.Path)
		}
	}
	return nil
}

// Shared storage containers are never payload deletion targets. Historical
// receipts at these paths cannot attribute all sibling payloads to one release.
func isCompletedStorageContainer(path string, roots []string) bool {
	for _, root := range roots {
		if within(path, root) || path == filepath.Join(root, "completed") {
			return true
		}
	}
	return false
}
