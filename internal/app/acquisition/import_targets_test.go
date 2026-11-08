package acquisition

import (
	"context"
	"errors"
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
	if _, _, err := s.lockAllImportTargets(ctx, 50*time.Millisecond); !errors.Is(err, ErrImportTargetsBusy) {
		t.Fatalf("whole-library authority while a title is held: err=%v, want busy", err)
	}
	releaseA()
	releaseA2()

	_, releaseAll, err := s.lockAllImportTargets(ctx, time.Second)
	if err != nil {
		t.Fatal(err)
	}
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

// Review finding: sync.RWMutex makes new readers wait behind a waiting
// writer, so one queued whole-library request would stop every title behind
// the slowest placement. The exclusive request must wait without queuing.
func TestWaitingWholeLibraryRequestDoesNotHoldUpOtherTitles(t *testing.T) {
	s := &Service{}
	ctx := context.Background()
	_, releaseSlow := s.lockImportTarget(ctx, 1) // a two-hour placement
	acquired := make(chan func(), 1)
	go func() {
		_, release, err := s.lockAllImportTargets(ctx, 0)
		if err != nil {
			t.Error(err)
			acquired <- func() {}
			return
		}
		acquired <- release
	}()
	time.Sleep(50 * time.Millisecond) // the exclusive request is now waiting
	releaseOther, ok := acquiredWithin(t, time.Second, func() func() { _, r := s.lockImportTarget(ctx, 2); return r })
	if !ok {
		t.Fatal("another title waited behind a pending whole-library request")
	}
	releaseOther()
	select {
	case <-acquired:
		t.Fatal("whole-library authority granted while a title was still held")
	default:
	}
	releaseSlow()
	select {
	case release := <-acquired:
		release()
	case <-time.After(3 * time.Second):
		t.Fatal("whole-library authority never acquired after the title released")
	}
}

// A holder passes its scope down; nested work for the same title, or any
// work under the global authority, must not lock again (sync locks are not
// reentrant, so that would deadlock), and asking for the whole library while
// holding one title is refused rather than deadlocking.
func TestHeldImportTargetCoversNestedWork(t *testing.T) {
	s := &Service{}
	ctx, release := s.lockImportTarget(context.Background(), 7)
	inner, ok := acquiredWithin(t, time.Second, func() func() { _, r := s.lockImportTarget(ctx, 7); return r })
	if !ok {
		t.Fatal("nested work on the held title deadlocked")
	}
	inner()
	if _, _, err := s.lockAllImportTargets(ctx, time.Second); err == nil || errors.Is(err, ErrImportTargetsBusy) {
		t.Fatalf("whole-library request under a held title: err=%v, want a refusal", err)
	}
	release()
	release() // idempotent

	ctx, releaseAll, err := s.lockAllImportTargets(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []int64{0, 7, 8} {
		inner, ok := acquiredWithin(t, time.Second, func() func() { _, r := s.lockImportTarget(ctx, item); return r })
		if !ok {
			t.Fatalf("nested work on title %d deadlocked under the global authority", item)
		}
		inner()
	}
	releaseAll()
}
