package acquisition

import (
	"context"
	"testing"
	"time"
)

func acquiredWithin(t *testing.T, d time.Duration, lock func() func()) (func(), bool) {
	t.Helper()
	got := make(chan func(), 1)
	go func() { got <- lock() }()
	select {
	case release := <-got:
		return release, true
	case <-time.After(d):
		return func() { (<-got)() }, false
	}
}

func TestImportTargetLocksSerializeOnlyTheSameItem(t *testing.T) {
	s := &Service{}
	ctx := context.Background()
	_, releaseA := s.lockImportTarget(ctx, 1)

	releaseB, ok := acquiredWithin(t, time.Second, func() func() { _, r := s.lockImportTarget(ctx, 2); return r })
	if !ok {
		t.Fatal("a different title's import waited behind title 1")
	}
	releaseB()

	releaseA2, ok := acquiredWithin(t, 50*time.Millisecond, func() func() { _, r := s.lockImportTarget(ctx, 1); return r })
	if ok {
		t.Fatal("two holders of the same title's import target at once")
	}
	releaseAll, ok := acquiredWithin(t, 50*time.Millisecond, func() func() { _, r := s.lockAllImportTargets(ctx); return r })
	if ok {
		t.Fatal("the global authority was granted while a title was held")
	}
	releaseA()
	releaseA2()
	releaseAll()

	_, releaseAll = s.lockAllImportTargets(ctx)
	releaseC, ok := acquiredWithin(t, 50*time.Millisecond, func() func() { _, r := s.lockImportTarget(ctx, 3); return r })
	if ok {
		t.Fatal("a title lock was granted under the global authority")
	}
	releaseAll()
	releaseC()
	if len(s.importTargets.items) != 0 {
		t.Fatalf("released title locks were retained: %d", len(s.importTargets.items))
	}
}

// A holder passes its scope down; nested work for the same title, or any
// work under the global authority, must not lock again (sync locks are not
// reentrant, so that would deadlock).
func TestHeldImportTargetCoversNestedWork(t *testing.T) {
	s := &Service{}
	ctx, release := s.lockImportTarget(context.Background(), 7)
	inner, ok := acquiredWithin(t, time.Second, func() func() { _, r := s.lockImportTarget(ctx, 7); return r })
	if !ok {
		t.Fatal("nested work on the held title deadlocked")
	}
	inner()
	release()

	ctx, release = s.lockAllImportTargets(context.Background())
	for _, item := range []int64{0, 7, 8} {
		inner, ok := acquiredWithin(t, time.Second, func() func() { _, r := s.lockImportTarget(ctx, item); return r })
		if !ok {
			t.Fatalf("nested work on title %d deadlocked under the global authority", item)
		}
		inner()
	}
	release()
}

// The field failure: one title's long placement held every other title's
// import. A delivery import must complete while a different title holds its
// target, and wait while its own title is held.
func TestDeliveryImportIsNotHeldByAnotherTitlesPlacement(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)

	_, releaseOther := svc.lockImportTarget(ctx, dl.MediaItemID+1000)
	done := make(chan error, 1)
	go func() { done <- svc.runDeliveryImport(ctx, dl, cfg, receiptID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("import beside another title's placement: %v", err)
		}
	case <-time.After(10 * time.Second):
		releaseOther()
		t.Fatal("import waited behind a different title's placement")
	}
	releaseOther()
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.State != "imported" {
		t.Fatalf("receipt = %+v err=%v, want imported", r, err)
	}
}
