package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAcceptedSidecarPublicationRejectsMediaReplacementDuringStaging(t *testing.T) {
	caption := []byte("subtitle from the accepted publication")
	ctx, svc, _, _, _, receipt, media, sidecar, _ := prepareSidecarMedia(t, caption)
	target := acceptedSidecarTarget(t, media.Path, media.SourcePath, sidecar.relative)
	stage := expectedCaptionStagePath(target)
	backup := media.Path + ".reviewed-original"

	// Hold the publication lock after copyAcceptedSidecar has captured the
	// media identity but before its final publish check. The source is staged
	// outside that lock, so this models a same-path library replacement in the
	// expensive-copy window.
	svc.importTargetMu.Lock()
	result := make(chan error, 1)
	locked := true
	finished := false
	defer func() {
		if locked {
			svc.importTargetMu.Unlock()
		}
		if !finished {
			<-result
		}
	}()
	go func() { result <- svc.copyAcceptedSidecar(ctx, receipt, sidecar, media.Path, target) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		digest, size, err := fileDigest(ctx, stage)
		if err == nil && digest == sidecar.digest && size == sidecar.size {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("verified caption stage did not become durable: digest=%q size=%d err=%v", digest, size, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.Rename(media.Path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(media.Path, []byte("replacement media"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backup); err != nil {
		t.Fatal(err)
	}
	svc.importTargetMu.Unlock()
	locked = false
	publishErr := <-result
	finished = true
	if publishErr == nil || !strings.Contains(publishErr.Error(), "associated library media changed") {
		t.Fatalf("sidecar publication accepted a replacement at the reviewed media path: %v", publishErr)
	}
	if got, err := os.ReadFile(media.Path); err != nil || string(got) != "replacement media" {
		t.Fatalf("replacement media was not preserved: %q err=%v", got, err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("caption was published beside unverified media: %v", err)
	}
	if digest, size, err := fileDigest(ctx, stage); err != nil || digest != sidecar.digest || size != sidecar.size {
		t.Fatalf("rejected publication did not preserve its exact stage for review/retry: digest=%q size=%d err=%v", digest, size, err)
	}
}

func TestExplicitDeliveryDeleteOverridesKeepOnlyForReviewedManifestSelection(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	sibling := "notes.unselected.bin"
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{sibling: []byte("kept sibling")})
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	receipt := receipts[0]
	if _, err = svc.verifyDeliveryManifest(ctx, cfg, receipt); err != nil {
		t.Fatalf("verify accepted manifest: %v", err)
	}
	// Keep is the Runner's explicit instruction to retain the payload. The
	// only permitted override is a fresh user-reviewed selection, which must
	// remain limited to the exact manifest member selected below.
	if _, err = db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET keep_source_on_failure=1 WHERE id=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	receipt, err = db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil || !receipt.Delivery.KeepSourceOnFailure {
		t.Fatalf("fixture did not persist Keep custody: %+v err=%v", receipt.Delivery, err)
	}
	receipt.ConsumerReceiptID = "keep-explicit-delete-review"
	receipt.NextAttemptAt = time.Now()
	if err := db.PrepareDeliveryAcceptance(ctx, receipt); err != nil {
		t.Fatalf("prepare local acceptance: %v", err)
	}
	dl.ImportPath, dl.State = publication, "downloaded"
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatalf("persist mapped completion association: %v", err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	inventory, err := svc.CompletedInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var entry *CompletedEntry
	for i := range inventory.Roots {
		for j := range inventory.Roots[i].Entries {
			if canonicalCompletedPath(inventory.Roots[i].Entries[j].Path) == canonicalCompletedPath(publication) {
				entry = &inventory.Roots[i].Entries[j]
			}
		}
	}
	if entry == nil || !entry.DeliveryManaged {
		t.Fatalf("kept delivery missing from explicit review inventory: %+v", inventory)
	}
	selectedPath := filepath.Join(publication, epRelease+".mkv")
	var selected CompletedFileSelection
	for _, file := range entry.ReviewFiles {
		if canonicalCompletedPath(file.Path) == canonicalCompletedPath(selectedPath) {
			selected = CompletedFileSelection{Path: file.Path, SnapshotIdentity: file.SnapshotIdentity}
		}
	}
	if selected.Path == "" {
		t.Fatalf("selected manifest member was absent from review: %+v", entry.ReviewFiles)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, entry.Path, entry.Fingerprint, []CompletedFileSelection{selected}); err != nil {
		t.Fatalf("explicit selection should override Keep for only its reviewed file: %v", err)
	}
	if _, err := os.Stat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("explicitly selected file still exists: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(publication, sibling)); err != nil || string(got) != "kept sibling" {
		t.Fatalf("Keep sibling was modified by selected deletion: %q err=%v", got, err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, epRelease+".mkv")
	if err != nil || row.CleanupState != "removed" || !strings.Contains(row.CleanupError, "operator explicitly selected") {
		t.Fatalf("selected Keep override lacks its durable audit: %+v err=%v", row, err)
	}
}
