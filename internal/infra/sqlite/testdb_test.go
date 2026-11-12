package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Tests that need a fresh, fully migrated database get a byte copy of one
// database migrated once per test binary. Migrating from scratch is most of
// a fixture's cost under -race: the pure-Go SQLite driver is instrumented
// too, so the full migration chain takes seconds per database instead of a
// fraction of one, and the fault matrices build a fixture per injected
// failure point. Tests about migrations themselves open and migrate their
// own database and do not come through here.
var migratedSQLiteTemplate struct {
	once sync.Once
	data []byte
	err  error
}

func migratedSQLiteImage(t testing.TB) []byte {
	t.Helper()
	migratedSQLiteTemplate.once.Do(func() {
		dir, err := os.MkdirTemp("", "sqlite-migrated-")
		if err != nil {
			migratedSQLiteTemplate.err = err
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		db, err := Open(dir)
		if err != nil {
			migratedSQLiteTemplate.err = err
			return
		}
		ctx := context.Background()
		if err = db.Migrate(ctx); err == nil {
			// Fold the WAL into the main file so the file alone is the image.
			err = db.Checkpoint(ctx)
		}
		if closeErr := db.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			migratedSQLiteTemplate.data, err = os.ReadFile(filepath.Join(dir, FileName))
		}
		migratedSQLiteTemplate.err = err
	})
	if migratedSQLiteTemplate.err != nil {
		t.Fatal(migratedSQLiteTemplate.err)
	}
	return migratedSQLiteTemplate.data
}

// openMigratedCopyIn writes the migrated image into dir and opens it; the
// handle is closed at cleanup. It does not run Migrate: goose keeps global
// state that parallel fault points would race on.
func openMigratedCopyIn(t *testing.T, dir string) *DB {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), migratedSQLiteImage(t), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// openMigratedCopy opens a private copy of the migrated template.
func openMigratedCopy(t *testing.T) *DB {
	t.Helper()
	return openMigratedCopyIn(t, t.TempDir())
}

// openTestDB is a migrated copy on which Migrate has also run (a no-op
// version check), so goose's package state is set up exactly as it is for a
// database migrated from scratch. Tests that step goose down rely on that.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db := openMigratedCopy(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}
