package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestManualAdditionalResolveImportsOnlySelectedMediaSidecarsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	mainSubtitle := strings.TrimSuffix(epRelease, ".mkv") + ".en.srt"
	secondMedia := "Test.Show.S01E02.1080p.WEB-DL.x264-GRP.mkv"
	secondSubtitle := strings.TrimSuffix(secondMedia, ".mkv") + ".en.srt"
	mainSubtitleBytes := []byte("subtitle for episode one")
	secondSubtitleBytes := []byte("subtitle for episode two")
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{
		mainSubtitle:   mainSubtitleBytes,
		secondMedia:    []byte("second episode media"),
		secondSubtitle: secondSubtitleBytes,
	})
	copyID, copyRoot := configureManualAdditional(t, ctx, db, &dl, publication, []string{"episode:" + itoa(dl.MediaItemID) + ":1:1"}, []string{filepath.Join(publication, epRelease+".mkv")})
	receiptID := receiptIDFor(t, db, dl.ID)
	var err error
	// Resolve records the exact selected media before a worker claims it. The
	// matching subtitle must travel with that selection after a process restart,
	// while the other episode and its subtitle remain untouched.
	if err = svc.QueueDeliveryImport(ctx, receiptID, []string{epRelease + ".mkv"}); err == nil || !strings.Contains(err.Error(), "worker is unavailable") {
		t.Fatalf("queue without a worker should preserve the request and report the unavailable worker: %v", err)
	}
	persistedDownload, err := db.GetDownload(ctx, dl.ID)
	if err != nil || len(persistedDownload.Handoff) == 0 || persistedDownload.Handoff[len(persistedDownload.Handoff)-1].Step != stepManualQueued || !strings.EqualFold(filepath.Base(persistedDownload.Handoff[len(persistedDownload.Handoff)-1].Paths[0]), epRelease+".mkv") {
		t.Fatalf("Resolve selection was not durably recorded before restart: %+v err=%v", persistedDownload.Handoff, err)
	}
	dataDir := filepath.Dir(db.Path)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := New(db, nil, nil, nil, func(ports.ClientConfig) ports.DownloadClient { return protocol })
	restarted.importCh = make(chan importJob, 2)
	restarted.importCtx = ctx
	restarted.importJobs = map[int64]*importTicket{}
	client, err := db.GetDownloadClient(ctx, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	restarted.scheduleDeliveryReceipts(ctx, client, protocol)
	var job importJob
	select {
	case job = <-restarted.importCh:
	default:
		t.Fatal("startup-style delivery sweep did not enqueue the persisted Resolve selection")
	}
	if job.deliveryReceiptID != receiptID || job.dl.CopyID != copyID || job.dl.Intent != "manual_additional" {
		t.Fatalf("recovered job lost receipt or manual-copy identity: %+v", job)
	}
	if err = restarted.runDeliveryImport(ctx, job.dl, job.cfg, receiptID); err != nil {
		t.Fatalf("recovered selected media import failed: %v", err)
	}
	assertSidecarOutcomes(t, ctx, db, receiptID, map[string]bool{
		epRelease + ".mkv": true,
		mainSubtitle:       true,
		secondMedia:        false,
		secondSubtitle:     false,
	})
	assertOutcomePlacedBytes(t, ctx, db, receiptID, mainSubtitle, mainSubtitleBytes)
	if _, err = os.Stat(filepath.Join(copyRoot, secondMedia)); !os.IsNotExist(err) {
		t.Fatalf("unselected sibling media was imported into manual additional copy: stat err=%v", err)
	}
	if _, err = os.Stat(filepath.Join(copyRoot, secondSubtitle)); !os.IsNotExist(err) {
		t.Fatalf("unselected sibling sidecar was imported into manual additional copy: stat err=%v", err)
	}
	if _, err = os.Stat(filepath.Join(publication, secondMedia)); err != nil {
		t.Fatalf("unselected sibling source was unexpectedly removed during import: %v", err)
	}
	if _, err = os.Stat(filepath.Join(publication, secondSubtitle)); err != nil {
		t.Fatalf("unselected sibling sidecar source was unexpectedly removed during import: %v", err)
	}
}

func TestManualAdditionalWithoutExplicitSelectionImportsAndCleansMatchedSubtitle(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	caption := []byte("manual subtitle payload")
	subtitleName := strings.TrimSuffix(epRelease, ".mkv") + ".en.srt"
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{subtitleName: caption})
	_, copyRoot := configureManualAdditional(t, ctx, db, &dl, publication, []string{"episode:" + itoa(dl.MediaItemID) + ":1:1"}, nil)
	receiptID := receiptIDFor(t, db, dl.ID)
	receipt, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := svc.verifyDeliveryManifest(ctx, cfg, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("normal manual-additional import without a Resolve selection failed: %v", err)
	}
	assertSidecarOutcomes(t, ctx, db, receiptID, map[string]bool{epRelease + ".mkv": true, subtitleName: true})
	assertOutcomePlacedBytes(t, ctx, db, receiptID, subtitleName, caption)
	outcome, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, subtitleName)
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(copyRoot, outcome.LibraryPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatalf("manual-additional subtitle was placed outside its copy: outcome=%+v relative=%q err=%v", outcome, relative, err)
	}
	if _, err := os.Stat(outcome.LibraryPath); err != nil {
		t.Fatalf("manual-additional subtitle target is missing: outcome=%+v err=%v", outcome, err)
	}
	replay, err := svc.importDeliverySidecars(ctx, receipt, verified, ImportResult{})
	if err != nil || replay.Imported != 1 || len(replay.Files) != 1 || !replay.Files[0].Imported {
		rows, _ := db.DeliveryFileOutcomes(ctx, receiptID)
		t.Fatalf("replaying the already committed media/subtitle mapping was not idempotent: result=%+v verified=%+v ledger=%+v err=%v", replay, verified, rows, err)
	}
	if err := svc.recordDeliveryImportOutcomes(ctx, receipt, verified, replay); err != nil {
		t.Fatalf("persist idempotent sidecar replay outcome: %v", err)
	}
	assertOutcomePlacedBytes(t, ctx, db, receiptID, subtitleName, caption)
	receipt, err = db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, receipt); err != nil {
		t.Fatalf("cleanup of successfully imported manual-additional payload failed: %v", err)
	}
	outcome, err = db.DeliveryFileOutcomeByPath(ctx, receiptID, subtitleName)
	if err != nil || outcome.CleanupState != "removed" {
		t.Fatalf("matched subtitle cleanup state=%q err=%v", outcome.CleanupState, err)
	}
	if _, err := os.Stat(filepath.Join(publication, subtitleName)); !os.IsNotExist(err) {
		t.Fatalf("matched imported subtitle remained in completed payload: stat err=%v", err)
	}
}

func TestAcceptedSidecarPublishesExistingVerifiedStageAfterRestart(t *testing.T) {
	caption := []byte("sidecar stage survived process exit")
	ctx, _, db, _, _, receipt, media, sidecar, result := prepareSidecarMedia(t, caption)
	if result.Imported != 1 || len(result.Files) != 1 || !result.Files[0].Imported {
		t.Fatalf("fixture media was not committed: %+v", result)
	}
	target := acceptedSidecarTarget(t, media.Path, media.SourcePath, sidecar.relative)
	stageName := expectedCaptionStagePath(target)
	if err := os.WriteFile(stageName, caption, 0o600); err != nil {
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
	defer func() { _ = reopened.Close() }()
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc := New(reopened, nil, nil, nil, nil)
	receipt, err = reopened.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err = svc.importDeliverySidecars(ctx, receipt, []verifiedDeliveryFile{sidecar}, ImportResult{})
	if err != nil || result.Imported != 1 || len(result.Files) != 1 {
		t.Fatalf("retry did not publish the verified deterministic stage: result=%+v err=%v", result, err)
	}
	if err := svc.recordDeliveryImportOutcomes(ctx, receipt, []verifiedDeliveryFile{sidecar}, result); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stageName); !os.IsNotExist(err) {
		t.Fatalf("successfully published stage file was not removed: %v", err)
	}
	got, size, err := fileDigest(ctx, target)
	if err != nil || size != int64(len(caption)) || got != sha256Hex(caption) {
		t.Fatalf("recovered sidecar target identity mismatch: sha256=%s size=%d err=%v", got, size, err)
	}
	assertOutcomePlacedBytes(t, ctx, reopened, receipt.ID, sidecar.relative, caption)
}

func TestAcceptedSidecarFinalPublishRejectsReplacedMediaDirectory(t *testing.T) {
	caption := []byte("sidecar must not follow a replaced directory")
	ctx, svc, _, _, _, receipt, media, sidecar, _ := prepareSidecarMedia(t, caption)
	mediaInfo, err := os.Lstat(media.Path)
	if err != nil {
		t.Fatal(err)
	}
	parentPath := filepath.Dir(media.Path)
	parentInfo, err := os.Lstat(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	target := acceptedSidecarTarget(t, media.Path, media.SourcePath, sidecar.relative)
	stageName := expectedCaptionStagePath(target)
	backup := parentPath + ".reviewed-before-replacement"
	if err := os.Rename(parentPath, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(parentPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stageName, caption, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.publishAcceptedSidecar(ctx, receipt, sidecar, media.Path, target, stageName, parentInfo, mediaInfo); err == nil {
		t.Fatal("final publish accepted a replacement directory at the reviewed media path")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("sidecar was published through a replaced directory: %v", err)
	}
	if _, err := os.Stat(stageName); err != nil {
		t.Fatalf("failed publish destroyed the verified recovery stage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(backup, filepath.Base(media.Path))); err != nil {
		t.Fatalf("original reviewed media directory was not preserved: %v", err)
	}
}

func prepareSidecarMedia(t *testing.T, caption []byte) (context.Context, *Service, *sqlite.DB, sqlite.Download, ports.ClientConfig, sqlite.DeliveryReceipt, FileOutcome, verifiedDeliveryFile, ImportResult) {
	t.Helper()
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	subtitleName := strings.TrimSuffix(epRelease, ".mkv") + ".en.srt"
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{subtitleName: caption})
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	r := receipts[0]
	verified, err := svc.verifyDeliveryManifest(ctx, cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	var media, sidecar verifiedDeliveryFile
	manifest := make([]sqlite.DeliveryManifestFile, 0, len(verified))
	for _, file := range verified {
		manifest = append(manifest, sqlite.DeliveryManifestFile{Path: file.relative, SizeBytes: file.size, SHA256: file.digest, FileGeneration: file.generation})
		if filename.IsVideo(file.relative) || filename.IsBook(file.relative) {
			media = file
		}
		if file.relative == subtitleName {
			sidecar = file
		}
	}
	if media.relative == "" || sidecar.relative == "" {
		t.Fatalf("fixture lacks associated media or subtitle: %+v", verified)
	}
	if err := db.EnsureDeliveryFileOutcomes(ctx, r.ID, manifest, nil, false); err != nil {
		t.Fatal(err)
	}
	mediaResult, err := svc.importDownloadFiles(ctx, dl, publication, []string{media.absolute}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.recordDeliveryImportOutcomes(ctx, r, verified, mediaResult); err != nil {
		t.Fatal(err)
	}
	var mediaOutcome FileOutcome
	for _, outcome := range mediaResult.Files {
		if outcome.Imported && outcome.Path != "" && (filename.IsVideo(outcome.SourcePath) || filename.IsBook(outcome.SourcePath)) {
			mediaOutcome = outcome
			break
		}
	}
	if mediaOutcome.Path == "" {
		t.Fatalf("fixture media import has no library target: %+v", mediaResult)
	}
	return ctx, svc, db, dl, cfg, r, mediaOutcome, sidecar, mediaResult
}

func acceptedSidecarTarget(t *testing.T, mediaPath, mediaSourcePath, sidecarRelative string) string {
	t.Helper()
	mediaSourceStem := strings.TrimSuffix(filepath.Base(mediaSourcePath), filepath.Ext(mediaSourcePath))
	sidecarStem := strings.TrimSuffix(filepath.Base(sidecarRelative), filepath.Ext(sidecarRelative))
	if !strings.HasPrefix(strings.ToLower(sidecarStem), strings.ToLower(mediaSourceStem)+".") {
		t.Fatalf("fixture sidecar %q does not match imported source media %q", sidecarRelative, mediaSourcePath)
	}
	suffix := sidecarStem[len(mediaSourceStem):]
	mediaStem := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	targetName := mediaStem + suffix + filepath.Ext(sidecarRelative)
	return filepath.Join(filepath.Dir(mediaPath), targetName)
}

func expectedCaptionStagePath(target string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(target)))
	return filepath.Join(filepath.Dir(target), ".monarr-caption-"+hex.EncodeToString(digest[:12])+".stage")
}

func configureManualAdditional(t *testing.T, ctx context.Context, db *sqlite.DB, dl *sqlite.Download, publication string, wantables, queuedPaths []string) (int64, string) {
	t.Helper()
	item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil {
		t.Fatal(err)
	}
	copyRoot := filepath.Join(t.TempDir(), "manual-additional-copy")
	if err = os.MkdirAll(copyRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	copyID, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: item.ID, Name: "Manual additional", QualityProfileID: item.QualityProfileID,
		Path: copyRoot, Monitored: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	dl.CopyID, dl.Intent, dl.State, dl.Season = copyID, "manual_additional", "downloaded", 1
	dl.WantableIDs = wantables
	dl.ImportPath, dl.SavePath = publication, "/remote/completed/"+epRelease
	if len(queuedPaths) > 0 {
		dl.Handoff = []sqlite.HandoffEntry{{Step: stepManualQueued, At: time.Now().UnixMilli(), Paths: queuedPaths}}
	}
	if err = db.UpdateDownloadTarget(ctx, *dl); err != nil {
		t.Fatal(err)
	}
	if len(queuedPaths) > 0 {
		if err = db.UpdateDownloadHandoff(ctx, *dl); err != nil {
			t.Fatal(err)
		}
	}
	wantableJSON, err := json.Marshal(wantables)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE downloads SET intent='manual_additional',season=?,wantables=? WHERE id=?`, dl.Season, string(wantableJSON), dl.ID); err != nil {
		t.Fatal(err)
	}
	receiptID := receiptIDFor(t, db, dl.ID)
	if _, err = db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET copy_id=? WHERE id=?`, copyID, receiptID); err != nil {
		t.Fatal(err)
	}
	return copyID, copyRoot
}

func assertSidecarOutcomes(t *testing.T, ctx context.Context, db *sqlite.DB, receiptID int64, wantPlaced map[string]bool) {
	t.Helper()
	rows, err := db.DeliveryFileOutcomes(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		want, exists := wantPlaced[row.Path]
		if !exists {
			continue
		}
		seen[row.Path] = true
		placed := row.Disposition == "placed" && row.LibraryPath != ""
		if placed != want {
			t.Errorf("manifest path %q disposition=%q library=%q, placed=%v; want placed=%v", row.Path, row.Disposition, row.LibraryPath, placed, want)
		}
	}
	for path := range wantPlaced {
		if !seen[path] {
			t.Errorf("manifest path %q has no durable outcome", path)
		}
	}
}

func assertOutcomePlacedBytes(t *testing.T, ctx context.Context, db *sqlite.DB, receiptID int64, relative string, content []byte) {
	t.Helper()
	row, err := db.DeliveryFileOutcomeByPath(ctx, receiptID, relative)
	if err != nil || row.Disposition != "placed" || row.LibraryPath == "" {
		t.Fatalf("placed sidecar ledger for %q=%+v err=%v", relative, row, err)
	}
	digest, size, err := fileDigest(ctx, row.LibraryPath)
	if err != nil || size != int64(len(content)) || digest != sha256Hex(content) {
		t.Fatalf("placed sidecar %q digest=%s size=%d err=%v", relative, digest, size, err)
	}
}
