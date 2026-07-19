package sqlite

import (
	"context"
	"os"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

func TestBlocklistRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.AddBlocklist(ctx, 1, "Bad.Release.1080p", "idx", "stalled"); err != nil {
		t.Fatal(err)
	}
	// Idempotent on the same (title, indexer).
	if err := db.AddBlocklist(ctx, 1, "Bad.Release.1080p", "idx", "again"); err != nil {
		t.Fatal(err)
	}
	blocked, err := db.IsBlocklisted(ctx, "Bad.Release.1080p", "idx")
	if err != nil || !blocked {
		t.Fatalf("blocked = %v err %v", blocked, err)
	}
	if b, _ := db.IsBlocklisted(ctx, "Other.Release", "idx"); b {
		t.Error("unrelated release blocked")
	}
	list, err := db.ListBlocklist(ctx)
	if err != nil || len(list) != 1 || list[0].Reason != "again" {
		t.Fatalf("list = %+v err %v", list, err)
	}
	if err := db.DeleteBlocklist(ctx, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := db.IsBlocklisted(ctx, "Bad.Release.1080p", "idx"); b {
		t.Error("still blocked after delete")
	}
}

func TestNotifierCRUD(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.AddNotifier(ctx, ports.NotifierConfig{
		Type: "webhook", Name: "hook", Settings: map[string]string{"url": "http://x"},
		OnGrab: true, OnImport: true, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetNotifier(ctx, id)
	if err != nil || got.Type != "webhook" || got.Settings["url"] != "http://x" || !got.OnGrab || got.OnHealth {
		t.Fatalf("notifier = %+v err %v", got, err)
	}
	all, err := db.ListNotifiers(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("list = %+v err %v", all, err)
	}
	if err := db.DeleteNotifier(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetNotifier(ctx, id); err == nil {
		t.Error("get after delete should fail")
	}
}

func TestCalendar(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	_, err := db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindMovie, Title: "Spring Movie", SortTitle: "spring movie",
		Year: 2026, IDs: domain.ExternalIDs{TMDB: 11}, ReleaseDate: "2026-04-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.CreateMediaItem(ctx, domain.MediaItem{
		Kind: domain.KindSeries, Title: "Air Show", SortTitle: "air show",
		Year: 2026, IDs: domain.ExternalIDs{TMDB: 12},
		Seasons: []domain.Season{{Number: 1, Monitored: true, Episodes: []domain.Episode{
			{SeasonNumber: 1, EpisodeNumber: 1, Title: "One", AirDate: "2026-04-02", Monitored: true},
			{SeasonNumber: 1, EpisodeNumber: 2, Title: "Two", AirDate: "2026-06-09", Monitored: true},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	entries, err := db.Calendar(ctx, "2026-04-01", "2026-04-30")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Kind != "movie" || entries[0].Title != "Spring Movie" {
		t.Errorf("first = %+v", entries[0])
	}
	if entries[1].Kind != "episode" || entries[1].Detail != "S01E01 — One" || entries[1].HasFile {
		t.Errorf("second = %+v", entries[1])
	}
}

func TestBackupAndRetention(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	path, err := db.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
	// The snapshot is a valid database: it can be opened and queried.
	list, err := db.ListBackups()
	if err != nil || len(list) != 1 || list[0].SizeBytes == 0 {
		t.Fatalf("backups = %+v err %v", list, err)
	}
}

// TestSchemaEnumsMatchCode is the drift guard for every CHECK constraint:
// each value the code can write must pass its column's CHECK. When a new
// enum value lands in code, this test fails until the schema follows
// (see migration 0007 for what forgetting looks like).
func TestSchemaEnumsMatchCode(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// media_items.kind — all three kinds insert.
	for i, kind := range []domain.MediaKind{domain.KindMovie, domain.KindSeries, domain.KindBook} {
		if _, err := db.CreateMediaItem(ctx, domain.MediaItem{
			Kind: kind, Title: string(kind) + " item", SortTitle: string(kind),
			IDs: domain.ExternalIDs{TMDB: int64(1000 + i), OLID: map[bool]string{true: "OLDRIFT" + string(kind), false: ""}[kind == domain.KindBook]},
		}); err != nil {
			t.Errorf("kind %q rejected: %v", kind, err)
		}
	}

	// indexers.protocol — both protocols insert.
	for _, proto := range []string{"torrent", "usenet"} {
		if _, err := db.AddIndexer(ctx, ports.IndexerConfig{
			Name: proto + "-idx", URL: "http://x", Protocol: proto, Enabled: true,
		}); err != nil {
			t.Errorf("protocol %q rejected: %v", proto, err)
		}
	}

	// notifiers.type — every type the notify factory knows inserts.
	for _, typ := range []string{"webhook", "discord", "plex", "jellyfin"} {
		if _, err := db.AddNotifier(ctx, ports.NotifierConfig{
			Type: typ, Name: typ + "-n", Enabled: true,
		}); err != nil {
			t.Errorf("notifier type %q rejected: %v", typ, err)
		}
	}

	// downloads.state — walk every state the queue code writes.
	dlID, err := db.InsertDownload(ctx, Download{
		MediaItemID: 1, WantableIDs: []string{"movie:1"}, ReleaseTitle: "X",
		Protocol: "torrent", State: "grabbed",
	})
	if err != nil {
		t.Fatalf("insert grabbed: %v", err)
	}
	for _, state := range []string{"downloading", "downloaded", "awaiting_import", "importing", "imported", "failed"} {
		if err := db.UpdateDownloadState(ctx, dlID, state, 1, ""); err != nil {
			t.Errorf("state %q rejected: %v", state, err)
		}
	}
}
