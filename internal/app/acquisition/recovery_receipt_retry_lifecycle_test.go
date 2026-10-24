package acquisition

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRecoveryReceiptOutboxRetriesRemoteAndLocalFailures(t *testing.T) {
	for _, scenario := range []string{"runner_unavailable", "runner_not_imported", "local_commit_failed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			svc, db, request, _ := recoveryFixture(t)
			preview, err := svc.PreviewRecovery(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			job, err := svc.QueueRecovery(ctx, preview.Request)
			if err != nil {
				t.Fatal(err)
			}
			markRecoveryImportingForTest(t, db, job.ID)
			if err := svc.runRecovery(ctx, job); err != nil {
				t.Fatalf("prepare durable receipt outbox: %v", err)
			}

			var receiptCalls atomic.Int32
			var runnerImported atomic.Bool
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/receipt") {
					http.NotFound(w, r)
					return
				}
				receiptCalls.Add(1)
				switch scenario {
				case "runner_unavailable":
					http.Error(w, "temporary receipt outage", http.StatusServiceUnavailable)
				case "runner_not_imported":
					w.Header().Set("Content-Type", "application/json")
					state := "partial"
					if runnerImported.Load() {
						state = "imported"
					}
					_ = json.NewEncoder(w).Encode(RunnerRecovery{ID: request.RecoveryID, State: state})
				default:
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(RunnerRecovery{ID: request.RecoveryID, State: "imported"})
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
			if scenario == "local_commit_failed" {
				if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_recovery_receipt_commit BEFORE UPDATE OF state ON recovery_imports WHEN NEW.state='imported' BEGIN SELECT RAISE(ABORT,'simulated local receipt commit outage'); END`); err != nil {
					t.Fatal(err)
				}
			}

			svc.deliverRecoveryReceipts(ctx)
			var delivered int
			var lastError string
			var importedState string
			if err := db.R.QueryRowContext(ctx, `SELECT delivered,last_error FROM recovery_receipt_outbox WHERE import_id=?`, job.ID).Scan(&delivered, &lastError); err != nil {
				t.Fatal(err)
			}
			pending, err := svc.RecoveryImport(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if delivered != 0 || pending.State != "receipt_pending" || receiptCalls.Load() != 1 {
				t.Fatalf("failed acknowledgement was not retained for replay: delivered=%d lastError=%q calls=%d import=%+v", delivered, lastError, receiptCalls.Load(), pending)
			}
			if scenario != "local_commit_failed" && lastError == "" {
				t.Fatalf("remote receipt failure was not recorded in the outbox: %q", lastError)
			}
			if scenario == "runner_not_imported" && !strings.Contains(lastError, "partial") {
				t.Fatalf("unexpected Runner state was not recorded: %q", lastError)
			}
			if scenario == "local_commit_failed" && !strings.Contains(lastError, "local receipt commit outage") {
				t.Fatalf("local transaction failure was not recorded in the outbox: %q", lastError)
			}

			if scenario == "runner_unavailable" {
				// Keep the Runner unavailable and prove a normal second pass records
				// another attempt without losing the durable receipt.
				svc.deliverRecoveryReceipts(ctx)
				if receiptCalls.Load() != 2 {
					t.Fatalf("pending receipt was not retried: calls=%d", receiptCalls.Load())
				}
				return
			}
			if scenario == "runner_not_imported" {
				// The Runner's state is authoritative. A later imported response
				// settles the same outbox row and local import.
				runnerImported.Store(true)
				svc.deliverRecoveryReceipts(ctx)
				if err := db.R.QueryRowContext(ctx, `SELECT state FROM recovery_imports WHERE id=?`, job.ID).Scan(&importedState); err != nil || importedState != "imported" {
					t.Fatalf("Runner imported response did not settle local state: state=%q err=%v", importedState, err)
				}
				if err := db.R.QueryRowContext(ctx, `SELECT delivered FROM recovery_receipt_outbox WHERE import_id=?`, job.ID).Scan(&delivered); err != nil || delivered != 1 {
					t.Fatalf("Runner imported response did not settle outbox: delivered=%d err=%v", delivered, err)
				}
				if receiptCalls.Load() != 2 {
					t.Fatalf("Runner-state retry call count=%d, want 2", receiptCalls.Load())
				}
				return
			}

			if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_recovery_receipt_commit`); err != nil {
				t.Fatal(err)
			}
			reopened := reopenLifecycleDB(t, db)
			restarted := New(reopened, nil, nil, nil, nil)
			restarted.deliverRecoveryReceipts(ctx)
			if err := reopened.R.QueryRowContext(ctx, `SELECT state FROM recovery_imports WHERE id=?`, job.ID).Scan(&importedState); err != nil || importedState != "imported" {
				t.Fatalf("local retry did not settle imported state: state=%q err=%v", importedState, err)
			}
			if err := reopened.R.QueryRowContext(ctx, `SELECT delivered FROM recovery_receipt_outbox WHERE import_id=?`, job.ID).Scan(&delivered); err != nil || delivered != 1 {
				t.Fatalf("atomic receipt outbox update did not settle: delivered=%d err=%v", delivered, err)
			}
			if receiptCalls.Load() != 2 {
				t.Fatalf("idempotent retry call count=%d, want 2", receiptCalls.Load())
			}
		})
	}
}
