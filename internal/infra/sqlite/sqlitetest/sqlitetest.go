// Package sqlitetest gives tests outside infra/sqlite a fully migrated
// database without running the migration chain again for every fixture.
//
// Under -race the pure-Go SQLite driver is instrumented along with
// everything else, and migrating a fresh database takes seconds instead of a
// fraction of one. Fixture-heavy suites (fault matrices, restart tests)
// build hundreds of databases, so that cost was most of the Go test time.
// Seed copies one database migrated once per test binary instead; opening
// the copy and calling Migrate is then a no-op version check, so callers
// keep their usual Open + Migrate sequence.
//
// Tests about migrations themselves must not use this: they need a database
// that has not been migrated yet.
package sqlitetest

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

var template struct {
	once sync.Once
	data []byte
	err  error
}

func image() ([]byte, error) {
	template.once.Do(func() {
		dir, err := os.MkdirTemp("", "sqlitetest-migrated-")
		if err != nil {
			template.err = err
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		db, err := sqlite.Open(dir)
		if err != nil {
			template.err = err
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
			template.data, err = os.ReadFile(filepath.Join(dir, sqlite.FileName))
		}
		template.err = err
	})
	return template.data, template.err
}

// Seed writes a fully migrated database into dir, which must not already
// hold one. sqlite.Open(dir) then opens it as it would a migrated database.
func Seed(tb testing.TB, dir string) {
	tb.Helper()
	data, err := image()
	if err != nil {
		tb.Fatalf("sqlitetest: migrating the template: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sqlite.FileName), data, 0o600); err != nil {
		tb.Fatalf("sqlitetest: seeding %s: %v", dir, err)
	}
}

// Dir returns a fresh test directory holding a migrated database.
func Dir(tb testing.TB) string {
	tb.Helper()
	dir := tb.TempDir()
	Seed(tb, dir)
	return dir
}
