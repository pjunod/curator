package acquisition

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStorageChangeActionsShareWholeLibraryAuthority(t *testing.T) {
	for _, action := range []string{"preview", "accept"} {
		t.Run(action, func(t *testing.T) {
			svc := &Service{}
			_, releaseTitle := svc.lockImportTarget(context.Background(), 7)
			defer releaseTitle()
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			request := StorageChangeRequest{Root: t.TempDir(), ExpectedIdentity: "old", ObservedIdentity: "new", ReviewToken: "reviewed"}
			var err error
			if action == "preview" {
				_, err = svc.PreviewCompletedStorageChange(ctx, request)
			} else {
				err = svc.AcceptCompletedStorageChange(ctx, request)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("storage %s crossed a held publication title: %v", action, err)
			}
			releaseTitle()
			_, releaseAll, err := svc.lockAllImportTargets(context.Background(), time.Second)
			if err != nil {
				t.Fatalf("storage %s leaked publication authority: %v", action, err)
			}
			releaseAll()
		})
	}
}

// Admission may unlock before network IO; deferred cleanup must remain safe
// after that early release, and a panic must release both authority layers.
func TestStorageDecisionReleaseIsIdempotentAndPanicSafe(t *testing.T) {
	svc := &Service{}
	ctx, release := svc.lockStorageDecision(context.Background())
	_, releaseNested := svc.lockStorageDecision(ctx)
	releaseNested()
	release()
	release()
	// Reusing an expired context must acquire a fresh lease.
	ctx, release = svc.lockStorageDecision(ctx)
	releaseNested() // an old nested release cannot release this lease
	release()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("fixture did not panic")
			}
		}()
		targetCtx, releaseTarget := svc.lockImportTarget(ctx, 7)
		defer releaseTarget()
		_, releaseDecision := svc.lockStorageDecision(targetCtx)
		defer releaseDecision()
		panic("admission interrupted")
	}()
	done := make(chan struct{})
	go func() {
		targetCtx, releaseTarget := svc.lockImportTarget(context.Background(), 7)
		defer releaseTarget()
		_, releaseDecision := svc.lockStorageDecision(targetCtx)
		defer releaseDecision()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("panic leaked admission authority")
	}
}

func TestTryImportTargetRespectsCurrentScope(t *testing.T) {
	svc := &Service{}
	ctx, release := svc.lockImportTarget(context.Background(), 7)
	_, nested, ok := svc.tryLockImportTarget(ctx, 7)
	if !ok {
		t.Fatal("held title not recognized")
	}
	nested()
	_, _, ok = svc.tryLockImportTarget(ctx, 8)
	if ok {
		release()
		t.Fatal("nested different title acquired")
	}
	_, _, ok = svc.tryLockImportTarget(context.Background(), 7)
	if ok {
		release()
		t.Fatal("busy title acquired")
	}
	_, other, ok := svc.tryLockImportTarget(context.Background(), 8)
	if !ok {
		release()
		t.Fatal("unrelated title blocked")
	}
	other()
	other()
	release()
	_, expired, ok := svc.tryLockImportTarget(ctx, 7)
	if !ok {
		t.Fatal("expired context did not reacquire")
	}
	expired()
	_, all, err := svc.lockAllImportTargets(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	all()
}
