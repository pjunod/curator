package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestDeliveryCleanupRemovesPackExtraOnlyAfterExactReplacementPlacement(t *testing.T) {
	ctx := context.Background()
	extraName := "Test.Show.S01E02.1080p.WEB-DL.x264-GRP.mkv"
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, &deliveryImportClient{}, map[string][]byte{extraName: []byte("unselected pack episode")})
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	r := receipts[0]
	verified, err := svc.verifyDeliveryManifest(ctx, cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	manifest := make([]sqlite.DeliveryManifestFile, 0, len(verified))
	selected := map[string]bool{epRelease + ".mkv": true}
	var excess verifiedDeliveryFile
	for _, file := range verified {
		manifest = append(manifest, sqlite.DeliveryManifestFile{Path: file.relative, SizeBytes: file.size, SHA256: file.digest, FileGeneration: file.generation})
		if file.relative == extraName {
			excess = file
		}
	}
	if excess.relative == "" {
		t.Fatal("pack extra is absent from verified manifest")
	}
	if err := db.EnsureDeliveryFileOutcomes(ctx, r.ID, manifest, selected, false); err != nil {
		t.Fatal(err)
	}
	item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) < 2 {
		t.Fatalf("expected two-episode series fixture, item=%+v err=%v", item, err)
	}
	episodeID := item.Seasons[0].Episodes[0].ID
	target := filepath.Join(item.Path, "selected replacement.mkv")
	targetBytes := []byte("committed selected replacement")
	if err := os.WriteFile(target, targetBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, dl.MediaItemID, 0, target, int64(len("committed selected replacement")))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{episodeID}); err != nil {
		t.Fatal(err)
	}
	var generation string
	if err := db.R.QueryRowContext(ctx, `SELECT file_generation FROM media_files WHERE id=?`, fileID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	sourceGeneration := "selected-source-generation"
	if _, err := db.W.ExecContext(ctx, `UPDATE media_files SET source_generation=? WHERE id=?`, sourceGeneration, fileID); err != nil {
		t.Fatal(err)
	}
	op := sqlite.ReplacementOperation{ID: "replacement-for-pack-excess", RequestID: "request-pack-excess", Fingerprint: "fingerprint", State: "complete", Intent: "replace_quality", Scope: "episodes", MediaItemID: dl.MediaItemID, SourceGeneration: sourceGeneration, EpisodeIDs: []int64{episodeID}, CreatedAt: time.Now()}
	op, _, err = db.CreateReplacementOperation(ctx, op, []sqlite.ReplacementFile{{MediaFileID: fileID, FileGeneration: generation, SourceGeneration: sourceGeneration, Path: target, EpisodeIDs: []int64{episodeID}, Disposition: "selected", RemovalState: "removed"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_file_placements(placement_id,media_file_id,file_generation,source_generation,path,sha256,committed_at,replacement_operation_id) VALUES(?,?,?,?,?,?,?,?)`, "placement-pack-extra-proof", fileID, generation, sourceGeneration, target, sha256Hex(targetBytes), time.Now().UnixMilli(), op.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDeliveryFileImport(ctx, sqlite.DeliveryFileImport{
		ReceiptID: r.ID, Path: excess.relative, SourceGeneration: r.GenerationKey,
		SourceTransfer: r.Delivery.RequestToken, SourcePayloadGeneration: r.Delivery.PublicationGeneration,
		ReplacementOperationID: op.ID, ReplacementDisposition: "excess",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDeliveryImported(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	r, err = db.DeliveryReceiptByID(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, r); err != nil {
		t.Fatal(err)
	}
	rows, err := db.DeliveryFileOutcomes(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(publication, filepath.FromSlash(excess.relative))); !os.IsNotExist(err) {
		t.Fatalf("committed selected-scope excess remained: stat error=%v ledger=%+v", err, rows)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("selected library placement was removed: %v", err)
	}
	for _, row := range rows {
		if row.Path == excess.relative && row.CleanupState != "removed" {
			t.Fatalf("pack excess cleanup state=%q, want removed", row.CleanupState)
		}
	}
}

func TestDeliveryRedundantEncodingCleanupUsesManifestAbsolutePathAndGeneration(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, _, publication := deliveryImportFixture(t, &deliveryImportClient{})
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	r := receipts[0]
	sourcePath := filepath.Join(publication, epRelease+".mkv")
	source, err := os.ReadFile(filepath.Join("..", "..", "domain", "mediainfo", "testdata", "mkv-1080p-h264-ac3-whole.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Lstat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	sourceGeneration := deliveryFileGeneration(sourceInfo)
	if err := db.EnsureDeliveryFileOutcomes(ctx, r.ID, []sqlite.DeliveryManifestFile{{Path: epRelease + ".mkv", SizeBytes: int64(len(source)), SHA256: sha256Hex(source), FileGeneration: sourceGeneration}}, map[string]bool{epRelease + ".mkv": true}, false); err != nil {
		t.Fatal(err)
	}
	item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil {
		t.Fatal(err)
	}
	targetSource, err := os.ReadFile(filepath.Join("..", "..", "domain", "mediainfo", "testdata", "mkv-1080p-h264-ac3-whole.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(item.Path, "same-quality-existing.mkv")
	if err := os.WriteFile(target, targetSource, 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, dl.MediaItemID, 0, target, int64(len(targetSource)))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{item.Seasons[0].Episodes[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetFileQualityFrom(ctx, fileID, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	var libraryGeneration string
	if err := db.R.QueryRowContext(ctx, `SELECT file_generation FROM media_files WHERE id=?`, fileID).Scan(&libraryGeneration); err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDeliveryFileDisposition(ctx, r.ID, epRelease+".mkv", "redundant", "same-copy usable equivalent", target); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDeliveryFileImport(ctx, sqlite.DeliveryFileImport{
		ReceiptID: r.ID, Path: epRelease + ".mkv", SourceGeneration: r.GenerationKey,
		SourceTransfer: r.Delivery.RequestToken, SourcePayloadGeneration: r.Delivery.PublicationGeneration,
		LibraryFileGeneration: libraryGeneration, LibraryFileIdentity: deliveryFileGeneration(targetInfo), LibraryPath: target,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDeliveryFileCleanup(ctx, r.ID, epRelease+".mkv", "pending", ""); err != nil {
		t.Fatal(err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, r.ID, epRelease+".mkv")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.removeVerifiedDeliveryFile(ctx, r, row); err != nil {
		t.Fatalf("cleanup after exact same-copy quality proof: %v", err)
	}
	if _, err := os.Stat(sourcePath); !os.IsNotExist(err) {
		t.Fatalf("redundant different encoding remains in completed payload: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("existing same-copy library file was removed: %v", err)
	}
}

func TestDeliveryCleanupCrashAfterUnlinkSettlesPendingLedgerOnReplay(t *testing.T) {
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	rid := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, rid); err != nil {
		t.Fatalf("prepare an actually committed placement: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, rid)
	if err != nil || r.State != "imported" {
		t.Fatalf("receipt did not reach terminal import state: %+v err=%v", r, err)
	}
	rows, err := db.DeliveryFileOutcomes(ctx, r.ID)
	if err != nil || len(rows) != 1 || rows[0].Disposition != "placed" {
		t.Fatalf("committed file outcome = %+v err=%v", rows, err)
	}
	f := rows[0]
	// This is the crash boundary: the anchored unlink completed but its
	// following SQLite cleanup-state write did not.
	if err := os.Remove(filepath.Join(publication, filepath.FromSlash(f.Path))); err != nil {
		t.Fatal(err)
	}
	dbDir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return &deliveryImportClient{} })
	r, err = reopened.DeliveryReceiptByID(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.cleanupDeliveryFiles(ctx, r); err != nil {
		t.Fatalf("replay cleanup after unlink: %v", err)
	}
	row, err := reopened.DeliveryFileOutcomeByPath(ctx, r.ID, f.Path)
	if err != nil || row.CleanupState != "removed" {
		t.Fatalf("replayed cleanup state=%+v err=%v", row, err)
	}
}
