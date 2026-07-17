package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/infra/scheduler"
	sqlitegen "github.com/monarr-media/monarr/internal/infra/sqlite/gen"
)

func sqlitegenSetMeta(key, value string) sqlitegen.SetMetaParams {
	return sqlitegen.SetMetaParams{Key: key, Value: value, UpdatedAt: time.Now().UnixMilli()}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func TestMigrateFromScratch(t *testing.T) {
	db := openTestDB(t)
	v, err := db.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if v < 1 {
		t.Errorf("schema version = %d, want >= 1", v)
	}
}

func TestWALModeActive(t *testing.T) {
	db := openTestDB(t)
	var mode string
	if err := db.R.QueryRowContext(context.Background(), "PRAGMA journal_mode;").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.Write.SetMeta(ctx, sqlitegenSetMeta("greeting", "hello")); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	got, err := db.Read.GetMeta(ctx, "greeting")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if got != "hello" {
		t.Errorf("GetMeta = %q, want hello", got)
	}

	// Upsert path.
	if err := db.Write.SetMeta(ctx, sqlitegenSetMeta("greeting", "hi again")); err != nil {
		t.Fatalf("SetMeta upsert: %v", err)
	}
	got, _ = db.Read.GetMeta(ctx, "greeting")
	if got != "hi again" {
		t.Errorf("GetMeta after upsert = %q", got)
	}
}

func TestTaskStoreRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	store := NewTaskStore(db)

	start := time.Now().Truncate(time.Millisecond)
	rec := scheduler.RunRecord{
		Name:            "health.check",
		IntervalSeconds: 60,
		StartedAt:       start,
		Duration:        250 * time.Millisecond,
		Err:             "",
		NextRunAt:       start.Add(time.Minute),
	}
	if err := store.RecordRun(ctx, rec); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	// Second run with an error overwrites.
	rec.Err = "boom"
	if err := store.RecordRun(ctx, rec); err != nil {
		t.Fatalf("RecordRun 2: %v", err)
	}

	loaded, err := store.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("LoadAll returned %d rows", len(loaded))
	}
	p := loaded[0]
	if p.Name != "health.check" || !p.LastRunAt.Equal(start) ||
		p.LastDuration != 250*time.Millisecond || p.LastError != "boom" {
		t.Errorf("loaded = %+v", p)
	}
}

func TestCheckpoint(t *testing.T) {
	db := openTestDB(t)
	if err := db.Checkpoint(context.Background()); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
}
