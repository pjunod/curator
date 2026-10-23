package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/filename"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type advancingDeliveryClient struct {
	fakeClient
	after []uint64
}

func (c *advancingDeliveryClient) ListDeliveries(_ context.Context, after uint64, _ int) (ports.RunnerDeliveryPage, error) {
	c.after = append(c.after, after)
	return ports.RunnerDeliveryPage{ProtocolVersion: 1, NextCursor: 5}, nil
}
func (c *advancingDeliveryClient) AcceptDelivery(context.Context, string, ports.RunnerDeliveryAcceptance) error {
	return nil
}

func TestManualDeliveryRetryKeepsOnlyUnresolvedVerifiedSelection(t *testing.T) {
	r := sqlite.DeliveryReceipt{
		MappedPath: "/completed/Release",
		Delivery: ports.RunnerDelivery{Files: []ports.RunnerFile{
			{Path: "S01E01.mkv"},
			{Path: "S01E02.mkv"},
		}},
	}
	prior := map[string]sqlite.DeliveryFileOutcome{
		"S01E01.mkv": {Path: "S01E01.mkv", Disposition: "placed", CleanupState: "removed"},
		"S01E02.mkv": {Path: "S01E02.mkv", Disposition: "unselected_recoverable"},
	}
	selected := []string{
		filepath.Join(r.MappedPath, "S01E01.mkv"),
		filepath.Join(r.MappedPath, "S01E02.mkv"),
	}
	if manualSelectionAlreadyResolved(selected[:1], r, prior) != true {
		t.Fatal("previously placed and cleaned selection should be considered satisfied")
	}
	if manualSelectionAlreadyResolved(selected, r, prior) {
		t.Fatal("partially resolved selection must still retry its unresolved sibling")
	}
}

func TestDeliverySuccessCleanupAndRestartReplayDoesNotPlaceTwice(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptIDFor(t, db, dl.ID)); err != nil {
		t.Fatalf("initial delivery import: %v", err)
	}
	receiptID := receiptIDFor(t, db, dl.ID)
	receipt, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatalf("delivery receipt=%+v err=%v", receipt, err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, receipt); err != nil {
		t.Fatalf("cleanup accepted publication: %v", err)
	}
	if _, err := os.Stat(filepath.Join(publication, epRelease+".mkv")); !os.IsNotExist(err) {
		t.Fatalf("resolved publication file remains after cleanup: %v", err)
	}
	filesBeforeRestart, err := db.ListFilesForItem(ctx, dl.MediaItemID)
	if err != nil || len(filesBeforeRestart) != 1 {
		t.Fatalf("placed library files=%+v err=%v", filesBeforeRestart, err)
	}
	libraryPath := filesBeforeRestart[0].Path
	if _, err := os.Stat(libraryPath); err != nil {
		t.Fatalf("library placement missing before restart: %v", err)
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
	protocol.deliveries = []ports.RunnerDelivery{receipt.Delivery}
	restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return protocol })
	restarted.discoverRunnerDeliveries(ctx, cfg, protocol)
	// Replayed ready state is a no-op after durable import+cleanup. This is the
	// path used when an old queue/history notification is observed after restart.
	if err := restarted.runDeliveryImport(ctx, dl, cfg, receipt.ID); err != nil {
		t.Fatalf("replayed delivery after restart: %v", err)
	}
	if _, err := os.Stat(libraryPath); err != nil {
		t.Fatalf("replay removed or replaced imported media: %v", err)
	}
	files, err := reopened.DeliveryFileOutcomes(ctx, receipt.ID)
	if err != nil || len(files) != 1 || files[0].Disposition != "placed" || files[0].CleanupState != "removed" {
		t.Fatalf("durable file outcome changed on replay: %+v err=%v", files, err)
	}
	var placements int
	if err := reopened.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_file_placements WHERE source_generation=? AND source_payload_generation=?`, dl.GenerationKey, "pub-1").Scan(&placements); err != nil || placements != 1 {
		t.Fatalf("placement journal has %d rows after replay, want exactly one: err=%v", placements, err)
	}
	events, err := reopened.DeliveryAttemptEvents(ctx, receipt.ID)
	if err != nil || len(events) < 4 {
		t.Fatalf("durable import/ack/cleanup attempt history lost on restart: %+v err=%v", events, err)
	}
}

func TestImportedDeliveryCanReopenOnlySelectedUnresolvedFileAfterRestart(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{failCount: 100}
	second := "Test.Show.S01E02.720p.WEB-DL.x264.mkv"
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{second: []byte("episode two")})
	receiptID := receiptIDFor(t, db, dl.ID)
	dl.Handoff = append(dl.Handoff, sqlite.HandoffEntry{Step: stepManualQueued, At: time.Now().UnixMilli(), Paths: []string{filepath.Join(dl.ImportPath, epRelease+".mkv")}})
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("initial import: %v", err)
	}
	rows, err := db.DeliveryFileOutcomes(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	var sibling sqlite.DeliveryFileOutcome
	for _, row := range rows {
		if row.Path == second {
			sibling = row
		}
	}
	if sibling.Path == "" || sibling.Disposition == "placed" {
		t.Fatalf("fixture did not leave episode two as a recoverable sibling: %+v", sibling)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, r); err != nil {
		t.Fatalf("clean first committed placement before resolving its sibling: %v", err)
	}
	if _, err := os.Stat(filepath.Join(publication, second)); err != nil {
		t.Fatalf("unresolved sibling was removed with the prior placement: %v", err)
	}
	if err := db.DeleteDownload(ctx, dl.ID); err != nil {
		t.Fatalf("dismiss Activity while retaining unresolved delivery custody: %v", err)
	}
	trace, err := svc.DeliveryLifecycleByID(ctx, receiptID)
	if err != nil || trace.ReceiptID != receiptID || len(trace.ManifestFiles) != 2 {
		t.Fatalf("Resolve could not reopen the durable receipt after Activity dismissal: receipt=%+v err=%v", trace, err)
	}

	// Queue with no running worker: the request and reopened receipt must both
	// be durable before the call reports that no in-memory queue is available.
	if err := svc.QueueDeliveryImport(ctx, receiptID, []string{second}); err == nil {
		t.Fatal("expected the stopped service to report that no import worker is available")
	}
	reopenedDir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(reopenedDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := reopened.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || current.State != "ready" || current.AckState != "pending" || current.ConsumerReceiptID == "" {
		t.Fatalf("selected unresolved delivery obligation was not durably reopened: %+v err=%v", current, err)
	}
	currentDL, err := reopened.GetDownload(ctx, dl.ID)
	if err != nil || !hasQueuedDeliverySelection(currentDL) || len(currentDL.Handoff[len(currentDL.Handoff)-1].Paths) != 1 {
		t.Fatalf("exact selected paths were not persisted with the reopened receipt: %+v err=%v", currentDL.Handoff, err)
	}

	restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return protocol })
	workerCtx, cancel := context.WithCancel(ctx)
	restarted.StartImporters(workerCtx)
	defer func() { cancel(); restarted.WaitImporters() }()
	restarted.scheduleDeliveryReceipts(ctx, cfg, protocol)
	waitFor(t, func() bool {
		current, err := reopened.DeliveryReceiptByID(ctx, receiptID)
		return err == nil && current.State == "imported"
	})
	imports, err := reopened.DeliveryFileImports(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	var siblingTarget string
	for _, imported := range imports {
		if imported.Path == second {
			siblingTarget = imported.LibraryPath
		}
	}
	if siblingTarget == "" {
		t.Fatalf("restarted worker did not persist the selected sibling's placement: %+v", imports)
	}
	if _, err := os.Stat(siblingTarget); err != nil {
		t.Fatalf("restarted worker did not import exact selected sibling at %s: %v", siblingTarget, err)
	}
	rows, err = reopened.DeliveryFileOutcomes(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Path == second && row.Disposition != "placed" {
			t.Fatalf("selected sibling did not acquire a committed outcome: %+v", row)
		}
		if row.Path != second && row.CleanupState == "removed" && row.Disposition != "placed" {
			t.Fatalf("reopening selected sibling rewrote prior placement/cleanup evidence: %+v", row)
		}
	}
}

func TestDeliveryResolveReloadsReceiptAfterWaitingForImporterLock(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{failCount: 100}
	second := "Test.Show.S01E02.720p.WEB-DL.x264.mkv"
	svc, db, dl, cfg, _ := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{second: []byte("episode two")})
	receiptID := receiptIDFor(t, db, dl.ID)
	dl.Handoff = append(dl.Handoff, sqlite.HandoffEntry{Step: stepManualQueued, At: time.Now().UnixMilli(), Paths: []string{filepath.Join(dl.ImportPath, epRelease+".mkv")}})
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatal(err)
	}
	// Present Resolve with a stale ready snapshot while it waits behind the
	// per-download import lock. The importer completes before Resolve acquires
	// it; Resolve must then observe imported and reopen only the selected
	// unresolved sibling.
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='ready' WHERE id=?`, receiptID); err != nil {
		t.Fatal(err)
	}
	unlock := svc.lockDownload(dl.ID)
	done := make(chan error, 1)
	go func() { done <- svc.QueueDeliveryImport(ctx, receiptID, []string{second}) }()
	time.Sleep(40 * time.Millisecond)
	if err := db.UpdateDownloadState(ctx, dl.ID, "imported", 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDeliveryImported(ctx, receiptID); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err == nil {
		t.Fatal("expected the stopped importer queue to report unavailable after durable reopen")
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.State != "ready" {
		t.Fatalf("Resolve used stale receipt state instead of reopening completed import: %+v err=%v", r, err)
	}
	fresh, err := db.GetDownload(ctx, dl.ID)
	if err != nil || fresh.State != "awaiting_import" || len(fresh.Handoff) == 0 || fresh.Handoff[len(fresh.Handoff)-1].Paths[0] != filepath.Join(dl.ImportPath, second) {
		t.Fatalf("exact unresolved selection was not reopened: %+v err=%v", fresh, err)
	}
}

func TestDeliveryCleanupWaitsForTerminalImportStamp(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatal(err)
	}
	// This models a crash after the per-file placement ledger committed but
	// before the receipt terminal state was stamped.
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET state='ready' WHERE id=?`, receiptID); err != nil {
		t.Fatal(err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(publication, epRelease+".mkv")); err != nil {
		t.Fatalf("pre-terminal cleanup removed the source required for restart replay: %v", err)
	}
	if _, err := svc.verifyDeliveryManifest(ctx, cfg, r); err != nil {
		t.Fatalf("replay could not re-open the accepted publication before terminal stamp: %v", err)
	}
}

func TestDeliveryLifecycleHistoryKeepsRunningAndFinishedAttemptsDistinct(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, _, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	running, err := db.BeginDeliveryAttempt(ctx, receiptID, "import")
	if err != nil {
		t.Fatal(err)
	}
	finished, err := db.BeginDeliveryAttempt(ctx, receiptID, "ack")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishDeliveryAttempt(ctx, finished.ID, "succeeded", ""); err != nil {
		t.Fatal(err)
	}
	trace, err := svc.DeliveryLifecycleByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.AttemptHistory) != 2 {
		t.Fatalf("attempt history = %+v", trace.AttemptHistory)
	}
	if trace.AttemptHistory[0].Result != "running" || !trace.AttemptHistory[0].FinishedAt.IsZero() {
		t.Fatalf("unfinished attempt was presented as completed: %+v", trace.AttemptHistory[0])
	}
	if trace.AttemptHistory[1].Result != "succeeded" || trace.AttemptHistory[1].FinishedAt.IsZero() || trace.AttemptHistory[1].FinishedAt.Before(trace.AttemptHistory[1].StartedAt) {
		t.Fatalf("finished attempt has invalid timing: %+v", trace.AttemptHistory[1])
	}
	if running.ID == finished.ID || running.Attempt != 1 || finished.Attempt != 1 {
		t.Fatalf("per-stage attempt numbering was not independent: running=%+v finished=%+v", running, finished)
	}
}

func TestDeliveryDiskFullRetriesSameManifestAfterDatabaseRestart(t *testing.T) {
	ctx := context.Background()
	subtitle := strings.TrimSuffix(epRelease, ".mkv") + ".en.srt"
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{subtitle: []byte("English subtitles")})
	receiptID := receiptIDFor(t, db, dl.ID)
	previous := placeFile
	failed := false
	placeFile = func(ctx context.Context, source, target string, progress func(done, total int64)) error {
		if !failed && strings.EqualFold(filepath.Ext(source), ".mkv") {
			failed = true
			return syscall.ENOSPC
		}
		return previous(ctx, source, target, progress)
	}
	t.Cleanup(func() { placeFile = previous })
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err == nil || !strings.Contains(strings.ToLower(err.Error()), "space") {
		t.Fatalf("first attempt should fail with disk full, got %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.State == "imported" {
		t.Fatalf("disk-full attempt unexpectedly completed: receipt=%+v err=%v", r, err)
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
	if err := reopened.ForceDeliveryRetryNow(ctx, receiptID); err != nil {
		t.Fatal(err)
	}
	started := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return protocol })
	started.newClient = func(ports.ClientConfig) ports.DownloadClient { return protocol }
	workerCtx, cancel := context.WithCancel(ctx)
	started.StartImporters(workerCtx)
	defer func() { cancel(); started.WaitImporters() }()
	started.scheduleDeliveryReceipts(ctx, cfg, protocol)
	waitFor(t, func() bool {
		current, err := reopened.DeliveryReceiptByID(ctx, receiptID)
		return err == nil && current.State == "imported"
	})
	imports, err := reopened.DeliveryFileImports(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	placedPath := ""
	subtitlePath := ""
	for _, imported := range imports {
		if imported.Path == epRelease+".mkv" && imported.LibraryPath != "" {
			placedPath = imported.LibraryPath
		}
		if imported.Path == subtitle && imported.LibraryPath != "" {
			subtitlePath = imported.LibraryPath
		}
	}
	if placedPath == "" {
		t.Fatalf("verified media placement was not recorded after retry: %+v", imports)
	}
	if _, err := os.Stat(placedPath); err != nil {
		t.Fatalf("verified media was not placed after retry at %s: %v", placedPath, err)
	}
	if subtitlePath == "" {
		t.Fatalf("verified sibling sidecar placement was not recorded after retry: %+v", imports)
	}
	if _, err := os.Stat(subtitlePath); err != nil {
		t.Fatalf("verified sibling sidecar was not placed after retry: %v", err)
	}
	files, err := reopened.DeliveryFileOutcomes(ctx, receiptID)
	if err != nil || len(files) != 2 {
		t.Fatalf("delivery file ledger=%+v err=%v", files, err)
	}
	for _, file := range files {
		if file.Disposition != "placed" {
			t.Errorf("verified selected file %q disposition=%q after retry", file.Path, file.Disposition)
		}
	}
	if _, err := os.Stat(filepath.Join(publication, epRelease+".mkv")); err != nil {
		t.Fatalf("source should remain until independent cleanup worker runs: %v", err)
	}
}

func receiptIDFor(t *testing.T, db *sqlite.DB, downloadID int64) int64 {
	t.Helper()
	receipts, _, err := db.DeliveryReceiptPageForDownload(context.Background(), downloadID, 0, 10)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt for download %d: %+v err=%v", downloadID, receipts, err)
	}
	return receipts[0].ID
}

type serializedAckDeliveryClient struct {
	deliveryImportClient
	count   atomic.Int32
	block   atomic.Bool
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (c *serializedAckDeliveryClient) AcceptDelivery(ctx context.Context, id string, acceptance ports.RunnerDeliveryAcceptance) error {
	n := c.count.Add(1)
	if n <= 2 {
		return errors.New("initial acknowledgement outage")
	}
	if c.block.Load() {
		c.once.Do(func() { close(c.entered) })
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func TestConcurrentDeliveryHandoffAndCleanupRetriesAreSerialized(t *testing.T) {
	ctx := context.Background()
	protocol := &serializedAckDeliveryClient{entered: make(chan struct{}), release: make(chan struct{})}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, &protocol.deliveryImportClient)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return protocol }
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("initial locally accepted import: %v", err)
	}
	// Model a previously failed pair of ACK attempts before opening the
	// serialized explicit-retry window below. The current import path makes
	// one due ACK attempt per pass and persists its retry deadline.
	protocol.count.Store(2)
	protocol.block.Store(true)
	results := make(chan error, 2)
	go func() { results <- svc.RetryDeliveryHandoff(ctx, receiptID) }()
	select {
	case <-protocol.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handoff retry did not reach Runner")
	}
	go func() { results <- svc.RetryDeliveryHandoff(ctx, receiptID) }()
	time.Sleep(50 * time.Millisecond)
	if got := protocol.count.Load(); got != 3 {
		t.Fatalf("overlapping handoff retries reached Runner concurrently: calls=%d, want first retry only", got)
	}
	close(protocol.release)
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatalf("handoff retry: %v", err)
		}
	}
	if got := protocol.count.Load(); got != 3 {
		t.Fatalf("accepted handoff was replayed after serialization: calls=%d", got)
	}

	// The cleanup job registry coalesces repeated endpoint retries to one
	// queued worker for this accepted receipt, even when requests race.
	svc.importMu.Lock()
	svc.importCh = make(chan importJob, 8)
	svc.importCtx = ctx
	svc.importJobs = map[int64]*importTicket{}
	svc.importMu.Unlock()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- svc.RetryCuratorDeliveryCleanup(ctx, receiptID) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("cleanup retry: %v", err)
		}
	}
	if got := len(svc.importCh); got != 1 {
		t.Fatalf("concurrent cleanup retries queued %d workers, want one", got)
	}
	if len(svc.deliveryCleanupJobs) != 1 || !svc.deliveryCleanupJobs[receiptID] {
		t.Fatalf("cleanup worker lease not coalesced: %+v", svc.deliveryCleanupJobs)
	}
	for len(svc.importCh) > 0 {
		job := <-svc.importCh
		if job.cleanupCancel != nil {
			job.cleanupCancel()
		}
	}
}

func TestEmptyPublicationCleanupIsBoundToAcceptedDirectoryAcrossRetries(t *testing.T) {
	ctx := context.Background()
	svc, db, _ := setup(t, nil, &fakeClient{})
	root := t.TempDir()
	publication := filepath.Join(root, "Release")
	if err := os.MkdirAll(filepath.Join(publication, "empty", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(ctx, CompletedRootsSetting, root); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	acceptedInfo, err := os.Stat(publication)
	if err != nil {
		t.Fatal(err)
	}
	acceptedIdentity := deliveryDirectoryObjectIdentity(acceptedInfo)
	if acceptedIdentity == "" {
		t.Fatal("platform did not provide a stable directory identity")
	}
	receipt := sqlite.DeliveryReceipt{MappedPath: publication, PublicationDirectoryIdentity: acceptedIdentity}
	// Nested directories emptied by this attempt should not invalidate the
	// accepted payload root; its name is removed after rechecking the same inode.
	removed, err := svc.removeEmptyDeliveryDirectories(ctx, receipt)
	if err != nil || !removed {
		t.Fatalf("empty nested publication cleanup removed=%v err=%v", removed, err)
	}
	if _, err := os.Stat(publication); !os.IsNotExist(err) {
		t.Fatalf("accepted empty publication was not removed: err=%v", err)
	}

	// A later empty directory at the same path is unrelated. Even though the
	// content is empty, a retry must retain it because it lacks the accepted
	// directory inode identity.
	oldPath := publication + ".old"
	if err := os.MkdirAll(publication, 0o755); err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(publication)
	if err != nil {
		t.Fatal(err)
	}
	if deliveryDirectoryObjectIdentity(newInfo) == acceptedIdentity {
		// Keep the old inode allocated if the filesystem rapidly recycled it.
		if err := os.Rename(publication, oldPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(publication, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(publication); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.removeEmptyDeliveryDirectories(ctx, receipt); err == nil {
		t.Fatal("same-path replacement directory inherited cleanup authority")
	}
	if info, err := os.Stat(publication); err != nil || !info.IsDir() {
		t.Fatalf("replacement directory was removed: info=%v err=%v", info, err)
	}
}

func TestExplicitDeliveryDeleteRemovesOnlySelectedUnimportedManifestFile(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	if _, err := svc.verifyDeliveryManifest(ctx, cfg, receipts[0]); err != nil {
		t.Fatalf("verify accepted manifest: %v", err)
	}
	prepared := receipts[0]
	prepared.ConsumerReceiptID = "explicit-delete-test-receipt"
	prepared.NextAttemptAt = time.Now()
	if err := db.PrepareDeliveryAcceptance(ctx, prepared); err != nil {
		t.Fatalf("prepare acceptance: %v", err)
	}
	// Model the authoritative native completion that associates this mapped
	// publication with the Activity row before an operator resolves its files.
	dl.ImportPath = publication
	dl.State = "downloaded"
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatalf("persist mapped completion association: %v", err)
	}
	manifestPath := filepath.Join(publication, epRelease+".mkv")
	if err := os.WriteFile(manifestPath, []byte("current file was modified after publication"), 0o644); err != nil {
		t.Fatal(err)
	}
	pendingReceipt, err := db.DeliveryReceiptByID(ctx, receipts[0].ID)
	if err != nil || pendingReceipt.AckState != "pending" {
		t.Fatalf("expected durable local acceptance pending on Runner outage: %+v err=%v", pendingReceipt, err)
	}
	if err := os.WriteFile(filepath.Join(publication, "unrelated.keep"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	inventory, err := svc.CompletedInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var entry *CompletedEntry
	for i := range inventory.Roots {
		for j := range inventory.Roots[i].Entries {
			if canonicalCompletedPath(inventory.Roots[i].Entries[j].Path) == canonicalCompletedPath(publication) {
				entry = &inventory.Roots[i].Entries[j]
			}
		}
	}
	if entry == nil || !entry.DeliveryManaged {
		t.Fatalf("delivery payload missing from inventory: %+v", inventory)
	}
	var selected CompletedFileSelection
	for _, file := range entry.ReviewFiles {
		if canonicalCompletedPath(file.Path) == canonicalCompletedPath(manifestPath) {
			selected = CompletedFileSelection{Path: file.Path, SnapshotIdentity: file.SnapshotIdentity}
		}
	}
	if selected.Path == "" {
		t.Fatalf("manifest file absent from current inventory: %+v", entry.ReviewFiles)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, entry.Path, entry.Fingerprint, []CompletedFileSelection{selected}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
		t.Fatalf("selected file remains: err=%v", err)
	}
	if got, err := os.ReadFile(filepath.Join(publication, "unrelated.keep")); err != nil || string(got) != "keep me" {
		t.Fatalf("unrelated sibling changed: %q err=%v", got, err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receipts[0].ID, epRelease+".mkv")
	if err != nil || row.CleanupState != "removed" || !strings.Contains(row.CleanupError, "operator explicitly selected") {
		t.Fatalf("explicit deletion audit row=%+v err=%v", row, err)
	}
}

func TestDiscoveryFollowsAdvancingEmptyPagesThenWraps(t *testing.T) {
	ctx := context.Background()
	ordinary := &fakeClient{}
	svc, db, _ := setup(t, nil, ordinary)
	cfg, err := db.GetDownloadClient(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	protocol := &advancingDeliveryClient{}
	svc.discoverRunnerDeliveries(ctx, cfg, protocol)
	if len(protocol.after) != 2 || protocol.after[0] != 0 || protocol.after[1] != 5 {
		t.Fatalf("empty advancing page cursors = %v, want [0 5]", protocol.after)
	}
	cursor, err := db.DeliveryCursor(ctx, cfg.ID)
	if err != nil || cursor != 0 {
		t.Fatalf("cursor after full sweep=%d err=%v, want wrap to 0", cursor, err)
	}
}

type deliveryImportClient struct {
	fakeClient
	accepts              int
	failCount            int
	acceptErr            error
	acceptEntered        chan struct{}
	acceptRelease        chan struct{}
	sourceCleanupRetries int
	sourceCleanupErr     error
	sourceCleanupEntered chan struct{}
	sourceCleanupRelease chan struct{}
	getDeliveryCalls     int
	conflict             bool
	deliveries           []ports.RunnerDelivery
	lookup               ports.RunnerDelivery
}

func (c *deliveryImportClient) ListDeliveries(_ context.Context, after uint64, limit int) (ports.RunnerDeliveryPage, error) {
	page := ports.RunnerDeliveryPage{ProtocolVersion: 1, NextCursor: after}
	for _, delivery := range c.deliveries {
		if delivery.Sequence <= after {
			continue
		}
		if len(page.Deliveries) == limit {
			break
		}
		page.Deliveries = append(page.Deliveries, delivery)
		page.NextCursor = delivery.Sequence
	}
	return page, nil
}
func (c *deliveryImportClient) AcceptDelivery(context.Context, string, ports.RunnerDeliveryAcceptance) error {
	c.accepts++
	if c.acceptEntered != nil {
		select {
		case c.acceptEntered <- struct{}{}:
		default:
		}
	}
	if c.acceptRelease != nil {
		<-c.acceptRelease
	}
	if c.acceptErr != nil {
		return c.acceptErr
	}
	if c.conflict {
		return ports.ErrDeliveryConflict
	}
	if c.accepts <= c.failCount {
		return errors.New("Runner temporarily unavailable")
	}
	return nil
}
func (c *deliveryImportClient) RetryDeliverySourceCleanup(context.Context, string) (ports.RunnerDelivery, error) {
	c.sourceCleanupRetries++
	if c.sourceCleanupEntered != nil {
		select {
		case c.sourceCleanupEntered <- struct{}{}:
		default:
		}
	}
	if c.sourceCleanupRelease != nil {
		<-c.sourceCleanupRelease
	}
	return c.lookup, c.sourceCleanupErr
}

type deliveryRetryAfterError struct{ deadline time.Time }

func (e deliveryRetryAfterError) Error() string            { return "Runner temporarily unavailable" }
func (e deliveryRetryAfterError) RetryDeadline() time.Time { return e.deadline }

func TestDeliveryACKHonorsPersistedRetryAfterWhileLocalImportContinues(t *testing.T) {
	ctx := context.Background()
	deadline := time.Now().Add(4 * time.Minute)
	protocol := &deliveryImportClient{acceptErr: deliveryRetryAfterError{deadline: deadline}}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("local import should proceed while ACK is deferred: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.State != "imported" || r.AckState != "pending" {
		t.Fatalf("ACK failure blocked local import or was incorrectly accepted: %+v err=%v", r, err)
	}
	if protocol.accepts != 1 {
		t.Fatalf("ACK was dispatched %d times in one import pass despite Retry-After", protocol.accepts)
	}
	if err := svc.completeDeliveryAcceptance(ctx, cfg, receiptID); err != nil {
		t.Fatal(err)
	}
	if protocol.accepts != 1 {
		t.Fatalf("immediate ACK replay bypassed persisted provider deadline: calls=%d", protocol.accepts)
	}
	due, err := db.DeliveryAcknowledgementRetryDue(ctx, receiptID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var persisted int64
	if err := db.R.QueryRowContext(ctx, `SELECT provider_retry_after FROM runner_delivery_receipts WHERE id=?`, receiptID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if time.UnixMilli(persisted).UnixMilli() < deadline.UnixMilli() || due {
		t.Fatalf("ACK deadline not durably stored on receipt: %v due=%v", time.UnixMilli(persisted), due)
	}
}

func TestConcurrentDeliveryAcknowledgementsSerializeBeforeDispatch(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("prepare accepted delivery: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET ack_state='pending',consumer_receipt_id='retry-ack',attempts=0,next_attempt_at=0,provider_retry_after=0 WHERE id=?`, receiptID); err != nil {
		t.Fatal(err)
	}
	protocol.acceptErr = deliveryRetryAfterError{deadline: time.Now().Add(4 * time.Minute)}
	protocol.acceptEntered = make(chan struct{}, 1)
	protocol.acceptRelease = make(chan struct{})
	type result struct{ err error }
	first := make(chan result, 1)
	second := make(chan result, 1)
	go func() { first <- result{svc.completeDeliveryAcceptance(ctx, cfg, receiptID)} }()
	select {
	case <-protocol.acceptEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first acknowledgement POST did not start")
	}
	go func() { second <- result{svc.completeDeliveryAcceptance(ctx, cfg, receiptID)} }()
	time.Sleep(75 * time.Millisecond)
	close(protocol.acceptRelease)
	if err := (<-first).err; err == nil {
		t.Fatal("first acknowledgement should surface the Runner error")
	}
	if err := (<-second).err; err != nil {
		t.Fatalf("second acknowledgement should observe pending retry deadline without dispatch: %v", err)
	}
	if protocol.accepts != 2 { // one initial acceptance plus one retry attempt
		t.Fatalf("concurrent acknowledgement bypassed persisted deadline: POST count=%d", protocol.accepts)
	}
}

func TestSourceCleanupRetryPersistsProviderDeadlineAndBlocksImmediateReplay(t *testing.T) {
	ctx := context.Background()
	deadline := time.Now().Add(4 * time.Minute)
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("prepare accepted delivery: %v", err)
	}
	protocol.sourceCleanupErr = deliveryRetryAfterError{deadline: deadline}
	if err := svc.RetryRunnerSourceCleanup(ctx, receiptID); err == nil {
		t.Fatal("first source-cleanup request should surface the Runner error")
	}
	if protocol.sourceCleanupRetries != 1 {
		t.Fatalf("source-cleanup POST count=%d, want 1", protocol.sourceCleanupRetries)
	}
	if err := svc.RetryRunnerSourceCleanup(ctx, receiptID); err == nil || !strings.Contains(err.Error(), "due after") {
		t.Fatalf("immediate source-cleanup replay error=%v, want persisted due deadline", err)
	}
	if protocol.sourceCleanupRetries != 1 {
		t.Fatalf("immediate click bypassed provider deadline: POST count=%d", protocol.sourceCleanupRetries)
	}
	_, due, err := db.DeliverySourceRefreshError(ctx, receiptID)
	if err != nil || due.UnixMilli() < deadline.UnixMilli() {
		t.Fatalf("source-cleanup deadline=%v err=%v, want >= %v", due, err, deadline)
	}
}

func TestPreviouslySelectedSourceRefreshCannotDispatchAfterPostRetryAfter(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("prepare accepted delivery: %v", err)
	}
	selected, err := db.DeliveryReceiptsNeedingSourceRefresh(ctx, cfg.ID, 4)
	if err != nil || len(selected) != 1 || selected[0].ID != receiptID {
		t.Fatalf("source refresh selection=%+v err=%v", selected, err)
	}
	protocol.sourceCleanupErr = deliveryRetryAfterError{deadline: time.Now().Add(4 * time.Minute)}
	if err := svc.RetryRunnerSourceCleanup(ctx, receiptID); err == nil {
		t.Fatal("explicit POST should surface the Runner Retry-After response")
	}
	// Exercise the queued poll row from before that POST, rather than selecting
	// again. Its dispatch boundary must reload the persisted provider deadline.
	if err := svc.refreshOneDeliverySourceCleanup(ctx, protocol, selected[0]); err != nil {
		t.Fatalf("stale selected refresh should be quietly skipped: %v", err)
	}
	if protocol.getDeliveryCalls != 0 {
		t.Fatalf("stale source poll dispatched GET after POST Retry-After: GET count=%d", protocol.getDeliveryCalls)
	}
}

func TestAutomaticSourceRefreshSkipsBusyReceiptAndResumesLater(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("prepare accepted delivery: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	protocol.lookup = r.Delivery
	unlock := svc.lockDeliverySourceControl(receiptID)
	if err := svc.refreshOneDeliverySourceCleanup(ctx, protocol, r); err != nil {
		t.Fatalf("busy receipt should be skipped without error: %v", err)
	}
	if protocol.getDeliveryCalls != 0 {
		t.Fatalf("automatic refresh dispatched while source-control lock was busy: GET count=%d", protocol.getDeliveryCalls)
	}
	unlock()
	if err := svc.refreshOneDeliverySourceCleanup(ctx, protocol, r); err != nil {
		t.Fatalf("later refresh should resume: %v", err)
	}
	if protocol.getDeliveryCalls != 1 {
		t.Fatalf("later due refresh GET count=%d, want 1", protocol.getDeliveryCalls)
	}
}

func TestConcurrentSourceCleanupRetriesRecheckDeadlineUnderLock(t *testing.T) {
	ctx := context.Background()
	deadline := time.Now().Add(4 * time.Minute)
	protocol := &deliveryImportClient{
		sourceCleanupErr:     deliveryRetryAfterError{deadline: deadline},
		sourceCleanupEntered: make(chan struct{}, 1),
		sourceCleanupRelease: make(chan struct{}),
	}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("prepare accepted delivery: %v", err)
	}
	type result struct{ err error }
	first := make(chan result, 1)
	second := make(chan result, 1)
	go func() { first <- result{svc.RetryRunnerSourceCleanup(ctx, receiptID)} }()
	select {
	case <-protocol.sourceCleanupEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first source-cleanup POST did not start")
	}
	go func() { second <- result{svc.RetryRunnerSourceCleanup(ctx, receiptID)} }()
	time.Sleep(75 * time.Millisecond)
	close(protocol.sourceCleanupRelease)
	firstResult := <-first
	secondResult := <-second
	if firstResult.err == nil {
		t.Fatal("first source-cleanup call should surface the Runner error")
	}
	if secondResult.err == nil || !strings.Contains(secondResult.err.Error(), "due after") {
		t.Fatalf("second call after waiting behind POST error=%v, want persisted deadline rejection", secondResult.err)
	}
	if protocol.sourceCleanupRetries != 1 {
		t.Fatalf("concurrent retry click bypassed provider deadline: POST count=%d", protocol.sourceCleanupRetries)
	}
}

func TestExplicitSourceCleanupRetryMayBypassLocalRefreshCadence(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("prepare accepted delivery: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	protocol.lookup = r.Delivery
	if err := db.RefreshDeliverySourceCleanup(ctx, receiptID, json.RawMessage(`{"state":"pending"}`), nil, 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := svc.RetryRunnerSourceCleanup(ctx, receiptID); err != nil {
		t.Fatalf("explicit retry should bypass Curator's ordinary refresh cadence: %v", err)
	}
	if protocol.sourceCleanupRetries != 1 {
		t.Fatalf("explicit source cleanup POST count=%d, want 1", protocol.sourceCleanupRetries)
	}
}

func (c *deliveryImportClient) GetDelivery(context.Context, string) (ports.RunnerDelivery, error) {
	c.getDeliveryCalls++
	return c.lookup, nil
}

func deliveryImportFixture(t *testing.T, protocol *deliveryImportClient) (*Service, *sqlite.DB, sqlite.Download, ports.ClientConfig, string) {
	return deliveryImportFixtureWithFiles(t, protocol, nil)
}

func deliveryImportFixtureWithFiles(t *testing.T, protocol *deliveryImportClient, extra map[string][]byte) (*Service, *sqlite.DB, sqlite.Download, ports.ClientConfig, string) {
	t.Helper()
	ordinary := &fakeClient{}
	svc, db, itemID := setup(t, nil, ordinary)
	svc.newClient = func(ports.ClientConfig) ports.DownloadClient { return protocol }
	ctx := context.Background()
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
	if err := os.MkdirAll(publication, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("video")
	filename := epRelease + ".mkv"
	if err := os.WriteFile(filepath.Join(publication, filename), content, 0o644); err != nil {
		t.Fatal(err)
	}
	manifestFiles := []ports.RunnerFile{{Path: filename, SizeBytes: int64(len(content)), SHA256: sha256Hex(content)}}
	for name, bytes := range extra {
		if err := os.WriteFile(filepath.Join(publication, name), bytes, 0o644); err != nil {
			t.Fatal(err)
		}
		manifestFiles = append(manifestFiles, ports.RunnerFile{Path: name, SizeBytes: int64(len(bytes)), SHA256: sha256Hex(bytes)})
	}
	if err := svc.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: itemID, WantableIDs: []string{"episode:" + itoa(itemID) + ":1:1"}, ReleaseTitle: epRelease, Indexer: "idx", Protocol: "usenet", ClientID: client.ID, Handle: "17", Transfer: "t-17", State: "downloaded", DeliveryExpected: true})
	if err != nil {
		t.Fatal(err)
	}
	delivery := ports.RunnerDelivery{Sequence: 1, DeliveryID: "delivery-1", PublicationGeneration: "pub-1", ArtifactID: "artifact-1", JobID: "17", RequestToken: "t-17", RunnerInstanceID: "runner-1", PublicationPath: "/remote/completed/" + epRelease, Files: manifestFiles}
	delivery.ManifestDigest, err = runnerManifestDigest(delivery.Files)
	if err != nil {
		t.Fatal(err)
	}
	r, err := db.SaveDiscoveredDelivery(ctx, client.ID, delivery, publication)
	if err != nil {
		t.Fatal(err)
	}
	r.DownloadID, r.GenerationKey, r.DownloadHandle = dl.ID, dl.GenerationKey, dl.Handle
	r.MediaItemID, r.CopyID = dl.MediaItemID, dl.CopyID
	if err := db.AssociateDelivery(ctx, r); err != nil {
		t.Fatal(err)
	}
	protocol.lookup = delivery
	dl.ImportPath, dl.SavePath = publication, delivery.PublicationPath
	return svc, db, dl, client, publication
}

func TestRunnerSourceCleanupProgressRefreshesDuringPendingAcknowledgement(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{failCount: 100}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receiptID := receiptIDFor(t, db, dl.ID)
	if err := svc.runDeliveryImport(ctx, dl, cfg, receiptID); err != nil {
		t.Fatalf("locally verified import during Runner outage: %v", err)
	}
	r, err := db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.State != "imported" || r.AckState != "pending" {
		t.Fatalf("fixture did not preserve independent pending acknowledgement: %+v err=%v", r, err)
	}
	protocol.lookup.SourceCleanup = json.RawMessage(`{"status":"succeeded","attempts":1}`)
	svc.refreshDeliverySourceCleanup(ctx, cfg, protocol)
	trace, err := svc.DeliveryLifecycleByID(ctx, receiptID)
	if err != nil {
		t.Fatal(err)
	}
	var progress struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(trace.SourceCleanup, &progress); err != nil || progress.Status != "succeeded" {
		t.Fatalf("Runner source cleanup progress was not refreshed while ack remained pending: raw=%s status=%q err=%v", trace.SourceCleanup, progress.Status, err)
	}
	r, err = db.DeliveryReceiptByID(ctx, receiptID)
	if err != nil || r.AckState != "pending" {
		t.Fatalf("source progress refresh incorrectly changed acknowledgement: %+v err=%v", r, err)
	}
}

func sha256Hex(content []byte) string {
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])
}

func TestAcceptedCaptionSidecarIsPlacedBesideImportedMedia(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	caption := []byte("subtitle payload")
	cover := []byte("cover image payload")
	svc, db, dl, cfg, _ := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{strings.TrimSuffix(epRelease, ".mkv") + ".en.srt": caption, "cover.jpg": cover})
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	if err := svc.runDeliveryImport(ctx, dl, cfg, receipts[0].ID); err != nil {
		t.Fatal(err)
	}
	rows, err := db.DeliveryFileOutcomes(ctx, receipts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("manifest outcomes=%+v", rows)
	}
	placed := 0
	for _, row := range rows {
		if !strings.HasSuffix(row.Path, ".en.srt") && row.Path != "cover.jpg" {
			continue
		}
		want := caption
		if row.Path == "cover.jpg" {
			want = cover
		}
		if row.Disposition != "placed" || row.LibraryPath == "" {
			t.Fatalf("recognized sidecar was not associated and placed: %+v", row)
		}
		got, size, err := fileDigest(ctx, row.LibraryPath)
		if err != nil || size != int64(len(want)) || got != sha256Hex(want) {
			t.Fatalf("placed sidecar digest=%s size=%d err=%v", got, size, err)
		}
		placed++
	}
	if placed != 2 {
		t.Fatalf("placed sidecar count=%d rows=%+v", placed, rows)
	}
}

func TestDeliverySidecarRetryUsesPriorCommittedMediaLedger(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	subtitleName := strings.TrimSuffix(epRelease, ".mkv") + ".en.srt"
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{subtitleName: []byte("subtitle")})
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
	for _, file := range verified {
		if filename.IsVideo(file.relative) {
			media = file
		} else if file.relative == subtitleName {
			sidecar = file
		}
	}
	if media.relative == "" || sidecar.relative == "" {
		t.Fatalf("verified media/sidecar missing: %+v", verified)
	}
	manifest := make([]sqlite.DeliveryManifestFile, 0, len(verified))
	for _, file := range verified {
		manifest = append(manifest, sqlite.DeliveryManifestFile{Path: file.relative, SizeBytes: file.size, SHA256: file.digest, FileGeneration: file.generation})
	}
	if err := db.EnsureDeliveryFileOutcomes(ctx, r.ID, manifest, nil, false); err != nil {
		t.Fatal(err)
	}
	result, err := svc.importDownloadFiles(ctx, dl, publication, []string{media.absolute}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.recordDeliveryImportOutcomes(ctx, r, []verifiedDeliveryFile{media, sidecar}, result); err != nil {
		t.Fatal(err)
	}
	// Simulate a later retry after the media source has already been placed and
	// skipped by this attempt's importer result. The sidecar must resolve from
	// the durable source-path → library-path ledger association.
	result, err = svc.importDeliverySidecars(ctx, r, []verifiedDeliveryFile{sidecar}, ImportResult{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 1 || len(result.Files) != 1 || !result.Files[0].Imported {
		t.Fatalf("retry did not place sidecar using prior ledger media: %+v", result)
	}
	if err := svc.recordDeliveryImportOutcomes(ctx, r, []verifiedDeliveryFile{sidecar}, result); err != nil {
		t.Fatal(err)
	}
	rows, err := db.DeliveryFileOutcomes(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Path == subtitleName && (row.Disposition != "placed" || row.LibraryPath == "") {
			t.Fatalf("sidecar retry ledger = %+v", row)
		}
	}
}

func TestDeliveryManifestRechecksPreviouslyUnselectedSiblingForResolve(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	second := []byte("second episode")
	secondName := "Another.Show.S01E02.1080p.WEB-DL-GRP.mkv"
	svc, db, dl, cfg, _ := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{secondName: second})
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	r := receipts[0]
	verified, err := svc.verifyDeliveryManifest(ctx, cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	selected := map[string]bool{epRelease + ".mkv": true}
	manifest := make([]sqlite.DeliveryManifestFile, 0, len(verified))
	for _, f := range verified {
		manifest = append(manifest, sqlite.DeliveryManifestFile{Path: f.relative, SizeBytes: f.size, SHA256: f.digest, FileGeneration: f.generation})
	}
	if err := db.EnsureDeliveryFileOutcomes(ctx, r.ID, manifest, selected, false); err != nil {
		t.Fatal(err)
	}
	rows, err := db.DeliveryFileOutcomes(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Path == secondName && row.Disposition != "unselected_recoverable" {
			t.Fatalf("expected explicit unselected baseline, got %+v", row)
		}
	}
	verified, err = svc.verifyDeliveryManifest(ctx, cfg, r)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range verified {
		if f.relative == secondName {
			return
		}
	}
	t.Fatalf("previously unselected sibling was skipped instead of reverified for a later explicit Resolve: %+v", verified)
}

func TestDeliveryRedundancyRequiresHealthySameCopyQualityEvidence(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		quality              quality.Quality
		provenance           mediainfo.Provenance
		wrongCopy            bool
		removeTarget         bool
		tamperTarget         bool
		alternateCopyProfile bool
		reviewFlag           bool
		sourceFixture        string
		targetFixture        string
		wantRedundant        bool
	}{
		{name: "equal quality and equal bytes", quality: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, provenance: mediainfo.ProvenanceFilename, targetFixture: "mkv-1080p-h264-ac3.mkv", wantRedundant: true},
		{name: "better encoding with different bytes", quality: quality.Quality{Source: quality.SourceBluray, Resolution: 1080}, provenance: mediainfo.ProvenanceFilename, sourceFixture: "mkv-1080p-h264-ac3-whole.mkv", targetFixture: "mkv-1080p-h264-ac3-ger-whole.mkv", wantRedundant: true},
		{name: "wrong copy", quality: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, provenance: mediainfo.ProvenanceFilename, wrongCopy: true},
		{name: "unhealthy file", quality: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, provenance: mediainfo.ProvenanceImplausible},
		{name: "same-size corruption after indexing", quality: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, provenance: mediainfo.ProvenanceFilename, targetFixture: "mkv-1080p-h264-ac3.mkv", tamperTarget: true},
		{name: "associated copy profile rejects candidate", quality: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, provenance: mediainfo.ProvenanceFilename, targetFixture: "mkv-1080p-h264-ac3.mkv", alternateCopyProfile: true},
		{name: "open review flag blocks redundancy", quality: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, provenance: mediainfo.ProvenanceFilename, targetFixture: "mkv-1080p-h264-ac3.mkv", reviewFlag: true},
		{name: "missing target", quality: quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, provenance: mediainfo.ProvenanceFilename, removeTarget: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			protocol := &deliveryImportClient{}
			svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
			receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
			if err != nil || len(receipts) != 1 {
				t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
			}
			r := receipts[0]
			verified, err := svc.verifyDeliveryManifest(ctx, cfg, r)
			if err != nil || len(verified) == 0 {
				t.Fatalf("manifest verification=%+v err=%v", verified, err)
			}
			f := verified[0]
			sourceFixture := tc.sourceFixture
			if sourceFixture == "" {
				sourceFixture = "mkv-1080p-h264-ac3.mkv"
			}
			if content, err := os.ReadFile(filepath.Join("..", "..", "domain", "mediainfo", "testdata", sourceFixture)); err != nil {
				t.Fatal(err)
			} else if err := os.WriteFile(f.absolute, content, 0o644); err != nil {
				t.Fatal(err)
			} else {
				f.size = int64(len(content))
				f.digest = sha256Hex(content)
				info, err := os.Stat(f.absolute)
				if err != nil {
					t.Fatal(err)
				}
				f.generation = deliveryFileGeneration(info)
			}
			if err := db.EnsureDeliveryFileOutcomes(ctx, r.ID, []sqlite.DeliveryManifestFile{{Path: f.relative, SizeBytes: f.size, SHA256: f.digest, FileGeneration: f.generation}}, nil, false); err != nil {
				t.Fatal(err)
			}
			item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
			if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) == 0 {
				t.Fatalf("item episodes=%+v err=%v", item.Seasons, err)
			}
			target := filepath.Join(item.Path, "already-imported.mkv")
			content := []byte("not a valid video")
			if tc.targetFixture != "" {
				content, err = os.ReadFile(filepath.Join("..", "..", "domain", "mediainfo", "testdata", tc.targetFixture))
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(target, content, 0o644); err != nil {
				t.Fatal(err)
			}
			copyID := int64(0)
			if tc.wrongCopy || tc.alternateCopyProfile {
				profileID := int64(1)
				if tc.alternateCopyProfile {
					floor := quality.Quality{Source: quality.SourceBluray, Resolution: 1080}
					profileID, err = db.AddProfile(ctx, quality.Profile{Name: "Bluray floor", Target: floor, Floor: &floor, UpgradesAllowed: true})
					if err != nil {
						t.Fatal(err)
					}
				}
				copyID, err = db.AddMediaCopy(ctx, domain.MediaCopy{MediaItemID: dl.MediaItemID, Name: "other copy", Path: t.TempDir(), QualityProfileID: profileID, Monitored: true})
				if err != nil {
					t.Fatal(err)
				}
				if tc.alternateCopyProfile {
					r.CopyID = copyID
				}
			}
			fileID, err := db.UpsertFile(ctx, dl.MediaItemID, copyID, target, int64(len(content)))
			if err != nil {
				t.Fatal(err)
			}
			if err := db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{item.Seasons[0].Episodes[0].ID}); err != nil {
				t.Fatal(err)
			}
			if err := db.SetFileQualityFrom(ctx, fileID, tc.quality, tc.provenance, mediainfo.ConfidenceNone); err != nil {
				t.Fatal(err)
			}
			if tc.reviewFlag {
				currentFiles, err := db.ListFilesForItem(ctx, dl.MediaItemID)
				if err != nil {
					t.Fatal(err)
				}
				var generation string
				for _, current := range currentFiles {
					if current.ID == fileID {
						generation = current.FileGeneration
						break
					}
				}
				if generation == "" {
					t.Fatal("indexed library file has no generation")
				}
				if err := db.CreateLibraryReviewFlag(ctx, dl.MediaItemID, fileID, generation, "damaged", "user reported a problem", ""); err != nil {
					t.Fatal(err)
				}
			}
			if tc.tamperTarget {
				if err := os.WriteFile(target, make([]byte, len(content)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.removeTarget {
				if err := os.Remove(target); err != nil {
					t.Fatal(err)
				}
			}
			_, _, redundant := svc.deliveryRedundantLibraryProof(ctx, r, f, "")
			if redundant != tc.wantRedundant {
				t.Fatalf("redundant=%v, want %v", redundant, tc.wantRedundant)
			}
		})
	}
}

func TestTransientDeliveryAckDoesNotBlockImportAndRestartNeedsNoPayload(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{failCount: 2}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	if err := svc.runDeliveryImport(ctx, dl, cfg, receipts[0].ID); err != nil {
		t.Fatal(err)
	}
	fresh, err := db.GetDownload(ctx, dl.ID)
	if err != nil || fresh.State != "imported" {
		t.Fatalf("download state=%q err=%v", fresh.State, err)
	}
	if protocol.accepts != 1 {
		t.Fatalf("accept attempts=%d, want one bounded attempt before local import", protocol.accepts)
	}
	outcomes, err := db.DeliveryFileOutcomes(ctx, receipts[0].ID)
	if err != nil || len(outcomes) != 1 || outcomes[0].Path != epRelease+".mkv" || outcomes[0].Disposition != "placed" || outcomes[0].CleanupState != "pending" {
		t.Fatalf("durable exact-path placement ledger=%+v err=%v", outcomes, err)
	}
	receipt, err := db.DeliveryReceiptByID(ctx, receipts[0].ID)
	if err != nil || receipt.AckState != "pending" {
		t.Fatalf("receipt after transient ack=%+v err=%v", receipt, err)
	}
	if err := os.RemoveAll(publication); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPayloadRemoved(ctx, dl.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteDownload(ctx, dl.ID); err != nil {
		t.Fatalf("dismiss Activity while acknowledgement is pending: %v", err)
	}
	if err := db.DeleteDownload(ctx, dl.ID); err != nil {
		t.Fatalf("remove dismissed Activity while acknowledgement is pending: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET next_attempt_at=0 WHERE id=?`, receipts[0].ID); err != nil {
		t.Fatal(err)
	}
	protocol.failCount = 0
	protocol.accepts = 0
	dataDir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := New(reopened, nil, nil, nil, func(ports.ClientConfig) ports.DownloadClient { return protocol })
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	restarted.StartImporters(workerCtx)
	restarted.discoverRunnerDeliveries(ctx, cfg, protocol)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		receipt, err = reopened.DeliveryReceiptByID(ctx, receipt.ID)
		if err == nil && receipt.AckState == "accepted" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || receipt.AckState != "accepted" || receipt.State != "imported" {
		t.Fatalf("receipt recovery after process restart and source removal=%+v err=%v", receipt, err)
	}
	item, err := reopened.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Files) != 1 {
		t.Fatalf("restart re-imported or lost library files: %+v", item.Files)
	}
	if protocol.accepts != 1 {
		t.Fatalf("journal-only acknowledgement attempts=%d, want 1", protocol.accepts)
	}
	if _, err := os.Stat(publication); !os.IsNotExist(err) {
		t.Fatalf("receipt recovery unexpectedly required or recreated publication: err=%v", err)
	}
}

func TestDeliveryConflictBlocksLibraryMutation(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{conflict: true}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	if err := svc.runDeliveryImport(ctx, dl, cfg, receipts[0].ID); !errors.Is(err, ports.ErrDeliveryConflict) {
		t.Fatalf("conflict result=%v", err)
	}
	item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Files) != 0 {
		t.Fatalf("409 conflict mutated library before acceptance: %+v", item.Files)
	}
	if _, err := os.Stat(publication); err != nil {
		t.Fatalf("publication was removed: %v", err)
	}
}

func TestLegacyCleanupReviewCohortCannotAutoImportDelivery(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, _ := deliveryImportFixture(t, protocol)
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES(?,?)`, dl.GenerationKey, receipts[0].MappedPath); err != nil {
		t.Fatal(err)
	}
	if err := svc.runDeliveryImport(ctx, dl, cfg, receipts[0].ID); err == nil {
		t.Fatal("legacy cohort delivery was automatically imported")
	}
	item, err := db.GetMediaItemFull(ctx, dl.MediaItemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Files) != 0 || protocol.accepts != 0 {
		t.Fatalf("legacy cohort crossed import/acceptance gate: files=%+v accepts=%d", item.Files, protocol.accepts)
	}
}

func TestPlacementRejectsSourceBytesThatChangedAfterManifestVerification(t *testing.T) {
	svc, db, itemID := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "accepted.mkv")
	if err := os.WriteFile(src, []byte("changed after manifest verification"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(item.Path, "accepted.mkv")
	ctx = withExpectedSourceDigests(ctx, map[string]string{src: "0000000000000000000000000000000000000000000000000000000000000000"})
	if _, err := svc.commitPlacement(ctx, item, importScope{Release: "accepted"}, src, dest, quality.Quality{}, nil, false); err == nil {
		t.Fatal("changed source bytes passed the accepted manifest digest check")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("library destination was mutated: err=%v", err)
	}
	pending, err := db.PendingPlacements(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("source mismatch created placement journal: %+v err=%v", pending, err)
	}
}

func TestRunnerManifestDigestGoldenUnicodeAndHTML(t *testing.T) {
	const fileHash = "2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"
	files := []ports.RunnerFile{
		{Path: "é<&.mkv", SizeBytes: 1, SHA256: fileHash},
		{Path: "z.mkv", SizeBytes: 1, SHA256: fileHash},
	}
	got, err := runnerManifestDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	const want = "a0acee1d6757529c7deca3dc2ac2fa2f0b6e96db781d83b8d85d732cedd6459f"
	if got != want {
		t.Fatalf("canonical manifest digest = %s, want %s", got, want)
	}
}

func TestRunnerManifestDigestRejectsEmpty(t *testing.T) {
	if _, err := runnerManifestDigest(nil); err == nil {
		t.Fatal("empty manifest was accepted")
	}
}

func TestSafeDeliveryRelativePath(t *testing.T) {
	for _, path := range []string{"../escape.mkv", "/absolute.mkv", `dir\\file.mkv`, "dir/../file.mkv", "./file.mkv", "dir//file.mkv", ""} {
		if safeDeliveryRelativePath(path) {
			t.Errorf("accepted unsafe manifest path %q", path)
		}
	}
	for _, path := range []string{"movie.mkv", "Season 1/episode 01.mkv", "épisode.mkv"} {
		if !safeDeliveryRelativePath(path) {
			t.Errorf("rejected safe manifest path %q", path)
		}
	}
}
