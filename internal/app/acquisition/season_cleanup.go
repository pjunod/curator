package acquisition

import (
	"context"
	"path/filepath"
	"time"
)

type partialCleanupIDKey struct{}

// Partial payloads retain explicit custody until rooted identity/ownership and
// reference checks permit deletion. Unsafe paths remain visible for review.
func (s *Service) cleanupPartialPayloads(ctx context.Context) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id FROM downloads WHERE cleanup_pending=1 AND state='failed' ORDER BY updated_at LIMIT 25`)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	_ = rows.Close()
	for _, id := range ids {
		dl, err := s.db.GetDownload(ctx, id)
		if err != nil || !dl.CleanupPending {
			continue
		}
		// Rotate even unsafe/unavailable candidates so they cannot starve safe
		// later payloads; failed attempts never release custody.
		_, _ = s.db.W.ExecContext(ctx, `UPDATE downloads SET updated_at=? WHERE id=?`, time.Now().UnixMilli(), id)
		path := filepath.Clean(dl.ImportPath)
		if dl.ImportPath == "" || !s.ownsDownloadPath(ctx, path) {
			continue
		}
		if payloadAbsent(path) {
			_, _ = s.db.W.ExecContext(ctx, `UPDATE downloads SET cleanup_pending=0,payload_removed=1 WHERE id=?`, id)
			continue
		}
		inventory, err := s.CompletedInventory(ctx)
		if err != nil {
			continue
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
			continue
		}
		own := context.WithValue(ctx, partialCleanupIDKey{}, id)
		if err = s.DeleteCompletedEntry(own, path, fingerprint); err != nil {
			s.log.Debug("partial payload remains in custody", "download", id, "reason", err)
			continue
		}
		_, _ = s.db.W.ExecContext(ctx, `UPDATE downloads SET cleanup_pending=0,payload_removed=1,parked_reason='' WHERE id=?`, id)
	}
}
