package acquisition

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestBookPlacementCleanupPreservesChangedSupersededFileAndRetries(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := bookSetup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	oldID, oldPath := acq2AddFile(t, svc, itemID, 0, item.Path, "old-edition.pdf", "")
	oldFiles, err := db.ListFilesForItem(ctx, itemID)
	if err != nil || len(oldFiles) != 1 || oldFiles[0].ID != oldID {
		t.Fatalf("old edition row=%+v err=%v", oldFiles, err)
	}
	oldDigest, _, err := fileDigest(ctx, oldPath)
	if err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(t.TempDir(), "new-edition.epub")
	payload := []byte("replacement ebook edition")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(payload))
	target := filepath.Join(item.Path, "new-edition.epub")
	p := sqlite.Placement{
		ID: "superseded-cleanup-retry", Source: source, Target: target,
		Temporary: filepath.Join(item.Path, ".monarr-stage-superseded-cleanup-retry"),
		Backup:    filepath.Join(item.Path, ".monarr-prior-superseded-cleanup-retry"),
		SHA256:    digest, Size: int64(len(payload)), ItemID: itemID, State: "prepared",
		CleanupSuperseded: true, Superseded: oldFiles,
		SupersededDigests: map[string]string{oldPath: oldDigest},
	}
	if err := db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := svc.preparePlacementPublication(ctx, &p); err != nil {
		t.Fatalf("prepare replacement edition: %v", err)
	}
	decisionCtx, release := svc.lockStorageDecision(ctx)
	if err := svc.publishPlacement(decisionCtx, &p); err != nil {
		release()
		t.Fatalf("publish replacement edition: %v", err)
	}
	if _, err := db.CommitPlacement(decisionCtx, p); err != nil {
		release()
		t.Fatalf("commit replacement edition: %v", err)
	}
	release()

	if err := os.WriteFile(oldPath, []byte("changed after the replacement was reviewed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupPlacement(ctx, p); err == nil {
		t.Fatal("cleanup deleted a superseded book whose bytes changed after review")
	}
	if got, err := os.ReadFile(oldPath); err != nil || string(got) != "changed after the replacement was reviewed" {
		t.Fatalf("changed prior edition was not preserved: %q err=%v", got, err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 2 {
		t.Fatalf("failed cleanup hid prior library ownership: files=%+v err=%v", files, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(payload) {
		t.Fatalf("committed replacement edition was lost: %q err=%v", got, err)
	}

	// Once the old bytes are restored to the exact reviewed content, reconciliation
	// may retire the old file and row and mark the committed journal cleaned.
	if err := os.WriteFile(oldPath, []byte("bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupPlacement(ctx, p); err != nil {
		t.Fatalf("cleanup did not retry after restoring reviewed bytes: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("superseded prior edition remains after successful retry: %v", err)
	}
	files, err = db.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 1 || files[0].Path != target {
		t.Fatalf("successful cleanup left stale or missing library ownership: files=%+v err=%v", files, err)
	}
	var state string
	if err := db.R.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE id=?`, p.ID).Scan(&state); err != nil || state != "cleaned" {
		t.Fatalf("cleanup journal state=%q err=%v", state, err)
	}
}

func TestBookPlacementCleanupReplaysAfterOldFileUnlinkAndRowDeleteFailure(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := bookSetup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	oldID, oldPath := acq2AddFile(t, svc, itemID, 0, item.Path, "old-edition.pdf", "")
	oldFiles, err := db.ListFilesForItem(ctx, itemID)
	if err != nil || len(oldFiles) != 1 || oldFiles[0].ID != oldID {
		t.Fatalf("old edition row=%+v err=%v", oldFiles, err)
	}
	oldDigest, _, err := fileDigest(ctx, oldPath)
	if err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(t.TempDir(), "replacement-edition.epub")
	payload := []byte("replacement ebook for cleanup replay")
	if err := os.WriteFile(source, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(payload))
	target := filepath.Join(item.Path, "replacement-edition.epub")
	p := sqlite.Placement{
		ID: "superseded-cleanup-replay", Source: source, Target: target,
		Temporary: filepath.Join(item.Path, ".monarr-stage-superseded-cleanup-replay"),
		Backup:    filepath.Join(item.Path, ".monarr-prior-superseded-cleanup-replay"),
		SHA256:    digest, Size: int64(len(payload)), ItemID: itemID, State: "prepared",
		CleanupSuperseded: true, Superseded: oldFiles,
		SupersededDigests: map[string]string{oldPath: oldDigest},
	}
	if err := db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := svc.preparePlacementPublication(ctx, &p); err != nil {
		t.Fatalf("prepare replacement edition: %v", err)
	}
	decisionCtx, release := svc.lockStorageDecision(ctx)
	if err := svc.publishPlacement(decisionCtx, &p); err != nil {
		release()
		t.Fatalf("publish replacement edition: %v", err)
	}
	if _, err := db.CommitPlacement(decisionCtx, p); err != nil {
		release()
		t.Fatalf("commit replacement edition: %v", err)
	}
	release()

	// Model a crash after unlinking the superseded bytes but before retiring the
	// database row. A failed row delete must leave enough journal state to retry.
	if err := os.Remove(oldPath); err != nil {
		t.Fatal(err)
	}
	trigger := fmt.Sprintf("CREATE TRIGGER fail_superseded_row_delete BEFORE DELETE ON media_files WHEN OLD.id=%d BEGIN SELECT RAISE(ABORT,'injected superseded row delete failure'); END", oldID)
	if _, err := db.W.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupPlacement(ctx, p); err == nil {
		t.Fatal("cleanup hid a database failure after unlinking the superseded bytes")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("unlinked superseded file reappeared after database failure: %v", err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 2 {
		t.Fatalf("failed row retirement lost old/new ownership: files=%+v err=%v", files, err)
	}
	if _, err := db.W.ExecContext(ctx, "DROP TRIGGER fail_superseded_row_delete"); err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupPlacement(ctx, p); err != nil {
		t.Fatalf("cleanup did not replay after database recovered: %v", err)
	}
	files, err = db.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 1 || files[0].Path != target {
		t.Fatalf("replayed cleanup left stale or missing ownership: files=%+v err=%v", files, err)
	}
	var state string
	if err := db.R.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE id=?`, p.ID).Scan(&state); err != nil || state != "cleaned" {
		t.Fatalf("replayed cleanup journal state=%q err=%v", state, err)
	}
}
