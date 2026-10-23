package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type cancelOnceStageContext struct {
	context.Context
	stagePath string
}

func (c cancelOnceStageContext) Err() error {
	if _, err := os.Lstat(c.stagePath); err == nil {
		return context.Canceled
	}
	return c.Context.Err()
}

func TestAcceptedSidecarInterruptedCopyKeepsSourceAndRetriesStageAfterRestart(t *testing.T) {
	caption := []byte("accepted caption remains recoverable after cancellation")
	ctx, svc, db, _, _, receipt, media, sidecar, _ := prepareSidecarMedia(t, caption)
	target := acceptedSidecarTarget(t, media.Path, media.SourcePath, sidecar.relative)
	stage := expectedCaptionStagePath(target)

	copyCtx := cancelOnceStageContext{Context: ctx, stagePath: stage}
	err := svc.copyAcceptedSidecar(copyCtx, receipt, sidecar, media.Path, target)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation after stage creation was not propagated: %v", err)
	}
	stageInfo, err := os.Lstat(stage)
	if err != nil || !stageInfo.Mode().IsRegular() || stageInfo.Size() != 0 {
		t.Fatalf("interrupted stage was not left in a deterministic recoverable state: info=%v err=%v", stageInfo, err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("cancelled sidecar copy published a partial target: %v", err)
	}
	if got, err := os.ReadFile(sidecar.absolute); err != nil || string(got) != string(caption) {
		t.Fatalf("cancelled copy changed the accepted source: %q err=%v", got, err)
	}

	reopened := reopenLifecycleDB(t, db)
	restarted := New(reopened, nil, nil, nil, nil)
	receipt, err = reopened.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.copyAcceptedSidecar(ctx, receipt, sidecar, media.Path, target); err != nil {
		t.Fatalf("retry did not discard the incomplete stage and publish accepted bytes: %v", err)
	}
	if _, err := os.Lstat(stage); !os.IsNotExist(err) {
		t.Fatalf("successful retry left a partial stage behind: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(caption) {
		t.Fatalf("retry did not publish the exact accepted caption: %q err=%v", got, err)
	}
}

func TestAcceptedSidecarNonemptyStageObstructionPreservesSourceForRetry(t *testing.T) {
	caption := []byte("accepted caption behind an obstructed stage path")
	ctx, svc, _, _, _, receipt, media, sidecar, _ := prepareSidecarMedia(t, caption)
	target := acceptedSidecarTarget(t, media.Path, media.SourcePath, sidecar.relative)
	stage := expectedCaptionStagePath(target)
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(stage, "retain.txt")
	if err := os.WriteFile(sentinel, []byte("unrelated obstruction"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.copyAcceptedSidecar(ctx, receipt, sidecar, media.Path, target); err == nil {
		t.Fatal("copy replaced a nonempty directory occupying the deterministic stage path")
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "unrelated obstruction" {
		t.Fatalf("copy error changed the blocking directory: %q err=%v", got, err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("copy error published through the blocked stage path: %v", err)
	}
	if got, err := os.ReadFile(sidecar.absolute); err != nil || string(got) != string(caption) {
		t.Fatalf("copy error changed the accepted source: %q err=%v", got, err)
	}
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stage); err != nil {
		t.Fatal(err)
	}
	if err := svc.copyAcceptedSidecar(ctx, receipt, sidecar, media.Path, target); err != nil {
		t.Fatalf("retry after removing the external obstruction failed: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(caption) {
		t.Fatalf("retry did not publish the accepted caption: %q err=%v", got, err)
	}
}

func TestEmptyDeliveryCleanupRejectsPublicationPathReplacedByRegularFile(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{
		"unrelated.keep": []byte("keeps the accepted directory reviewable"),
	})
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted media: %v", err)
	}
	receipt, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || receipt.PublicationDirectoryIdentity == "" {
		t.Fatalf("accepted directory identity was not recorded: receipt=%+v err=%v", receipt, err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, receipt); err != nil {
		t.Fatalf("cleanup should retain the unresolved sibling: %v", err)
	}
	held := publication + ".accepted-tree"
	if err := os.Rename(publication, held); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(held) }()
	if err := os.WriteFile(publication, []byte("replacement file at directory path"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(publication) }()
	removed, err := svc.removeEmptyDeliveryDirectories(ctx, receipt)
	if err == nil || removed {
		t.Fatalf("cleanup treated a regular-file replacement as the accepted directory: removed=%v err=%v", removed, err)
	}
	if got, err := os.ReadFile(publication); err != nil || string(got) != "replacement file at directory path" {
		t.Fatalf("same-path regular file was removed or modified: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(held, "unrelated.keep")); err != nil || string(got) != "keeps the accepted directory reviewable" {
		t.Fatalf("accepted directory moved away from its reviewed path was altered: %q err=%v", got, err)
	}
}
