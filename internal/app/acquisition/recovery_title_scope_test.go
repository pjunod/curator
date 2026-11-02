package acquisition

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

// Recovery is leased against its own title. Another title importing — holding
// its import authority and committing changes to the library the whole time —
// neither holds the recovery up nor voids its preview.
func TestRecoveryRunsWhileAnotherTitleImports(t *testing.T) {
	svc, db, req, receipts := recoveryFixture(t)
	ctx := context.Background()
	other, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Busy title", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, releaseOther := svc.lockImportTarget(ctx, other)
	defer releaseOther()

	preview, err := svc.PreviewRecovery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET title='Busy title (2026)' WHERE id=?`, other); err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatalf("queueing waited on or was voided by another title: %v", err)
	}
	original := placeFile
	t.Cleanup(func() { placeFile = original })
	placeFile = func(ctx context.Context, src, dest string, progress func(int64, int64)) error {
		if err := original(ctx, src, dest, progress); err != nil {
			return err
		}
		_, err := db.W.ExecContext(ctx, `UPDATE media_items SET title='Busy title (2026) restored' WHERE id=?`, other)
		return err
	}
	done := make(chan struct{})
	go func() { svc.recoverySweep(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("recovery waited behind another title's import")
	}
	result, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "imported" || receipts.Load() != 1 {
		t.Fatalf("state=%s error=%q receipts=%d, want imported with one receipt", result.State, result.Error, receipts.Load())
	}
}

// The same title still excludes recovery: queueing reports busy instead of
// hanging, and a held title is never handed to a second holder.
func TestRecoveryWaitsOnlyForItsOwnTitle(t *testing.T) {
	s := &Service{}
	ctx := context.Background()
	_, release := s.lockImportTarget(ctx, 7)
	if _, _, err := s.waitImportTarget(ctx, 7, 50*time.Millisecond); !errors.Is(err, ErrImportTargetBusy) {
		t.Fatalf("same title while held: err=%v, want busy", err)
	}
	_, releaseOther, err := s.waitImportTarget(ctx, 8, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("another title while 7 is held: %v", err)
	}
	releaseOther()
	release()
	held, releaseHeld, err := s.waitImportTarget(ctx, 7, time.Second)
	if err != nil {
		t.Fatalf("released title not granted: %v", err)
	}
	if _, _, err := s.waitImportTarget(held, 9, time.Second); err == nil {
		t.Fatal("a second title was granted while holding one")
	}
	releaseHeld()
	if len(s.importTargets.items) != 0 {
		t.Fatalf("released title locks were retained: %d", len(s.importTargets.items))
	}
}

// Acknowledging a cancellation needs the recovery's title. While that title
// is importing, the sweep leaves the cancellation pending and moves on
// instead of blocking every other recovery until the import finishes.
func TestRecoveryCancellationDoesNotHoldTheSweepOnABusyTitle(t *testing.T) {
	svc, _, req, _ := recoveryFixture(t)
	ctx := context.Background()
	preview, err := svc.PreviewRecovery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CancelRecoveryImport(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	_, release := svc.lockImportTarget(ctx, req.MediaItemID)
	done := make(chan struct{})
	go func() { svc.recoverySweep(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		release()
		t.Fatal("the recovery sweep blocked on a busy title's cancellation")
	}
	pending, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil || pending.State != "cancel_pending" {
		t.Fatalf("cancellation while busy: state=%s err=%v, want cancel_pending", pending.State, err)
	}
	release()
	svc.recoverySweep(ctx)
	done2, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil || done2.State != "cancelled" {
		t.Fatalf("cancellation after the title freed: state=%s error=%q err=%v, want cancelled", done2.State, done2.Error, err)
	}
}
