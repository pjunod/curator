package acquisition

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type downloadClientWithoutAdmission struct{ ports.DownloadClient }

func TestReplacementAttemptCleanupGateFailsClosedBeforeSearching(t *testing.T) {
	for _, scenario := range []string{"missing_transfer_token", "legacy_client_without_admission", "admission_binding_write_failed"} {
		t.Run(scenario, func(t *testing.T) {
			client := &fakeClient{}
			svc, db, itemID := autoSetup(t, []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-DO-NOT-GRAB", 20)}, client)
			ctx := context.Background()
			clients, err := db.ListDownloadClients(ctx)
			if err != nil || len(clients) == 0 {
				t.Fatalf("download clients=%+v err=%v", clients, err)
			}
			op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
				ID: "cleanup-gate-" + scenario, RequestID: "cleanup-gate-" + scenario,
				Fingerprint: "cleanup-gate-fingerprint-" + scenario, State: "searching", Intent: "replace_bad",
				Scope: "download", MediaItemID: itemID, ReasonCode: "bad_media", Reason: "Runner cleanup requires exact authority", CreatedAt: time.Now(),
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			transfer := ""
			if scenario != "missing_transfer_token" {
				transfer = "transfer-" + scenario
			}
			dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{
				MediaItemID: itemID, ClientID: clients[0].ID, State: "failed", ReplacementOperationID: op.ID,
				DeliveryExpected: true, CleanupPending: true, Transfer: transfer, Handle: "runner-job-" + scenario,
			})
			if err != nil {
				t.Fatal(err)
			}
			if transfer != "" {
				if err := db.SetDownloadHandle(ctx, dl.ID, dl.Handle, transfer); err != nil {
					t.Fatal(err)
				}
			}
			want := "no exact Runner transfer token"
			switch scenario {
			case "legacy_client_without_admission":
				want = "no authoritative Runner admission endpoint"
				svc.newClient = func(ports.ClientConfig) ports.DownloadClient {
					return downloadClientWithoutAdmission{DownloadClient: client}
				}
			case "admission_binding_write_failed":
				want = "Runner admission identity changed"
				client.admissionFound = true
				client.admission = ports.RequestAdmission{
					ProtocolVersion: 1, RequestToken: transfer, JobID: dl.Handle,
					RunnerInstanceID: "runner-instance", Generation: "generation-1",
					Outcome: "failed", TerminalOutcome: "FAILURE/FETCH", CustodyState: "no_payload",
				}
				if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_admission_binding BEFORE UPDATE OF runner_instance_id ON downloads WHEN NEW.id=`+sqliteInt(dl.ID)+` BEGIN SELECT RAISE(ABORT,'simulated admission binding outage'); END`); err != nil {
					t.Fatal(err)
				}
			}

			if err := svc.processReplacementOperation(ctx, op); err != nil {
				t.Fatalf("worker should persist the authority hold: %v", err)
			}
			blocked, err := db.GetReplacementOperation(ctx, op.ID)
			if err != nil || blocked.State != "cleanup_blocked" || !strings.Contains(blocked.LastError, want) {
				t.Fatalf("worker did not persist the required cleanup authority hold: operation=%+v err=%v", blocked, err)
			}
			if len(client.added) != 0 {
				t.Fatalf("worker searched or admitted without authoritative cleanup state: %v", client.added)
			}
		})
	}
}

func sqliteInt(value int64) string { return fmt.Sprintf("%d", value) }
