package acquisition

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
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
	ctx = context.Background()
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
	receipt, err := db.DeliveryReceiptByID(ctx, receiptID)
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
