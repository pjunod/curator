package sqlite

import (
	"github.com/pjunod/monarr/internal/ports"
	"github.com/pressly/goose/v3"
	"reflect"
	"testing"
)

func TestNativeTorrentClientMigrationPreservesConfigTriggersAndRefusesUnsafeDown(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	if err := goose.DownToContext(ctx, db.W, "migrations", 67); err != nil {
		t.Fatal(err)
	}
	old := ports.ClientConfig{Type: "nzbd", Name: "existing usenet", URL: "http://runner:6789", Username: "user", Password: "secret-fixture", Category: "movies", Enabled: true, ManualApproval: true, RemoveCompleted: true, Mode: "push", PathMappings: []ports.PathMapping{{Remote: "/old", Local: "/local"}}}
	id, err := db.AddDownloadClient(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	before, err := db.GetDownloadClient(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := db.GetDownloadClient(ctx, id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("old config changed: %+v %+v %v", before, after, err)
	}
	revision, err := db.LibraryRevision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after.Name = "existing renamed"
	if err := db.UpdateDownloadClient(ctx, after); err != nil {
		t.Fatal(err)
	}
	next, err := db.LibraryRevision(ctx)
	if err != nil || next <= revision {
		t.Fatalf("client revision trigger lost: %d -> %d %v", revision, next, err)
	}
	nativeID, err := db.AddDownloadClient(ctx, ports.ClientConfig{Type: "runner-torrent", Name: "native", URL: "http://runner:6789", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := goose.DownContext(ctx, db.W, "migrations"); err == nil {
		t.Fatal("downgrade erased configured native adapter contract")
	}
	native, err := db.GetDownloadClient(ctx, nativeID)
	if err != nil || native.Type != "runner-torrent" {
		t.Fatalf("failed down changed native config: %+v %v", native, err)
	}
	if err := db.UpdateDownloadClient(ctx, after); err != nil {
		t.Fatal(err)
	}
}
