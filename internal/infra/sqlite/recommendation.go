package sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// LookupSeries performs at most one narrow read per external-ID namespace.
func (d *DB) LookupSeries(ctx context.Context, ids []domain.ExternalIDs) ([]ports.Ownership, error) {
	type match struct {
		id  int64
		ids domain.ExternalIDs
	}
	all := map[int64]match{}
	for _, namespace := range []string{"tmdb_id", "tvdb_id", "imdb_id"} {
		args := []any{}
		seen := map[string]bool{}
		for _, id := range ids {
			var v any
			switch namespace {
			case "tmdb_id":
				if id.TMDB > 0 {
					v = id.TMDB
				}
			case "tvdb_id":
				if id.TVDB > 0 {
					v = id.TVDB
				}
			case "imdb_id":
				if id.IMDB != "" {
					v = id.IMDB
				}
			}
			if v != nil {
				key := fmt.Sprint(v)
				if !seen[key] {
					seen[key] = true
					args = append(args, v)
				}
			}
		}
		if len(args) == 0 {
			continue
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
		rows, err := d.R.QueryContext(ctx, "SELECT id,tmdb_id,tvdb_id,imdb_id FROM media_items WHERE kind='series' AND "+namespace+" IN ("+marks+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var m match
			if err = rows.Scan(&m.id, &m.ids.TMDB, &m.ids.TVDB, &m.ids.IMDB); err != nil {
				_ = rows.Close()
				return nil, err
			}
			all[m.id] = m
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	out := make([]ports.Ownership, len(ids))
	for i, id := range ids {
		out[i].State = "absent"
		matches := 0
		for _, m := range all {
			same := id.TMDB > 0 && id.TMDB == m.ids.TMDB || id.TVDB > 0 && id.TVDB == m.ids.TVDB || id.IMDB != "" && strings.EqualFold(id.IMDB, m.ids.IMDB)
			if !same {
				continue
			}
			matches++
			out[i].State = "present"
			out[i].LibraryItemID = m.id
			if domain.ExternalIDsConflict(id, m.ids) {
				out[i].Conflict = true
			}
		}
		if matches > 1 || out[i].Conflict {
			out[i].State = "ambiguous"
			out[i].LibraryItemID = 0
			out[i].Conflict = true
		}
	}
	return out, nil
}
