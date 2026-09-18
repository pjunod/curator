package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func TestIdentitySnapshotLifecycleAndManualSurvival(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	item := sampleSeries()
	item.Source = "tvmaze"
	id, err := db.CreateMediaItem(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	initialRevision, _ := db.IdentityRevision(ctx)
	manual, err := db.AddManualAlias(ctx, id, "My Local Show", true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	aliases := []domain.TitleAlias{
		{Title: "Have I Got News for You US", SourceID: "78874", Scope: "work", Role: "alternate"},
		{Title: "Former Canonical", SourceID: "78874", Scope: "work", Role: "historical"},
	}
	countries := []domain.CountryEvidence{{Code: "US", Source: "tvmaze", Basis: "network"}}
	if err := db.ReplaceIdentitySnapshot(ctx, id, "tvmaze", aliases, countries, now); err != nil {
		t.Fatal(err)
	}
	// Empty success clears replaceable aliases but preserves manual/historical.
	if err := db.ReplaceIdentitySnapshot(ctx, id, "tvmaze", nil, countries, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, alias := range got.Aliases {
		seen[alias.Title] = true
	}
	if !seen[manual.Title] || !seen["Former Canonical"] || seen["Have I Got News for You US"] {
		t.Fatalf("aliases after refresh = %+v", got.Aliases)
	}
	if len(got.IdentitySources) != 1 || got.IdentitySources[0].FetchedAt != now.Add(time.Hour) {
		t.Fatalf("source = %+v", got.IdentitySources)
	}
	if err := db.RecordIdentityFailure(ctx, id, "tvmaze", now.Add(2*time.Hour), now.Add(3*time.Hour), "rate limited"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetMediaItemFull(ctx, id)
	if got.IdentitySources[0].LastError == "" || !seen[manual.Title] {
		t.Fatalf("failed refresh erased state: %+v", got)
	}
	if err := db.DeleteManualAlias(ctx, id, manual.ID); err != nil {
		t.Fatal(err)
	}
	finalRevision, _ := db.IdentityRevision(ctx)
	if finalRevision <= initialRevision {
		t.Fatalf("revision %d did not advance past %d", finalRevision, initialRevision)
	}
}

func TestUpdateMediaIdentityPreservesSourceAndRejectsCollisions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	first := sampleSeries()
	first.Source = "tvmaze"
	first.IDs.TMDB = 0
	id, err := db.CreateMediaItem(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMediaIdentity(ctx, id, domain.ExternalIDs{TMDB: 79063, IMDB: "tt16867040"}); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetMediaItemFull(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "tvmaze" || got.IDs.TMDB != 79063 || got.IDs.IMDB != "tt16867040" {
		t.Fatalf("enriched = %+v", got)
	}
	second := sampleSeries()
	second.Source = "tmdb"
	second.IDs.TMDB = 999
	second.IDs.TVDB = 999
	secondID, err := db.CreateMediaItem(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateMediaIdentity(ctx, secondID, domain.ExternalIDs{IMDB: "tt16867040"}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("collision err = %v", err)
	}
	if err := db.UpdateMediaIdentity(ctx, id, domain.ExternalIDs{TVDB: 1}); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("contradiction err = %v", err)
	}
}

func TestFindMediaItemsByExternalIDReturnsAllLegacyClaimants(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, title := range []string{"One", "Two"} {
		_, err := db.W.ExecContext(ctx, `INSERT INTO media_items(kind,title,sort_title,source,tvdb_id,added_at,updated_at) VALUES('series',?,?,'tvmaze',42,0,0)`, title, title)
		if err != nil {
			t.Fatal(err)
		}
	}
	items, err := db.FindMediaItemsByExternalID(ctx, domain.KindSeries, domain.ExternalRef{Provider: "tvdb", Value: "42"})
	if err != nil || len(items) != 2 {
		t.Fatalf("items=%d err=%v", len(items), err)
	}
}
