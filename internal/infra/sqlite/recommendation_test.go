package sqlite

import (
	"context"
	"github.com/pjunod/monarr/internal/domain"
	"testing"
)

func TestRecommendationOwnershipAlignedAndConflicting(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	first := sampleSeries()
	first.IDs = domain.ExternalIDs{TMDB: 101, TVDB: 201, IMDB: "tt301"}
	id, err := db.CreateMediaItem(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	second := sampleSeries()
	second.Title = "Other"
	second.IDs = domain.ExternalIDs{TMDB: 102, TVDB: 202, IMDB: "tt302"}
	if _, err = db.CreateMediaItem(ctx, second); err != nil {
		t.Fatal(err)
	}
	ids := []domain.ExternalIDs{{TMDB: 999}, {TVDB: 201}, {TMDB: 101, TVDB: 202}, {TMDB: 101, TVDB: 999}, {TMDB: 101, TVDB: 201, IMDB: "tt301"}}
	got, err := db.LookupSeries(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"absent", "present", "ambiguous", "ambiguous", "present"}
	for i, state := range want {
		if got[i].State != state {
			t.Fatalf("row %d: %+v", i, got[i])
		}
	}
	if got[1].LibraryItemID != id || got[4].LibraryItemID != id {
		t.Fatal("aligned ownership IDs lost")
	}
}
