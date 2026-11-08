package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func TestDeliveryReceiptFeedUsesStableBoundedPages(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		v := ports.RunnerDelivery{Sequence: uint64(i), DeliveryID: fmt.Sprintf("page-%d", i), PublicationGeneration: "g", ArtifactID: "a", JobID: "17", RequestToken: "t", RunnerInstanceID: "runner", PublicationPath: "/completed/a", ManifestDigest: "digest", ReadyAt: time.Unix(1, 0), Files: []ports.RunnerFile{{Path: "x.mkv", SizeBytes: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}}}
		seedLegacyReceipt(t, db, 1, v, "/mounted/a")
	}
	first, next, err := db.DeliveryReceiptPage(ctx, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || next != first[1].ID {
		t.Fatalf("first page=%d next=%d", len(first), next)
	}
	second, next, err := db.DeliveryReceiptPage(ctx, next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || next != 0 || second[0].ID >= first[1].ID {
		t.Fatalf("second page=%+v next=%d", second, next)
	}
	filtered, next, err := db.DeliveryReceiptPageForDownload(ctx, 999, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 0 || next != 0 {
		t.Fatalf("download-filtered page=%+v next=%d", filtered, next)
	}
}

func TestClearImportedDismissesPendingRunnerAckAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at) VALUES(91,'movie','Delivery','delivery',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads(id,media_item_id,release_title,client_id,protocol,handle,transfer,state,added_at,updated_at,generation_key,payload_removed,import_path) VALUES(91,91,'Delivery',3,'usenet','17','t-17','imported',1,1,'generation-91',0,'/completed/delivery')`); err != nil {
		t.Fatal(err)
	}
	v := ports.RunnerDelivery{Sequence: 1, DeliveryID: "dismiss-pending-ack", PublicationGeneration: "publication-1", ArtifactID: "artifact-1", JobID: "17", RequestToken: "t-17", RunnerInstanceID: "runner-1", PublicationPath: "/completed/delivery", ManifestDigest: "digest", ReadyAt: time.Unix(1, 0), Files: []ports.RunnerFile{{Path: "movie.mkv", SizeBytes: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}}}
	receipt := seedLegacyReceipt(t, db, 3, v, "/mounted/completed/delivery")
	receipt.DownloadID, receipt.GenerationKey, receipt.DownloadHandle, receipt.MediaItemID = 91, "generation-91", "17", 91
	seedLegacyAssociation(t, db, receipt)
	seedLegacyReceiptState(t, db, receipt.ID, "imported", "pending", "consumer-receipt-1")
	hidden, err := db.ClearImportedDownloads(ctx)
	if err != nil || hidden != 1 {
		t.Fatalf("ClearImportedDownloads hidden=%d err=%v", hidden, err)
	}
	var dismissed int
	if err := db.R.QueryRowContext(ctx, `SELECT activity_dismissed FROM downloads WHERE id=91`).Scan(&dismissed); err != nil || dismissed != 1 {
		t.Fatalf("pending receipt was not dismissed from Activity: dismissed=%d err=%v", dismissed, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, err := db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil || loaded.State != "imported" || loaded.AckState != "pending" {
		t.Fatalf("pending acknowledgement did not survive restart: %+v err=%v", loaded, err)
	}
	if err := db.MarkPayloadRemoved(ctx, 91); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM downloads WHERE id=91 AND activity_dismissed=1 AND payload_removed=1`).Scan(&dismissed); err != nil || dismissed != 1 {
		t.Fatalf("cleanup deleted a dismissed row with pending ACK: count=%d err=%v", dismissed, err)
	}
	seedLegacyReceiptState(t, db, loaded.ID, "imported", "accepted", loaded.ConsumerReceiptID)
	if err := db.MarkPayloadRemoved(ctx, 91); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM downloads WHERE id=91`).Scan(&dismissed); err != nil || dismissed != 0 {
		t.Fatalf("resolved dismissed Activity row remains: count=%d err=%v", dismissed, err)
	}
}

func TestClearFailedSoftDismissesUnresolvedRunnerReceipt(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at) VALUES(92,'movie','Failed delivery','failed delivery',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads(id,media_item_id,release_title,client_id,protocol,handle,transfer,state,added_at,updated_at,generation_key) VALUES(92,92,'Failed delivery',3,'usenet','18','t-18','failed',1,1,'generation-92')`); err != nil {
		t.Fatal(err)
	}
	v := ports.RunnerDelivery{Sequence: 1, DeliveryID: "failed-unresolved", PublicationGeneration: "publication-2", ArtifactID: "artifact-2", JobID: "18", RequestToken: "t-18", RunnerInstanceID: "runner-1", PublicationPath: "/completed/failed", ManifestDigest: "digest-2", TerminalOutcome: "FAILURE/UNPACK", ReadyAt: time.Unix(1, 0), Files: []ports.RunnerFile{{Path: "movie.mkv", SizeBytes: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}}}
	receipt := seedLegacyReceipt(t, db, 3, v, "/mounted/completed/failed")
	receipt.DownloadID, receipt.GenerationKey, receipt.DownloadHandle, receipt.MediaItemID = 92, "generation-92", "18", 92
	seedLegacyAssociation(t, db, receipt)
	cleared, err := db.ClearFailedDownloads(ctx)
	if err != nil || cleared != 1 {
		t.Fatalf("ClearFailedDownloads cleared=%d err=%v", cleared, err)
	}
	var count, dismissed int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*),max(activity_dismissed) FROM downloads WHERE id=92`).Scan(&count, &dismissed); err != nil || count != 1 || dismissed != 1 {
		t.Fatalf("unresolved failure did not retain hidden Activity receipt: count=%d dismissed=%d err=%v", count, dismissed, err)
	}
}

func TestDeliveryFileLedgerUsesFullManifestPathAndRetainsDispositionAfterRemoval(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	v := ports.RunnerDelivery{Sequence: 1, DeliveryID: "ledger-1", PublicationGeneration: "g", ArtifactID: "a", JobID: "17", RequestToken: "t", RunnerInstanceID: "runner", PublicationPath: "/completed/job", ManifestDigest: "digest", ReadyAt: time.Unix(1, 0), Files: []ports.RunnerFile{{Path: "Season 01/Episode.mkv", SizeBytes: 1, SHA256: "abc"}, {Path: "Season 02/Episode.mkv", SizeBytes: 1, SHA256: "abc"}}}
	r := seedLegacyReceipt(t, db, 4, v, "/mounted/job")
	manifest := []DeliveryManifestFile{{Path: "Season 01/Episode.mkv", SizeBytes: 1, SHA256: "abc"}, {Path: "Season 02/Episode.mkv", SizeBytes: 1, SHA256: "abc"}}
	seedLegacyFileOutcomes(t, db, r.ID, manifest, nil)
	if err := db.UpdateDeliveryFileDisposition(ctx, r.ID, "Season 01/Episode.mkv", "placed", "imported", "/library/Season 01/Episode.mkv"); err != nil {
		t.Fatal(err)
	}
	firstPlacement := DeliveryFileImport{ReceiptID: r.ID, Path: "Season 01/Episode.mkv", SourceGeneration: "download-generation", SourceTransfer: "transfer-token", SourcePayloadGeneration: "publication-generation", PlacementID: "placement-1", LibraryFileGeneration: "library-generation-1", LibraryFileIdentity: "stat-identity-1", LibraryPath: "/library/Season 01/Episode.mkv"}
	if err := recordDeliveryFileImport(ctx, db.W, firstPlacement); err != nil {
		t.Fatal(err)
	}
	if err := recordDeliveryFileImport(ctx, db.W, firstPlacement); err != nil {
		t.Fatal(err)
	}
	secondPlacement := firstPlacement
	secondPlacement.ReplacementOperationID = "replace-op"
	secondPlacement.ReplacementDisposition = "selected"
	secondPlacement.PlacementID = "placement-2"
	secondPlacement.LibraryFileGeneration = "library-generation-2"
	if err := recordDeliveryFileImport(ctx, db.W, secondPlacement); err != nil {
		t.Fatal(err)
	}
	imports, err := db.DeliveryFileImports(ctx, r.ID)
	if err != nil || len(imports) != 2 || imports[0].SourceGeneration != "download-generation" || imports[0].SourcePayloadGeneration != "publication-generation" || imports[0].LibraryFileIdentity != "stat-identity-1" || imports[1].ReplacementOperationID != "replace-op" || imports[1].PlacementID != "placement-2" {
		t.Fatalf("immutable file-import provenance = %+v err=%v", imports, err)
	}
	if err := db.SetDeliveryFileCleanup(ctx, r.ID, "Season 01/Episode.mkv", "pending", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDeliveryFileCleanup(ctx, r.ID, "Season 01/Episode.mkv", "retry", "transient unlink failure"); err != nil {
		t.Fatal(err)
	}
	retryRow, err := db.DeliveryFileOutcomeByPath(ctx, r.ID, "Season 01/Episode.mkv")
	if err != nil || retryRow.CleanupAttempts != 1 || retryRow.CleanupBlockedSince.IsZero() || retryRow.CleanupNextAttemptAt.Before(time.Now().Add(20*time.Second)) || retryRow.CleanupNextAttemptAt.After(time.Now().Add(35*time.Second)) {
		t.Fatalf("first cleanup retry was not durably backed off: %+v err=%v", retryRow, err)
	}
	if err := db.SetDeliveryFileCleanup(ctx, r.ID, "Season 01/Episode.mkv", "removed", ""); err != nil {
		t.Fatal(err)
	}
	rows, err := db.DeliveryFileOutcomes(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Path == rows[1].Path {
		t.Fatalf("ledger collapsed duplicate basenames: %+v", rows)
	}
	var removed *DeliveryFileOutcome
	for i := range rows {
		if rows[i].Path == "Season 01/Episode.mkv" {
			removed = &rows[i]
		}
	}
	if removed == nil || removed.Disposition != "placed" || removed.CleanupState != "removed" || removed.LibraryPath != "/library/Season 01/Episode.mkv" {
		t.Fatalf("audit rationale did not survive cleanup: %+v", removed)
	}
}
