package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// The screenshot that prompted this: the settings button said "Review 4
// folders…" and the window it opened listed one. The button counted the scan
// report; the window listed the persisted queue; a scan after the last
// adoption run put three folders in the first and none in the second.
func TestReviewQueueIncludesFoldersFoundSinceTheLastAdoption(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{}
	ctx := context.Background()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Cunk on Britain"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindSeries)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	// An adoption run writes the queue: one folder, nothing matched.
	if _, err := svc.RunAdoption(ctx); err != nil {
		t.Fatal(err)
	}
	if got := svc.ReviewQueue(ctx, "", "", 25, 0).Counts.Total; got != 1 {
		t.Fatalf("after the first pass the queue should hold 1, got %d", got)
	}

	// Three more folders appear on disk and a scan finds them. No adoption
	// run since, so the persisted queue still says one.
	for _, name := range []string{"Hijack", "My 600-lb Life", "Fall, The (2006)"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	report, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.UnmatchedTotal != 4 {
		t.Fatalf("the scan should find 4 unmatched folders, got %d", report.UnmatchedTotal)
	}

	page := svc.ReviewQueue(ctx, "", "", 25, 0)
	if page.Counts.Total != 4 {
		t.Errorf("the review queue must account for all 4, got %d", page.Counts.Total)
	}
	names := map[string]bool{}
	for _, p := range page.Items {
		names[p.Name] = true
	}
	for _, want := range []string{"Cunk on Britain", "Hijack", "My 600-lb Life", "Fall, The (2006)"} {
		if !names[want] {
			t.Errorf("%q is on disk and unmatched but absent from the review window", want)
		}
	}
	// The stored proposal keeps its parse rather than being replaced by a
	// bare entry synthesised from the folder name.
	for _, p := range page.Items {
		if p.Name == "Cunk on Britain" && p.Kind != domain.KindSeries {
			t.Errorf("the stored proposal should survive the merge, got kind %q", p.Kind)
		}
	}
}

// A dismissal has to leave the count and the window agreeing. Pruning the
// list without recomputing the total kept a dismissed folder in the button's
// number while it was absent from the window that number described.
func TestDismissingAFolderDropsItFromBothTheCountAndTheQueue(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{}
	ctx := context.Background()

	root := t.TempDir()
	for _, name := range []string{"Keeper", "Not Media At All"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	if err := svc.IgnoreDir(ctx, filepath.Join(root, "Not Media At All"), "not media"); err != nil {
		t.Fatal(err)
	}

	report, ok, err := svc.LastScanReport(ctx)
	if err != nil || !ok {
		t.Fatalf("report: %v ok=%v", err, ok)
	}
	if report.UnmatchedTotal != 1 {
		t.Errorf("the stored total must drop with the entry, got %d", report.UnmatchedTotal)
	}
	if got := svc.ReviewQueue(ctx, "", "", 25, 0).Counts.Total; got != 1 {
		t.Errorf("the queue should hold 1 after a dismissal, got %d", got)
	}
	if report.IgnoredDirs != 1 {
		t.Errorf("the dismissal should be counted as such, got %d", report.IgnoredDirs)
	}
}

// Adopting from the window has to drop the folder from the count as well, or
// the button advertises work that is finished.
func TestAdoptingAFolderDropsItFromTheCount(t *testing.T) {
	svc, _, _ := newService(t)
	prov := adoptProvider{movies: []ports.SearchResult{movie(1, "Arrival", 2016)}}
	svc.meta = prov
	ctx := context.Background()

	root := t.TempDir()
	for _, name := range []string{"Arrival (2016)", "Something Else"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	if err := svc.AdoptOne(ctx, filepath.Join(root, "Arrival (2016)"),
		movie(1, "Arrival", 2016), false); err != nil {
		t.Fatal(err)
	}

	if got := svc.ReviewQueue(ctx, "", "", 25, 0).Counts.Total; got != 1 {
		t.Errorf("one of two folders was adopted, so 1 remains; got %d", got)
	}
	report, _, err := svc.LastScanReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.UnmatchedTotal != 1 {
		t.Errorf("the report's total must agree, got %d", report.UnmatchedTotal)
	}
}
