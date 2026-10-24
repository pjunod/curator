package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestDeliveryCleanupRejectsReceiptTupleChangeBeforeUnlink(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted payload: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, epRelease+".mkv")
	if err != nil || row.Disposition != "placed" || row.FileGeneration == "" {
		t.Fatalf("accepted file outcome=%+v err=%v", row, err)
	}
	payload := filepath.Join(publication, filepath.FromSlash(row.Path))
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET request_token='replaced-transfer' WHERE id=?`, receiptID); err != nil {
		t.Fatal(err)
	}
	if err := svc.removeVerifiedDeliveryFile(ctx, r, row); err == nil {
		t.Fatal("cleanup accepted a stale receipt tuple")
	}
	if _, err := os.Stat(payload); err != nil {
		t.Fatalf("stale receipt cleanup removed the accepted payload: %v", err)
	}
}

func TestEmptyDeliveryDirectoryCleanupPreservesUnrelatedFilesAndSymlinkTargets(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted payload: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.removeVerifiedDeliveryFile(ctx, r, mustDeliveryOutcome(t, db, receiptID, epRelease+".mkv")); err != nil {
		t.Fatalf("remove the now-resolved media file: %v", err)
	}
	keepDir := filepath.Join(publication, "unrelated", "nested")
	if err := os.MkdirAll(keepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(keepDir, "keep.txt")
	if err := os.WriteFile(keep, []byte("unowned"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(publication, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("create test symlink: %v", err)
	}
	removed, err := svc.removeEmptyDeliveryDirectories(ctx, r)
	if err != nil {
		t.Fatalf("inspect protected publication: %v", err)
	}
	if removed {
		t.Fatal("publication containing unrelated data was removed")
	}
	if got, err := os.ReadFile(keep); err != nil || string(got) != "unowned" {
		t.Fatalf("unrelated publication file changed: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(outsideFile); err != nil || string(got) != "outside" {
		t.Fatalf("symlink target was followed or modified: %q err=%v", got, err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("publication symlink was removed or followed: info=%v err=%v", info, err)
	}
}

func TestAnchoredDeliveryPathReadersRejectTraversalAndSymlinkComponents(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "safe"), 0o755); err != nil {
		t.Fatal(err)
	}
	regular := filepath.Join(rootPath, "safe", "video.mkv")
	if err := os.WriteFile(regular, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.mkv"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "jump")); err != nil {
		t.Fatalf("create test symlink: %v", err)
	}
	base, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = base.Close() }()

	for _, rel := range []string{"../outside", filepath.Join("jump", ".")} {
		if got, err := openDeliveryDirectoryNoSymlinks(base, rel); err == nil {
			_ = got.Close()
			t.Errorf("directory path %q unexpectedly opened", rel)
		}
	}
	parent, file, named, err := openDeliveryFileNoSymlinks(base, filepath.Join("safe", "video.mkv"))
	if err != nil {
		t.Fatalf("valid anchored file did not open: %v", err)
	}
	if !named.Mode().IsRegular() {
		t.Errorf("anchored file metadata mode=%v", named.Mode())
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := parent.Close(); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		filepath.Join("jump", "outside.mkv"),
		filepath.Join("safe", "missing.mkv"),
		"safe",
	} {
		parent, file, _, err := openDeliveryFileNoSymlinks(base, rel)
		if err == nil {
			_ = file.Close()
			_ = parent.Close()
			t.Errorf("unsafe/non-file path %q unexpectedly opened", rel)
		}
	}
}

func TestDeliveryFileCleanupRefusesSymlinkSubstitution(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted payload: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	row := mustDeliveryOutcome(t, db, receiptID, epRelease+".mkv")
	source := filepath.Join(publication, filepath.FromSlash(row.Path))
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	if err := os.WriteFile(outside, []byte("preserve outside bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, source); err != nil {
		t.Fatalf("create test symlink: %v", err)
	}
	if err := svc.removeVerifiedDeliveryFile(ctx, r, row); err == nil {
		t.Fatal("cleanup followed a substituted payload symlink")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "preserve outside bytes" {
		t.Fatalf("symlink target changed: %q err=%v", got, err)
	}
	if info, err := os.Lstat(source); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("substituted symlink was removed: info=%v err=%v", info, err)
	}
}

func TestRedundantDeliveryCleanupRetainsPayloadWhenLibraryProofDisappears(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("import accepted payload: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	row := mustDeliveryOutcome(t, db, receiptID, epRelease+".mkv")
	files, err := db.ListFilesForItem(ctx, dl.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("imported library file rows=%+v err=%v", files, err)
	}
	if err := db.UpdateDeliveryFileDisposition(ctx, receiptID, row.Path, "redundant", "same-copy proof under test", files[0].Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(files[0].Path); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(publication, filepath.FromSlash(row.Path))
	if err := svc.removeVerifiedDeliveryFile(ctx, r, mustDeliveryOutcome(t, db, receiptID, row.Path)); err == nil {
		t.Fatal("redundant cleanup proceeded after its library copy disappeared")
	}
	if _, err := os.Stat(payload); err != nil {
		t.Fatalf("payload was discarded after redundancy proof disappeared: %v", err)
	}
}

func TestDeliveryCleanupDueQuerySkipsProtectedBackoffRowsWithoutStarvingLaterReceipt(t *testing.T) {
	ctx := context.Background()
	_, db, dl, cfg, _ := deliveryImportFixture(t, &deliveryImportClient{})
	root := t.TempDir()
	const delayed = 20
	for i := 0; i < delayed; i++ {
		delivery := fairnessFixtureDelivery(i + 1)
		mapped := filepath.Join(root, "delayed-"+strconv.Itoa(i))
		receipt, err := db.SaveDiscoveredDelivery(ctx, cfg.ID, delivery, mapped)
		if err != nil {
			t.Fatalf("save delayed receipt %d: %v", i, err)
		}
		if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='imported',directory_cleanup_state='retry',directory_cleanup_next_attempt_at=? WHERE id=?`, time.Now().Add(time.Hour).UnixMilli(), receipt.ID); err != nil {
			t.Fatal(err)
		}
	}
	dueDelivery := fairnessFixtureDelivery(delayed + 1)
	due, err := db.SaveDiscoveredDelivery(ctx, cfg.ID, dueDelivery, filepath.Join(root, "due"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='imported',directory_cleanup_state='retry',directory_cleanup_next_attempt_at=0 WHERE id=?`, due.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := db.DeliveryFileCleanupReceipts(ctx, cfg.ID, 16)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id != due.ID {
			t.Fatalf("not-yet-due protected receipt %d entered cleanup work set %v", id, ids)
		}
	}
	if len(ids) != 1 || ids[0] != due.ID {
		t.Fatalf("later due receipt was starved behind protected retries: got %v, want [%d] (download %d)", ids, due.ID, dl.ID)
	}
}

func fairnessFixtureDelivery(sequence int) ports.RunnerDelivery {
	delivery := ports.RunnerDelivery{
		Sequence: uint64(sequence), DeliveryID: "fairness-" + strconv.Itoa(sequence),
		PublicationGeneration: "publication-" + strconv.Itoa(sequence), ArtifactID: "artifact-" + strconv.Itoa(sequence),
		JobID: strconv.Itoa(sequence), RequestToken: "transfer-" + strconv.Itoa(sequence), RunnerInstanceID: "runner-1",
		PublicationPath: "/remote/completed/fairness-" + strconv.Itoa(sequence),
		Files:           []ports.RunnerFile{{Path: "episode.mkv", SizeBytes: 5, SHA256: sha256Hex([]byte("video"))}},
	}
	delivery.ManifestDigest, _ = runnerManifestDigest(delivery.Files)
	return delivery
}

func mustDeliveryOutcome(t *testing.T, db *sqlite.DB, receiptID int64, path string) sqlite.DeliveryFileOutcome {
	t.Helper()
	row, err := db.DeliveryFileOutcomeByPath(context.Background(), receiptID, path)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

var _ ports.DeliveryProtocol = (*deliveryImportClient)(nil)
