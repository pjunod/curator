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
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	sqlitegen "github.com/monarr-media/monarr/internal/infra/sqlite/gen"
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

// Open creates the data dir if needed, opens the database with WAL mode and
// sane pragmas, and verifies connectivity. Call Migrate before first use.
func Open(dataDir string) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("sqlite: creating data dir: %w", err)
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
	return nil
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
