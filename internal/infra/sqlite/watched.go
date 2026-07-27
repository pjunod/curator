package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/monarr-media/monarr/internal/domain"
	sqlitegen "github.com/monarr-media/monarr/internal/infra/sqlite/gen"
)

// PlurxWatch is one thing somebody finished, as plurx reported it.
//
// Thin on purpose. These rows are other people's viewing history held by an
// application whose job is downloading: who, what, when, and nothing else.
type PlurxWatch struct {
	MediaItemID int64     `json:"mediaItemId"`
	Username    string    `json:"username,omitempty"`
	Season      int64     `json:"season,omitempty"`
	Episode     int64     `json:"episode,omitempty"`
	WatchedAt   time.Time `json:"watchedAt"`
}

// RecordPlurxWatched stores one watched notification.
func (d *DB) RecordPlurxWatched(ctx context.Context, w PlurxWatch) error {
	return d.Write.RecordPlurxWatched(ctx, sqlitegen.RecordPlurxWatchedParams{
		MediaItemID: w.MediaItemID,
		Username:    w.Username,
		Season:      w.Season,
		Episode:     w.Episode,
		WatchedAt:   w.WatchedAt.UnixMilli(),
		CreatedAt:   time.Now().UnixMilli(),
	})
}

// ListPlurxWatched returns an item's watch records, newest first.
func (d *DB) ListPlurxWatched(ctx context.Context, itemID, limit int64) ([]PlurxWatch, error) {
	rows, err := d.Read.ListPlurxWatched(ctx, sqlitegen.ListPlurxWatchedParams{
		MediaItemID: itemID, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]PlurxWatch, 0, len(rows))
	for _, r := range rows {
		out = append(out, PlurxWatch{
			MediaItemID: r.MediaItemID, Username: r.Username,
			Season: r.Season, Episode: r.Episode,
			WatchedAt: time.UnixMilli(r.WatchedAt),
		})
	}
	return out, nil
}

// ActivelyWatched returns item ids somebody has finished something from
// since `since`, and when.
//
// This is what "prefer upgrades for what is being watched" reads. A set
// rather than a score: the decision it feeds is an ordering, and inventing a
// numeric intensity from a handful of timestamps would be precision nobody
// asked for.
func (d *DB) ActivelyWatched(ctx context.Context, since time.Time) (map[int64]time.Time, error) {
	rows, err := d.Read.RecentlyWatchedItems(ctx, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	out := make(map[int64]time.Time, len(rows))
	for _, r := range rows {
		if ms, ok := r.LastWatched.(int64); ok {
			out[r.MediaItemID] = time.UnixMilli(ms)
		}
	}
	return out, nil
}

// FindItemByIDs resolves the media item a remote application is talking
// about, by external id.
//
// Ids only, never titles. An application that has to guess which item you
// meant is the failure mode this whole integration was built to remove; a
// notification for something monarr does not have is not an error, it is a
// notification for something monarr does not have.
func (d *DB) FindItemByIDs(ctx context.Context, kind domain.MediaKind, tmdbID int64, imdbID string) (int64, error) {
	if tmdbID != 0 {
		row, err := d.Read.FindItemByTmdb(ctx, sqlitegen.FindItemByTmdbParams{
			Kind: string(kind), TmdbID: tmdbID,
		})
		if err == nil {
			return row.ID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	if imdbID != "" {
		row, err := d.Read.FindItemByImdb(ctx, sqlitegen.FindItemByImdbParams{
			Kind: string(kind), ImdbID: imdbID,
		})
		if err == nil {
			return row.ID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	return 0, ErrNotFound
}
