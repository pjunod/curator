package acquisition

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// A failed replacement attempt is not eligible for another search until the
// exact Runner admission can be reconciled. Exercise the full worker path for
// both a temporary status outage and a stale Runner generation, then prove the
// same operation resumes when the pinned no-payload outcome becomes available.
func TestReplacementWorkerHoldsForRunnerAdmissionAndResumesAfterReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		initial     func(*fakeClient)
		wantBlocked string
	}{
		{
			name: "Runner status temporarily unavailable",
			initial: func(client *fakeClient) {
				client.admissionErr = errors.New("Runner status endpoint timed out")
			},
			wantBlocked: "could not verify current Runner cleanup or Keep decision",
		},
		{
			name: "Runner admission generation changed",
			initial: func(client *fakeClient) {
				client.admissionFound = true
				client.admission = ports.RequestAdmission{
					ProtocolVersion: 1, RequestToken: "transfer-worker-outage", JobID: "runner-job-7",
					RunnerInstanceID: "runner-instance", Generation: "unexpected-generation",
					Outcome: "failed", TerminalOutcome: "FAILURE/FETCH", CustodyState: "no_payload",
				}
			},
			wantBlocked: "no longer matches the pinned failed attempt",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeClient{}
			svc, db, itemID := autoSetup(t, []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-RECOVERY", 20)}, client)
			ctx := context.Background()
			clients, err := db.ListDownloadClients(ctx)
			if err != nil || len(clients) == 0 {
				t.Fatalf("clients=%+v err=%v", clients, err)
			}
			op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
				ID: "runner-admission-outage", RequestID: "runner-admission-outage", Fingerprint: "runner-admission-outage-fingerprint",
				State: "searching", Intent: "replace_bad", Scope: "download", MediaItemID: itemID,
				ReasonCode: "bad_media", Reason: "failed source requires authoritative Runner reconciliation", CreatedAt: time.Now(),
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{
				MediaItemID: itemID, ClientID: clients[0].ID, ReleaseTitle: "Test.Movie.2024.720p.WEB-DL.x264-FAILED",
				Indexer: "idx", Protocol: "torrent", State: "failed", ReplacementOperationID: op.ID,
				DeliveryExpected: true, CleanupPending: true, Transfer: "transfer-worker-outage", Handle: "runner-job-7",
				RunnerInstanceID: "runner-instance", RunnerAdmissionGeneration: "pinned-generation",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.SetDownloadHandle(ctx, dl.ID, dl.Handle, dl.Transfer); err != nil {
				t.Fatal(err)
			}
			if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET runner_instance_id=?,runner_admission_generation=? WHERE id=? AND generation_key=?`,
				"runner-instance", "pinned-generation", dl.ID, dl.GenerationKey); err != nil {
				t.Fatal(err)
			}
			tc.initial(client)

			if err := svc.processReplacementOperation(ctx, op); err != nil {
				t.Fatalf("temporary reconciliation issue should be persisted, not returned as a terminal worker error: %v", err)
			}
			blocked, err := db.GetReplacementOperation(ctx, op.ID)
			if err != nil || blocked.State != "cleanup_blocked" || !strings.Contains(blocked.LastError, tc.wantBlocked) {
				t.Fatalf("worker did not persist the precise Runner reconciliation hold: operation=%+v err=%v", blocked, err)
			}
			if len(client.added) != 0 {
				t.Fatalf("replacement was admitted without resolving the failed attempt: %v", client.added)
			}

			client.mu.Lock()
			client.admissionErr = nil
			client.admissionFound = true
			client.admission = ports.RequestAdmission{
				ProtocolVersion: 1, RequestToken: dl.Transfer, JobID: dl.Handle,
				RunnerInstanceID: "runner-instance", Generation: "pinned-generation",
				Outcome: "failed", TerminalOutcome: "FAILURE/FETCH", CustodyState: "no_payload",
			}
			client.mu.Unlock()

			if err := svc.processReplacementOperation(ctx, blocked); err != nil {
				t.Fatalf("worker did not resume the held operation after authoritative no-payload reconciliation: %v", err)
			}
			resumed, err := db.GetReplacementOperation(ctx, op.ID)
			if err != nil || resumed.State != "replacing" || resumed.LastError != "" {
				t.Fatalf("worker did not return to replacement search after reconciliation: operation=%+v err=%v", resumed, err)
			}
			if len(client.added) != 1 || client.added[0] != "http://dl/Test.Movie.2024.1080p.WEB-DL.x264-RECOVERY" {
				t.Fatalf("worker did not admit exactly one replacement after reconciliation: %v", client.added)
			}
		})
	}
}

func TestReplacementAdmissionMismatchDoesNotSearchForDifferentAttempt(t *testing.T) {
	client := &fakeClient{admissionFound: true, admission: ports.RequestAdmission{
		ProtocolVersion: 1, RequestToken: "transfer-pinned", JobID: "runner-job-other",
		RunnerInstanceID: "runner-instance", Generation: "pinned-generation", Outcome: "failed", CustodyState: "no_payload",
	}}
	svc, db, itemID := autoSetup(t, []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-WRONG-ATTEMPT", 20)}, client)
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("clients=%+v err=%v", clients, err)
	}
	op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
		ID: "runner-admission-token-mismatch", RequestID: "runner-admission-token-mismatch", Fingerprint: "runner-admission-token-mismatch-fingerprint",
		State: "searching", Intent: "replace_bad", Scope: "download", MediaItemID: itemID,
		ReasonCode: "bad_media", Reason: "mismatched Runner receipt must remain protected", CreatedAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{
		MediaItemID: itemID, ClientID: clients[0].ID, State: "failed", ReplacementOperationID: op.ID,
		DeliveryExpected: true, CleanupPending: true, Transfer: "transfer-pinned", Handle: "runner-job-pinned",
		RunnerInstanceID: "runner-instance", RunnerAdmissionGeneration: "pinned-generation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetDownloadHandle(ctx, dl.ID, dl.Handle, "transfer-pinned"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET runner_instance_id=?,runner_admission_generation=? WHERE id=? AND generation_key=?`,
		"runner-instance", "pinned-generation", dl.ID, dl.GenerationKey); err != nil {
		t.Fatal(err)
	}

	if err := svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	blocked, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || blocked.State != "cleanup_blocked" || !strings.Contains(blocked.LastError, "job handle changed") {
		t.Fatalf("mismatched request receipt did not fail closed: operation=%+v err=%v", blocked, err)
	}
	if len(client.added) != 0 {
		t.Fatalf("worker searched/admitted a different request's replacement: %v", client.added)
	}
}
