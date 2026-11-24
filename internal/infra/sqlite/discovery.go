package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/pjunod/monarr/internal/ports"
)

func (d *DB) DiscoverySeeds(ctx context.Context, q string, offset int) ([]ports.LibrarySeed, bool, error) {
	if offset < 0 || offset > 100000 || len(q) > 200 {
		return nil, false, fmt.Errorf("invalid library seed page")
	}
	rows, err := d.R.QueryContext(ctx, `SELECT id,kind,tmdb_id,tvdb_id,imdb_id,title,year,overview,poster_path FROM media_items WHERE kind IN ('movie','series') AND tmdb_id>0 AND (?='' OR instr(lower(title),lower(?))>0) ORDER BY sort_title,id LIMIT 51 OFFSET ?`, strings.TrimSpace(q), strings.TrimSpace(q), offset)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	out := []ports.LibrarySeed{}
	for rows.Next() {
		var seed ports.LibrarySeed
		if err = rows.Scan(&seed.LibraryItemID, &seed.Item.Kind, &seed.Item.TMDBID, &seed.Item.TVDBID, &seed.Item.IMDBID, &seed.Item.Title, &seed.Item.Year, &seed.Item.Overview, &seed.Item.PosterPath); err != nil {
			return nil, false, err
		}
		seed.Item.Source = "library"
		out = append(out, seed)
	}
	if err = rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > 50
	if more {
		out = out[:50]
	}
	return out, more, nil
}
func (d *DB) DiscoverySeed(ctx context.Context, id int64) (ports.LibrarySeed, error) {
	var seed ports.LibrarySeed
	err := d.R.QueryRowContext(ctx, `SELECT id,kind,tmdb_id,tvdb_id,imdb_id,title,year,overview,poster_path FROM media_items WHERE id=? AND kind IN ('movie','series')`, id).Scan(&seed.LibraryItemID, &seed.Item.Kind, &seed.Item.TMDBID, &seed.Item.TVDBID, &seed.Item.IMDBID, &seed.Item.Title, &seed.Item.Year, &seed.Item.Overview, &seed.Item.PosterPath)
	if err == sql.ErrNoRows {
		return seed, ErrNotFound
	}
	seed.Item.Source = "library"
	return seed, err
}
