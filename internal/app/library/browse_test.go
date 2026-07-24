package library

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
)

func browseFixture(t *testing.T) (string, *Service) {
	t.Helper()
	svc, _, _ := newService(t)
	base := t.TempDir()
	for _, name := range []string{"Movies", "TV", "Books", ".hidden"} {
		if err := os.Mkdir(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return base, svc
}

func TestBrowseListsChildDirectoriesOnly(t *testing.T) {
	base, svc := browseFixture(t)

	res, err := svc.Browse(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range res.Dirs {
		names = append(names, d.Name)
	}
	want := []string{"Books", "Movies", "TV"} // sorted, no dotfiles, no files
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("dirs = %v, want %v", names, want)
	}
	if res.Parent == "" {
		t.Error("parent should be set below the filesystem root")
	}
}

// The typeahead contract: a partial final component filters the parent's
// children, and a trailing separator lists everything.
func TestSuggestFiltersByPrefix(t *testing.T) {
	base, svc := browseFixture(t)
	ctx := context.Background()

	res, err := svc.Suggest(ctx, filepath.Join(base, "M"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Dirs) != 1 || res.Dirs[0].Name != "Movies" {
		t.Fatalf("typing M should suggest Movies, got %+v", res.Dirs)
	}

	// Case-insensitive, because nobody types Capitals mid-path.
	if res, err := svc.Suggest(ctx, filepath.Join(base, "tv")); err != nil {
		t.Fatal(err)
	} else if len(res.Dirs) != 1 || res.Dirs[0].Name != "TV" {
		t.Fatalf("typing tv should suggest TV, got %+v", res.Dirs)
	}

	if res, err := svc.Suggest(ctx, base+string(filepath.Separator)); err != nil {
		t.Fatal(err)
	} else if len(res.Dirs) != 3 {
		t.Fatalf("a trailing separator lists everything, got %+v", res.Dirs)
	}
}

// Already-registered roots are flagged so the picker shows them as taken
// rather than offering a path that will be rejected as a duplicate.
func TestBrowseFlagsRegisteredRoots(t *testing.T) {
	base, svc := browseFixture(t)
	ctx := context.Background()

	movies := filepath.Join(base, "Movies")
	if _, err := svc.AddRootFolder(ctx, movies, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Browse(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range res.Dirs {
		if d.Name == "Movies" && !d.Registered {
			t.Error("Movies is a registered root and should be flagged")
		}
		if d.Name == "TV" && d.Registered {
			t.Error("TV is not registered and must not be flagged")
		}
	}
}

// Missing and forbidden must be indistinguishable: the difference is the
// useful part to someone probing the host and noise to someone picking a
// folder.
func TestBrowseDoesNotDistinguishMissingFromForbidden(t *testing.T) {
	_, svc := browseFixture(t)
	ctx := context.Background()

	_, missingErr := svc.Browse(ctx, "/definitely/not/here-xyz")
	if missingErr == nil {
		t.Fatal("want an error for a missing path")
	}

	// A file is not a directory — same shape of answer, no detail leaked.
	base, _ := browseFixture(t)
	file := filepath.Join(base, "notes.txt")
	_, fileErr := svc.Browse(ctx, file)
	if fileErr == nil {
		t.Fatal("want an error for a non-directory")
	}
	if !strings.Contains(missingErr.Error(), "not readable") ||
		!strings.Contains(fileErr.Error(), "not readable") {
		t.Errorf("errors should be uniform: %v / %v", missingErr, fileErr)
	}
}

func TestBrowseRejectsRelativePaths(t *testing.T) {
	_, svc := browseFixture(t)
	if _, err := svc.Browse(context.Background(), "relative/path"); err == nil {
		t.Fatal("want an error for a relative path")
	}
}

// A symlink is resolved before listing, so it cannot present one path while
// enumerating another.
func TestBrowseResolvesSymlinks(t *testing.T) {
	base, svc := browseFixture(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(base, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	res, err := svc.Browse(context.Background(), link)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path == link {
		t.Error("the reported path should be the resolved target, not the link")
	}
	if len(res.Dirs) != 3 {
		t.Fatalf("listing through a symlink should show the target's dirs, got %+v", res.Dirs)
	}
}
