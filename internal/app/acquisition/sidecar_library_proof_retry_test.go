package acquisition

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestAcceptedSidecarRequiresCurrentLibraryMediaOwnership(t *testing.T) {
	caption := []byte("subtitle requires the exact media placement proof")
	ctx, svc, db, _, _, receipt, media, sidecar, _ := prepareSidecarMedia(t, caption)
	target := acceptedSidecarTarget(t, media.Path, media.SourcePath, sidecar.relative)
	stage := expectedCaptionStagePath(target)
	if err := os.WriteFile(stage, caption, 0o600); err != nil {
		t.Fatal(err)
	}
	mediaRows, err := db.ListFilesForItem(ctx, receipt.MediaItemID)
	if err != nil || len(mediaRows) != 1 {
		t.Fatalf("library media rows=%+v err=%v", mediaRows, err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE media_files SET path=? WHERE id=?`, mediaRows[0].Path+".moved", mediaRows[0].ID); err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Lstat(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	mediaInfo, err := os.Lstat(media.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.publishAcceptedSidecar(ctx, receipt, sidecar, media.Path, target, stage, parentInfo, mediaInfo); err == nil {
		t.Fatal("sidecar published after the exact media path lost current library ownership")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("stale media proof published a sidecar target: %v", err)
	}
	if got, err := os.ReadFile(stage); err != nil || string(got) != string(caption) {
		t.Fatalf("rejected sidecar stage was not preserved for retry: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(sidecar.absolute); err != nil || string(got) != string(caption) {
		t.Fatalf("rejected sidecar publication changed accepted source: %q err=%v", got, err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE media_files SET path=? WHERE id=?`, mediaRows[0].Path, mediaRows[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.publishAcceptedSidecar(ctx, receipt, sidecar, media.Path, target, stage, parentInfo, mediaInfo); err != nil {
		t.Fatalf("exact media proof restoration did not release the staged sidecar: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(caption) {
		t.Fatalf("restored media proof did not publish accepted caption: %q err=%v", got, err)
	}
}

func TestSidecarCopyAfterRestartRejectsSameBytesFromReplacedArchiveObject(t *testing.T) {
	caption := []byte("same manifest bytes from a different archive object")
	ctx, _, db, _, _, receipt, media, sidecar, _ := prepareSidecarMedia(t, caption)
	target := acceptedSidecarTarget(t, media.Path, media.SourcePath, sidecar.relative)
	archived := sidecar.absolute + ".held-accepted-object"
	if err := os.Rename(sidecar.absolute, archived); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.Remove(sidecar.absolute)
		_ = os.Rename(archived, sidecar.absolute)
	}()
	if err := os.WriteFile(sidecar.absolute, caption, 0o600); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(archived)
	if err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := os.Stat(sidecar.absolute)
	if err != nil || os.SameFile(originalInfo, replacementInfo) {
		t.Fatalf("fixture did not replace the archived object: original=%v replacement=%v err=%v", originalInfo, replacementInfo, err)
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
	persisted, err := reopened.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = restarted.copyAcceptedSidecar(ctx, persisted, sidecar, media.Path, target)
	if err == nil {
		t.Fatal("restart copied same-digest bytes from a different archived file generation")
	}
	if got, err := os.ReadFile(sidecar.absolute); err != nil || string(got) != string(caption) {
		t.Fatalf("rejected archive replacement bytes changed: %q err=%v", got, err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("stale archive generation gained library sidecar ownership: %v", err)
	}
	if _, err := os.Lstat(expectedCaptionStagePath(target)); !os.IsNotExist(err) {
		t.Fatalf("stale archive source created a publishable stage: %v", err)
	}
}
