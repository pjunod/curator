package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestImportRetryStatusTracksRealBlockedImportAcrossRetry(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{}
	svc, db, itemID := setupUsenet(t, client)
	payload := t.TempDir()
	source := filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv")
	if err := os.WriteFile(source, corpusFile(t, wholeCorpus720), 0o644); err != nil {
		t.Fatal(err)
	}
	dl := insertImportable(t, db, itemID, payload)

	// Exercise the ordinary import failure path so the status view is derived
	// from the same durable handoff trace the Activity API exposes.
	restore := failPlacement(t, syscall.ENOSPC)
	if err := svc.runImport(ctx, dl); !errors.Is(err, ErrImportBlocked) {
		t.Fatalf("initial blocked import error=%v, want ErrImportBlocked", err)
	}
	row, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, blockedSince, retryAt, retryable := svc.ImportRetryStatus(row)
	if !retryable || attempts != 0 || blockedSince.IsZero() || retryAt.IsZero() {
		t.Fatalf("initial import retry status=(%d,%v,%v,%v), want blocked with a durable schedule", attempts, blockedSince, retryAt, retryable)
	}
	var firstFailure int64
	for _, event := range row.Handoff {
		if event.Step == stepFailed {
			firstFailure = event.At
			break
		}
	}
	if firstFailure == 0 || blockedSince.UnixMilli() != firstFailure || !retryAt.Equal(time.UnixMilli(firstFailure).Add(30*time.Second)) {
		t.Fatalf("retry status does not match durable failure event: failedAt=%d blockedSince=%v retryAt=%v", firstFailure, blockedSince, retryAt)
	}

	// Retry through the scheduled service entrypoint while storage still fails.
	// The attempt and its next deadline must survive as a second trace event.
	previousBackoff := importRetryBackoff
	importRetryBackoff = 0
	t.Cleanup(func() { importRetryBackoff = previousBackoff })
	if err := svc.RetryBlockedImports(ctx); err != nil {
		t.Fatalf("scheduled retry: %v", err)
	}
	// Restore the production schedule for observation; the zero delay above is
	// only what made this second service attempt immediately runnable.
	importRetryBackoff = previousBackoff
	row, err = db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, blockedSince, retryAt, retryable = svc.ImportRetryStatus(row)
	if !retryable || attempts != 1 || blockedSince.UnixMilli() != firstFailure || retryAt.IsZero() {
		t.Fatalf("retried import status=(%d,%v,%v,%v), want one retry and original blocked time", attempts, blockedSince, retryAt, retryable)
	}
	lastAttempt, ok := lastImportAttempt(row)
	if !ok || !retryAt.Equal(lastAttempt.Add(time.Minute)) {
		t.Fatalf("retry deadline=%v, want latest persisted attempt %v plus one-minute backoff", retryAt, lastAttempt)
	}

	// Make the third service attempt due without waiting. Once it succeeds it
	// must stop advertising a retry even though
	// its historical failure and retry events remain in the trace.
	restore()
	importRetryBackoff = 0
	if err := svc.RetryBlockedImports(ctx); err != nil {
		t.Fatalf("recovery retry: %v", err)
	}
	row, err = db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, retryable := svc.ImportRetryStatus(row); retryable || row.State != "imported" {
		t.Fatalf("successful import still advertises retry: state=%q", row.State)
	}
}

func TestDeliverySchedulerPersistsAmbiguousAssociationThenQueuesExactMatchAfterRestart(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	// Keep the fixture's pre-associated delivery out of this scheduling pass.
	if err := db.SetDeliveryConflict(ctx, receiptIDFor(t, db, dl.ID), "unrelated fixture receipt"); err != nil {
		t.Fatal(err)
	}

	duplicate, err := db.InsertDownloadRecord(ctx, sqlite.Download{
		MediaItemID: dl.MediaItemID, WantableIDs: dl.WantableIDs, ReleaseTitle: "duplicate handle attempt",
		Indexer: "idx", Protocol: "usenet", ClientID: cfg.ID, Handle: dl.Handle, Transfer: dl.Transfer,
		State: "downloaded", DeliveryExpected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := []ports.RunnerFile{{Path: epRelease + ".mkv", SizeBytes: int64(len("video")), SHA256: sha256Hex([]byte("video"))}}
	delivery := ports.RunnerDelivery{
		Sequence: 2, DeliveryID: "delivery-ambiguous-association", PublicationGeneration: "publication-ambiguous-association",
		ArtifactID: "artifact-ambiguous-association", JobID: dl.Handle, RequestToken: dl.Transfer,
		RunnerInstanceID: "runner-1", PublicationPath: "/remote/completed/ambiguous-association", Files: manifest,
	}
	delivery.ManifestDigest, err = runnerManifestDigest(delivery.Files)
	if err != nil {
		t.Fatal(err)
	}
	r, err := db.SaveDiscoveredDelivery(ctx, cfg.ID, delivery, publication)
	if err != nil {
		t.Fatal(err)
	}

	// Two exact client/handle/token candidates are ambiguous. The scheduler
	// must persist uncertainty and must not guess a target or start an import.
	svc.importCh = make(chan importJob, 1)
	svc.importCtx = ctx
	svc.importJobs = make(map[int64]*importTicket)
	svc.scheduleDeliveryReceipts(ctx, cfg, protocol)
	unresolved, err := db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unresolved.DownloadID != 0 || unresolved.Attempts != 1 || unresolved.Error == "" || protocol.accepts != 0 {
		t.Fatalf("ambiguous receipt was guessed or not durably deferred: receipt=%+v accepts=%d", unresolved, protocol.accepts)
	}
	select {
	case job := <-svc.importCh:
		job.ticket.cancel()
		t.Fatalf("ambiguous delivery unexpectedly queued download %d", job.dl.ID)
	default:
	}

	// Resolve the ambiguity by changing only the unrelated row, reopen the
	// SQLite journal, and retry from the persisted receipt. The exact surviving
	// admission is associated and receives one bounded worker lease.
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET transfer='different-request-token' WHERE id=?`, duplicate.ID); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reopened.ForceDeliveryRetryNow(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, nil, nil, nil, func(ports.ClientConfig) ports.DownloadClient { return protocol })
	restarted.importCh = make(chan importJob, 1)
	restarted.importCtx = ctx
	restarted.importJobs = make(map[int64]*importTicket)
	restarted.scheduleDeliveryReceipts(ctx, cfg, protocol)
	associated, err := reopened.DeliveryReceiptByID(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if associated.DownloadID != dl.ID || associated.GenerationKey != dl.GenerationKey || associated.DownloadHandle != dl.Handle || associated.Attempts != 1 || associated.Error != "" || !associated.NextAttemptAt.After(time.Now()) {
		t.Fatalf("resolved receipt did not associate and lease the original exact admission: %+v", associated)
	}
	select {
	case job := <-restarted.importCh:
		if job.dl.ID != dl.ID || job.deliveryReceiptID != r.ID {
			job.ticket.cancel()
			t.Fatalf("scheduler queued wrong durable identity: download=%d receipt=%d", job.dl.ID, job.deliveryReceiptID)
		}
		job.ticket.cancel()
	default:
		t.Fatal("resolved durable receipt was not enqueued after restart")
	}
	if protocol.accepts != 0 {
		t.Fatalf("association-only recovery unexpectedly accepted a Runner delivery: calls=%d", protocol.accepts)
	}
}
