package acquisition

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestRecoveryQueueReplayPinsRequestAndRejectsChangedSelectionAcrossRestart(t *testing.T) {
	ctx := context.Background()
	svc, db, req, _ := recoveryFixture(t)
	preview, err := svc.PreviewRecovery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	first, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil || first.State != "queued" {
		t.Fatalf("initial recovery queue=%+v err=%v", first, err)
	}

	// The Runner identity is the same, but changing the accepted selection
	// must not silently replace the already queued request.
	changed := preview.Request
	changed.FileIDs = nil // all files differs from the explicit single-file choice.
	if _, err := svc.QueueRecovery(ctx, changed); err == nil {
		t.Fatal("same recovery generation accepted a changed file selection")
	}
	stillQueued, err := svc.RecoveryImport(ctx, first.ID)
	if err != nil || stillQueued.State != "queued" || len(stillQueued.Request.FileIDs) != 1 || stillQueued.Request.FileIDs[0] != "file" {
		t.Fatalf("conflicting replay changed the durable request: %+v err=%v", stillQueued, err)
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
	restarted := New(reopened, nil, nil, nil, nil)
	replayed, err := restarted.QueueRecovery(ctx, preview.Request)
	if err != nil || replayed.ID != first.ID || replayed.State != "queued" {
		t.Fatalf("same durable request replay after restart=%+v err=%v; want original queued identity %q", replayed, err, first.ID)
	}
	if _, err := restarted.QueueRecovery(ctx, changed); err == nil {
		t.Fatal("restart forgot the queued recovery request's selected-file scope")
	}
	var count int
	if err := reopened.R.QueryRowContext(ctx, `SELECT count(*) FROM recovery_imports`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recovery replay created %d durable imports, err=%v", count, err)
	}
	if req.RecoveryID != first.Recovery.ID {
		t.Fatalf("fixture request identity changed unexpectedly: %q vs %q", req.RecoveryID, first.Recovery.ID)
	}
}

func TestRecoveryCancelAckPersistenceFailureReplaysSafelyAfterRestart(t *testing.T) {
	ctx := context.Background()
	svc, db, req, receipts := recoveryFixture(t)
	preview, err := svc.PreviewRecovery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}

	var cancelCalls, ackCalls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Recovery-Token") != "credential" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/recoveries/abc123/cancel":
			cancelCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(RunnerRecovery{ID: "abc123", State: "cancel_pending"})
		case "/api/v1/recoveries/abc123/cancel-ack":
			ackCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	client, err := db.GetDownloadClient(ctx, req.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	client.URL = remote.URL
	if err := db.UpdateDownloadClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	if err := svc.CancelRecoveryImport(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_recovery_cancel_commit BEFORE UPDATE OF state ON recovery_imports WHEN NEW.state='cancelled' BEGIN SELECT RAISE(ABORT, 'injected local cancellation commit failure'); END`); err != nil {
		t.Fatal(err)
	}

	// Runner acknowledges cancellation, but the local terminal write fails.
	// The durable row must stay pending so the ordinary recovery sweep can
	// replay the idempotent remote cancellation rather than lose the outcome.
	svc.recoverySweep(ctx)
	local, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil || local.State != "cancel_pending" || cancelCalls.Load() != 1 || ackCalls.Load() != 1 {
		t.Fatalf("failed local cancellation commit state=%+v cancel=%d ack=%d err=%v", local, cancelCalls.Load(), ackCalls.Load(), err)
	}
	if receipts.Load() != 0 {
		t.Fatal("cancelled recovery emitted an import receipt")
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_recovery_cancel_commit`); err != nil {
		t.Fatal(err)
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
	restarted := New(reopened, nil, nil, nil, nil)
	restarted.recoverySweep(ctx)
	settled, err := restarted.RecoveryImport(ctx, job.ID)
	if err != nil || settled.State != "cancelled" || cancelCalls.Load() != 2 || ackCalls.Load() != 2 {
		t.Fatalf("restart did not replay cancellation to a durable terminal state: state=%+v cancel=%d ack=%d err=%v", settled, cancelCalls.Load(), ackCalls.Load(), err)
	}
	if receipts.Load() != 0 {
		t.Fatal("replayed cancellation delivered an import receipt")
	}
}
