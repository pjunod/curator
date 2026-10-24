package acquisition

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRecoveryCancelAckOutageKeepsCancellationPendingForReplay(t *testing.T) {
	ctx := context.Background()
	svc, db, request, receipts := recoveryFixture(t)
	preview, err := svc.PreviewRecovery(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	original, err := svc.recoveryClient(ctx, request.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	var manifest RunnerRecovery
	if err = original.RecoveryRequest(ctx, http.MethodGet, "recoveries/"+request.RecoveryID, "", nil, &manifest); err != nil {
		t.Fatal(err)
	}
	var cancelCalls, ackCalls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/recoveries/abc123" {
			observed := manifest
			observed.State = "cancel_pending"
			_ = json.NewEncoder(w).Encode(observed)
			return
		}
		if r.Method != http.MethodPost || r.Header.Get("X-Recovery-Token") != "credential" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/recoveries/abc123/cancel":
			cancelCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			cancelled := manifest
			cancelled.State = "cancel_pending"
			_ = json.NewEncoder(w).Encode(cancelled)
		case "/api/v1/recoveries/abc123/cancel-ack":
			if ackCalls.Add(1) == 1 {
				http.Error(w, "temporary acknowledgement outage", http.StatusServiceUnavailable)
				return
			}
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
	if err := db.UpdateDownloadClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	if err := svc.CancelRecoveryImport(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	svc.recoverySweep(ctx)
	pending, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil || pending.State != "cancel_pending" || cancelCalls.Load() != 1 || ackCalls.Load() != 1 {
		t.Fatalf("remote acknowledgement outage lost the pending cancellation: import=%+v cancelCalls=%d ackCalls=%d err=%v", pending, cancelCalls.Load(), ackCalls.Load(), err)
	}
	if receipts.Load() != 0 {
		t.Fatal("cancellation outage emitted an import receipt")
	}
	files, err := db.ListFilesForItem(ctx, request.MediaItemID)
	if err != nil {
		t.Fatal(err)
	} else if len(files) != 0 {
		t.Fatalf("cancellation outage committed library files: %+v", files)
	}

	svc.recoverySweep(ctx)
	settled, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil || settled.State != "cancelled" || cancelCalls.Load() != 2 || ackCalls.Load() != 2 {
		t.Fatalf("replayed cancellation did not settle after Runner recovery: import=%+v cancelCalls=%d ackCalls=%d err=%v", settled, cancelCalls.Load(), ackCalls.Load(), err)
	}
	if receipts.Load() != 0 {
		t.Fatal("replayed cancellation emitted an import receipt")
	}
}
