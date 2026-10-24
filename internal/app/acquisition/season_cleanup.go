package acquisition

import (
	"context"
	"path/filepath"
	"strings"
	"time"
)

type partialCleanupIDKey struct{}

type partialCleanupOwner struct {
	DownloadID    int64
	GenerationKey string
}

// Partial payloads retain explicit custody until rooted identity/ownership and
// reference checks permit deletion. Unsafe paths remain visible for review.
func (s *Service) cleanupPartialPayloads(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT d.id, d.generation_key FROM downloads d
		WHERE d.cleanup_pending=1 AND d.state='failed' AND d.delivery_expected=0
		AND NOT EXISTS (SELECT 1 FROM legacy_cleanup_review l
			WHERE l.generation_key=d.generation_key
			OR (d.import_path != '' AND l.path=d.import_path))
		ORDER BY d.updated_at LIMIT 25`)
	if err != nil {
		return
	}
	var candidates []partialCleanupOwner
	for rows.Next() {
		var row partialCleanupOwner
		if err = rows.Scan(&row.DownloadID, &row.GenerationKey); err != nil {
			_ = rows.Close()
			return
		}
		candidates = append(candidates, row)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return
	}
	for _, candidate := range candidates {
		s.cleanupPartialPayloadCandidate(ctx, candidate)
	}
}

// cleanupPartialPayloadCandidate owns the per-download lock for the complete
// candidate decision. The snapshot generation is checked before any rotation,
// filesystem operation, or custody settlement.
func (s *Service) cleanupPartialPayloadCandidate(ctx context.Context, candidate partialCleanupOwner) {
	unlockDownload := s.lockDownload(candidate.DownloadID)
	defer unlockDownload()
	id := candidate.DownloadID
	dl, err := s.db.GetDownload(ctx, id)
	if err != nil || candidate.GenerationKey == "" || dl.GenerationKey != candidate.GenerationKey ||
		dl.State != "failed" || !dl.CleanupPending || dl.DeliveryExpected {
		return
	}
	// Rotate even unsafe/unavailable candidates so they cannot starve safe later
	// payloads. The generation-qualified CAS cannot rotate a newer owner.
	result, err := s.db.W.ExecContext(ctx, `UPDATE downloads SET updated_at=?
		WHERE id=? AND generation_key=? AND state='failed' AND cleanup_pending=1 AND delivery_expected=0`,
		time.Now().UnixMilli(), id, candidate.GenerationKey)
	if err != nil {
		return
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return
	}
	dl, err = s.db.GetDownload(ctx, id)
	if err != nil || dl.GenerationKey != candidate.GenerationKey || dl.State != "failed" || !dl.CleanupPending || dl.DeliveryExpected {
		return
	}
	if strings.TrimSpace(dl.ImportPath) == "" || !filepath.IsAbs(dl.ImportPath) {
		return
	}
	path := filepath.Join(canonicalCompletedPath(filepath.Dir(dl.ImportPath)), filepath.Base(dl.ImportPath))
	if !s.ownsDownloadPath(ctx, path) {
		return
	}
	if payloadAbsent(path) {
		s.settleAbsentPartialPayload(ctx, candidate, path)
		return
	}
	inventory, err := s.CompletedInventory(ctx)
	if err != nil {
		return
	}
	fingerprint := ""
	for _, root := range inventory.Roots {
		if root.Error != "" {
			continue
		}
		for _, entry := range root.Entries {
			if entry.Path == path {
				fingerprint = entry.Fingerprint
			}
		}
	}
	if fingerprint == "" {
		return
	}
	if err = s.DeleteCompletedEntry(context.WithValue(ctx, partialCleanupIDKey{}, candidate), path, fingerprint); err != nil {
		s.log.Debug("partial payload remains in custody", "download", id, "reason", err)
		return
	}
	s.recordPartialPayloadRemoved(ctx, candidate)
}

// settleAbsentPartialPayload verifies the configured storage while holding the
// final target/storage authority. The caller already holds the download lock.
func (s *Service) settleAbsentPartialPayload(ctx context.Context, candidate partialCleanupOwner, path string) {
	s.importTargetMu.Lock()
	defer s.importTargetMu.Unlock()
	decisionCtx, releaseStorage := s.lockStorageDecision(ctx)
	defer releaseStorage()
	current, err := s.db.GetDownload(decisionCtx, candidate.DownloadID)
	if err != nil || current.GenerationKey != candidate.GenerationKey || current.State != "failed" ||
		!current.CleanupPending || current.DeliveryExpected || current.ImportPath == "" || !filepath.IsAbs(current.ImportPath) {
		return
	}
	receipts, err := s.db.CompletedReceipts(decisionCtx)
	if err != nil {
		return
	}
	roots, err := s.completedRoots(decisionCtx, receipts)
	if err != nil || storagePath(current.ImportPath, roots) != storagePath(path, roots) || !s.ownsDownloadPath(decisionCtx, path) {
		return
	}
	if s.legacyCleanupReview(decisionCtx, downloadRef{ID: candidate.DownloadID, GenerationKey: current.GenerationKey, ImportPath: current.ImportPath}) ||
		s.checkStorageIdentity(decisionCtx, path) != nil || !payloadAbsent(path) {
		return
	}
	s.recordPartialPayloadRemoved(decisionCtx, candidate)
}

func (s *Service) recordPartialPayloadRemoved(ctx context.Context, candidate partialCleanupOwner) {
	result, err := s.db.W.ExecContext(ctx, `UPDATE downloads SET cleanup_pending=0,payload_removed=1,parked_reason=''
		WHERE id=? AND generation_key=? AND state='failed' AND cleanup_pending=1 AND delivery_expected=0`,
		candidate.DownloadID, candidate.GenerationKey)
	if err != nil {
		s.log.Warn("partial cleanup: could not record removed payload", "download", candidate.DownloadID, "err", err)
	} else if affected, _ := result.RowsAffected(); affected != 1 {
		s.log.Debug("partial cleanup: row changed before removed payload was recorded", "download", candidate.DownloadID)
	}
}
