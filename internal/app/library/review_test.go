package library

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
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

	first := svc.ReviewQueue(ctx, "", 25, 0)
	if len(first.Items) != 25 {
		t.Fatalf("page size = %d, want 25", len(first.Items))
	}
	if first.Total != 300 || first.Counts.Total != 300 {
		t.Fatalf("total = %d / counts.total = %d, want 300", first.Total, first.Counts.Total)
	}

	second := svc.ReviewQueue(ctx, "", 25, 25)
	if len(second.Items) != 25 {
		t.Fatalf("second page size = %d, want 25", len(second.Items))
	}
	if second.Items[0].Path == first.Items[0].Path {
		t.Error("page 2 must not repeat page 1")
	}

	// The last page is short, and an offset past the end is empty rather
	// than an error — a stale page number after a dismissal is normal.
	last := svc.ReviewQueue(ctx, "", 25, 275)
	if len(last.Items) != 25 {
		t.Fatalf("last page = %d, want 25", len(last.Items))
	}
	past := svc.ReviewQueue(ctx, "", 25, 1000)
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

	page := svc.ReviewQueue(ctx, "Film 007", 25, 0)
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("filter matched %d, want 1", page.Total)
	}
	// Counts stay over the whole queue so tab labels do not flicker while
	// the user narrows the list.
	if page.Counts.Total != 120 {
		t.Errorf("counts.total = %d, want the unfiltered 120", page.Counts.Total)
	}
	if lower := svc.ReviewQueue(ctx, "film 007", 25, 0); lower.Total != 1 {
		t.Error("filtering should be case-insensitive")
	}
}

// Limits are clamped: a caller asking for everything does not get to
// undo the reason pagination exists.
func TestReviewQueueClampsLimit(t *testing.T) {
	svc, _ := seedManyCandidates(t, 300)
	ctx := context.Background()

	if got := len(svc.ReviewQueue(ctx, "", 100000, 0).Items); got != maxReviewLimit {
		t.Fatalf("limit = %d, want it clamped to %d", got, maxReviewLimit)
	}
	if got := svc.ReviewQueue(ctx, "", 0, 0).Limit; got != defaultReviewLimit {
		t.Errorf("limit 0 should default to %d, got %d", defaultReviewLimit, got)
	}
	if got := svc.ReviewQueue(ctx, "", 25, -5).Offset; got != 0 {
		t.Errorf("negative offset should clamp to 0, got %d", got)
	}
}

// Dismissals disappear from the queue immediately, without rewriting the
// stored blob.
func TestReviewQueueHidesDismissals(t *testing.T) {
	svc, root := seedManyCandidates(t, 10)
	ctx := context.Background()

	before := svc.ReviewQueue(ctx, "", 25, 0)
	if before.Total != 10 {
		t.Fatalf("total = %d, want 10", before.Total)
	}
	if err := svc.IgnoreDir(ctx, filepath.Join(root, "Film 003 (2003)"), ""); err != nil {
		t.Fatal(err)
	}
	after := svc.ReviewQueue(ctx, "", 25, 0)
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
	page := svc.ReviewQueue(context.Background(), "", 25, 0)
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
	page := svc.ReviewQueue(ctx, "", 25, 0)
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
