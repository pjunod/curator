package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pressly/goose/v3"
)

func TestMigration83BootstrapCannotTrustLaterScanFiles(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err = goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpToContext(ctx, db.W, "migrations", 82); err != nil {
		t.Fatal(err)
	}
	root, err := db.AddRootFolder(ctx, "/legacy", domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Legacy", RootFolderID: root.ID, Path: "/legacy/movie", IDs: domain.ExternalIDs{TMDB: 1}})
	if err != nil {
		t.Fatal(err)
	}
	old, err := db.UpsertFile(ctx, id, 0, "/legacy/movie/old.mkv", 4)
	if err != nil {
		t.Fatal(err)
	}
	// Titles untouched since migration 70 have no per-title revision row.
	if _, err = db.W.ExecContext(ctx, `DELETE FROM lifecycle_item_revision WHERE media_item_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	later, err := db.UpsertFile(ctx, id, 0, "/legacy/movie/later.mkv", 4)
	if err != nil {
		t.Fatal(err)
	}
	observation := func(ids ...int64) PresenceObservation {
		rev, shared, e := db.PresenceRevision(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		return PresenceObservation{ItemID: id, ItemRevision: rev, SharedRevision: shared, RootID: root.ID, RootPath: root.Path, RootIdentity: "legacy-volume", ScopePath: "/legacy/movie", CheckedAt: time.Now(), PresentFileIDs: ids}
	}
	if err = db.SavePresence(ctx, observation()); err == nil {
		t.Fatal("empty untrusted volume pinned")
	}
	if err = db.SavePresence(ctx, observation(later)); err == nil {
		t.Fatal("new scan file bootstrapped its own trust")
	}
	if err = db.SavePresence(ctx, observation(old)); err != nil {
		t.Fatal("pre-upgrade evidence rejected", err)
	}
	// A legacy title with no revision row remains readable as revision zero.
	if _, err = db.W.ExecContext(ctx, `DELETE FROM lifecycle_item_revision WHERE media_item_id=?`, id); err != nil {
		t.Fatal(err)
	}
	o := observation(old)
	o.RootGeneration = 1
	if err = db.SavePresence(ctx, o); err != nil {
		t.Fatal(err)
	}
	v := lookupOne(t, db, domain.AvailabilityTarget{Kind: domain.KindMovie, IDs: domain.ExternalIDs{TMDB: 1}})
	if v.Presence != "present" {
		t.Fatalf("legacy revision zero %+v", v)
	}
}
