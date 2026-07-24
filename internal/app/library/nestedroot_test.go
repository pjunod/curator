package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
)

// Registering a base like /media and then /media/Movies used to be allowed,
// which offered every movie folder twice and is what makes a scan look like
// it is trawling the whole tree (ADR 0009 §3).
func TestAddRootFolderRejectsNesting(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	base := t.TempDir()
	movies := filepath.Join(base, "Movies")
	if err := os.Mkdir(movies, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("child inside registered parent", func(t *testing.T) {
		if _, err := svc.AddRootFolder(ctx, base, domain.KindMixed); err != nil {
			t.Fatal(err)
		}
		_, err := svc.AddRootFolder(ctx, movies, domain.RootKindOf(domain.KindMovie))
		if !errors.Is(err, ErrNestedRoot) {
			t.Fatalf("want ErrNestedRoot, got %v", err)
		}
	})

	t.Run("parent containing registered child", func(t *testing.T) {
		svc2, _, _ := newService(t)
		if _, err := svc2.AddRootFolder(ctx, movies, domain.RootKindOf(domain.KindMovie)); err != nil {
			t.Fatal(err)
		}
		_, err := svc2.AddRootFolder(ctx, base, domain.KindMixed)
		if !errors.Is(err, ErrNestedRoot) {
			t.Fatalf("want ErrNestedRoot, got %v", err)
		}
	})
}

// Siblings must stay legal, including ones whose names share a prefix —
// a naive strings.HasPrefix check calls /media/tv2 a child of /media/tv.
func TestAddRootFolderAllowsSiblingsAndPrefixNeighbours(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	base := t.TempDir()
	for _, name := range []string{"tv", "tv2", "movies"} {
		dir := filepath.Join(base, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AddRootFolder(ctx, dir, domain.KindMixed); err != nil {
			t.Fatalf("%s should be registrable alongside its siblings: %v", name, err)
		}
	}
}

// Trailing slashes and dot segments must not defeat the check.
func TestAddRootFolderNestingSurvivesUncleanPaths(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	base := t.TempDir()
	tv := filepath.Join(base, "TV")
	if err := os.Mkdir(tv, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddRootFolder(ctx, tv, domain.RootKindOf(domain.KindSeries)); err != nil {
		t.Fatal(err)
	}
	_, err := svc.AddRootFolder(ctx, filepath.Join(base, "TV", "..", "TV")+"/", domain.KindMixed)
	if !errors.Is(err, ErrNestedRoot) && err == nil {
		t.Fatal("an unclean path to an already-registered root must not register twice")
	}
}
