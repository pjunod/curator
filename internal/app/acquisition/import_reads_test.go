package acquisition

import (
	"context"
	"testing"
)

// Field data 2026-11-01: a retried delivery read its multi-gigabyte payload
// twice per attempt — verification, then a recheck moments later with
// nothing in between. The recheck guards the acknowledgement's network round
// trip; an attempt with no acknowledgement reads each file once.
func TestAcceptedDeliveryRetryReadsEachPayloadFileOnce(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, _ := deliveryImportFixtureNamed(t, &deliveryImportClient{}, "321d9664e6a50c195e7e830b3f78b487.mkv", []byte("video"),
		map[string][]byte{"15713ef716b33f8a63def50814ec76d0.mkv": []byte("other")})
	receiptID := receiptIDFor(t, db, dl.ID)

	before := deliveryPayloadReads.Load()
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err == nil {
		t.Fatal("fixture: two anonymous files must fail import so the receipt is retried")
	}
	if reads := deliveryPayloadReads.Load() - before; reads != 4 {
		t.Fatalf("first attempt read payload files %d times, want 4 (verify + recheck across the acknowledgement, two files)", reads)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.AckState != "accepted" {
		t.Fatalf("fixture: receipt ack=%q err=%v, want accepted", r.AckState, err)
	}

	before = deliveryPayloadReads.Load()
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err == nil {
		t.Fatal("retry unexpectedly imported")
	}
	if reads := deliveryPayloadReads.Load() - before; reads != 2 {
		t.Fatalf("a retry of an accepted receipt read payload files %d times, want 2 (each file once)", reads)
	}
}

// Cleanup work goes to its own worker and never occupies an import slot.
func TestPayloadCleanupNeverTakesAnImportSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc, db, dl, cfg, _ := deliveryImportFixture(t, &deliveryImportClient{})
	r, err := db.DeliveryReceiptByID(ctx, receiptIDFor(t, db, dl.ID))
	if err != nil {
		t.Fatal(err)
	}
	svc.importMu.Lock()
	svc.importCh = make(chan importJob, 1)
	svc.cleanupCh = make(chan importJob, 1)
	svc.importCtx = ctx
	svc.importJobs = map[int64]*importTicket{}
	svc.importMu.Unlock()
	if !svc.queueDeliveryCleanup(ctx, cfg, r) {
		t.Fatal("cleanup was not queued")
	}
	if len(svc.importCh) != 0 || len(svc.cleanupCh) != 1 {
		t.Fatalf("cleanup landed on the import queue: imports=%d cleanup=%d", len(svc.importCh), len(svc.cleanupCh))
	}
	job := <-svc.cleanupCh
	if job.cleanupCancel != nil {
		job.cleanupCancel()
	}
}
