package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestDeliveryLifecycleListPagesAndFiltersDurableReceipts(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	firstID := receiptIDFor(t, db, dl.ID)

	secondDelivery := ports.RunnerDelivery{
		Sequence: 2, DeliveryID: "delivery-2", PublicationGeneration: "pub-2",
		ArtifactID: "artifact-2", JobID: "18", RequestToken: "t-18",
		RunnerInstanceID: "runner-1", PublicationPath: "/remote/completed/release-2",
		Files: []ports.RunnerFile{{Path: "episode.mkv", SizeBytes: 5, SHA256: sha256Hex([]byte("video"))}},
	}
	secondDelivery.ManifestDigest, _ = runnerManifestDigest(secondDelivery.Files)
	second, err := db.SaveDiscoveredDelivery(ctx, cfg.ID, secondDelivery, filepath.Join(t.TempDir(), "release-2"))
	if err != nil {
		t.Fatal(err)
	}

	page, err := svc.DeliveryLifecycle(ctx, 0, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ReceiptID != second.ID || page.NextBeforeID != second.ID {
		t.Fatalf("first lifecycle page=%+v, want newest receipt and cursor %d", page, second.ID)
	}
	page, err = svc.DeliveryLifecycle(ctx, page.NextBeforeID, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ReceiptID != firstID || page.NextBeforeID != 0 {
		t.Fatalf("second lifecycle page=%+v, want older receipt %d and end cursor", page, firstID)
	}
	filtered, err := svc.DeliveryLifecycle(ctx, 0, dl.ID, 10)
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].ReceiptID != firstID {
		t.Fatalf("download-filtered lifecycle=%+v err=%v", filtered, err)
	}
	for _, limit := range []int{0, 101} {
		if _, err := svc.DeliveryLifecycle(ctx, 0, 0, limit); err == nil {
			t.Errorf("limit %d was accepted", limit)
		}
	}
	if _, err := svc.DeliveryLifecycle(ctx, 0, dl.ID, -1); err == nil {
		t.Fatal("invalid download-filtered limit was accepted")
	}
}

func TestRunnerDeliveryCapabilityUsesPersistedStateAndSafeFallback(t *testing.T) {
	ctx := context.Background()
	svc, db, _, cfg, _ := deliveryImportFixture(t, &deliveryImportClient{})
	if enabled := svc.runnerDeliveryEnabled(ctx, cfg); enabled {
		t.Fatal("unadvertised qBittorrent client was treated as Runner delivery capable")
	}
	if err := db.EnableRunnerDeliveryV1(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	fresh := New(db, nil, nil, nil, nil)
	if enabled := fresh.runnerDeliveryEnabled(ctx, cfg); !enabled {
		t.Fatal("persisted delivery-v1 capability was not restored")
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_client_instances SET delivery_v1_enabled=0 WHERE client_id=?`, cfg.ID); err != nil {
		t.Fatal(err)
	}
	fresh.rememberRunnerDelivery(cfg.ID)
	if enabled := fresh.runnerDeliveryEnabled(ctx, cfg); !enabled {
		t.Fatal("positive capability observation was lost from the service cache")
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	fallback := New(db, nil, nil, nil, nil)
	if enabled := fallback.runnerDeliveryEnabled(ctx, ports.ClientConfig{ID: cfg.ID, Name: "Runner", Type: "nzbd"}); !enabled {
		t.Fatal("NZBd capability-read failure did not conservatively fence delivery cleanup")
	}
	if enabled := fallback.runnerDeliveryEnabled(ctx, ports.ClientConfig{ID: cfg.ID, Name: "qBittorrent", Type: "qbittorrent"}); enabled {
		t.Fatal("unknown client capability-read failure authorized Runner delivery")
	}
}

func TestScheduleDeliveryReceiptsRetriesOnlyDuePendingAcknowledgements(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='imported',ack_state='pending',consumer_receipt_id='receipt-1',next_attempt_at=0,attempts=0,provider_retry_after=0 WHERE id=?`, receiptID); err != nil {
		t.Fatal(err)
	}
	protocol.acceptErr = errors.New("Runner offline")
	svc.scheduleDeliveryReceipts(ctx, cfg, protocol)
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.accepts != 1 || r.State != "imported" || r.AckState != "pending" || r.Attempts != 1 || r.Error == "" || !r.NextAttemptAt.After(time.Now()) {
		t.Fatalf("failed due acknowledgement state: calls=%d receipt=%+v", protocol.accepts, r)
	}

	// A second queue sweep must respect the persisted due time and leave the
	// imported local result alone while the Runner is unavailable.
	svc.scheduleDeliveryReceipts(ctx, cfg, protocol)
	if protocol.accepts != 1 {
		t.Fatalf("not-yet-due acknowledgement was dispatched again: calls=%d", protocol.accepts)
	}

	dataDir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, nil, nil, nil, func(ports.ClientConfig) ports.DownloadClient { return protocol })
	restarted.scheduleDeliveryReceipts(ctx, cfg, protocol)
	persisted, err := reopened.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || persisted.AckState != "pending" || persisted.NextAttemptAt.Before(time.Now().Add(20*time.Second)) {
		t.Fatalf("restart lost the acknowledgement retry deadline: %+v err=%v", persisted, err)
	}
	if protocol.accepts != 1 {
		t.Fatalf("restart replayed an acknowledgement before its persisted deadline: calls=%d", protocol.accepts)
	}
	protocol.acceptErr = nil
	if err := reopened.ForceDeliveryRetryNow(ctx, receiptID); err != nil {
		t.Fatal(err)
	}
	restarted.scheduleDeliveryReceipts(ctx, cfg, protocol)
	accepted, err := reopened.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || accepted.AckState != "accepted" || protocol.accepts != 2 {
		t.Fatalf("due acknowledgement did not recover after restart: calls=%d receipt=%+v err=%v", protocol.accepts, accepted, err)
	}
}

func TestScheduleDeliveryReceiptsAssociatesNothingByGuessAndMarksAcceptedImport(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	baseID := receiptIDFor(t, db, dl.ID)

	unknown := ports.RunnerDelivery{
		Sequence: 9, DeliveryID: "unknown-download", PublicationGeneration: "pub-unknown",
		ArtifactID: "artifact-unknown", JobID: "999", RequestToken: "t-999",
		RunnerInstanceID: "runner-1", PublicationPath: "/remote/completed/unknown",
		Files: []ports.RunnerFile{{Path: "unknown.mkv", SizeBytes: 5, SHA256: sha256Hex([]byte("video"))}},
	}
	unknown.ManifestDigest, _ = runnerManifestDigest(unknown.Files)
	unknownReceipt, err := db.SaveDiscoveredDelivery(ctx, cfg.ID, unknown, filepath.Join(t.TempDir(), "unknown"))
	if err != nil {
		t.Fatal(err)
	}
	svc.scheduleDeliveryReceipts(ctx, cfg, protocol)
	unresolved, err := db.DeliveryReceiptByID(ctx, unknownReceipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unresolved.DownloadID != 0 || unresolved.Attempts != 1 || unresolved.Error == "" || !unresolved.NextAttemptAt.After(time.Now()) {
		t.Fatalf("unassociated delivery was guessed or not durably retried: %+v", unresolved)
	}
	if protocol.accepts != 0 {
		t.Fatalf("unassociated delivery reached acceptance: calls=%d", protocol.accepts)
	}

	// The exact associated row can transition from an already imported Runner
	// job to imported receipt state only after acceptance is authoritative.
	if err := db.UpdateDownloadState(ctx, dl.ID, "imported", 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='ready',ack_state='accepted',consumer_receipt_id='receipt-accepted',next_attempt_at=0 WHERE id=?`, baseID); err != nil {
		t.Fatal(err)
	}
	svc.scheduleDeliveryReceipts(ctx, cfg, protocol)
	accepted, err := db.DeliveryReceiptByID(ctx, baseID)
	if err != nil || accepted.State != "imported" || accepted.AckState != "accepted" {
		t.Fatalf("accepted existing download was not marked imported: %+v err=%v", accepted, err)
	}
}

func TestQueueDeliveryImportPersistsLeaseAndRollsBackWhenQueueUnavailable(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	receipt, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}

	// A running ticket suppresses duplicate work but leaves a durable future
	// lease so later sweeps do not churn the same payload.
	svc.importJobs = map[int64]*importTicket{dl.ID: {queued: false}}
	svc.queueDeliveryImport(ctx, cfg, dl, receipt)
	leased, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || leased.NextAttemptAt.Before(time.Now().Add(4*time.Minute)) || leased.NextAttemptAt.After(time.Now().Add(6*time.Minute)) {
		t.Fatalf("running import lease was not persisted for the five-minute window: %+v err=%v", leased, err)
	}

	// If admission won the lease but the bounded worker queue is full, roll the
	// receipt due immediately so the next sweep can enqueue it.
	if err := db.ForceDeliveryRetryNow(ctx, receiptID); err != nil {
		t.Fatal(err)
	}
	leased, err = db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	svc.importJobs = map[int64]*importTicket{}
	svc.importCh = make(chan importJob, 1)
	svc.importCh <- importJob{}
	svc.queueDeliveryImport(ctx, cfg, dl, leased)
	retried, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || retried.NextAttemptAt.After(time.Now()) {
		t.Fatalf("full queue left a long lease that would strand delivery: %+v err=%v", retried, err)
	}
}

func TestMarkDeliveryImportedPreservesConflictAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, _, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	svc.markDeliveryImported(ctx, receiptID)
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.State != "imported" {
		t.Fatalf("receipt was not marked imported: %+v err=%v", r, err)
	}
	svc.markDeliveryImported(ctx, receiptID)
	r, err = db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.State != "imported" {
		t.Fatalf("repeated imported transition changed state: %+v err=%v", r, err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='conflict' WHERE id=?`, receiptID); err != nil {
		t.Fatal(err)
	}
	svc.markDeliveryImported(ctx, receiptID)
	r, err = db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.State != "conflict" {
		t.Fatalf("imported marker overwrote a conflict: %+v err=%v", r, err)
	}
	// A missing receipt is an observable logged recovery miss, not a panic.
	svc.markDeliveryImported(ctx, receiptID+10000)
}

func TestQueueDeliveryImportEnqueuesOneDurableReceiptJob(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	receipt, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	svc.importCh = make(chan importJob, 1)
	svc.importCtx = context.Background()
	svc.importJobs = make(map[int64]*importTicket)
	svc.queueDeliveryImport(ctx, cfg, dl, receipt)
	select {
	case job := <-svc.importCh:
		if job.deliveryReceiptID != receiptID || job.dl.ID != dl.ID || job.ticket == nil || !job.ticket.queued {
			t.Fatalf("queued delivery job lost durable identity: %+v", job)
		}
		job.ticket.cancel()
	case <-time.After(time.Second):
		t.Fatal("delivery job was not enqueued")
	}
	current, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || current.NextAttemptAt.Before(time.Now().Add(4*time.Minute)) {
		t.Fatalf("enqueued receipt lacks durable lease: %+v err=%v", current, err)
	}
}

func TestDeliveryCleanupLeavesUnresolvedSiblingAndDirectoryReviewable(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{
		"unrelated.bin": []byte("unassociated release payload"),
	})
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import selected media from payload: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.State != "imported" {
		t.Fatalf("receipt was not terminally imported: %+v err=%v", r, err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, r); err != nil {
		t.Fatal(err)
	}
	mediaPath := filepath.Join(publication, epRelease+".mkv")
	if _, err := os.Stat(mediaPath); !os.IsNotExist(err) {
		t.Fatalf("resolved media payload was not cleaned: %v", err)
	}
	metadataPath := filepath.Join(publication, "unrelated.bin")
	if contents, err := os.ReadFile(metadataPath); err != nil || string(contents) != "unassociated release payload" {
		t.Fatalf("unresolved sibling was removed or changed: contents=%q err=%v", contents, err)
	}
	metadata, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, "unrelated.bin")
	if err != nil || metadata.Disposition != "unknown_recoverable" || metadata.CleanupState == "removed" {
		t.Fatalf("unresolved sibling ledger=%+v err=%v", metadata, err)
	}
	if r, err = db.DeliveryReceiptByID(ctx, receiptID); err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, r); err != nil {
		t.Fatal(err)
	}
	var state, message string
	if err := db.R.QueryRowContext(ctx, `SELECT directory_cleanup_state,directory_cleanup_error FROM runner_delivery_receipts WHERE id=?`, receiptID).Scan(&state, &message); err != nil {
		t.Fatal(err)
	}
	if state != "protected" || !strings.Contains(message, "recoverable or unrelated") {
		t.Fatalf("publication directory was not left reviewable: state=%q message=%q", state, message)
	}
}

func TestDeliveryCleanupRetriesChangedPayloadGenerationWithoutUnlink(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted payload: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	payloadPath := filepath.Join(publication, epRelease+".mkv")
	if err := os.WriteFile(payloadPath, []byte("replacement bytes with changed generation"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, r); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(payloadPath)
	if err != nil || string(contents) != "replacement bytes with changed generation" {
		t.Fatalf("cleanup removed a changed payload generation: contents=%q err=%v", contents, err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, epRelease+".mkv")
	if err != nil || row.CleanupState != "retry" || !strings.Contains(row.CleanupError, "changed") {
		t.Fatalf("changed payload was not durably held for review/retry: %+v err=%v", row, err)
	}
}

func TestDeliveryCleanupRemovesNestedEmptyDirectoriesAfterResolvedFile(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted payload: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(publication, "nested", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(publication); !os.IsNotExist(err) {
		t.Fatalf("fully resolved payload tree was not removed: %v", err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, epRelease+".mkv")
	if err != nil || row.CleanupState != "removed" {
		t.Fatalf("resolved file cleanup ledger=%+v err=%v", row, err)
	}
	var state string
	if err := db.R.QueryRowContext(ctx, `SELECT directory_cleanup_state FROM runner_delivery_receipts WHERE id=?`, receiptID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "removed" {
		t.Fatalf("empty nested directory tree cleanup state=%q, want removed", state)
	}
}

func TestScheduleDeliveryReceiptsStopsAfterFirstSharedAckOutage(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{acceptErr: errors.New("Runner transport unavailable")}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	firstID := receiptIDFor(t, db, dl.ID)
	secondDelivery := ports.RunnerDelivery{
		Sequence: 2, DeliveryID: "delivery-after-outage", PublicationGeneration: "publication-after-outage",
		ArtifactID: "artifact-after-outage", JobID: "82", RequestToken: "t-82", RunnerInstanceID: "runner-1",
		PublicationPath: "/remote/completed/after-outage",
		Files:           []ports.RunnerFile{{Path: "episode.mkv", SizeBytes: 5, SHA256: sha256Hex([]byte("video"))}},
	}
	secondDelivery.ManifestDigest, _ = runnerManifestDigest(secondDelivery.Files)
	second, err := db.SaveDiscoveredDelivery(ctx, cfg.ID, secondDelivery, filepath.Join(t.TempDir(), "after-outage"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{firstID, second.ID} {
		if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='imported',ack_state='pending',consumer_receipt_id=?,next_attempt_at=0,attempts=0 WHERE id=?`, fmt.Sprintf("consumer-%d", id), id); err != nil {
			t.Fatal(err)
		}
	}

	// One timed-out acceptance indicates a shared Runner outage. The scheduler
	// stops this pass instead of serially spending a timeout on every receipt;
	// later due receipts remain durable and eligible for a future sweep.
	svc.scheduleDeliveryReceipts(ctx, cfg, protocol)
	if protocol.accepts != 1 {
		t.Fatalf("shared Runner outage caused %d sequential ACK calls, want one", protocol.accepts)
	}
	first, err := db.DeliveryReceiptByID(ctx, firstID)
	if err != nil || first.AckState != "pending" || first.Attempts != 1 || first.NextAttemptAt.IsZero() {
		t.Fatalf("first ACK failure was not durably deferred: %+v err=%v", first, err)
	}
	deferred, err := db.DeliveryReceiptByID(ctx, second.ID)
	if err != nil || deferred.AckState != "pending" || deferred.Attempts != 0 || deferred.NextAttemptAt.After(time.Now()) {
		t.Fatalf("later due ACK was consumed during shared outage: %+v err=%v", deferred, err)
	}
}
