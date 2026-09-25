package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/pressly/goose/v3"

	"github.com/pjunod/monarr/internal/domain/naming"
)

// Migration 32 makes "one folder, one library item" a property of the
// schema instead of a hope.
//
// Folders were derived from "Title (Year)" alone, so two different works
// with the same title and year — there really are two films called
// Leviticus from 2022 — were given the same folder. Only one of them can
// own the files in it: whichever scan ran last linked them, and the other
// was a card that looked real and held nothing. The library service now
// adds the provider id ("Leviticus (2022) {tmdb-123}") when the plain name
// is taken; this migration repairs libraries that already have the damage
// and then puts a unique index under the column so it cannot come back.
//
// The repair only rewrites media_items.path. Nothing on disk moves. For
// each shared folder the item that owns files keeps it (most media_files
// rows, then the older item); every other item is pointed at its own
// disambiguated folder, which import creates when that item is grabbed.
func init() {
	goose.AddNamedMigrationContext("0032_unique_item_folder.go", up0032, down0032)
}

type folderHolder struct {
	id    int64
	path  string
	tmdb  int64
	tvdb  int64
	imdb  string
	olid  string
	files int64
}

func up0032(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT m.id, m.path, m.tmdb_id, m.tvdb_id, m.imdb_id, m.olid,
		       (SELECT count(*) FROM media_files f WHERE f.media_item_id = m.id)
		  FROM media_items m WHERE m.path != ''`)
	if err != nil {
		return err
	}
	byPath := map[string][]folderHolder{}
	taken := map[string]bool{}
	for rows.Next() {
		var h folderHolder
		if err := rows.Scan(&h.id, &h.path, &h.tmdb, &h.tvdb, &h.imdb, &h.olid, &h.files); err != nil {
			_ = rows.Close()
			return err
		}
		clean := filepath.Clean(h.path)
		byPath[clean] = append(byPath[clean], h)
		taken[clean] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}

	// The index compares bytes and every writer stores cleaned paths, so
	// stored spellings are cleaned first ("/m/Film/" → "/m/Film"). Without
	// this a keeper that kept "/m/Film/" would still be one item on one
	// folder, but a second item added as "/m/Film" would sail past the
	// index. Grouping below is by the cleaned path for the same reason.
	for p, holders := range byPath {
		for _, h := range holders {
			if h.path == p {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE media_items SET path = ? WHERE id = ?`, p, h.id); err != nil {
				return fmt.Errorf("cleaning path of item %d: %w", h.id, err)
			}
		}
	}

	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		holders := byPath[p]
		if len(holders) < 2 {
			continue
		}
		sort.SliceStable(holders, func(i, j int) bool {
			if holders[i].files != holders[j].files {
				return holders[i].files > holders[j].files
			}
			return holders[i].id < holders[j].id
		})
		for _, h := range holders[1:] {
			next := freeTaggedPath(p, holderTag(h), taken)
			taken[next] = true
			if _, err := tx.ExecContext(ctx,
				`UPDATE media_items SET path = ? WHERE id = ?`, next, h.id); err != nil {
				return fmt.Errorf("moving item %d off %s: %w", h.id, p, err)
			}
			slog.Default().Info("migration 0032: item given its own folder",
				"item", h.id, "was", p, "now", next, "kept by", holders[0].id)
		}
	}

	// Paths are stored cleaned by every writer; the index compares bytes.
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS
		idx_media_items_path ON media_items (path) WHERE path != ''`); err != nil {
		return fmt.Errorf("creating idx_media_items_path: %w", err)
	}
	return nil
}

func down0032(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_media_items_path`)
	return err
}

// holderTag is the provider hint for an item, in the order the library
// service uses (see library.folderTag): TMDB, TVDB, IMDb, Open Library,
// and Monarr's own id when no provider backs the item.
func holderTag(h folderHolder) string {
	switch {
	case h.tmdb != 0:
		return naming.FolderTag("tmdb", strconv.FormatInt(h.tmdb, 10))
	case h.tvdb != 0:
		return naming.FolderTag("tvdb", strconv.FormatInt(h.tvdb, 10))
	case h.imdb != "":
		return naming.FolderTag("imdb", h.imdb)
	case h.olid != "":
		return naming.FolderTag("olid", h.olid)
	default:
		return naming.FolderTag("monarr", strconv.FormatInt(h.id, 10))
	}
}

// freeTaggedPath appends tag to base, then a counter if even that is taken.
func freeTaggedPath(base, tag string, taken map[string]bool) string {
	candidate := base + " " + tag
	for n := 2; taken[candidate]; n++ {
		candidate = fmt.Sprintf("%s %s (%d)", base, tag, n)
	}
	return candidate
}
