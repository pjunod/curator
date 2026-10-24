package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestPlacedDeliveryCleanupRetainsSourceWhenCurrentLibraryProofChanges(t *testing.T) {
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
	row := mustDeliveryOutcome(t, db, receiptID, epRelease+".mkv")
	if row.Disposition != "placed" || row.LibraryPath == "" {
		t.Fatalf("fixture did not commit a placed library proof: %+v", row)
	}
	acceptedSource := filepath.Join(publication, filepath.FromSlash(row.Path))
	originalLibraryBytes, err := os.ReadFile(row.LibraryPath)
	if err != nil {
		t.Fatal(err)
	}
	oldLibraryPath := row.LibraryPath + ".reviewed-original"
	if err := os.Rename(row.LibraryPath, oldLibraryPath); err != nil {
		t.Fatal(err)
	}
	const replacementBytes = "different bytes now occupy the indexed library path"
	if err := os.WriteFile(row.LibraryPath, []byte(replacementBytes), 0o644); err != nil {
		t.Fatal(err)
	}

	// A still-present DB row is not enough: the actual library file generation
	// must continue to match the committed import ledger before source cleanup.
	if err := svc.cleanupDeliveryFiles(ctx, receipt); err != nil {
		t.Fatalf("cleanup should persist a retry for changed library proof: %v", err)
	}
	if got, err := os.ReadFile(row.LibraryPath); err != nil || string(got) != replacementBytes {
		t.Fatalf("cleanup changed the replacement library bytes: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(oldLibraryPath); err != nil || string(got) != string(originalLibraryBytes) {
		t.Fatalf("original library evidence changed during proof change: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(acceptedSource); err != nil || string(got) != "video" {
		t.Fatalf("accepted source bytes changed after its library proof changed: %q err=%v", got, err)
	}
	updated, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, row.Path)
	if err != nil || updated.CleanupState != "retry" || !strings.Contains(updated.CleanupError, "library placement is no longer present") {
		t.Fatalf("changed library proof did not remain durably retryable: outcome=%+v err=%v", updated, err)
	}
}

func TestExplicitDeleteReplayRejectsChangedNestedParentThenRetriesExactJournal(t *testing.T) {
	ctx := context.Background()
	const relative = "nested/extra.mkv"
	const originalBytes = "reviewed nested manifest member"
	svc, db, receipt, publication := reviewedNestedDeliveryDeleteFixture(t, relative, []byte(originalBytes))
	entry := completedEntryForPath(t, svc, publication)
	selection := completedSelectionForPath(t, entry, filepath.Join(publication, filepath.FromSlash(relative)))
	fingerprint := entry.Fingerprint
	defaultSource := filepath.Join(publication, epRelease+".mkv")
	defaultBytes, err := os.ReadFile(defaultSource)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_nested_delete_history
		BEFORE INSERT ON history_events WHEN NEW.type='delivery_file_deleted'
		BEGIN SELECT RAISE(ABORT,'nested delete history unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, []CompletedFileSelection{selection}); err == nil || !strings.Contains(err.Error(), "nested delete history unavailable") {
		t.Fatalf("did not reach post-unlink completion transaction failure: %v", err)
	}
	selectedPath := selection.Path
	if _, err := os.Lstat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("fixture did not unlink selected member before DB completion failure: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_nested_delete_history`); err != nil {
		t.Fatal(err)
	}
	batchID, err := explicitDeliveryDeleteBatchID(publication, fingerprint, []CompletedFileSelection{selection})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := db.ExplicitDeliveryDeleteBatch(ctx, batchID)
	if err != nil || len(batch.Files) != 1 || batch.Files[0].HistoryRecorded {
		t.Fatalf("failed completion lost its pending exact batch: batch=%+v err=%v", batch, err)
	}
	outcome, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, relative)
	if err != nil || outcome.CleanupState != "retry" || !strings.Contains(outcome.CleanupError, "nested delete history unavailable") {
		t.Fatalf("failed transaction did not preserve a durable retry outcome: outcome=%+v err=%v", outcome, err)
	}

	// Reuse the lexical parent path with a new directory object. The selected
	// file is absent there, but the stale journal cannot borrow that new
	// directory's identity to finish the old unlink.
	parent := filepath.Join(publication, "nested")
	heldParent := filepath.Join(publication, "nested.reviewed-parent")
	if err := os.Rename(parent, heldParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(parent, "unrelated.keep")
	if err := os.WriteFile(sentinel, []byte("replacement parent must survive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, []CompletedFileSelection{selection}); err == nil || !strings.Contains(err.Error(), "reviewed parent") {
		t.Fatalf("replay accepted a different nested parent identity: %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "replacement parent must survive" {
		t.Fatalf("refused replay changed replacement parent bytes: %q err=%v", got, err)
	}
	if entries, err := os.ReadDir(heldParent); err != nil || len(entries) != 0 {
		t.Fatalf("original reviewed parent changed after unlink: entries=%v err=%v", entries, err)
	}
	batch, err = db.ExplicitDeliveryDeleteBatch(ctx, batchID)
	if err != nil || len(batch.Files) != 1 || batch.Files[0].HistoryRecorded {
		t.Fatalf("parent mismatch changed the pending batch: batch=%+v err=%v", batch, err)
	}

	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(heldParent, parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, []CompletedFileSelection{selection}); err != nil {
		t.Fatalf("exact journal did not retry after the reviewed parent was restored: %v", err)
	}
	completed, err := db.ExplicitDeliveryDeleteBatch(ctx, batchID)
	if err != nil || len(completed.Files) != 1 || !completed.Files[0].HistoryRecorded {
		t.Fatalf("exact retry did not durably complete the selected journal: batch=%+v err=%v", completed, err)
	}
	var historyCount int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM history_events WHERE type='delivery_file_deleted'`).Scan(&historyCount); err != nil || historyCount != 1 {
		t.Fatalf("exact retry should write one deletion history event: count=%d err=%v", historyCount, err)
	}
	if _, err := os.Lstat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("selected source remains after exact retry: %v", err)
	}
	if got, err := os.ReadFile(defaultSource); err != nil || string(got) != string(defaultBytes) {
		t.Fatalf("unselected manifest sibling changed during retry: %q err=%v", got, err)
	}
}

func reviewedNestedDeliveryDeleteFixture(t *testing.T, relative string, content []byte) (*Service, *sqlite.DB, sqlite.DeliveryReceipt, string) {
	t.Helper()
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	ordinary := &fakeClient{}
	svc, db, itemID := setup(t, nil, ordinary)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return protocol }
	client, err := db.GetDownloadClient(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	client.PathMappings = []ports.PathMapping{{Remote: "/remote/completed", Local: root}}
	if err := db.UpdateDownloadClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	publication := filepath.Join(root, epRelease)
	if err := os.MkdirAll(filepath.Join(publication, filepath.Dir(relative)), 0o755); err != nil {
		t.Fatal(err)
	}
	mainContent := []byte("video")
	mainPath := epRelease + ".mkv"
	if err := os.WriteFile(filepath.Join(publication, mainPath), mainContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(publication, filepath.FromSlash(relative)), content, 0o644); err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: itemID, WantableIDs: []string{"episode:" + itoa(itemID) + ":1:1"}, ReleaseTitle: epRelease, Indexer: "idx", Protocol: "usenet", ClientID: client.ID, Handle: "17", Transfer: "t-17", State: "downloaded", DeliveryExpected: true})
	if err != nil {
		t.Fatal(err)
	}
	files := []ports.RunnerFile{
		{Path: mainPath, SizeBytes: int64(len(mainContent)), SHA256: sha256Hex(mainContent)},
		{Path: filepath.ToSlash(relative), SizeBytes: int64(len(content)), SHA256: sha256Hex(content)},
	}
	delivery := ports.RunnerDelivery{Sequence: 1, DeliveryID: "delivery-1", PublicationGeneration: "pub-1", ArtifactID: "artifact-1", JobID: "17", RequestToken: "t-17", RunnerInstanceID: "runner-1", PublicationPath: "/remote/completed/" + epRelease, Files: files}
	delivery.ManifestDigest, err = runnerManifestDigest(delivery.Files)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := db.SaveDiscoveredDelivery(ctx, client.ID, delivery, publication)
	if err != nil {
		t.Fatal(err)
	}
	receipt.DownloadID, receipt.GenerationKey, receipt.DownloadHandle = dl.ID, dl.GenerationKey, dl.Handle
	receipt.MediaItemID, receipt.CopyID = dl.MediaItemID, dl.CopyID
	if err := db.AssociateDelivery(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	protocol.lookup = delivery
	if err := svc.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.verifyDeliveryManifest(ctx, client, receipt); err != nil {
		t.Fatalf("verify nested manifest: %v", err)
	}
	receipt.ConsumerReceiptID = "selected-delete-nested-parent"
	receipt.NextAttemptAt = time.Now()
	if err := db.PrepareDeliveryAcceptance(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	dl.ImportPath, dl.State = publication, "downloaded"
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	return svc, db, receipt, publication
}
