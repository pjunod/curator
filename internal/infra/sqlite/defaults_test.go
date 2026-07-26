package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/quality"
)

// TestDefaultProfileFallsBackToTheBuiltIn: an install that has never touched
// the setting must behave exactly as it did when the default was a constant in
// library.Add. Anything else turns "we added a setting" into "your adds changed
// under you".
func TestDefaultProfileFallsBackToTheBuiltIn(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if got := db.DefaultProfileID(ctx, domain.KindMovie); got != 1 {
		t.Fatalf("movie default = %d, want 1", got)
	}
	if got := db.DefaultProfileID(ctx, domain.KindSeries); got != 1 {
		t.Fatalf("series default = %d, want 1", got)
	}
	if got := db.DefaultProfileID(ctx, domain.KindBook); got != quality.EbookProfileID {
		t.Fatalf("book default = %d, want %d", got, quality.EbookProfileID)
	}
}

// TestSetAndReadDefaultProfile: the round trip, which is the whole feature.
func TestSetAndReadDefaultProfile(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// 3 is the seeded 4K profile.
	if err := db.SetDefaultProfile(ctx, domain.KindMovie, 3); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got := db.DefaultProfileID(ctx, domain.KindMovie); got != 3 {
		t.Fatalf("movie default = %d, want 3", got)
	}
	// Per kind, so setting films must not move television.
	if got := db.DefaultProfileID(ctx, domain.KindSeries); got != 1 {
		t.Fatalf("series default = %d after setting movies, want 1", got)
	}
	// And the resolved map agrees with the individual reads.
	all := db.DefaultProfiles(ctx)
	if all[domain.KindMovie] != 3 || all[domain.KindSeries] != 1 ||
		all[domain.KindBook] != quality.EbookProfileID {
		t.Fatalf("DefaultProfiles = %v", all)
	}
}

// TestDefaultProfileRefusesTheWrongFamily: a book on a video profile is not a
// preference, it is an item that can never be satisfied — nothing an ebook
// indexer returns competes with a resolution target (ADR 0014 §7). Refusing at
// the moment of choosing is the only point where the person can still see why.
func TestDefaultProfileRefusesTheWrongFamily(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.SetDefaultProfile(ctx, domain.KindBook, 1); err == nil {
		t.Fatal("book default on the 1080p profile was accepted")
	} else if !errors.Is(err, ErrProfileFamilyMismatch) {
		t.Fatalf("error = %v, want ErrProfileFamilyMismatch", err)
	}
	if err := db.SetDefaultProfile(ctx, domain.KindMovie, quality.EbookProfileID); err == nil {
		t.Fatal("movie default on the Ebook profile was accepted")
	}
	// A refusal must not have written anything.
	if got := db.DefaultProfileID(ctx, domain.KindBook); got != quality.EbookProfileID {
		t.Fatalf("book default = %d after a refused write, want %d", got, quality.EbookProfileID)
	}
}

// TestDefaultProfileRefusesAProfileThatDoesNotExist keeps the setting honest
// at write time rather than discovering it at add time.
func TestDefaultProfileRefusesAProfileThatDoesNotExist(t *testing.T) {
	db := openTestDB(t)
	if err := db.SetDefaultProfile(context.Background(), domain.KindMovie, 9999); err == nil {
		t.Fatal("default on a nonexistent profile was accepted")
	}
}

// TestDanglingDefaultResolvesToTheBuiltIn: the setting names a profile that has
// since been deleted. Adding a movie must still work — a preference is never
// allowed to be the reason an add fails.
func TestDanglingDefaultResolvesToTheBuiltIn(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if err := db.SetMeta(ctx, DefaultProfileSetting(domain.KindMovie), "424242"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := db.DefaultProfileID(ctx, domain.KindMovie); got != 1 {
		t.Fatalf("dangling default resolved to %d, want the built-in 1", got)
	}
	// Garbage in the column is the same story.
	if err := db.SetMeta(ctx, DefaultProfileSetting(domain.KindMovie), "not a number"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := db.DefaultProfileID(ctx, domain.KindMovie); got != 1 {
		t.Fatalf("malformed default resolved to %d, want the built-in 1", got)
	}
}

// TestDeleteProfileRefusedWhileItIsADefault: the reference is real even though
// no row holds it. Without this, deleting the profile leaves the setting
// pointing at nothing and every future add silently lands on the built-in.
func TestDeleteProfileRefusedWhileItIsADefault(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.AddProfile(ctx, quality.Profile{
		Name:            "Spare",
		Target:          quality.Quality{Source: quality.SourceWEBDL, Resolution: 720},
		UpgradesAllowed: true,
	})
	if err != nil {
		t.Fatalf("add profile: %v", err)
	}
	if err := db.SetDefaultProfile(ctx, domain.KindSeries, id); err != nil {
		t.Fatalf("set default: %v", err)
	}

	err = db.DeleteProfile(ctx, id)
	if !errors.Is(err, ErrProfileIsDefault) {
		t.Fatalf("delete error = %v, want ErrProfileIsDefault", err)
	}

	// Point the kind elsewhere and the delete goes through.
	if err := db.SetDefaultProfile(ctx, domain.KindSeries, 1); err != nil {
		t.Fatalf("repoint: %v", err)
	}
	if err := db.DeleteProfile(ctx, id); err != nil {
		t.Fatalf("delete after repointing: %v", err)
	}
}

// TestBuiltInFallbackIsNotADeleteBlocker: profile 1 is where unset kinds land,
// but nobody chose it, so it is not a reference. Treating the fallback as one
// would invent a rule that no setting expresses.
func TestBuiltInFallbackIsNotADeleteBlocker(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	id, err := db.AddProfile(ctx, quality.Profile{
		Name:            "Unreferenced",
		Target:          quality.Quality{Source: quality.SourceWEBDL, Resolution: 720},
		UpgradesAllowed: true,
	})
	if err != nil {
		t.Fatalf("add profile: %v", err)
	}
	if err := db.DeleteProfile(ctx, id); err != nil {
		t.Fatalf("delete of an unreferenced profile: %v", err)
	}
}
