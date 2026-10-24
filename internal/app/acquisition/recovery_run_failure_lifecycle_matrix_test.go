package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveryWorkerHoldsSourceWhenAdmissionOrPinnedInputChanges(t *testing.T) {
	for _, scenario := range []string{"runner_claim_rejected", "source_bytes_changed", "target_generation_changed"} {
		t.Run(scenario, func(t *testing.T) {
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
			switch scenario {
			case "runner_claim_rejected":
				settings, err := svc.RecoverySettings(ctx)
				if err != nil {
					t.Fatal(err)
				}
				settings.ConsumerToken = "incorrect-token"
				if err := svc.SetRecoverySettings(ctx, settings); err != nil {
					t.Fatal(err)
				}
			case "source_bytes_changed":
				root, err := svc.recoveryPayload(ctx, job.Recovery)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, job.Recovery.Files[0].Path)
				if err := os.WriteFile(path, []byte("same path, changed source generation"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "target_generation_changed":
				if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET title='Changed after preview' WHERE id=?`, request.MediaItemID); err != nil {
					t.Fatal(err)
				}
			}

			svc.recoverySweep(ctx)
			held, err := svc.RecoveryImport(ctx, job.ID)
			if err != nil || held.State != "review" || held.Error == "" {
				t.Fatalf("worker did not turn the changed admission/input into a durable review hold: import=%+v err=%v", held, err)
			}
			switch scenario {
			case "runner_claim_rejected":
				if !strings.Contains(held.Error, "401") {
					t.Fatalf("Runner claim failure was lost: %q", held.Error)
				}
			case "source_bytes_changed":
				if !strings.Contains(held.Error, "changed before import") {
					t.Fatalf("changed source bytes were not identified: %q", held.Error)
				}
			case "target_generation_changed":
				if !strings.Contains(held.Error, "library changed since preview") {
					t.Fatalf("changed local target generation was not identified: %q", held.Error)
				}
			}
			files, err := db.ListFilesForItem(ctx, request.MediaItemID)
			if err != nil || len(files) != 0 {
				t.Fatalf("held recovery created library ownership: files=%+v err=%v", files, err)
			}
			if receipts.Load() != 0 {
				t.Fatal("held recovery delivered a source release receipt")
			}
		})
	}
}
