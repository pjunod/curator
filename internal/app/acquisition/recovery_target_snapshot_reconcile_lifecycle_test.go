package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestQueuedRecoveryHoldsWhenAnExistingLibraryFileChangesOnDisk(t *testing.T) {
	for _, scenario := range []string{"existing_file_changed", "existing_file_removed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			svc, db, request, receipts := recoveryFixture(t)
			item, err := db.GetMediaItemFull(ctx, request.MediaItemID)
			if err != nil {
				t.Fatal(err)
			}
			existingPath := filepath.Join(item.Path, "existing-library-file.mkv")
			existing := []byte("library bytes captured by recovery preview")
			if err := os.WriteFile(existingPath, existing, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := db.UpsertFile(ctx, request.MediaItemID, 0, existingPath, int64(len(existing))); err != nil {
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
			switch scenario {
			case "existing_file_changed":
				if err := os.WriteFile(existingPath, []byte("outside edit after accepted preview"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "existing_file_removed":
				if err := os.Remove(existingPath); err != nil {
					t.Fatal(err)
				}
			}

			svc.recoverySweep(ctx)
			held, err := svc.RecoveryImport(ctx, job.ID)
			if err != nil || held.State != "review" || !strings.Contains(held.Error, "library files changed") {
				t.Fatalf("worker did not hold recovery after an existing library-file change: import=%+v err=%v", held, err)
			}
			files, err := db.ListFilesForItem(ctx, request.MediaItemID)
			if err != nil || len(files) != 1 || files[0].Path != existingPath {
				t.Fatalf("held recovery changed library ownership: files=%+v err=%v", files, err)
			}
			if receipts.Load() != 0 {
				t.Fatal("held recovery acknowledged the Runner publication")
			}
			settings, err := svc.RecoverySettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(settings.LocalRoot, "abc123", "payload", preview.Files[0].Path)
			if got, err := os.ReadFile(source); err != nil || string(got) != string(corpusFile(t, wholeCorpus1080)) {
				t.Fatalf("review hold changed accepted Runner source: bytes=%d err=%v", len(got), err)
			}
		})
	}
}

func TestPlacementRestartDoesNotCommitPublishedRecoveryAfterTargetAuthorityChanges(t *testing.T) {
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
	settings, err := svc.RecoverySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(settings.LocalRoot, "abc123", "payload", preview.Files[0].Path)
	payload, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	target := preview.Files[0].Destination
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	p := sqlite.Placement{
		ID: "recovery-target-changed-after-publish", Source: source, Target: target,
		Temporary: filepath.Join(filepath.Dir(target), ".monarr-stage-recovery-authority"),
		Backup:    filepath.Join(filepath.Dir(target), ".monarr-prior-recovery-authority"),
		SHA256:    preview.Files[0].SHA256, Size: int64(len(payload)), ItemID: request.MediaItemID,
		RecoveryImport: job.ID, RecoveryFile: preview.Files[0].ID, State: "prepared",
	}
	if err := db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE media_items SET title='Changed after recovery publication' WHERE id=?`, request.MediaItemID); err != nil {
		t.Fatal(err)
	}

	reopened := reopenLifecycleDB(t, db)
	restarted := New(reopened, nil, nil, nil, nil)
	restarted.reconcilePlacements(ctx)
	pending, err := reopened.PendingPlacements(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != p.ID || pending[0].State != "prepared" {
		t.Fatalf("restart reconciled a stale recovery placement: pending=%+v err=%v", pending, err)
	}
	files, err := reopened.ListFilesForItem(ctx, request.MediaItemID)
	if err != nil || len(files) != 0 {
		t.Fatalf("stale published bytes gained library ownership: files=%+v err=%v", files, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(payload) {
		t.Fatalf("restart destroyed already-published bytes while authority was stale: bytes=%d err=%v", len(got), err)
	}
	if receipts.Load() != 0 {
		t.Fatal("restart acknowledged a recovery that failed its accepted-target check")
	}
}
