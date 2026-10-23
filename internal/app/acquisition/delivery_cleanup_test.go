package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/parser"
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

func TestReplacementPackFileWithSelectedSiblingOrUnknownEpisodeIsNotDisposableExcess(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := db.GetProfile(ctx, item.QualityProfileID)
	if err != nil {
		t.Fatal(err)
	}
	selectedEpisode := item.Seasons[0].Episodes[0].ID
	for _, tc := range []struct {
		name     string
		episodes []int
	}{
		{name: "selected sibling", episodes: []int{1, 2}},
		{name: "unknown episode", episodes: []int{2, 99}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, callErr := svc.importEpisodeFile(ctx, item, importScope{Planned: true, ReplacementOperationID: "replace-one-episode", AllowedEpisodeIDs: []int64{selectedEpisode}}, profile,
				map[int64]episodeState{}, filepath.Join(t.TempDir(), "Test.Show.S01E01E02.1080p.WEB-DL.mkv"),
				parser.Parsed{Season: 1, Episodes: tc.episodes}, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, "Test.Show.S01E01E02.1080p.WEB-DL", false)
			if callErr == nil {
				t.Fatal("indivisible file unexpectedly imported outside its selected scope")
			}
			if errors.Is(callErr, errReplacementFileOutsideScope) {
				t.Fatalf("unsafe or mixed source was classified as disposable excess: %v", callErr)
			}
		})
	}
}

func TestPackFallbackExcessCleanupWakesAfterReplacementCommitAndRestart(t *testing.T) {
	ctx := context.Background()
	content, err := os.ReadFile(filepath.Join("..", "..", "domain", "mediainfo", "testdata", "mkv-1080p-h264-ac3-whole.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	const extraName = "Test.Show.S01E02.1080p.WEB-DL.x264-AUTO.mkv"
	extraSidecar := extraName + ".en.srt"
	const unmatchedSidecar = "unmatched.notes.srt"
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixtureWithMainContent(t, protocol, content, map[string][]byte{
		extraName: content, extraSidecar: []byte("subtitle for out-of-scope episode"), unmatchedSidecar: []byte("unmatched notes"),
	})
	item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) < 2 {
		t.Fatalf("episode fixture=%+v err=%v", item, err)
	}
	selectedEpisode := item.Seasons[0].Episodes[0].ID
	oldBytes := []byte("reviewed lower quality copy")
	oldPath := filepath.Join(item.Path, "The Test Show - S01E01 - Pilot [720p WEB-DL].mkv")
	if err = os.WriteFile(oldPath, oldBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	oldFileID, err := db.UpsertFile(ctx, dl.MediaItemID, 0, oldPath, int64(len(oldBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReplaceFileEpisodeLinks(ctx, oldFileID, []int64{selectedEpisode}); err != nil {
		t.Fatal(err)
	}
	if err = db.SetFileQualityFrom(ctx, oldFileID, quality.Quality{Source: quality.SourceWEBDL, Resolution: 720}, mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	var oldGeneration string
	if err = db.R.QueryRowContext(ctx, `SELECT file_generation FROM media_files WHERE id=?`, oldFileID).Scan(&oldGeneration); err != nil {
		t.Fatal(err)
	}
	oldSourceGeneration := "reviewed-old-source"
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation=? WHERE id=?`, oldSourceGeneration, oldFileID); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Lstat(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	storageIdentity, err := expectedStorageIdentity(item.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.PinReplacementStorageIdentity(ctx, filepath.Clean(item.Path), storageIdentity, "pack-fallback-test-observation"); err != nil {
		t.Fatal(err)
	}
	op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
		ID: "replacement-pack-fallback-wake", RequestID: "request-pack-fallback-wake", Fingerprint: "pack-fallback-fingerprint",
		State: "replacing", Intent: "replace_quality", Scope: "episodes", MediaItemID: dl.MediaItemID,
		SourceGeneration: oldSourceGeneration, EpisodeIDs: []int64{selectedEpisode},
	}, []sqlite.ReplacementFile{{
		MediaFileID: oldFileID, FileGeneration: oldGeneration, SourceGeneration: oldSourceGeneration,
		FileIdentity: deliveryFileStableIdentity(oldInfo), StorageIdentity: storageIdentity, Size: oldInfo.Size(),
		Path: oldPath, SHA256: sha256Hex(oldBytes), EpisodeIDs: []int64{selectedEpisode},
		Disposition: "selected", RemovalState: "pending",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE downloads SET replacement_operation_id=? WHERE id=? AND generation_key=?`, op.ID, dl.ID, dl.GenerationKey); err != nil {
		t.Fatal(err)
	}
	dl.ReplacementOperationID = op.ID
	receiptID := receiptIDFor(t, db, dl.ID)
	if err = svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("pack fallback import: %v", err)
	}
	imports, err := db.DeliveryFileImports(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	var excessImport *sqlite.DeliveryFileImport
	for i := range imports {
		if imports[i].Path == extraName {
			excessImport = &imports[i]
		}
	}
	if excessImport == nil || excessImport.ReplacementOperationID != op.ID || excessImport.ReplacementDisposition != "excess" {
		t.Fatalf("actual out-of-scope pack file was not durably classified under the operation: %+v", imports)
	}
	var excessSidecarImport *sqlite.DeliveryFileImport
	for i := range imports {
		if imports[i].Path == extraSidecar {
			excessSidecarImport = &imports[i]
		}
	}
	if excessSidecarImport == nil || excessSidecarImport.ReplacementOperationID != op.ID || excessSidecarImport.ReplacementDisposition != "excess" {
		t.Fatalf("exactly matched pack sidecar lacks excess lineage: %+v", imports)
	}
	for _, imported := range imports {
		if imported.Path == unmatchedSidecar && imported.ReplacementDisposition == "excess" {
			t.Fatalf("unmatched sidecar gained replacement deletion authority: %+v", imported)
		}
	}
	sidecarRow, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, extraSidecar)
	if err != nil || sidecarRow.Disposition != "known_incidental" || sidecarRow.CleanupState != "protected" {
		t.Fatalf("pack sidecar should remain protected before operation completion: %+v err=%v", sidecarRow, err)
	}
	unmatchedRow, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, unmatchedSidecar)
	if err != nil || unmatchedRow.Disposition != "known_incidental" || unmatchedRow.CleanupState != "protected" {
		t.Fatalf("unmatched sidecar should remain reviewable: %+v err=%v", unmatchedRow, err)
	}
	if err = svc.cleanupDeliveryFiles(ctx, mustDeliveryReceipt(t, db, receiptID)); err != nil {
		t.Fatalf("early cleanup before replacement completion: %v", err)
	}
	if _, err = os.Stat(filepath.Join(publication, extraName)); err != nil {
		t.Fatalf("pack excess was removed before selected replacement committed: %v", err)
	}
	if _, err = os.Stat(filepath.Join(publication, extraSidecar)); err != nil {
		t.Fatalf("pack sidecar was removed before selected replacement committed: %v", err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, extraName)
	if err != nil || row.CleanupState != "protected" {
		t.Fatalf("pre-commit excess state=%+v err=%v", row, err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatalf("commit selected replacement operation: %v", err)
	}
	committed, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || committed.State != "complete" {
		t.Fatalf("operation did not durably complete: %+v err=%v", committed, err)
	}
	placements, err := db.CurrentReplacementPlacements(ctx, op.ID)
	if err != nil || len(placements) == 0 {
		t.Fatalf("replacement placement lookup=%+v err=%v", placements, err)
	}
	if err = db.CreateLibraryReviewFlag(ctx, item.ID, placements[0].MediaFileID, placements[0].FileGeneration,
		"test-review", "replacement requires review", "test evidence"); err != nil {
		t.Fatal(err)
	}
	// A completed operation is not enough if its selected placement is now
	// flagged for review. The excess media and its matched sidecar stay
	// protected, and the derived receipt wake gets a durable retry deadline.
	if err = svc.cleanupDeliveryFiles(ctx, mustDeliveryReceipt(t, db, receiptID)); err != nil {
		t.Fatalf("cleanup with newly flagged replacement placement: %v", err)
	}
	for _, path := range []string{extraName, extraSidecar} {
		blocked, readErr := db.DeliveryFileOutcomeByPath(ctx, receiptID, path)
		if readErr != nil || blocked.CleanupState != "protected" || !blocked.CleanupNextAttemptAt.After(time.Now()) || blocked.CleanupAttempts == 0 {
			t.Fatalf("unproven excess %q did not persist a protected retry deadline: %+v err=%v", path, blocked, readErr)
		}
	}
	dueReceipts, err := db.DeliveryFileCleanupReceipts(ctx, cfg.ID, 16)
	if err != nil {
		t.Fatal(err)
	}
	for _, dueID := range dueReceipts {
		if dueID == receiptID {
			t.Fatalf("receipt with protected excess proof deadline was immediately reselected: %v", dueReceipts)
		}
	}
	flagRows, err := db.ListOpenMediaFileReviewFlags(ctx, item.ID)
	if err != nil || len(flagRows) != 1 {
		t.Fatalf("review flag lookup=%+v err=%v", flagRows, err)
	}
	if err = db.ResolveMediaFileReviewFlag(ctx, item.ID, flagRows[0].ID, placements[0].FileGeneration); err != nil {
		t.Fatal(err)
	}
	// Advance the persisted retry deadline to model the scheduled delay elapsing
	// without sleeping through the production 30-second first backoff.
	if _, err = db.W.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_next_attempt_at=0 WHERE receipt_id=? AND path IN (?,?)`, receiptID, extraName, extraSidecar); err != nil {
		t.Fatal(err)
	}
	// Reopen SQLite and use the normal receipt scheduler/worker path. The
	// exact immutable excess-import row plus completed operation is the wake
	// condition; the old protected outcome itself is not broadened.
	dbDir := filepath.Dir(db.Path)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	worker := New(reopened, bus.New(nil), nil,
		func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} },
		func(ports.ClientConfig) ports.DownloadClient { return protocol })
	worker.importRecoverOnce.Do(func() {})
	workerCtx, cancel := context.WithCancel(ctx)
	worker.StartImporters(workerCtx)
	defer func() {
		cancel()
		worker.WaitImporters()
	}()
	ids, err := reopened.DeliveryFileCleanupReceipts(ctx, cfg.ID, 16)
	if err != nil || len(ids) != 1 || ids[0] != receiptID {
		t.Fatalf("completed operation did not wake its exact receipt after restart: ids=%v err=%v", ids, err)
	}
	worker.scheduleDeliveryFileCleanup(ctx, cfg)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		row, err = reopened.DeliveryFileOutcomeByPath(ctx, receiptID, extraName)
		sidecarRow, err = reopened.DeliveryFileOutcomeByPath(ctx, receiptID, extraSidecar)
		if err == nil && row.CleanupState == "removed" && sidecarRow.CleanupState == "removed" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || row.CleanupState != "removed" {
		t.Fatalf("scheduled worker did not remove committed pack excess: row=%+v err=%v", row, err)
	}
	sidecarRow, err = reopened.DeliveryFileOutcomeByPath(ctx, receiptID, extraSidecar)
	if err != nil || sidecarRow.CleanupState != "removed" {
		t.Fatalf("scheduled worker did not remove the exact committed pack sidecar: row=%+v err=%v", sidecarRow, err)
	}
	if _, err := os.Stat(filepath.Join(publication, extraName)); !os.IsNotExist(err) {
		t.Fatalf("committed out-of-scope pack file remains after worker cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(publication, extraSidecar)); !os.IsNotExist(err) {
		t.Fatalf("committed out-of-scope pack sidecar remains after worker cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(publication, unmatchedSidecar)); err != nil {
		t.Fatalf("unmatched sidecar was removed without exact media association: %v", err)
	}
	unmatchedRow, err = reopened.DeliveryFileOutcomeByPath(ctx, receiptID, unmatchedSidecar)
	if err != nil || unmatchedRow.CleanupState != "protected" || unmatchedRow.Disposition != "known_incidental" {
		t.Fatalf("unmatched sidecar lost its protected review state: %+v err=%v", unmatchedRow, err)
	}
}

func mustDeliveryReceipt(t *testing.T, db *sqlite.DB, receiptID int64) sqlite.DeliveryReceipt {
	t.Helper()
	r, err := db.DeliveryReceiptByID(context.Background(), receiptID)
	if err != nil {
		t.Fatal(err)
	}
	return r
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
