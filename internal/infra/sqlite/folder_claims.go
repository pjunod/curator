package sqlite

import (
	"context"
	"path/filepath"
	"strings"
)

// ErrFolderTaken is returned when a write would point a second library item
// at a folder another item already holds. It comes from the unique index
// migration 0032 put on media_items.path, so it is the last line of defence:
// the library service picks a free folder before it ever gets here, and this
// is what a race between two adds, or a caller that skipped the service,
// runs into instead of silently creating a shared folder.
var ErrFolderTaken = errFolderTaken{}

type errFolderTaken struct{}

func (errFolderTaken) Error() string { return "folder already belongs to another library item" }

// isPathConstraint reports whether err is the media_items.path unique index
// refusing a write. Only that index names the path column, so it cannot be
// confused with the provider-id indexes that mean "already in the library".
func isPathConstraint(err error) bool {
	return isConstraint(err) && strings.Contains(err.Error(), "media_items.path")
}

// FolderClaim is one folder the library has assigned: an item's own folder,
// or a copy's separate one.
type FolderClaim struct {
	ItemID int64
	Title  string
	Path   string
	Copy   bool
}

// ListFolderClaims returns every non-empty folder assigned to an item or a
// copy, in one read. Paths are filepath.Clean'd so "/a/b/" and "/a/b" are
// the same claim.
func (d *DB) ListFolderClaims(ctx context.Context) ([]FolderClaim, error) {
	rows, err := d.R.QueryContext(ctx, `
		SELECT id, title, path, 0 FROM media_items WHERE path != ''
		UNION ALL
		SELECT c.media_item_id, m.title, c.path, 1
		  FROM media_copies c JOIN media_items m ON m.id = c.media_item_id
		 WHERE c.path != ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []FolderClaim
	for rows.Next() {
		var c FolderClaim
		var isCopy int
		if err := rows.Scan(&c.ItemID, &c.Title, &c.Path, &isCopy); err != nil {
			return nil, err
		}
		c.Path = filepath.Clean(c.Path)
		c.Copy = isCopy == 1
		out = append(out, c)
	}
	return out, rows.Err()
}
