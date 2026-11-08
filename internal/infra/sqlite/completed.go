package sqlite

import (
	"context"
	"time"
)

// CompletedReceipt outlives Activity and is an association, not permission to
// delete a path. Names can be reused and historical imports can be partial.
type CompletedReceipt struct {
	DownloadID      int64  `json:"downloadId"`
	MediaItemID     int64  `json:"mediaItemId,omitempty"`
	CopyID          int64  `json:"copyId,omitempty"`
	Path            string `json:"path"`
	ClientID        int64  `json:"clientId"`
	Title           string `json:"title"`
	State           string `json:"state"`
	Protocol        string `json:"protocol"`
	PayloadRemoved  bool   `json:"payloadRemoved"`
	Live            bool   `json:"live"`
	GenerationKey   string `json:"-"`
	LegacyReview    bool   `json:"legacyReview"`
	DeliveryManaged bool   `json:"deliveryManaged"`
	// Internal custody can outlive the visible Activity row.
	Dismissed bool `json:"-"`
}

func (d *DB) CompletedReceipts(ctx context.Context) ([]CompletedReceipt, error) {
	fences, err := d.storageChangeFences(ctx)
	if err != nil {
		return nil, err
	}
	orphans, err := d.storageReviewOrphanPaths(ctx)
	if err != nil {
		return nil, err
	}
	for i := range orphans {
		orphans[i] = canonicalStorageReviewPath(orphans[i])
	}
	rows, err := d.R.QueryContext(ctx, `SELECT r.download_id, r.path, r.client_id,
		r.title, r.state, r.protocol, r.payload_removed,
		EXISTS(SELECT 1 FROM downloads d WHERE d.generation_key = r.generation_key),
		EXISTS(SELECT 1 FROM downloads d WHERE d.generation_key = r.generation_key AND d.activity_dismissed = 1),
		r.generation_key,
		0,
		(EXISTS(SELECT 1 FROM downloads d WHERE d.generation_key=r.generation_key AND d.delivery_expected=1)
		 OR EXISTS(SELECT 1 FROM publications p WHERE p.path = r.path))
		,coalesce(d.media_item_id,0), coalesce(d.copy_id,0)
		FROM completed_receipts r LEFT JOIN downloads d ON d.generation_key=r.generation_key
		ORDER BY r.download_id, r.path, r.generation_key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []CompletedReceipt{}
	for rows.Next() {
		var r CompletedReceipt
		if err := rows.Scan(&r.DownloadID, &r.Path, &r.ClientID, &r.Title, &r.State,
			&r.Protocol, &r.PayloadRemoved, &r.Live, &r.Dismissed, &r.GenerationKey, &r.LegacyReview, &r.DeliveryManaged,
			&r.MediaItemID, &r.CopyID); err != nil {
			return nil, err
		}
		// Held for review: the storage under the payload changed (a fence on
		// its generation in that root, or an orphan path fence around it).
		r.LegacyReview = storageReviewFor(fences, r.GenerationKey, r.Path)
		if !r.LegacyReview && r.Path != "" {
			p := canonicalStorageReviewPath(r.Path)
			for _, o := range orphans {
				if storageReviewWithin(o, p) {
					r.LegacyReview = true
					break
				}
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) SaveCompletedScan(ctx context.Context, root, report string) error {
	_, err := d.W.ExecContext(ctx, `INSERT INTO completed_scans(root, report) VALUES (?, ?)
		ON CONFLICT(root) DO UPDATE SET report = excluded.report`, root, report)
	return err
}

func (d *DB) CompletedScans(ctx context.Context) ([]string, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT report FROM completed_scans ORDER BY root`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var report string
		if err := rows.Scan(&report); err != nil {
			return nil, err
		}
		out = append(out, report)
	}
	return out, rows.Err()
}

// ImportedCleanupCandidates rotates attempts so a permanently blocked row cannot
// starve the rest of the backlog. Disabled/seeding clients do not consume slots.
func (d *DB) ImportedCleanupCandidates(ctx context.Context, limit int64) ([]Download, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT d.id FROM downloads d
		JOIN download_clients c ON c.id = d.client_id
        LEFT JOIN completed_cleanup_attempts a ON a.download_id = d.id
		WHERE d.state = 'imported' AND c.remove_completed = 1 AND d.delivery_expected=0
		AND d.payload_removed = 0
		AND NOT EXISTS (SELECT 1 FROM publications p WHERE p.download_id=d.id OR (d.import_path != '' AND p.path=d.import_path))
		ORDER BY coalesce(a.tried_at, 0), d.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	out := []Download{}
	for _, id := range ids {
		dl, err := d.GetDownload(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, dl)
	}
	return out, nil
}

func (d *DB) TouchPayloadCleanup(ctx context.Context, id int64) error {
	_, err := d.W.ExecContext(ctx, `INSERT INTO completed_cleanup_attempts(download_id, tried_at) VALUES (?, ?) ON CONFLICT(download_id) DO UPDATE SET tried_at = excluded.tried_at`, id, time.Now().UnixMilli())
	return err
}

func (d *DB) ForgetCompletedScan(ctx context.Context, root string) error {
	_, err := d.W.ExecContext(ctx, `DELETE FROM completed_scans WHERE root = ?`, root)
	return err
}
