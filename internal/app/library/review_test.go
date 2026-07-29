package library

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

// A folder of several hundred movies is the case that motivated paging, so
// that is the case the test uses.
func seedManyCandidates(t *testing.T, n int) (*Service, string) {
	t.Helper()
	svc, _, _ := newService(t)
	ctx := context.Background()
	root := t.TempDir()
	for i := 0; i < n; i++ {
		if err := os.Mkdir(filepath.Join(root, fmt.Sprintf("Film %03d (20%02d)", i, i%20)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	return svc, root
}

func TestReviewQueuePaginates(t *testing.T) {
	svc, _ := seedManyCandidates(t, 300)
	ctx := context.Background()

	first := svc.ReviewQueue(ctx, "", "", 25, 0)
	if len(first.Items) != 25 {
		t.Fatalf("page size = %d, want 25", len(first.Items))
	}
	if first.Total != 300 || first.Counts.Total != 300 {
		t.Fatalf("total = %d / counts.total = %d, want 300", first.Total, first.Counts.Total)
	}

	second := svc.ReviewQueue(ctx, "", "", 25, 25)
	if len(second.Items) != 25 {
		t.Fatalf("second page size = %d, want 25", len(second.Items))
	}
	if second.Items[0].Path == first.Items[0].Path {
		t.Error("page 2 must not repeat page 1")
	}

	// The last page is short, and an offset past the end is empty rather
	// than an error — a stale page number after a dismissal is normal.
	last := svc.ReviewQueue(ctx, "", "", 25, 275)
	if len(last.Items) != 25 {
		t.Fatalf("last page = %d, want 25", len(last.Items))
	}
	past := svc.ReviewQueue(ctx, "", "", 25, 1000)
	if len(past.Items) != 0 {
		t.Fatalf("offset past the end should be empty, got %d", len(past.Items))
	}
	if past.Total != 300 {
		t.Errorf("total should still be reported past the end, got %d", past.Total)
	}
}

func TestReviewQueueFilters(t *testing.T) {
	svc, _ := seedManyCandidates(t, 120)
	ctx := context.Background()

	page := svc.ReviewQueue(ctx, "", "Film 007", 25, 0)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("filter matched %d, want 1", page.Total)
	}
	// Counts stay over the whole queue so tab labels do not flicker while
	// the user narrows the list.
	if page.Counts.Total != 120 {
		t.Errorf("counts.total = %d, want the unfiltered 120", page.Counts.Total)
	}
	if lower := svc.ReviewQueue(ctx, "", "film 007", 25, 0); lower.Total != 1 {
		t.Error("filtering should be case-insensitive")
	}
}

// Limit semantics, which are easy to get subtly wrong: a positive limit is
// clamped to a sane page, 0 means "all" and is honoured, and an unspecified
// limit falls back to the default page size.
func TestReviewQueueLimitSemantics(t *testing.T) {
	svc, _ := seedManyCandidates(t, 300)
	ctx := context.Background()

	if got := len(svc.ReviewQueue(ctx, "", "", 100000, 0).Items); got != maxReviewLimit {
		t.Fatalf("an absurd limit should clamp to %d, got %d", maxReviewLimit, got)
	}

	// 0 = All. Honoured rather than clamped: refusing it would just push
	// people into clicking Next twelve times.
	all := svc.ReviewQueue(ctx, "", "", 0, 0)
	if len(all.Items) != 300 {
		t.Fatalf("limit 0 should return everything, got %d", len(all.Items))
	}
	if all.Limit != 0 {
		t.Errorf("limit 0 should be reported back as 0, got %d", all.Limit)
	}

	// Unspecified (negative) is the absent marker, and gets the default.
	if got := svc.ReviewQueue(ctx, "", "", -1, 0).Limit; got != defaultReviewLimit {
		t.Errorf("an unspecified limit should default to %d, got %d", defaultReviewLimit, got)
	}
	if got := svc.ReviewQueue(ctx, "", "", 25, -5).Offset; got != 0 {
		t.Errorf("negative offset should clamp to 0, got %d", got)
	}
}

// Kind tabs are a server-side filter, and the counts behind the tab labels
// stay over the whole queue so they do not move while the list narrows.
func TestReviewQueueFiltersByKind(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{}
	ctx := context.Background()

	movieRoot := filepath.Join(t.TempDir(), "movies")
	tvRoot := filepath.Join(t.TempDir(), "tv")
	mixedRoot := filepath.Join(t.TempDir(), "mixed")
	for _, d := range []string{movieRoot, tvRoot, mixedRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for dir, names := range map[string][]string{
		movieRoot: {"Arrival (2016)", "Dune (2021)"},
		tvRoot:    {"Severance"},
		mixedRoot: {"Anime Thing"},
	} {
		for _, n := range names {
			if err := os.Mkdir(filepath.Join(dir, n), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	for dir, kind := range map[string]domain.RootKind{
		movieRoot: domain.RootKindOf(domain.KindMovie),
		tvRoot:    domain.RootKindOf(domain.KindSeries),
		mixedRoot: domain.KindMixed,
	} {
		if _, err := svc.AddRootFolder(ctx, dir, kind); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunAdoption(ctx); err != nil {
		t.Fatal(err)
	}

	movies := svc.ReviewQueue(ctx, domain.KindMovie, "", 25, 0)
	if movies.Total != 2 {
		t.Errorf("movie tab total = %d, want 2", movies.Total)
	}
	tv := svc.ReviewQueue(ctx, domain.KindSeries, "", 25, 0)
	if tv.Total != 1 {
		t.Errorf("tv tab total = %d, want 1", tv.Total)
	}
	books := svc.ReviewQueue(ctx, domain.KindBook, "", 25, 0)
	if books.Total != 0 {
		t.Errorf("books tab total = %d, want 0", books.Total)
	}

	// Counts are unfiltered, so tab labels hold still.
	c := movies.Counts
	if c.Total != 4 || c.Movie != 2 || c.Series != 1 || c.Unknown != 1 {
		t.Errorf("counts = %+v, want total 4 / movie 2 / series 1 / unknown 1 (the mixed root)", c)
	}

	// Everything, including the mixed-root entry whose kind never resolved.
	if got := svc.ReviewQueue(ctx, "", "", 25, 0).Total; got != 4 {
		t.Errorf("no kind filter should return all 4, got %d", got)
	}
}

// Dismissals disappear from the queue immediately, without rewriting the
// stored blob.
func TestReviewQueueHidesDismissals(t *testing.T) {
	svc, root := seedManyCandidates(t, 10)
	ctx := context.Background()

	before := svc.ReviewQueue(ctx, "", "", 25, 0)
	if before.Total != 10 {
		t.Fatalf("total = %d, want 10", before.Total)
	}
	if err := svc.IgnoreDir(ctx, filepath.Join(root, "Film 003 (2003)"), ""); err != nil {
		t.Fatal(err)
	}
	after := svc.ReviewQueue(ctx, "", "", 25, 0)
	if after.Total != 9 {
		t.Fatalf("after dismissal total = %d, want 9", after.Total)
	}
	for _, p := range after.Items {
		if p.Name == "Film 003 (2003)" {
			t.Fatal("a dismissed folder must not appear in the review queue")
		}
	}
}

// Without an adoption run the queue still lists what the scan found, or a
// user who scans and does not press match concludes the scan found nothing.
func TestReviewQueueFallsBackToTheScanReport(t *testing.T) {
	svc, _ := seedManyCandidates(t, 5)
	page := svc.ReviewQueue(context.Background(), "", "", 25, 0)
	if page.Total != 5 {
		t.Fatalf("total = %d, want 5 from the scan report fallback", page.Total)
	}
	for _, p := range page.Items {
		if p.Confidence != ConfidenceNone {
			t.Errorf("fallback entries should be unjudged, got %s", p.Confidence)
		}
		// The fallback still parses, so the row can say what it read
		// rather than echoing the folder name back.
		if p.ParsedYear == 0 || p.ParsedTitle == p.Name {
			t.Errorf("fallback entry %q should carry a parsed title and year, got %q/%d",
				p.Name, p.ParsedTitle, p.ParsedYear)
		}
	}
}

// The queue survives a restart: a review list you have to regenerate with
// hundreds of provider searches is a report, not a queue.
func TestReviewQueuePersistsWhatAdoptionCouldNotResolve(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{
		movie(1, "Dune", 2021), movie(2, "Dune", 1984),
	}}
	ctx := context.Background()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Dune"), 0o755); err != nil {
		t.Fatal(err)
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ConfirmRootAdopted(ctx, rf.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunAdoption(ctx); err != nil {
		t.Fatal(err)
	}

	// Bare "Dune" matches two films, so it lands in the queue carrying both
	// candidates — the whole point of proposing rather than listing.
	page := svc.ReviewQueue(ctx, "", "", 25, 0)
	if page.Total != 1 {
		t.Fatalf("total = %d, want 1", page.Total)
	}
	got := page.Items[0]
	if got.Confidence != ConfidenceAmbiguous {
		t.Errorf("confidence = %s, want ambiguous", got.Confidence)
	}
	if len(got.Candidates) != 2 {
		t.Fatalf("candidates = %d, want both Dunes offered", len(got.Candidates))
	}
	if page.Counts.Ambiguous != 1 {
		t.Errorf("counts.ambiguous = %d, want 1", page.Counts.Ambiguous)
	}
}
