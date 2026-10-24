package acquisition

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func nativeBookDeliveryFixture(t *testing.T, relative string, contents []byte, extra map[string][]byte) (*Service, *sqlite.DB, sqlite.Download, ports.ClientConfig, sqlite.DeliveryReceipt, string) {
	t.Helper()
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, itemID := bookSetup(t, nil, &fakeClient{})
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
	if err := svc.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	publication := filepath.Join(root, "native-book-edition")
	if err := os.MkdirAll(publication, 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(publication, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{
		MediaItemID: itemID, WantableIDs: []string{"book:" + itoa(itemID)},
		ReleaseTitle: "Andy Weir - Project Hail Mary EPUB", Indexer: "idx", Protocol: "torrent",
		ClientID: client.ID, Handle: "book-job-17", Transfer: "book-transfer-17", State: "downloaded", DeliveryExpected: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestFiles := []ports.RunnerFile{{Path: relative, SizeBytes: int64(len(contents)), SHA256: sha256Hex(contents)}}
	for name, data := range extra {
		path := filepath.Join(publication, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		manifestFiles = append(manifestFiles, ports.RunnerFile{Path: name, SizeBytes: int64(len(data)), SHA256: sha256Hex(data)})
	}
	delivery := ports.RunnerDelivery{
		Sequence: 1, DeliveryID: "native-book-delivery", PublicationGeneration: "book-pub-1",
		ArtifactID: "book-artifact-1", JobID: dl.Handle, RequestToken: dl.Transfer,
		RunnerInstanceID: "book-runner-1", PublicationPath: "/remote/completed/native-book-edition",
		Files: manifestFiles,
	}
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
	dl.ImportPath, dl.SavePath = publication, delivery.PublicationPath
	return svc, db, dl, client, receipt, publication
}

func addNativeBookFile(t *testing.T, db *sqlite.DB, itemID int64, name string, contents []byte, source quality.Source) string {
	t.Helper()
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, name)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, itemID, 0, path, int64(len(contents)))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQualityFrom(ctx, fileID, quality.Quality{Source: source}, mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNativeBookDeliveryRedundancyRequiresExactBytesAndNoUpgrade(t *testing.T) {
	t.Run("exact epub duplicate is durably redundant before cleanup", func(t *testing.T) {
		ctx := context.Background()
		payload := []byte("same EPUB edition bytes")
		unknownSidecar := []byte("operator-owned unclassified payload")
		svc, db, dl, cfg, receipt, publication := nativeBookDeliveryFixture(t, "download.epub", payload, map[string][]byte{"unclassified.dat": unknownSidecar})
		libraryPath := addNativeBookFile(t, db, dl.MediaItemID, "Project Hail Mary - Andy Weir.epub", payload, quality.SourceEPUB)
		if err := svc.runDeliveryImport(ctx, dl, cfg, receipt.ID); err != nil {
			t.Fatalf("complete exact duplicate delivery: %v", err)
		}
		storedReceipt, err := db.DeliveryReceiptByID(ctx, receipt.ID)
		if err != nil || storedReceipt.State != "imported" {
			t.Fatalf("exact duplicate receipt=%+v err=%v", storedReceipt, err)
		}
		freshDownload, err := db.GetDownload(ctx, dl.ID)
		if err != nil || len(freshDownload.Handoff) == 0 || !strings.Contains(freshDownload.Handoff[len(freshDownload.Handoff)-1].Detail, "all selected media already present as verified redundant files") {
			t.Fatalf("timeline did not describe the all-redundant completion: %+v err=%v", freshDownload.Handoff, err)
		}
		row, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, "download.epub")
		if err != nil || row.Disposition != "redundant" || row.CleanupState != "pending" || filepath.Clean(row.LibraryPath) != filepath.Clean(libraryPath) {
			t.Fatalf("exact EPUB duplicate outcome=%+v err=%v", row, err)
		}
		if err := svc.cleanupDeliveryFiles(ctx, storedReceipt); err != nil {
			t.Fatalf("clean exact duplicate payload: %v", err)
		}
		if _, err := os.Stat(filepath.Join(publication, "download.epub")); !os.IsNotExist(err) {
			t.Fatalf("verified duplicate payload remains after cleanup: %v", err)
		}
		if got, err := os.ReadFile(libraryPath); err != nil || string(got) != string(payload) {
			t.Fatalf("existing EPUB changed during duplicate cleanup: %q err=%v", got, err)
		}
		if got, err := os.ReadFile(filepath.Join(publication, "unclassified.dat")); err != nil || string(got) != string(unknownSidecar) {
			t.Fatalf("unclassified sidecar was deleted with the redundant media file: %q err=%v", got, err)
		}
		row, err = db.DeliveryFileOutcomeByPath(ctx, receipt.ID, "download.epub")
		if err != nil || row.CleanupState != "removed" || row.Disposition != "redundant" {
			t.Fatalf("duplicate cleanup outcome=%+v err=%v", row, err)
		}
	})

	t.Run("different bytes in same format remain recoverable", func(t *testing.T) {
		ctx := context.Background()
		libraryBytes, payload := []byte("current EPUB edition"), []byte("different EPUB edition")
		svc, db, dl, cfg, receipt, publication := nativeBookDeliveryFixture(t, "download.epub", payload, nil)
		libraryPath := addNativeBookFile(t, db, dl.MediaItemID, "Project Hail Mary - Andy Weir.epub", libraryBytes, quality.SourceEPUB)
		if err := svc.runDeliveryImport(ctx, dl, cfg, receipt.ID); err == nil || !strings.Contains(err.Error(), "does not improve") {
			t.Fatalf("same-format changed bytes should remain an unresolved import retry, err=%v", err)
		}
		row, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, "download.epub")
		if err != nil || row.Disposition != "unknown_recoverable" || row.CleanupState == "removed" {
			t.Fatalf("changed same-format source was discarded or treated as redundant: row=%+v err=%v", row, err)
		}
		if got, err := os.ReadFile(filepath.Join(publication, "download.epub")); err != nil || string(got) != string(payload) {
			t.Fatalf("changed source bytes were not retained: %q err=%v", got, err)
		}
		if got, err := os.ReadFile(libraryPath); err != nil || string(got) != string(libraryBytes) {
			t.Fatalf("existing EPUB changed despite equal format: %q err=%v", got, err)
		}
	})

	t.Run("upgrade bytes are not treated as redundant when placement is blocked", func(t *testing.T) {
		ctx := context.Background()
		payload := []byte("identical bytes with higher EPUB format claim")
		svc, db, dl, cfg, receipt, publication := nativeBookDeliveryFixture(t, "download.epub", payload, nil)
		pdfPath := addNativeBookFile(t, db, dl.MediaItemID, "Project Hail Mary - Andy Weir.pdf", payload, quality.SourcePDF)
		item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
		if err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "symlink-target.epub")
		if err := os.WriteFile(outside, []byte("unrelated target must stay intact"), 0o600); err != nil {
			t.Fatal(err)
		}
		epubDestination := filepath.Join(item.Path, "Project Hail Mary - Andy Weir.epub")
		if err := os.Symlink(outside, epubDestination); err != nil {
			t.Fatal(err)
		}
		if err := svc.runDeliveryImport(ctx, dl, cfg, receipt.ID); err == nil || !strings.Contains(err.Error(), "symlink requires review") {
			t.Fatalf("blocked EPUB upgrade should remain retryable, err=%v", err)
		}
		row, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, "download.epub")
		if err != nil || row.Disposition != "unknown_recoverable" || row.CleanupState == "removed" {
			t.Fatalf("format upgrade was misclassified as disposable redundancy: row=%+v err=%v", row, err)
		}
		if got, err := os.ReadFile(filepath.Join(publication, "download.epub")); err != nil || string(got) != string(payload) {
			t.Fatalf("blocked upgrade payload was removed: %q err=%v", got, err)
		}
		if got, err := os.ReadFile(pdfPath); err != nil || string(got) != string(payload) {
			t.Fatalf("existing PDF changed without an EPUB placement: %q err=%v", got, err)
		}
		if got, err := os.ReadFile(outside); err != nil || string(got) != "unrelated target must stay intact" {
			t.Fatalf("blocked upgrade modified symlink target: %q err=%v", got, err)
		}
		if err := os.Remove(epubDestination); err != nil {
			t.Fatal(err)
		}
		if err := svc.runDeliveryImport(ctx, dl, cfg, receipt.ID); err != nil {
			t.Fatalf("retry EPUB upgrade after destination repair: %v", err)
		}
		if _, err := os.Stat(pdfPath); !os.IsNotExist(err) {
			t.Fatalf("old PDF was not replaced after EPUB placement committed: %v", err)
		}
		if got, err := os.ReadFile(epubDestination); err != nil || string(got) != string(payload) {
			t.Fatalf("repaired destination does not contain the accepted EPUB: %q err=%v", got, err)
		}
		storedReceipt, err := db.DeliveryReceiptByID(ctx, receipt.ID)
		if err != nil || storedReceipt.State != "imported" {
			t.Fatalf("repaired upgrade receipt=%+v err=%v", storedReceipt, err)
		}
		if err := svc.cleanupDeliveryFiles(ctx, storedReceipt); err != nil {
			t.Fatalf("cleanup accepted EPUB after committed upgrade: %v", err)
		}
		if _, err := os.Stat(filepath.Join(publication, "download.epub")); !os.IsNotExist(err) {
			t.Fatalf("accepted EPUB payload remains after committed upgrade cleanup: %v", err)
		}
	})
}

func TestAllRedundantVideoDeliveryCompletesAndCleansExactDuplicate(t *testing.T) {
	ctx := context.Background()
	fixture := "mkv-1080p-h264-ac3-ger-whole.mkv"
	payload, err := os.ReadFile(filepath.Join("..", "..", "domain", "mediainfo", "testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	svc, db, dl, cfg, publication := deliveryImportFixtureWithMainContent(t, &deliveryImportClient{}, payload, nil)
	receiptID := receiptIDFor(t, db, dl.ID)
	receipt, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) == 0 {
		t.Fatalf("media item fixture=%+v err=%v", item.Seasons, err)
	}
	target := filepath.Join(item.Path, "already-imported.mkv")
	if err := os.WriteFile(target, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, dl.MediaItemID, 0, target, int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{item.Seasons[0].Episodes[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQualityFrom(ctx, fileID, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	if err := svc.runDeliveryImport(ctx, dl, cfg, receipt.ID); err != nil {
		t.Fatalf("complete all-redundant video delivery: %v", err)
	}
	stored, err := db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil || stored.State != "imported" {
		t.Fatalf("video duplicate receipt=%+v err=%v", stored, err)
	}
	rows, err := db.DeliveryFileOutcomes(ctx, receipt.ID)
	if err != nil || len(rows) != 1 || rows[0].Disposition != "redundant" || rows[0].CleanupState != "pending" {
		t.Fatalf("video exact duplicate outcome=%+v err=%v", rows, err)
	}
	manifestPath := filepath.Join(publication, dl.ReleaseTitle+".mkv")
	if got, err := os.ReadFile(manifestPath); err != nil || string(got) != string(payload) {
		t.Fatalf("accepted video manifest payload before cleanup=%q err=%v", got, err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, stored); err != nil {
		t.Fatalf("cleanup exact video duplicate: %v", err)
	}
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("exact duplicate video payload remains after cleanup: %v", err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, dl.ReleaseTitle+".mkv")
	if err != nil || row.Disposition != "redundant" || row.CleanupState != "removed" {
		t.Fatalf("video duplicate cleanup outcome=%+v err=%v", row, err)
	}
	files, err := db.ListFilesForItem(ctx, receipt.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("existing video library entry changed during duplicate cleanup: %+v err=%v", files, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(payload) {
		t.Fatalf("existing video library bytes changed during duplicate cleanup: %q err=%v", got, err)
	}
}

func TestAllRedundantBookDeliveryLedgerFailureRetainsSourceAndRetries(t *testing.T) {
	ctx := context.Background()
	payload := []byte("same EPUB edition with a transient outcome write failure")
	svc, db, dl, cfg, receipt, publication := nativeBookDeliveryFixture(t, "download.epub", payload, nil)
	libraryPath := addNativeBookFile(t, db, dl.MediaItemID, "Project Hail Mary - Andy Weir.epub", payload, quality.SourceEPUB)
	if _, err := db.W.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER fail_redundant_delivery_outcome
		BEFORE UPDATE OF disposition ON runner_delivery_file_outcomes
		WHEN NEW.receipt_id=%d AND NEW.disposition='redundant'
		BEGIN SELECT RAISE(ABORT, 'injected redundant outcome write failure'); END`, receipt.ID)); err != nil {
		t.Fatal(err)
	}
	firstErr := svc.runDeliveryImport(ctx, dl, cfg, receipt.ID)
	if firstErr == nil {
		t.Fatal("delivery completed without persisting its redundant outcome")
	}
	stored, err := db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil || stored.State == "imported" {
		t.Fatalf("failed ledger write completed receipt: %+v err=%v", stored, err)
	}
	if !strings.Contains(stored.Error, "injected redundant outcome write failure") {
		t.Fatalf("persisted retry error omitted redundant outcome write failure: %q", stored.Error)
	}
	if _, err := os.Stat(filepath.Join(publication, "download.epub")); err != nil {
		t.Fatalf("source was removed after redundant outcome write failed: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_redundant_delivery_outcome`); err != nil {
		t.Fatal(err)
	}
	if err := svc.runDeliveryImport(ctx, dl, cfg, receipt.ID); err != nil {
		t.Fatalf("retry after redundant outcome write recovery: %v", err)
	}
	stored, err = db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil || stored.State != "imported" {
		t.Fatalf("recovered redundant receipt=%+v err=%v", stored, err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, "download.epub")
	if err != nil || row.Disposition != "redundant" || row.LibraryPath != libraryPath || row.CleanupState != "pending" {
		t.Fatalf("recovered redundant ledger row=%+v err=%v", row, err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, stored); err != nil {
		t.Fatalf("cleanup after redundant ledger retry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(publication, "download.epub")); !os.IsNotExist(err) {
		t.Fatalf("recovered duplicate source remains after cleanup: %v", err)
	}
	if got, err := os.ReadFile(libraryPath); err != nil || string(got) != string(payload) {
		t.Fatalf("existing EPUB changed during ledger retry: %q err=%v", got, err)
	}
}
