package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/ports"
)

func TestDueDeliveryReceiptQueryDoesNotStarveBehindDeferredRows(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	for i := 0; i < 65; i++ {
		v := ports.RunnerDelivery{Sequence: uint64(i + 1), DeliveryID: fmt.Sprintf("d-%d", i), PublicationGeneration: "g", ArtifactID: "a", JobID: "17", RequestToken: "t", RunnerInstanceID: "runner", PublicationPath: "/completed/a", ManifestDigest: "digest", ReadyAt: time.Unix(1, 0), Files: []ports.RunnerFile{{Path: "x.mkv", SizeBytes: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}}}
		r, err := db.SaveDiscoveredDelivery(ctx, 1, v, "/mounted/a")
		if err != nil {
			t.Fatal(err)
		}
		if i < 64 {
			r.NextAttemptAt = future
			r.Error = "backoff"
			if err := db.SetDeliveryRetry(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := db.DeliveryReceiptsForClient(ctx, 1, 64)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Delivery.DeliveryID != "d-64" {
		t.Fatalf("due query returned %d rows, want the 65th immediately due delivery: %+v", len(got), got)
	}
}

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
		if _, err := db.SaveDiscoveredDelivery(ctx, 1, v, "/mounted/a"); err != nil {
			t.Fatal(err)
		}
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

func TestDeliveryReceiptIsImmutableAndClientPinnedToRunnerInstance(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.EnableRunnerDeliveryV1(ctx, 3); err != nil {
		t.Fatal(err)
	}
	enabled, err := db.RunnerDeliveryV1Enabled(ctx, 3)
	if err != nil || !enabled {
		t.Fatalf("delivery v1 capability=%v err=%v", enabled, err)
	}
	publicationPath := "The Sample Film.mkv"
	base := ports.RunnerDelivery{Sequence: 1, DeliveryID: "d1", PublicationGeneration: "g1", SourceArtifactID: "source-a", SourceGeneration: "source-g1", ArtifactID: "a1", JobID: "17", RequestToken: "t-17", RunnerInstanceID: "runner-A", PublicationPath: "/completed/one", ManifestDigest: "digest", TerminalOutcome: "PAR_FAILURE", FailureStage: "par_verify", FailureCause: "missing recovery blocks", KeepSourceOnFailure: true, FileOutcomes: []ports.RunnerFileOutcome{{SourcePath: "The Sample Film.mkv", PublicationPath: &publicationPath, Classification: "selected_output", Outcome: "published", Reason: "verified output", SourceCleanupStatus: "pending"}, {SourcePath: "é<&.par2", Classification: "repair_input", Outcome: "unresolved", Reason: "needed for later repair", SourceCleanupStatus: "retained"}}, ReadyAt: time.Unix(1, 0), Files: []ports.RunnerFile{{Path: "movie.mkv", SizeBytes: 1, SHA256: "abc"}}}
	r, err := db.SaveDiscoveredDelivery(ctx, 3, base, "/mounted/completed/one")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != "unresolved" || r.ID == 0 {
		t.Fatalf("new receipt = %+v", r)
	}
	base.SourceCleanup = json.RawMessage(`{"state":"pending"}`)
	if _, err := db.SaveDiscoveredDelivery(ctx, 3, base, "/mounted/completed/one"); err != nil {
		t.Fatal(err)
	}
	base.SourceCleanup = json.RawMessage(`{"state":"removed"}`)
	duplicate, err := db.SaveDiscoveredDelivery(ctx, 3, base, "/other/local/path")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.ID != r.ID || duplicate.State != "unresolved" {
		t.Fatalf("duplicate changed pinned receipt: %+v", duplicate)
	}
	if string(duplicate.Delivery.SourceCleanup) != `{"state":"removed"}` {
		t.Fatalf("mutable source cleanup progress was not refreshed: %s", duplicate.Delivery.SourceCleanup)
	}
	if duplicate.Delivery.TerminalOutcome != "PAR_FAILURE" || duplicate.Delivery.FailureStage != "par_verify" || !duplicate.Delivery.KeepSourceOnFailure || len(duplicate.Delivery.FileOutcomes) != 2 || duplicate.Delivery.FileOutcomes[0].SourceCleanupStatus != "pending" {
		t.Fatalf("immutable terminal/file outcome facts were not persisted: %+v", duplicate.Delivery)
	}
	base.FileOutcomes[0].SourceCleanupStatus = "removed"
	updated, err := db.SaveDiscoveredDelivery(ctx, 3, base, "/mounted/completed/one")
	if err != nil || updated.ID != r.ID || updated.Delivery.FileOutcomes[0].SourceCleanupStatus != "removed" {
		t.Fatalf("mutable per-file cleanup status was not refreshed: %+v err=%v", updated.Delivery.FileOutcomes, err)
	}
	changedTerminal := base
	changedTerminal.TerminalOutcome = "SUCCESS"
	if sameDeliveryImmutable(base, changedTerminal) {
		t.Fatal("changed immutable terminal outcome was accepted")
	}
	var conflict DeliveryReceipt
	other := base
	other.Sequence = 2
	other.DeliveryID = "d2"
	other.RunnerInstanceID = "runner-B"
	conflict, err = db.SaveDiscoveredDelivery(ctx, 3, other, "/mounted/completed/two")
	if err != nil {
		t.Fatal(err)
	}
	if conflict.State != "conflict" {
		t.Fatalf("client instance rebind was not fenced: %+v", conflict)
	}
	source := base
	source.DeliveryID, source.Sequence = "source-change", 3
	if _, err := db.SaveDiscoveredDelivery(ctx, 3, source, "/mounted/source-change"); err != nil {
		t.Fatal(err)
	}
	source.SourceGeneration = "source-g2"
	conflict, err = db.SaveDiscoveredDelivery(ctx, 3, source, "/mounted/source-change")
	if err != nil || conflict.State != "conflict" {
		t.Fatalf("source processing generation change was not fenced: %+v err=%v", conflict, err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_items(id,kind,title,sort_title,quality_profile_id,added_at,updated_at) VALUES(91,'movie','Delivery','delivery',1,1,1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO downloads(id,media_item_id,release_title,client_id,protocol,handle,transfer,state,added_at,updated_at,generation_key) VALUES(91,91,'Delivery',3,'usenet','17','t-17','importing',1,1,'generation-91')`); err != nil {
		t.Fatal(err)
	}
	matched, err := db.FindDownloadsForDelivery(ctx, 3, "17", "t-17")
	if err != nil || len(matched) != 1 || matched[0].GenerationKey != "generation-91" {
		t.Fatalf("exact delivery binding=%+v err=%v", matched, err)
	}
	wrongToken, err := db.FindDownloadsForDelivery(ctx, 3, "17", "reused-token")
	if err != nil || len(wrongToken) != 0 {
		t.Fatalf("job ID without exact request token bound to %+v, err=%v", wrongToken, err)
	}
	r.DownloadID, r.GenerationKey, r.DownloadHandle, r.MediaItemID = 91, "generation-91", "17", 91
	if err := db.AssociateDelivery(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.ConsumerReceiptID = "consumer-1"
	if err := db.PrepareDeliveryAcceptance(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.Error, r.Attempts, r.BlockedSince, r.NextAttemptAt = "ack pending", 2, time.Now(), time.Now().Add(time.Minute)
	if err := db.SetDeliveryRetry(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDeliveryImported(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	pendingAck, err := db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil || pendingAck.State != "imported" || pendingAck.AckState != "pending" || pendingAck.Attempts != 2 || pendingAck.Error != "ack pending" {
		t.Fatalf("local import regressed pending acknowledgement: %+v err=%v", pendingAck, err)
	}
	if _, err := db.W.ExecContext(ctx, `DELETE FROM downloads WHERE id=91`); err == nil {
		t.Fatal("delivery with pending acknowledgement was deletable")
	}
	if err := db.SetDeliveryAccepted(ctx, pendingAck); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `DELETE FROM downloads WHERE id=91`); err != nil {
		t.Fatalf("resolved delivery association should be removable: %v", err)
	}
	if err := db.SetDeliveryCursor(ctx, 3, 42); err != nil {
		t.Fatal(err)
	}
	cursor, err := db.DeliveryCursor(ctx, 3)
	if err != nil || cursor != 42 {
		t.Fatalf("cursor=%d err=%v", cursor, err)
	}
	changed := base
	changed.ManifestDigest = "different"
	conflict, err = db.SaveDiscoveredDelivery(ctx, 3, changed, "/mounted/completed/one")
	if err != nil {
		t.Fatal(err)
	}
	if conflict.ID != r.ID || conflict.State != "conflict" {
		t.Fatalf("immutable tuple change was not fenced: %+v", conflict)
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
	receipt, err := db.SaveDiscoveredDelivery(ctx, 3, v, "/mounted/completed/delivery")
	if err != nil {
		t.Fatal(err)
	}
	receipt.DownloadID, receipt.GenerationKey, receipt.DownloadHandle, receipt.MediaItemID = 91, "generation-91", "17", 91
	if err := db.AssociateDelivery(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	receipt.ConsumerReceiptID = "consumer-receipt-1"
	if err := db.PrepareDeliveryAcceptance(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDeliveryImported(ctx, receipt.ID); err != nil {
		t.Fatal(err)
	}
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
	if err := db.SetDeliveryAccepted(ctx, loaded); err != nil {
		t.Fatal(err)
	}
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
	receipt, err := db.SaveDiscoveredDelivery(ctx, 3, v, "/mounted/completed/failed")
	if err != nil {
		t.Fatal(err)
	}
	receipt.DownloadID, receipt.GenerationKey, receipt.DownloadHandle, receipt.MediaItemID = 92, "generation-92", "18", 92
	if err := db.AssociateDelivery(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	cleared, err := db.ClearFailedDownloads(ctx)
	if err != nil || cleared != 1 {
		t.Fatalf("ClearFailedDownloads cleared=%d err=%v", cleared, err)
	}
	var count, dismissed int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*),max(activity_dismissed) FROM downloads WHERE id=92`).Scan(&count, &dismissed); err != nil || count != 1 || dismissed != 1 {
		t.Fatalf("unresolved failure did not retain hidden Activity receipt: count=%d dismissed=%d err=%v", count, dismissed, err)
	}
}

func TestDeliverySourceRefreshFailureIsDurableAndBackedOff(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	v := ports.RunnerDelivery{Sequence: 1, DeliveryID: "refresh-1", PublicationGeneration: "pub", ArtifactID: "artifact", JobID: "17", RequestToken: "token", RunnerInstanceID: "runner", PublicationPath: "/completed/a", ManifestDigest: "digest", ReadyAt: time.Now(), Files: []ports.RunnerFile{{Path: "movie.mkv", SizeBytes: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}}}
	r, err := db.SaveDiscoveredDelivery(ctx, 1, v, "/mounted/a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET ack_state='accepted' WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.FailDeliverySourceRefresh(ctx, r.ID, "connection refused"); err != nil {
		t.Fatal(err)
	}
	message, next, err := db.DeliverySourceRefreshError(ctx, r.ID)
	if err != nil || message != "connection refused" || !next.After(time.Now()) {
		t.Fatalf("persisted source refresh failure=%q next=%v err=%v", message, next, err)
	}
	due, err := db.DeliveryReceiptsNeedingSourceRefresh(ctx, 1, 4)
	if err != nil || len(due) != 0 {
		t.Fatalf("receipt remained due immediately after transport failure: %+v err=%v", due, err)
	}
}

func TestProviderRetryDeadlinesSurviveRetryNowAndRestart(t *testing.T) {
	ctx := context.Background()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	v := ports.RunnerDelivery{Sequence: 1, DeliveryID: "retry-after", PublicationGeneration: "pub", ArtifactID: "artifact", JobID: "17", RequestToken: "token", RunnerInstanceID: "runner", PublicationPath: "/completed/a", ManifestDigest: "digest", ReadyAt: time.Now(), Files: []ports.RunnerFile{{Path: "movie.mkv", SizeBytes: 1, SHA256: "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}}}
	r, err := db.SaveDiscoveredDelivery(ctx, 1, v, "/mounted/a")
	if err != nil {
		t.Fatal(err)
	}
	provider := time.UnixMilli(time.Now().Add(20 * time.Minute).UnixMilli())
	r.NextAttemptAt = time.Now().Add(30 * time.Second)
	r.Attempts = 1
	r.Error = "429"
	if err = db.SetDeliveryRetry(ctx, r, provider); err != nil {
		t.Fatal(err)
	}
	if err = db.ForceDeliveryRetryNow(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	var due int64
	if err = db.R.QueryRowContext(ctx, `SELECT next_attempt_at FROM runner_delivery_receipts WHERE id=?`, r.ID).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if due < provider.UnixMilli() {
		t.Fatalf("Retry Now bypassed provider deadline: due=%v provider=%v", time.UnixMilli(due), provider)
	}
	if err = db.FailDeliverySourceRefresh(ctx, r.ID, "429", provider); err != nil {
		t.Fatal(err)
	}
	if err = db.ForceDeliverySourceRefreshNow(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	var sourceDue int64
	if err = db.R.QueryRowContext(ctx, `SELECT next_attempt_at FROM runner_delivery_source_refresh WHERE receipt_id=?`, r.ID).Scan(&sourceDue); err != nil {
		t.Fatal(err)
	}
	if sourceDue < provider.UnixMilli() {
		t.Fatalf("source refresh Retry Now bypassed provider deadline: %v < %v", time.UnixMilli(sourceDue), provider)
	}
	dir := filepath.Dir(db.Path)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = reopened.R.QueryRowContext(ctx, `SELECT provider_retry_after FROM runner_delivery_receipts WHERE id=?`, r.ID).Scan(&due); err != nil {
		t.Fatal(err)
	}
	if time.UnixMilli(due).Before(provider) {
		t.Fatalf("provider deadline did not survive reopen: %v", time.UnixMilli(due))
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
	r, err := db.SaveDiscoveredDelivery(ctx, 4, v, "/mounted/job")
	if err != nil {
		t.Fatal(err)
	}
	manifest := []DeliveryManifestFile{{Path: "Season 01/Episode.mkv", SizeBytes: 1, SHA256: "abc"}, {Path: "Season 02/Episode.mkv", SizeBytes: 1, SHA256: "abc"}}
	if err := db.EnsureDeliveryFileOutcomes(ctx, r.ID, manifest, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDeliveryFileDisposition(ctx, r.ID, "Season 01/Episode.mkv", "placed", "imported", "/library/Season 01/Episode.mkv"); err != nil {
		t.Fatal(err)
	}
	firstPlacement := DeliveryFileImport{ReceiptID: r.ID, Path: "Season 01/Episode.mkv", SourceGeneration: "download-generation", SourceTransfer: "transfer-token", SourcePayloadGeneration: "publication-generation", PlacementID: "placement-1", LibraryFileGeneration: "library-generation-1", LibraryFileIdentity: "stat-identity-1", LibraryPath: "/library/Season 01/Episode.mkv"}
	if err := db.RecordDeliveryFileImport(ctx, firstPlacement); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDeliveryFileImport(ctx, firstPlacement); err != nil {
		t.Fatal(err)
	}
	secondPlacement := firstPlacement
	secondPlacement.ReplacementOperationID = "replace-op"
	secondPlacement.ReplacementDisposition = "selected"
	secondPlacement.PlacementID = "placement-2"
	secondPlacement.LibraryFileGeneration = "library-generation-2"
	if err := db.RecordDeliveryFileImport(ctx, secondPlacement); err != nil {
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
	ids, err := db.DeliveryFileCleanupReceipts(ctx, 4, 10)
	if err != nil || len(ids) != 0 {
		t.Fatalf("future cleanup retry was scheduled early: ids=%v err=%v", ids, err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_next_attempt_at=0 WHERE receipt_id=? AND path=?`, r.ID, "Season 01/Episode.mkv"); err != nil {
		t.Fatal(err)
	}
	ids, err = db.DeliveryFileCleanupReceipts(ctx, 4, 10)
	if err != nil || len(ids) != 1 || ids[0] != r.ID {
		t.Fatalf("resolved file outcome was not independently schedulable: ids=%v err=%v", ids, err)
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
