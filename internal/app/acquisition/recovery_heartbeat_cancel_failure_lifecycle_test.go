package acquisition

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestRecoveryHeartbeatCancelWriteFailureRecoversOnRestartWithoutReceiptReplay(t *testing.T) {
	ctx := context.Background()
	svc, db, request, _ := recoveryFixture(t)
	originalClient, err := svc.recoveryClient(ctx, request.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	var manifest RunnerRecovery
	if err := originalClient.RecoveryRequest(ctx, http.MethodGet, "recoveries/"+request.RecoveryID, "", nil, &manifest); err != nil || manifest.ID == "" {
		t.Fatalf("read fixture source manifest=%+v err=%v", manifest, err)
	}
	manifest.Artifact = "artifact-heartbeat-cancel"

	var mu sync.Mutex
	remoteState := "published"
	jobID := ""
	var claimCalls, receiptCalls, cancelCalls, cancelAckCalls atomic.Int32
	getCancelPending := make(chan struct{})
	allowGetResponse := make(chan struct{})
	var cancelPendingGET sync.Once
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/recoveries/abc123/claim":
			if r.Header.Get("X-Recovery-Token") != "credential" {
				http.Error(w, "wrong consumer credential", http.StatusUnauthorized)
				return
			}
			if claimCalls.Add(1) == 1 {
				mu.Lock()
				remoteState = "claimed"
				mu.Unlock()
				claimed := manifest
				claimed.State = "claimed"
				_ = json.NewEncoder(w).Encode(claimed)
				return
			}
			mu.Lock()
			remoteState = "cancel_pending"
			mu.Unlock()
			http.Error(w, "claim is cancelling", http.StatusConflict)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/recoveries/abc123":
			mu.Lock()
			state := remoteState
			mu.Unlock()
			if state == "cancel_pending" {
				cancelPendingGET.Do(func() { close(getCancelPending) })
				select {
				case <-allowGetResponse:
				case <-r.Context().Done():
					return
				}
			}
			current := manifest
			current.State = state
			current.ImportID = jobID
			_ = json.NewEncoder(w).Encode(current)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/recoveries/abc123/receipt":
			receiptCalls.Add(1)
			mu.Lock()
			state := remoteState
			mu.Unlock()
			if state != "claimed" {
				http.Error(w, "recovery is not claimed", http.StatusConflict)
				return
			}
			completed := manifest
			completed.State = "imported"
			_ = json.NewEncoder(w).Encode(completed)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/recoveries/abc123/cancel-ack":
			cancelAckCalls.Add(1)
			mu.Lock()
			remoteState = "cancelled"
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/recoveries/abc123/cancel":
			cancelCalls.Add(1)
			mu.Lock()
			state := remoteState
			mu.Unlock()
			current := manifest
			current.State = state
			current.ImportID = jobID
			_ = json.NewEncoder(w).Encode(current)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	client, err := db.GetDownloadClient(ctx, request.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	client.URL = remote.URL
	if err = db.UpdateDownloadClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	preview, err := svc.PreviewRecovery(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	jobID = job.ID
	if _, err = db.W.ExecContext(ctx, `CREATE TABLE heartbeat_cancel_attempts(import_id TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `CREATE TRIGGER fail_heartbeat_cancel BEFORE UPDATE OF state ON recovery_imports
		WHEN NEW.state='cancel_pending' BEGIN
		INSERT INTO heartbeat_cancel_attempts(import_id) VALUES(NEW.id);
		SELECT RAISE(FAIL, 'injected cancellation persistence failure');
	END`); err != nil {
		t.Fatal(err)
	}

	// Publication staging and hashing proceed, but the final placement commit
	// waits on this short authority lock while the real 30-second heartbeat runs.
	svc.storageDecisionMu.Lock()
	storageLocked := true
	defer func() {
		if storageLocked {
			svc.storageDecisionMu.Unlock()
		}
	}()
	sweepDone := make(chan struct{})
	go func() {
		svc.recoverySweep(ctx)
		close(sweepDone)
	}()
	select {
	case <-getCancelPending:
	case <-time.After(45 * time.Second):
		t.Fatal("worker did not reach the recovery heartbeat cancellation check")
	}
	close(allowGetResponse)

	// RAISE(FAIL) leaves the trigger's audit row visible while rejecting this
	// exact state write, proving the actual heartbeat branch attempted and lost
	// its durable cancellation update.
	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		var attempts int
		if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM heartbeat_cancel_attempts WHERE import_id=?`, job.ID).Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts == 1 {
			break
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("heartbeat did not attempt the cancellation state write")
		}
	}
	if _, err = db.W.ExecContext(ctx, `DROP TRIGGER fail_heartbeat_cancel`); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = db.R.QueryRowContext(ctx, `SELECT state FROM recovery_imports WHERE id=?`, job.ID).Scan(&state); err != nil || state != "importing" {
		t.Fatalf("failed cancellation write changed local state: %q err=%v", state, err)
	}
	svc.storageDecisionMu.Unlock()
	storageLocked = false

	select {
	case <-sweepDone:
	case <-time.After(20 * time.Second):
		t.Fatal("recovery worker did not finish after placement authority was released")
	}
	// Receipt reconciliation durably parks the import. The next sweep performs
	// the existing prepared-only rollback/quiescence handshake, but simulate a
	// crash after Runner's acknowledgement and before the local terminal stamp.
	if _, err = db.W.ExecContext(ctx, `CREATE TRIGGER fail_local_cancel_stamp BEFORE UPDATE OF state ON recovery_imports
		WHEN NEW.state='cancelled' BEGIN SELECT RAISE(ABORT, 'injected local cancellation stamp failure'); END`); err != nil {
		t.Fatal(err)
	}
	svc.recoverySweep(ctx)
	pending, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil || pending.State != "cancel_pending" || receiptCalls.Load() != 1 || cancelCalls.Load() != 1 || cancelAckCalls.Load() != 1 {
		t.Fatalf("failed local finalization lost acknowledged cancellation: state=%+v receipts=%d cancels=%d cancelAcks=%d err=%v", pending, receiptCalls.Load(), cancelCalls.Load(), cancelAckCalls.Load(), err)
	}
	files, err := db.ListFilesForItem(ctx, request.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("committed library placement was lost during cancellation reconciliation: files=%+v err=%v", files, err)
	}
	if _, err = os.Stat(files[0].Path); err != nil {
		t.Fatalf("committed library bytes were removed by cancellation: %v", err)
	}
	gotDigest, _, err := fileDigest(ctx, files[0].Path)
	if err != nil || gotDigest != manifest.Files[0].SHA256 {
		t.Fatalf("committed library bytes changed during cancellation: digest=%q want=%q err=%v", gotDigest, manifest.Files[0].SHA256, err)
	}
	var delivered int
	var lastError string
	if err = db.R.QueryRowContext(ctx, `SELECT delivered,last_error FROM recovery_receipt_outbox WHERE import_id=?`, job.ID).Scan(&delivered, &lastError); err != nil || delivered != 0 {
		t.Fatalf("cancellation incorrectly marked receipt delivered: delivered=%d last_error=%q err=%v", delivered, lastError, err)
	}
	if _, err = db.W.ExecContext(ctx, `DROP TRIGGER fail_local_cancel_stamp`); err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Dir(db.Path)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err = reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, nil, nil, nil, nil)
	restarted.recoverySweep(ctx)
	last, err := restarted.RecoveryImport(ctx, job.ID)
	if err != nil || last.State != "cancelled" || receiptCalls.Load() != 1 || cancelCalls.Load() != 1 || cancelAckCalls.Load() != 1 {
		t.Fatalf("restart resent receipt or stranded cancellation: state=%+v receipts=%d cancelAcks=%d err=%v", last, receiptCalls.Load(), cancelAckCalls.Load(), err)
	}
	if err = reopened.R.QueryRowContext(ctx, `SELECT state FROM recovery_imports WHERE id=?`, job.ID).Scan(&state); err != nil || state != "cancelled" {
		t.Fatalf("reopened receipt state=%q err=%v", state, err)
	}
	if err = reopened.R.QueryRowContext(ctx, `SELECT delivered,last_error FROM recovery_receipt_outbox WHERE import_id=?`, job.ID).Scan(&delivered, &lastError); err != nil || delivered != 0 || !strings.Contains(lastError, "cancellation acknowledged") {
		t.Fatalf("restart did not settle cancellation audit without delivering receipt: delivered=%d last_error=%q err=%v", delivered, lastError, err)
	}
	metrics, err := restarted.RecoveryMetrics(ctx)
	if err != nil || metrics["receipt_outbox_pending"] != 0 {
		t.Fatalf("terminal cancellation still appears as actionable receipt backlog: metrics=%v err=%v", metrics, err)
	}
}

func TestRecoveryReceiptConflictWithMismatchedImportIDDoesNotCancelOrRemoveLibraryBytes(t *testing.T) {
	ctx := context.Background()
	svc, db, request, _ := recoveryFixture(t)
	originalClient, err := svc.recoveryClient(ctx, request.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	var manifest RunnerRecovery
	if err = originalClient.RecoveryRequest(ctx, http.MethodGet, "recoveries/"+request.RecoveryID, "", nil, &manifest); err != nil || manifest.ID == "" {
		t.Fatalf("read source manifest: %+v err=%v", manifest, err)
	}
	manifest.Artifact = "artifact-mismatch-check"
	var receiptCalls, cancelCalls atomic.Int32
	var mismatchMu sync.Mutex
	identityMismatch := "import_id"
	jobID := ""
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/recoveries/abc123":
			current := manifest
			if receiptCalls.Load() > 0 {
				current.State = "cancel_pending"
				mismatchMu.Lock()
				field := identityMismatch
				mismatchMu.Unlock()
				if field == "import_id" {
					current.ImportID = "a-different-import"
				} else {
					current.ImportID = jobID
					current.Artifact = "a-different-artifact"
				}
			} else {
				current.State = "published"
			}
			_ = json.NewEncoder(w).Encode(current)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/recoveries/abc123/claim":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/recoveries/abc123/receipt":
			receiptCalls.Add(1)
			http.Error(w, "receipt conflicts with current recovery", http.StatusConflict)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/recoveries/abc123/cancel":
			cancelCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	client, err := db.GetDownloadClient(ctx, request.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	client.URL = remote.URL
	if err = db.UpdateDownloadClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	preview, err := svc.PreviewRecovery(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	jobID = job.ID
	svc.recoverySweep(ctx)
	local, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil || local.State != "receipt_pending" || receiptCalls.Load() != 1 || cancelCalls.Load() != 0 {
		t.Fatalf("mismatched remote import identity authorized cancellation: local=%+v receipts=%d cancels=%d err=%v", local, receiptCalls.Load(), cancelCalls.Load(), err)
	}
	files, err := db.ListFilesForItem(ctx, request.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("mismatched remote identity changed committed media records: files=%+v err=%v", files, err)
	}
	if _, err = os.Stat(files[0].Path); err != nil {
		t.Fatalf("mismatched remote identity removed committed bytes: %v", err)
	}
	var delivered int
	if err = db.R.QueryRowContext(ctx, `SELECT delivered FROM recovery_receipt_outbox WHERE import_id=?`, job.ID).Scan(&delivered); err != nil || delivered != 0 {
		t.Fatalf("unaccepted receipt was marked delivered: delivered=%d err=%v", delivered, err)
	}
	if !strings.Contains(local.Error, "receipt conflicts") && local.Error == "" {
		t.Fatalf("expected visible receipt conflict diagnostic, got %+v", local)
	}
	// A later conflict with a correct token but a changed artifact identity is
	// also insufficient authority to cancel the local recovery.
	mismatchMu.Lock()
	identityMismatch = "artifact"
	mismatchMu.Unlock()
	svc.recoverySweep(ctx)
	local, err = svc.RecoveryImport(ctx, job.ID)
	if err != nil || local.State != "receipt_pending" || receiptCalls.Load() != 2 || cancelCalls.Load() != 0 {
		t.Fatalf("changed artifact identity authorized cancellation: local=%+v receipts=%d cancels=%d err=%v", local, receiptCalls.Load(), cancelCalls.Load(), err)
	}
	files, err = db.ListFilesForItem(ctx, request.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("changed artifact identity altered committed media: files=%+v err=%v", files, err)
	}
}
