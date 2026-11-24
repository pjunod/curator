package sqlite

import (
	"context"
	"time"
)

// SearchRecordsKept is how many search records one wantable keeps.
const SearchRecordsKept = 20

// SearchRecord is one search of one wantable (migration 0078): who ran it,
// what the indexers returned, why the matched releases were refused, and
// what it led to. Skipped is set when the wantable was not searched.
type SearchRecord struct {
	ID           int64     `json:"id"`
	MediaItemID  int64     `json:"mediaItemId"`
	CopyID       int64     `json:"copyId"`
	WantableID   string    `json:"wantableId"`
	Season       int       `json:"season"`
	Episode      int       `json:"episode"`
	At           time.Time `json:"at"`
	Trigger      string    `json:"trigger"`
	Seen         int       `json:"seen"`
	Matched      int       `json:"matched"`
	Accepted     int       `json:"accepted"`
	TopRejection string    `json:"topRejection,omitempty"`
	Grabbed      string    `json:"grabbed,omitempty"`
	DownloadID   int64     `json:"downloadId,omitempty"`
	Skipped      string    `json:"skipped,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// RecordSearch appends one search record and keeps the newest
// SearchRecordsKept for its wantable, in one transaction.
func (d *DB) RecordSearch(ctx context.Context, r SearchRecord) error {
	if r.At.IsZero() {
		r.At = time.Now()
	}
	tx, err := d.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO search_records
		(media_item_id, copy_id, wantable_id, season, episode, at, trigger, seen, matched, accepted, top_rejection, grabbed, download_id, skipped, error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.MediaItemID, r.CopyID, r.WantableID, r.Season, r.Episode, r.At.UnixMilli(), r.Trigger,
		r.Seen, r.Matched, r.Accepted, r.TopRejection, r.Grabbed, r.DownloadID, r.Skipped, r.Error); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM search_records WHERE wantable_id=? AND id NOT IN
		(SELECT id FROM search_records WHERE wantable_id=? ORDER BY id DESC LIMIT ?)`,
		r.WantableID, r.WantableID, SearchRecordsKept); err != nil {
		return err
	}
	return tx.Commit()
}

// ItemSearchRecords returns the search records of one item's primary copy
// in one season (-1 for a movie or book), newest first.
func (d *DB) ItemSearchRecords(ctx context.Context, itemID int64, season, limit int) ([]SearchRecord, error) {
	rows, err := d.R.QueryContext(ctx, `SELECT id, media_item_id, copy_id, wantable_id, season, episode, at, trigger,
		seen, matched, accepted, top_rejection, grabbed, download_id, skipped, error
		FROM search_records WHERE media_item_id=? AND season=? AND copy_id=0 ORDER BY id DESC LIMIT ?`, itemID, season, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SearchRecord
	for rows.Next() {
		var r SearchRecord
		var at int64
		if err := rows.Scan(&r.ID, &r.MediaItemID, &r.CopyID, &r.WantableID, &r.Season, &r.Episode, &at, &r.Trigger,
			&r.Seen, &r.Matched, &r.Accepted, &r.TopRejection, &r.Grabbed, &r.DownloadID, &r.Skipped, &r.Error); err != nil {
			return nil, err
		}
		r.At = time.UnixMilli(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ItemDownloads returns an item's newest download rows, newest first.
func (d *DB) ItemDownloads(ctx context.Context, itemID int64, limit int) ([]Download, error) {
	rows, err := d.downloadsWhere(ctx, `id IN (SELECT id FROM downloads WHERE media_item_id=? ORDER BY id DESC LIMIT ?)`, itemID, limit)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

// LastHistoryAt is when an item last had a history event of this type
// (zero time if never), read through idx_history_item.
func (d *DB) LastHistoryAt(ctx context.Context, itemID int64, eventType string) (time.Time, error) {
	var ts int64
	err := d.R.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts),0) FROM history_events WHERE media_item_id=? AND type=?`, itemID, eventType).Scan(&ts)
	if err != nil || ts == 0 {
		return time.Time{}, err
	}
	return time.UnixMilli(ts), nil
}
