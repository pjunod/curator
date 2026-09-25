package library

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/naming"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

// One folder, one item.
//
// A library folder can only belong to one item: the scan links the files in
// it to whichever item it processes last, and any other item pointed there
// is a card that looks real and holds nothing. The naming rule alone
// ("Title (Year)") cannot guarantee that — two different films can share a
// title and a year — so every place that chooses a folder goes through
// freeFolder, and every place that accepts one from the caller goes through
// ensureFolderFree. The unique index from migration 0032 is the backstop.

// folderTag is the provider hint that tells two same-named items apart on
// disk. The order matches migration 0032 so a repaired library and a newly
// added item disambiguate the same way.
func folderTag(item domain.MediaItem) string {
	switch {
	case item.IDs.TMDB != 0:
		return naming.FolderTag("tmdb", strconv.FormatInt(item.IDs.TMDB, 10))
	case item.IDs.TVDB != 0:
		return naming.FolderTag("tvdb", strconv.FormatInt(item.IDs.TVDB, 10))
	case item.IDs.IMDB != "":
		return naming.FolderTag("imdb", item.IDs.IMDB)
	case item.IDs.OLID != "":
		return naming.FolderTag("olid", item.IDs.OLID)
	case item.ID != 0:
		return naming.FolderTag("monarr", strconv.FormatInt(item.ID, 10))
	default:
		return ""
	}
}

// baseFolder is the naming rule's folder for item under root.
func baseFolder(root string, item domain.MediaItem) string {
	if item.Kind == domain.KindBook {
		return filepath.Join(root, naming.BookFolder(item.Author, item.Title))
	}
	return filepath.Join(root, naming.FolderName(item.Title, item.Year))
}

// folderHolder returns the item (other than self) that already holds path,
// as its own folder or a copy's.
func (s *Service) folderHolder(ctx context.Context, path string, self int64) (sqlite.FolderClaim, bool, error) {
	if path == "" {
		return sqlite.FolderClaim{}, false, nil
	}
	claims, err := s.db.ListFolderClaims(ctx)
	if err != nil {
		return sqlite.FolderClaim{}, false, err
	}
	path = filepath.Clean(path)
	for _, c := range claims {
		if c.ItemID != self && c.Path == path {
			return c, true, nil
		}
	}
	return sqlite.FolderClaim{}, false, nil
}

// freeFolder returns the folder item should live in under root: the naming
// rule's "Title (Year)" when nobody else holds it, and "Title (Year)
// {tmdb-123}" when somebody does. self is the item's own id (0 while it is
// being added), so an item never collides with itself.
func (s *Service) freeFolder(ctx context.Context, root string, item domain.MediaItem) (string, error) {
	claims, err := s.db.ListFolderClaims(ctx)
	if err != nil {
		return "", err
	}
	taken := map[string]bool{}
	for _, c := range claims {
		if c.ItemID != item.ID || item.ID == 0 {
			taken[c.Path] = true
		}
	}
	base := baseFolder(root, item)
	if !taken[base] {
		return base, nil
	}
	tag := folderTag(item)
	if tag == "" {
		tag = "(2)"
	}
	candidate := base + " " + tag
	for n := 2; taken[candidate]; n++ {
		candidate = fmt.Sprintf("%s %s (%d)", base, tag, n)
	}
	s.log.Info("library: folder name already in use, disambiguating",
		"title", item.Title, "taken", base, "using", candidate)
	return candidate, nil
}

// ensureFolderFree refuses a caller-chosen folder another item holds.
func (s *Service) ensureFolderFree(ctx context.Context, path string, self int64) error {
	holder, held, err := s.folderHolder(ctx, path, self)
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("%w: %s already belongs to %q (item %d); each library item needs its own folder",
			ErrFolderConflict, filepath.Clean(path), holder.Title, holder.ItemID)
	}
	return nil
}

// folderTakenErr turns the index's refusal into the service's sentinel, so
// a race the pre-check could not see still answers 409 rather than 500.
func folderTakenErr(err error, path string) error {
	if errors.Is(err, sqlite.ErrFolderTaken) {
		return fmt.Errorf("%w: %s already belongs to another library item", ErrFolderConflict, path)
	}
	return err
}
