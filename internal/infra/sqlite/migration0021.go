package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/pressly/goose/v3"
)

// Migration 21 repairs a schema that goose believes is at 20 but isn't.
//
// goose identifies a migration by the number in its filename and nothing
// else — no checksum, no name. Two branches that each add an `0020_*.sql`
// are, as far as goose is concerned, the same migration: whichever one runs
// first records version 20, and the other is skipped forever, silently, with
// no error at startup and no line in the log. That is not hypothetical here;
// it is how a database ends up rejecting `INSERT INTO download_clients …
// mode` with "no column named mode" while the binary is perfectly up to
// date. Several agents work in this repo at once, so it will happen again.
//
// So this migration does not assume what 0020 did or didn't do. It looks at
// the schema, and makes it match what 0020 intended:
//
//   - `downloads.transfer` — the id that names one transfer end to end.
//   - `download_clients.mode` — 'poll' | 'push'.
//   - `download_clients.type` accepting 'nzbd'.
//
// Every step is conditional, so on a database where 0020 applied correctly
// this is a no-op that costs three reads of `sqlite_master`. It is written
// in Go rather than SQL because SQLite has no `ADD COLUMN IF NOT EXISTS`,
// and a plain rebuild would have to name a column that may not be there.
func init() {
	// The name is what goose derives the version from; the `.go` extension
	// is what tells it this is a registered function and not a file it
	// should go looking for on disk.
	goose.AddNamedMigrationContext("0021_nzbd_native_repair.go", up0021, nil)
}

// The shape `download_clients` must end up in — 0020's table, with `mode`
// declared inline rather than bolted on afterwards.
const downloadClientsShape = `CREATE TABLE download_clients_repair (
    id              INTEGER PRIMARY KEY,
    type            TEXT NOT NULL CHECK (type IN ('qbittorrent', 'sabnzbd', 'transmission', 'deluge', 'nzbget', 'nzbd')),
    name            TEXT NOT NULL,
    url             TEXT NOT NULL,
    username        TEXT NOT NULL DEFAULT '',
    password        TEXT NOT NULL DEFAULT '',
    category        TEXT NOT NULL DEFAULT 'monarr',
    enabled         INTEGER NOT NULL DEFAULT 1,
    path_mappings   TEXT NOT NULL DEFAULT '[]',
    manual_approval INTEGER NOT NULL DEFAULT 0,
    added_at        INTEGER NOT NULL,
    mode            TEXT NOT NULL DEFAULT 'poll' CHECK (mode IN ('poll', 'push'))
) STRICT`

func up0021(ctx context.Context, tx *sql.Tx) error {
	hasTransfer, err := columnExists(ctx, tx, "downloads", "transfer")
	if err != nil {
		return err
	}
	if !hasTransfer {
		if _, err := tx.ExecContext(ctx,
			`ALTER TABLE downloads ADD COLUMN transfer TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("repairing downloads.transfer: %w", err)
		}
	}

	hasMode, err := columnExists(ctx, tx, "download_clients", "mode")
	if err != nil {
		return err
	}
	ddl, err := tableDDL(ctx, tx, "download_clients")
	if err != nil {
		return err
	}
	// The CHECK is the other half of 0020: without 'nzbd' in it, adding a
	// native client fails on the constraint instead of on the column, which
	// looks like a completely different bug.
	knowsNzbd := strings.Contains(ddl, "'nzbd'")
	if hasMode && knowsNzbd {
		return nil
	}

	// Preserve whatever `mode` holds when the column is already there — an
	// operator who turned push on should not have it turned back off by a
	// repair.
	modeExpr := "'poll'"
	if hasMode {
		modeExpr = "mode"
	}
	stmts := []string{
		downloadClientsShape,
		`INSERT INTO download_clients_repair
		    (id, type, name, url, username, password, category, enabled,
		     path_mappings, manual_approval, added_at, mode)
		 SELECT id, type, name, url, username, password, category, enabled,
		     path_mappings, manual_approval, added_at, ` + modeExpr + `
		 FROM download_clients`,
		`DROP TABLE download_clients`,
		`ALTER TABLE download_clients_repair RENAME TO download_clients`,
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("rebuilding download_clients: %w", err)
		}
	}
	return nil
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func columnExists(ctx context.Context, q querier, table, column string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("looking for %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}

func tableDDL(ctx context.Context, q querier, table string) (string, error) {
	var ddl string
	err := q.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&ddl)
	if err != nil {
		return "", fmt.Errorf("reading the definition of %s: %w", table, err)
	}
	return ddl, nil
}
