package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func deliveryRedundancyFixture(t *testing.T, sourceFixture, targetFixture string) (*Service, *sqlite.DB, context.Context, sqlite.DeliveryReceipt, verifiedDeliveryFile, int64, string) {
	t.Helper()
	ctx := context.Background()
	svc, db, dl, cfg, publication := deliveryImportFixture(t, &deliveryImportClient{})
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	receipt := receipts[0]
	verified, err := svc.verifyDeliveryManifest(ctx, cfg, receipt)
	if err != nil || len(verified) != 1 {
		t.Fatalf("verify manifest=%+v err=%v", verified, err)
	}
	source := verified[0]
	mediaDir := filepath.Join("..", "..", "domain", "mediainfo", "testdata")
	sourceBytes, err := os.ReadFile(filepath.Join(mediaDir, sourceFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.absolute, sourceBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(source.absolute)
	if err != nil {
		t.Fatal(err)
	}
	source.size, source.digest, source.generation, source.info = int64(len(sourceBytes)), sha256Hex(sourceBytes), deliveryFileGeneration(sourceInfo), sourceInfo
	if err := db.EnsureDeliveryFileOutcomes(ctx, receipt.ID, []sqlite.DeliveryManifestFile{{
		Path: source.relative, SizeBytes: source.size, SHA256: source.digest, FileGeneration: source.generation,
	}}, nil, false); err != nil {
		t.Fatal(err)
	}

	item, err := db.GetMediaItemFull(ctx, receipt.MediaItemID)
	if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) == 0 {
		t.Fatalf("media item fixture=%+v err=%v", item, err)
	}
	targetBytes, err := os.ReadFile(filepath.Join(mediaDir, targetFixture))
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(item.Path, "already-indexed.mkv")
	if err := os.WriteFile(targetPath, targetBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, item.ID, receipt.CopyID, targetPath, int64(len(targetBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{item.Seasons[0].Episodes[0].ID}); err != nil {
		t.Fatal(err)
	}
	indexedQuality := quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}
	if err := db.SetFileQualityFrom(ctx, fileID, indexedQuality, mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	return svc, db, ctx, receipt, source, fileID, publication
}

func TestDeliveryRedundancyDoesNotDiscardManifestWithUnmetAudioLanguage(t *testing.T) {
	svc, db, ctx, receipt, source, _, publication := deliveryRedundancyFixture(t,
		"mkv-1080p-h264-ac3-ger-whole.mkv", "mkv-1080p-h264-ac3-whole.mkv")
	defer func() { _ = db.Close() }()
	item, err := db.GetMediaItemFull(ctx, receipt.MediaItemID)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := db.GetProfile(ctx, item.QualityProfileID)
	if err != nil {
		t.Fatal(err)
	}
	profile.Languages = []string{"en"}
	if err := db.UpdateProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	result := ImportResult{Files: []FileOutcome{{SourcePath: source.absolute, Reason: "candidate was not placed"}}}
	if err := svc.recordDeliveryImportOutcomes(ctx, receipt, []verifiedDeliveryFile{source}, result); err != nil {
		t.Fatal(err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, source.relative)
	if err != nil || row.Disposition != "unknown_recoverable" || row.CleanupState != "protected" {
		t.Fatalf("audio-incompatible candidate was treated as redundant: row=%+v err=%v", row, err)
	}
	if err := svc.removeVerifiedDeliveryFile(ctx, receipt, row); err == nil {
		t.Fatal("cleanup discarded an audio-incompatible manifest file")
	}
	if _, err := os.Stat(filepath.Join(publication, filepath.FromSlash(source.relative))); err != nil {
		t.Fatalf("unresolved manifest source was removed: %v", err)
	}
}

func TestDeliveryRedundancyDoesNotTrustFileAlreadySelectedAsBad(t *testing.T) {
	fixture := "mkv-1080p-h264-ac3-ger-whole.mkv"
	svc, db, ctx, receipt, source, fileID, publication := deliveryRedundancyFixture(t, fixture, fixture)
	defer func() { _ = db.Close() }()
	files, err := db.ListFilesForItem(ctx, receipt.MediaItemID)
	if err != nil {
		t.Fatal(err)
	}
	var indexed domain.MediaFile
	for _, file := range files {
		if file.ID == fileID {
			indexed = file
			break
		}
	}
	if indexed.ID == 0 || indexed.FileGeneration == "" {
		t.Fatalf("indexed file generation unavailable: %+v", indexed)
	}
	if _, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
		ID: "delivery-redundancy-bad-file", RequestID: "delivery-redundancy-bad-file-request",
		Fingerprint: "delivery-redundancy-bad-file-fingerprint", State: "accepted", Intent: "replace_bad",
		Scope: "download", MediaItemID: receipt.MediaItemID, CopyID: receipt.CopyID,
		ReasonCode: "bad_media", Reason: "existing exact byte copy was already reported bad", CreatedAt: time.Now(),
	}, []sqlite.ReplacementFile{{
		MediaFileID: indexed.ID, FileGeneration: indexed.FileGeneration, SourceGeneration: indexed.SourceGeneration,
		Path: indexed.Path, Size: indexed.Size, Disposition: "selected", RemovalState: "pending",
	}}); err != nil {
		t.Fatalf("persist active bad-file selection: %v", err)
	}
	if err := svc.recordDeliveryImportOutcomes(ctx, receipt, []verifiedDeliveryFile{source}, ImportResult{
		Files: []FileOutcome{{SourcePath: source.absolute, Reason: "candidate was not placed"}},
	}); err != nil {
		t.Fatal(err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, source.relative)
	if err != nil || row.Disposition != "unknown_recoverable" || row.CleanupState != "protected" {
		t.Fatalf("replacement-target bytes were accepted as healthy redundancy: row=%+v err=%v", row, err)
	}
	if err := svc.removeVerifiedDeliveryFile(ctx, receipt, row); err == nil {
		t.Fatal("cleanup discarded a manifest file matching a library generation selected as bad")
	}
	if _, err := os.Stat(filepath.Join(publication, filepath.FromSlash(source.relative))); err != nil {
		t.Fatalf("manifest file selected for recovery was removed: %v", err)
	}
}
