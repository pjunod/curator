package sqlite

import (
	"context"
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
