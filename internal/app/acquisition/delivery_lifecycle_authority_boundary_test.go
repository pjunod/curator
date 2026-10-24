package acquisition

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func TestSourceCleanupRetryQuarantinesChangedRunnerIdentity(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("prepare accepted delivery: %v", err)
	}
	before, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || before.State != "imported" || before.AckState != "accepted" {
		t.Fatalf("fixture did not reach accepted delivery state: %+v err=%v", before, err)
	}
	protocol.lookup = before.Delivery
	protocol.lookup.RequestToken = "different-runner-request-token"
	protocol.lookup.SourceCleanup = []byte(`{"state":"succeeded","attempts":9}`)
	protocol.lookup.FileOutcomes = []ports.RunnerFileOutcome{{SourcePath: "replacement.mkv", Outcome: "removed"}}
	if err := svc.RetryRunnerSourceCleanup(ctx, receiptID); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("retry with changed immutable identity error=%v, want identity conflict", err)
	}
	after, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != "conflict" || after.Delivery.RequestToken != before.Delivery.RequestToken ||
		!bytes.Equal(after.Delivery.SourceCleanup, before.Delivery.SourceCleanup) || len(after.Delivery.FileOutcomes) != len(before.Delivery.FileOutcomes) {
		t.Fatalf("changed Runner response crossed the saved receipt authority boundary: before=%+v after=%+v", before, after)
	}
	if protocol.sourceCleanupRetries != 1 {
		t.Fatalf("source-cleanup retry count=%d, want one request before conflict quarantine", protocol.sourceCleanupRetries)
	}
}

func TestDeliveryLifecycleOperatorRetriesRefuseConflictWithoutSideEffects(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, _, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := db.SetDeliveryConflict(ctx, receiptID, "Runner publication identity changed"); err != nil {
		t.Fatal(err)
	}
	beforeDownload, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RetryRunnerSourceCleanup(ctx, receiptID); err == nil || !strings.Contains(err.Error(), "non-conflicting") {
		t.Fatalf("source-cleanup operator retry on conflict error=%v", err)
	}
	if err := svc.QueueDeliveryImport(ctx, receiptID, []string{epRelease + ".mkv"}); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("selected reimport on conflict error=%v", err)
	}
	afterDownload, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterDownload.Handoff) != len(beforeDownload.Handoff) || protocol.sourceCleanupRetries != 0 || len(svc.importJobs) != 0 {
		t.Fatalf("conflicted receipt admitted retry side effects: handoff before=%+v after=%+v Runner POSTs=%d import jobs=%+v", beforeDownload.Handoff, afterDownload.Handoff, protocol.sourceCleanupRetries, svc.importJobs)
	}
}

func TestQueueDeliveryImportRejectsReceiptIdentityChangedWhileWaitingForDownloadLock(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("prepare accepted delivery: %v", err)
	}
	beforeDownload, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	unlock := svc.lockDownload(dl.ID)
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- svc.QueueDeliveryImport(requestCtx, receiptID, []string{epRelease + ".mkv"}) }()
	// Observe the request goroutine blocked in lockDownload, which proves its
	// first receipt read completed before changing the pinned identity.
	waitForQueueDeliveryImportLockWait(t)
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET publication_generation='newer-publication-generation' WHERE id=?`, receiptID); err != nil {
		t.Fatal(err)
	}
	unlock()
	locked = false
	var selectionErr error
	select {
	case selectionErr = <-done:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("selected-import request did not finish after releasing the download lock")
	}
	if selectionErr == nil || !strings.Contains(selectionErr.Error(), "identity changed while waiting") {
		t.Fatalf("selected import after receipt identity changed error=%v", selectionErr)
	}
	afterDownload, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterDownload.Handoff) != len(beforeDownload.Handoff) || len(svc.importJobs) != 0 {
		t.Fatalf("stale selection mutated durable handoff or queued work: before=%+v after=%+v jobs=%+v", beforeDownload.Handoff, afterDownload.Handoff, svc.importJobs)
	}
	current, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || current.Delivery.PublicationGeneration != "newer-publication-generation" {
		t.Fatalf("receipt identity fixture was not changed while selection waited: %+v err=%v", current, err)
	}
}

func waitForQueueDeliveryImportLockWait(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	stack := make([]byte, 1<<20)
	for {
		n := runtime.Stack(stack, true)
		for _, goroutine := range strings.Split(string(stack[:n]), "\n\n") {
			if strings.Contains(goroutine, "QueueDeliveryImport") &&
				strings.Contains(goroutine, "lockDownload") && strings.Contains(goroutine, "sync.(*Mutex).Lock") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("QueueDeliveryImport did not reach the per-download lock after reading its receipt")
		}
		<-ticker.C
	}
}

func TestQueueDeliveryImportRefusesAlreadyRunningExactDownload(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("prepare accepted delivery: %v", err)
	}
	beforeDownload, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A worker ticket is the in-memory running-import authority checked by the
	// public selected-import operation. The worker itself is deliberately not
	// started, so this request cannot race filesystem placement in the fixture.
	svc.importJobs = map[int64]*importTicket{dl.ID: {queued: false}}
	if err := svc.QueueDeliveryImport(ctx, receiptID, []string{epRelease + ".mkv"}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("selected import while exact download is running error=%v", err)
	}
	afterDownload, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterDownload.Handoff) != len(beforeDownload.Handoff) || len(svc.importCh) != 0 {
		t.Fatalf("running import was replaced by a second selection: before=%+v after=%+v queued=%d", beforeDownload.Handoff, afterDownload.Handoff, len(svc.importCh))
	}
}
