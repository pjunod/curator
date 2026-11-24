package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
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
