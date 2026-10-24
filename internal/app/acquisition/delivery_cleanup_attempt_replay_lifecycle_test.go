package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/ports"
)

func TestDeliveryCleanupAttemptFaultsPreserveAndReplayObligation(t *testing.T) {
	for _, scenario := range []string{"attempt_insert_failed", "attempt_finish_failed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
			receiptID := receiptIDFor(t, db, dl.ID)
			if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
				t.Fatalf("import accepted payload: %v", err)
			}
			receipt, err := db.DeliveryReceiptByID(ctx, receiptID)
			if err != nil {
				t.Fatal(err)
			}
			payload := filepath.Join(publication, epRelease+".mkv")
			trigger := `CREATE TRIGGER fail_cleanup_attempt BEFORE INSERT ON runner_delivery_attempt_events WHEN NEW.stage LIKE 'cleanup_file:%' BEGIN SELECT RAISE(ABORT,'simulated cleanup attempt insert outage'); END`
			wantError := "cleanup attempt insert outage"
			if scenario == "attempt_finish_failed" {
				trigger = `CREATE TRIGGER fail_cleanup_attempt BEFORE UPDATE OF finished_at ON runner_delivery_attempt_events WHEN NEW.result='succeeded' AND NEW.stage LIKE 'cleanup_file:%' BEGIN SELECT RAISE(ABORT,'simulated cleanup attempt finish outage'); END`
				wantError = "cleanup attempt finish outage"
			}
			if _, err := db.W.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			if err := svc.cleanupDeliveryFiles(ctx, receipt); err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("fault did not interrupt the cleanup attempt at its intended boundary: %v", err)
			}
			row, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, epRelease+".mkv")
			if err != nil || row.CleanupState == "removed" {
				t.Fatalf("database fault lost the pending cleanup obligation: row=%+v err=%v", row, err)
			}
			if scenario == "attempt_insert_failed" {
				if _, err := os.Stat(payload); err != nil {
					t.Fatalf("failed attempt journaling happened after unlink: %v", err)
				}
			} else if _, err := os.Lstat(payload); !os.IsNotExist(err) {
				t.Fatalf("finish fault was not injected after authorized unlink: %v", err)
			}

			if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_cleanup_attempt`); err != nil {
				t.Fatal(err)
			}
			reopened := reopenLifecycleDB(t, db)
			restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return &deliveryImportClient{} })
			receipt, err = reopened.DeliveryReceiptByID(ctx, receiptID)
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.cleanupDeliveryFiles(ctx, receipt); err != nil {
				t.Fatalf("restart did not replay the cleanup obligation: %v", err)
			}
			row, err = reopened.DeliveryFileOutcomeByPath(ctx, receiptID, epRelease+".mkv")
			if err != nil || row.CleanupState != "removed" {
				t.Fatalf("replayed cleanup did not settle its ledger: row=%+v err=%v", row, err)
			}
			if _, err := os.Lstat(publication); !os.IsNotExist(err) {
				t.Fatalf("replayed directory cleanup did not remove the empty accepted tree: %v", err)
			}
		})
	}
}
