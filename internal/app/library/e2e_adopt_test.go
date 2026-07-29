package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// A whole-library rehearsal against real directories: the shape a user
// actually has, including the junk that used to be re-offered forever.
func TestEndToEndAdoptionOverARealisticLibrary(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{
		movies: []ports.SearchResult{
			movie(1, "Arrival", 2016),
			movie(2, "Fight Club", 1999),
			movie(3, "Dune", 2021),
			movie(4, "Dune", 1984),
		},
		series: []ports.SearchResult{series(10, "Severance", 2022)},
	}
	ctx := context.Background()

	movieRoot := filepath.Join(t.TempDir(), "movies")
	tvRoot := filepath.Join(t.TempDir(), "tv")
	for _, d := range []string{movieRoot, tvRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A realistic movie root: two clean titles, one scene-named, one
	// ambiguous (no year, two matching films), and NAS junk.
	for _, name := range []string{
		"Arrival (2016)", "Fight.Club.1999.1080p.BluRay.x264-GRP", "Dune",
		"@eaDir", "#recycle", ".DS_Store_dir", "Home Movies",
	} {
		if err := os.Mkdir(filepath.Join(movieRoot, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(tvRoot, "Severance"), 0o755); err != nil {
		t.Fatal(err)
	}

	mRoot, err := svc.AddRootFolder(ctx, movieRoot, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	tRoot, err := svc.AddRootFolder(ctx, tvRoot, domain.RootKindOf(domain.KindSeries))
	if err != nil {
		t.Fatal(err)
	}

	report, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Junk never reaches the candidate list.
	if report.SkippedDirs != 3 {
		t.Errorf("skipped = %d, want 3 (@eaDir, #recycle, dotdir)", report.SkippedDirs)
	}
	if len(report.UnmatchedDirs) != 5 {
		t.Fatalf("candidates = %d, want 5: %+v", len(report.UnmatchedDirs), report.UnmatchedDirs)
	}

	// Confirm both roots so this run applies rather than proposing.
	for _, id := range []int64{mRoot.ID, tRoot.ID} {
		if err := svc.ConfirmRootAdopted(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	res, err := svc.RunAdoption(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Arrival, Fight Club (scene-named), and Severance adopt themselves.
	// Bare "Dune" matches two films and must ask. "Home Movies" finds
	// nothing.
	if len(res.Adopted) != 3 {
		t.Fatalf("adopted = %d, want 3: %+v", len(res.Adopted), names(res.Adopted))
	}
	if len(res.Review) != 2 {
		t.Fatalf("review = %d, want 2 (Dune, Home Movies): %+v", len(res.Review), names(res.Review))
	}
	if len(res.Failures) != 0 {
		t.Fatalf("failures: %v", res.Failures)
	}

	// Every adopted item points at the folder that already exists.
	items, err := svc.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("library holds %d items, want 3", len(items))
	}
	for _, it := range items {
		if _, err := os.Stat(it.Path); err != nil {
			t.Errorf("%s points at %s which is not on disk: %v", it.Title, it.Path, err)
		}
	}

	// Dismissing the one that is not media keeps it dismissed.
	if err := svc.IgnoreDir(ctx, filepath.Join(movieRoot, "Home Movies"), "home video"); err != nil {
		t.Fatal(err)
	}
	after, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Only bare "Dune" is left to ask about.
	if len(after.UnmatchedDirs) != 1 || after.UnmatchedDirs[0].Name != "Dune" {
		t.Fatalf("after adoption and dismissal, want only Dune outstanding, got %+v", after.UnmatchedDirs)
	}
}

func names(ps []Proposal) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}
