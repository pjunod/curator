package library

import (
	"context"
	"errors"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

// A typed root refuses an item of another kind, which is the whole point of
// ADR 0009 — the *arrs got this for free by being separate applications.
func TestAddRejectsItemOfWrongKindForRoot(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	tv, err := svc.AddRootFolder(ctx, t.TempDir(), domain.RootKindOf(domain.KindSeries))
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: tv.ID, Monitored: true,
	})
	if !errors.Is(err, ErrRootKindMismatch) {
		t.Fatalf("want ErrRootKindMismatch putting a movie in a series root, got %v", err)
	}
}

func TestAddAcceptsMatchingAndMixedRoots(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		kind domain.RootKind
	}{
		{"matching kind", domain.RootKindOf(domain.KindMovie)},
		{"mixed root", domain.KindMixed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rf, err := svc.AddRootFolder(ctx, t.TempDir(), tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.Add(ctx, AddRequest{
				Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
			}); err != nil {
				t.Fatalf("movie into %s root: %v", tc.kind, err)
			}
			// Each subtest adds the same TMDB id, so clear it between runs.
			items, _ := svc.List(ctx, domain.KindMovie)
			for _, it := range items {
				_ = svc.Delete(ctx, it.ID)
			}
		})
	}
}

// An empty kind means mixed, so old callers and old rows behave identically.
func TestAddRootFolderDefaultsToMixed(t *testing.T) {
	svc, _, _ := newService(t)
	rf, err := svc.AddRootFolder(context.Background(), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if rf.Kind != domain.KindMixed {
		t.Fatalf("kind = %q, want mixed", rf.Kind)
	}
}

func TestAddRootFolderRejectsUnknownKind(t *testing.T) {
	svc, _, _ := newService(t)
	if _, err := svc.AddRootFolder(context.Background(), t.TempDir(), "audiobook"); err == nil {
		t.Fatal("want error for an unknown root kind")
	}
}

// Retyping routes future decisions only; items already placed keep theirs.
func TestSetRootFolderKindLeavesExistingItemsAlone(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	rf, err := svc.AddRootFolder(ctx, t.TempDir(), domain.KindMixed)
	if err != nil {
		t.Fatal(err)
	}
	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetRootFolderKind(ctx, rf.ID, domain.RootKindOf(domain.KindSeries)); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Get(ctx, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != domain.KindMovie || got.RootFolderID != rf.ID {
		t.Fatalf("retyping the root changed the item: kind=%s root=%d", got.Kind, got.RootFolderID)
	}
}

func TestRootKindAccepts(t *testing.T) {
	if !domain.KindMixed.Accepts(domain.KindBook) {
		t.Error("mixed must accept every kind")
	}
	if domain.RootKindOf(domain.KindMovie).Accepts(domain.KindSeries) {
		t.Error("a movie root must not accept a series")
	}
	if !domain.RootKindOf(domain.KindMovie).Accepts(domain.KindMovie) {
		t.Error("a movie root must accept a movie")
	}
}
