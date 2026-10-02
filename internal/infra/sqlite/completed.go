package sqlite

import (
	"context"
	"time"
)

// CompletedReceipt outlives Activity and is an association, not permission to
// delete a path. Names can be reused and historical imports can be partial.
type CompletedReceipt struct {
	DownloadID     int64  `json:"downloadId"`
	Path           string `json:"path"`
	ClientID       int64  `json:"clientId"`
	Title          string `json:"title"`
	State          string `json:"state"`
	Protocol       string `json:"protocol"`
	PayloadRemoved bool   `json:"payloadRemoved"`
	Live           bool   `json:"live"`
	// Internal custody can outlive the visible Activity row.
	Dismissed bool `json:"-"`
}

func (d *DB) CompletedReceipts(ctx context.Context) ([]CompletedReceipt, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT r.download_id, r.path, r.client_id,
		r.title, r.state, r.protocol, r.payload_removed,
		EXISTS(SELECT 1 FROM downloads d WHERE d.id = r.download_id AND d.added_at = r.added_at AND d.import_path = r.path),
		EXISTS(SELECT 1 FROM downloads d WHERE d.id = r.download_id AND d.added_at = r.added_at AND d.import_path = r.path AND d.activity_dismissed = 1)
		FROM completed_receipts r ORDER BY r.download_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []CompletedReceipt{}
	for rows.Next() {
		var r CompletedReceipt
		if err := rows.Scan(&r.DownloadID, &r.Path, &r.ClientID, &r.Title, &r.State,
			&r.Protocol, &r.PayloadRemoved, &r.Live, &r.Dismissed); err != nil {
			return nil, err
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
		WHERE d.state = 'imported' AND c.remove_completed = 1
		AND d.payload_removed = 0
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
