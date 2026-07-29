package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

// fakeQueue records what would be enqueued.
type fakeQueue struct {
	enqueued []domain.Job
	dupes    map[string]bool
	err      error
}

func (q *fakeQueue) EnqueueUnique(_ context.Context, j domain.Job) (bool, error) {
	if q.err != nil {
		return false, q.err
	}
	if q.dupes[j.DedupeKey] {
		return false, nil
	}
	q.enqueued = append(q.enqueued, j)
	return true, nil
}

func (q *fakeQueue) kinds() []string {
	out := make([]string, 0, len(q.enqueued))
	for _, j := range q.enqueued {
		out = append(out, j.Kind)
	}
	return out
}

// The link between scanning and matching is a promise, not an
// implementation detail: a scan that leaves a list of names is the thing
// ADR 0010 exists to fix. This test exists because an earlier version of
// the chain was lost in an unrelated edit and nothing caught it.
func TestScanChainsIntoAdoption(t *testing.T) {
	svc, _, _ := newService(t)
	q := &fakeQueue{}
	svc.WithQueue(q)
	ctx := context.Background()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Arrival (2016)"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}

	if err := svc.ScanThenAdopt(ctx); err != nil {
		t.Fatal(err)
	}
	if got := q.kinds(); len(got) != 1 || got[0] != JobAdopt {
		t.Fatalf("a scan should queue exactly one adoption pass, got %v", got)
	}
}

// The handler registered for the scan kind must be the chaining one — a
// registration that quietly wires up a bare Scan is the regression this
// guards.
func TestRegisteredScanHandlerChains(t *testing.T) {
	svc, _, _ := newService(t)
	q := &fakeQueue{}
	svc.WithQueue(q)
	ctx := context.Background()

	root := t.TempDir()
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}

	handlers := map[string]func(context.Context, domain.Job) error{}
	if err := RegisterJobHandlers(svc, func(kind string, h func(context.Context, domain.Job) error) error {
		handlers[kind] = h
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{JobScan, JobAdopt} {
		if handlers[kind] == nil {
			t.Fatalf("no handler registered for %q", kind)
		}
	}
	if err := handlers[JobScan](ctx, domain.Job{Kind: JobScan}); err != nil {
		t.Fatal(err)
	}
	if got := q.kinds(); len(got) != 1 || got[0] != JobAdopt {
		t.Fatalf("the registered scan handler must queue adoption, got %v", got)
	}
}

// Coalescing: asking twice while one is pending is a no-op, not a second
// pass over the whole library.
func TestEnqueueCoalesces(t *testing.T) {
	svc, _, _ := newService(t)
	q := &fakeQueue{dupes: map[string]bool{JobAdopt: true, JobScan: true}}
	svc.WithQueue(q)
	ctx := context.Background()

	if err := svc.EnqueueAdoption(ctx); err != nil {
		t.Fatalf("a coalesced request is not an error: %v", err)
	}
	if err := svc.EnqueueScan(ctx); err != nil {
		t.Fatalf("a coalesced request is not an error: %v", err)
	}
	if len(q.enqueued) != 0 {
		t.Fatalf("nothing should be enqueued when one is already pending, got %v", q.kinds())
	}
}

// Without a queue the work still happens inline, which is what keeps the
// queue an addition rather than a hard dependency.
func TestEnqueueFallsBackToInlineWithoutAQueue(t *testing.T) {
	svc, _, _ := newService(t)
	ctx := context.Background()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Dune (2021)"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}
	if err := svc.EnqueueScan(ctx); err != nil {
		t.Fatal(err)
	}
	// The scan really ran, so a report exists.
	if _, ok, err := svc.LastScanReport(ctx); err != nil || !ok {
		t.Fatalf("scan should have run inline: ok=%v err=%v", ok, err)
	}
}

// An enqueue failure is surfaced rather than swallowed: silently not
// scanning is worse than a visible error.
func TestEnqueueSurfacesQueueErrors(t *testing.T) {
	svc, _, _ := newService(t)
	svc.WithQueue(&fakeQueue{err: errors.New("queue down")})
	if err := svc.EnqueueAdoption(context.Background()); err == nil {
		t.Fatal("want the queue error surfaced")
	}
}
