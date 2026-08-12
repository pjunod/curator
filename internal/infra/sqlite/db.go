// Package sqlite owns Monarr's storage: one SQLite file in the data dir,
// WAL mode, goose migrations embedded in the binary, sqlc-generated queries.
//
// Single-writer discipline (ADR 0004): all writes flow through W, a pool
// capped at one connection; reads use the R pool. This sidesteps SQLite's
// write-lock contention entirely at homelab scale.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	sqlitegen "github.com/pjunod/monarr/internal/infra/sqlite/gen"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// FileName is the database file created inside the data dir.
const FileName = "monarr.db"

// DB bundles the write handle, the read pool, and sqlc query objects bound
// to each.
type DB struct {
	W     *sql.DB // single-connection write handle
	R     *sql.DB // read pool
	Write *sqlitegen.Queries
	Read  *sqlitegen.Queries
	Path  string
}

// Open creates the data dir, proves it is writable, then opens the database
// with WAL mode and sane pragmas and verifies connectivity. Call Migrate
// before first use.
//
// The writability check runs first on purpose: a root-owned bind mount
// otherwise fails several layers down as an opaque driver error that says
// nothing about ownership (see CheckDataDir).
func Open(dataDir string) (*DB, error) {
	if err := CheckDataDir(dataDir); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, FileName)
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: opening write handle: %w", err)
	}
	w.SetMaxOpenConns(1)

	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("sqlite: opening read pool: %w", err)
	}
	r.SetMaxOpenConns(4)

	db := &DB{
		W:     w,
		R:     r,
		Write: sqlitegen.New(w),
		Read:  sqlitegen.New(r),
		Path:  path,
	}
	if err := w.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: ping: %w", err)
	}
	return db, nil
}

// Migrate applies all embedded goose migrations through the write handle.
func (d *DB) Migrate(ctx context.Context) error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("sqlite: setting goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, d.W, "migrations"); err != nil {
		return fmt.Errorf("sqlite: migrating: %w", err)
	}
	return d.verifySchema(ctx)
}

// Columns the query layer depends on and that a migration is supposed to
// have added. Deliberately not the whole schema — this is a tripwire for one
// specific failure, not a substitute for the migrations.
var requiredColumns = []struct{ table, column string }{
	{"downloads", "transfer"},
	{"download_clients", "mode"},
	{"media_items", "book_type"},
	{"media_copies", "book_type"},
}

// verifySchema refuses to start on a database that goose considers migrated
// but isn't.
//
// goose identifies a migration solely by the number in its filename, so two
// branches that both add an `0020_*.sql` collide: the first to run records
// version 20 and the second is skipped permanently, without an error. The
// symptom surfaces much later and somewhere else — a "SQL logic error: table
// download_clients has no column named mode" in the settings UI, from a
// binary whose code is entirely correct. That is a miserable thing to debug
// from the far end.
//
// So the check happens here, where the cause is still visible, and it is
// fatal: a database missing a column the query layer writes to is not a
// degraded server, it is one that will fail at some unpredictable later
// moment. Migration 21 repairs the known instance of this; this exists to
// name the next one out loud.
func (d *DB) verifySchema(ctx context.Context) error {
	var missing []string
	for _, want := range requiredColumns {
		ok, err := columnExists(ctx, d.R, want.table, want.column)
		if err != nil {
			return fmt.Errorf("sqlite: verifying schema: %w", err)
		}
		if !ok {
			missing = append(missing, want.table+"."+want.column)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	version, err := goose.GetDBVersionContext(ctx, d.R)
	if err != nil {
		version = -1
	}
	return fmt.Errorf(
		"sqlite: the database reports schema version %d, but %s %s missing — a "+
			"migration was recorded as applied without running, which happens when two "+
			"branches add migrations with the same number. Check goose_db_version against "+
			"the files in internal/infra/sqlite/migrations",
		version, strings.Join(missing, ", "), plural(len(missing), "is", "are"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// SchemaVersion reports the current goose migration version.
func (d *DB) SchemaVersion(ctx context.Context) (int64, error) {
	return goose.GetDBVersionContext(ctx, d.R)
}

// Checkpoint truncates the WAL. Run periodically by the scheduler so the
// -wal file doesn't grow without bound between backups.
func (d *DB) Checkpoint(ctx context.Context) error {
	_, err := d.W.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE);")
	return err
}

// SetMeta upserts a key in app_meta. Exposed as a method so callers outside
// this package never touch the generated query code directly.
func (d *DB) SetMeta(ctx context.Context, key, value string) error {
	return d.Write.SetMeta(ctx, sqlitegen.SetMetaParams{
		Key:       key,
		Value:     value,
		UpdatedAt: time.Now().UnixMilli(),
	})
}

// GetMeta reads a key from app_meta; sql.ErrNoRows if absent.
func (d *DB) GetMeta(ctx context.Context, key string) (string, error) {
	return d.Read.GetMeta(ctx, key)
}

// Ping verifies both handles.
func (d *DB) Ping(ctx context.Context) error {
	if err := d.W.PingContext(ctx); err != nil {
		return err
	}
	return d.R.PingContext(ctx)
}

// Close closes both handles.
func (d *DB) Close() error {
	var first error
	if d.R != nil {
		if err := d.R.Close(); err != nil {
			first = err
		}
	}
	if d.W != nil {
		if err := d.W.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
