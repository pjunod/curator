package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestPlacementMetadataCommitFailureReconcilesExactPublicationAfterRestart(t *testing.T) {
	ctx := context.Background()
	svc, db, request, receipts := recoveryFixture(t)
	preview, err := svc.PreviewRecovery(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	markRecoveryImportingForTest(t, db, job.ID)
	if _, err = db.W.ExecContext(ctx, `CREATE TRIGGER fail_placement_metadata_commit BEFORE INSERT ON media_files
		BEGIN SELECT RAISE(ABORT,'injected placement metadata commit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err = svc.runRecovery(ctx, job); err == nil || !strings.Contains(err.Error(), "injected placement metadata commit failure") {
		t.Fatalf("recovery did not fail at the metadata commit after publication: %v", err)
	}
	var rows int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE media_item_id=?`, request.MediaItemID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("failed placement transaction partially committed library ownership: rows=%d err=%v", rows, err)
	}
	pending, err := db.PendingPlacements(ctx)
	if err != nil || len(pending) != 1 || pending[0].State != "prepared" || pending[0].PublishedFileGeneration == "" {
		t.Fatalf("failed commit did not retain published prepared authority: placements=%+v err=%v", pending, err)
	}
	target := pending[0].Target
	payload := corpusFile(t, wholeCorpus1080)
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != string(payload) {
		t.Fatalf("published bytes changed at failed metadata commit: bytes=%d err=%v", len(got), readErr)
	}
	if receipts.Load() != 0 {
		t.Fatalf("Runner receipt advanced before local metadata commit: %d", receipts.Load())
	}
	sourceRoot, err := svc.recoveryPayload(ctx, job.Recovery)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(sourceRoot, job.Recovery.Files[0].Path)
	if got, readErr := os.ReadFile(source); readErr != nil || string(got) != string(payload) {
		t.Fatalf("Runner-owned source changed at failed commit: bytes=%d err=%v", len(got), readErr)
	}

	reopened := reopenLifecycleDB(t, db)
	if _, err = reopened.W.ExecContext(ctx, `DROP TRIGGER fail_placement_metadata_commit`); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} })
	restarted.reconcilePlacements(ctx)
	filesAfterReconcile, err := reopened.ListFilesForItem(ctx, request.MediaItemID)
	if err != nil || len(filesAfterReconcile) != 1 || filesAfterReconcile[0].Path != target {
		t.Fatalf("startup reconciliation did not commit the exact published placement: files=%+v err=%v", filesAfterReconcile, err)
	}
	if receipts.Load() != 0 {
		t.Fatalf("placement reconciliation sent a Runner receipt before recovery resumed: %d", receipts.Load())
	}
	restarted.recoverySweep(ctx)
	settled, err := restarted.RecoveryImport(ctx, job.ID)
	if err != nil || settled.State != "imported" {
		t.Fatalf("worker did not reconcile the exact placement and receipt after restart: import=%+v err=%v", settled, err)
	}
	files, err := reopened.ListFilesForItem(ctx, request.MediaItemID)
	if err != nil || len(files) != 1 || files[0].Path != target {
		t.Fatalf("retry did not commit exactly one library row: files=%+v err=%v", files, err)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != string(payload) {
		t.Fatalf("reconciled target bytes=%d err=%v", len(got), readErr)
	}
	if receipts.Load() != 1 {
		t.Fatalf("exact retry should deliver one receipt, got %d", receipts.Load())
	}
}

func TestCancelPublishedFirstAddRollsBackOnlyNewTargetAfterRestart(t *testing.T) {
	ctx := context.Background()
	svc, db, request, _ := recoveryFixture(t)
	manifest := recoveryManifestFromFixture(t, svc, request)
	var remoteMu sync.Mutex
	remoteState, remoteImport := "published", ""
	var cancelAcks, receiptCalls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			var body struct {
				ImportID string `json:"import_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			remoteMu.Lock()
			remoteState, remoteImport = "claimed", body.ImportID
			current := manifest
			current.State, current.ImportID = remoteState, remoteImport
			remoteMu.Unlock()
			_ = json.NewEncoder(w).Encode(current)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+manifest.ID):
			remoteMu.Lock()
			current := manifest
			current.State, current.ImportID = remoteState, remoteImport
			remoteMu.Unlock()
			_ = json.NewEncoder(w).Encode(current)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			remoteMu.Lock()
			remoteState = "cancel_pending"
			current := manifest
			current.State, current.ImportID = remoteState, remoteImport
			remoteMu.Unlock()
			_ = json.NewEncoder(w).Encode(current)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel-ack"):
			cancelAcks.Add(1)
			remoteMu.Lock()
			remoteState = "cancelled"
			remoteMu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/receipt"):
			receiptCalls.Add(1)
			http.Error(w, "unexpected receipt for cancelled first-add", http.StatusConflict)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	redirectRecoveryClient(t, db, request.ClientID, remote.URL)
	preview, err := svc.PreviewRecovery(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	markRecoveryImportingForTest(t, db, job.ID)
	if _, err = db.W.ExecContext(ctx, `CREATE TRIGGER fail_published_generation BEFORE UPDATE OF data ON import_placements
		WHEN COALESCE(json_extract(NEW.data,'$.published_file_generation'),'') <> ''
		BEGIN SELECT RAISE(ABORT,'stop after first-add publication'); END`); err != nil {
		t.Fatal(err)
	}
	if err = svc.runRecovery(ctx, job); err == nil || !strings.Contains(err.Error(), "stop after first-add publication") {
		t.Fatalf("test did not stop after publishing the new target: %v", err)
	}
	pending, err := db.PendingPlacements(ctx)
	if err != nil || len(pending) != 1 || pending[0].PreviousSHA256 != "" || pending[0].PublishedFileGeneration != "" {
		t.Fatalf("expected published first-add placement without a prior target: rows=%+v err=%v", pending, err)
	}
	target := pending[0].Target
	payload := corpusFile(t, wholeCorpus1080)
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != string(payload) {
		t.Fatalf("fixture did not retain the newly published target: bytes=%d err=%v", len(got), readErr)
	}
	if err = svc.CancelRecoveryImport(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	reopened := reopenLifecycleDB(t, db)
	if _, err = reopened.W.ExecContext(ctx, `DROP TRIGGER fail_published_generation`); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} })
	restarted.recoverySweep(ctx)
	settled, err := restarted.RecoveryImport(ctx, job.ID)
	if err != nil || settled.State != "cancelled" || cancelAcks.Load() != 1 {
		t.Fatalf("restart did not complete exact cancellation: import=%+v ackCalls=%d err=%v", settled, cancelAcks.Load(), err)
	}
	if _, err = os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled first-add target was not removed: %v", err)
	}
	if got, readErr := os.ReadFile(filepath.Join(mustRecoveryPayload(t, restarted, job), job.Recovery.Files[0].Path)); readErr != nil || string(got) != string(payload) {
		t.Fatalf("cancellation removed or changed Runner source: bytes=%d err=%v", len(got), readErr)
	}
	files, err := reopened.ListFilesForItem(ctx, request.MediaItemID)
	if err != nil || len(files) != 0 {
		t.Fatalf("cancelled first-add gained library metadata: files=%+v err=%v", files, err)
	}
	if receiptCalls.Load() != 0 {
		t.Fatalf("cancelled first-add sent an import receipt: %d", receiptCalls.Load())
	}
	var state string
	if err = reopened.R.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE id=?`, pending[0].ID).Scan(&state); err != nil || state != "rolled_back" {
		t.Fatalf("first-add placement state=%q err=%v, want rolled_back", state, err)
	}
}

func TestRecoveryCancelResponseIdentityChangeWithholdsAckAndPreservesCommittedMedia(t *testing.T) {
	ctx := context.Background()
	svc, db, request, _ := recoveryFixture(t)
	manifest := recoveryManifestFromFixture(t, svc, request)
	var remoteMu sync.Mutex
	remoteState, remoteImport := "published", ""
	var cancelCalls, cancelAcks, receiptCalls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/claim"):
			var body struct {
				ImportID string `json:"import_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			remoteMu.Lock()
			remoteImport, remoteState = body.ImportID, "claimed"
			current := manifest
			current.ImportID, current.State = remoteImport, remoteState
			remoteMu.Unlock()
			_ = json.NewEncoder(w).Encode(current)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/"+manifest.ID):
			remoteMu.Lock()
			current := manifest
			current.ImportID, current.State = remoteImport, remoteState
			remoteMu.Unlock()
			_ = json.NewEncoder(w).Encode(current)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/receipt"):
			receiptCalls.Add(1)
			remoteMu.Lock()
			remoteState = "cancel_pending"
			remoteMu.Unlock()
			http.Error(w, "receipt conflict", http.StatusConflict)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel"):
			cancelCalls.Add(1)
			mismatch := manifest
			mismatch.ImportID, mismatch.State = remoteImport, "cancel_pending"
			mismatch.Artifact = "different-artifact-after-get"
			_ = json.NewEncoder(w).Encode(mismatch)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cancel-ack"):
			cancelAcks.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(remote.Close)
	redirectRecoveryClient(t, db, request.ClientID, remote.URL)
	preview, err := svc.PreviewRecovery(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	markRecoveryImportingForTest(t, db, job.ID)
	if err = svc.runRecovery(ctx, job); err != nil {
		t.Fatalf("publish accepted recovery locally: %v", err)
	}
	files, err := db.ListFilesForItem(ctx, request.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("recovery did not commit its library file before receipt conflict: files=%+v err=%v", files, err)
	}
	committedPath := files[0].Path
	committedBytes, err := os.ReadFile(committedPath)
	if err != nil {
		t.Fatal(err)
	}
	if receiptCalls.Load() != 0 {
		t.Fatalf("runRecovery unexpectedly sent a receipt: %d", receiptCalls.Load())
	}
	svc.deliverRecoveryReceipts(ctx)
	local, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil || local.State != "cancel_pending" || receiptCalls.Load() != 1 {
		t.Fatalf("receipt conflict did not persist the exact cancellation handoff: import=%+v receiptCalls=%d err=%v", local, receiptCalls.Load(), err)
	}
	restarted := New(db, bus.New(nil), nil, func(ports.IndexerConfig) ports.Indexer { return fakeIndexer{} }, func(ports.ClientConfig) ports.DownloadClient { return &fakeClient{} })
	restarted.recoverySweep(ctx)
	local, err = restarted.RecoveryImport(ctx, job.ID)
	if err != nil || local.State != "cancel_pending" || cancelCalls.Load() != 1 || cancelAcks.Load() != 0 || receiptCalls.Load() != 1 {
		t.Fatalf("mismatched cancel response was acknowledged or receipt retried: import=%+v cancels=%d acks=%d receipts=%d err=%v", local, cancelCalls.Load(), cancelAcks.Load(), receiptCalls.Load(), err)
	}
	files, err = db.ListFilesForItem(ctx, request.MediaItemID)
	if err != nil || len(files) != 1 || files[0].Path != committedPath {
		t.Fatalf("cancel response mismatch changed committed library records: files=%+v err=%v", files, err)
	}
	if got, readErr := os.ReadFile(committedPath); readErr != nil || string(got) != string(committedBytes) {
		t.Fatalf("committed library bytes changed after cancel response mismatch: bytes=%d err=%v", len(got), readErr)
	}
}

func recoveryManifestFromFixture(t *testing.T, svc *Service, request RecoveryRequest) RunnerRecovery {
	t.Helper()
	client, err := svc.recoveryClient(context.Background(), request.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	var manifest RunnerRecovery
	if err = client.RecoveryRequest(context.Background(), http.MethodGet, "recoveries/"+request.RecoveryID, "", nil, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func redirectRecoveryClient(t *testing.T, db *sqlite.DB, clientID int64, url string) {
	t.Helper()
	client, err := db.GetDownloadClient(context.Background(), clientID)
	if err != nil {
		t.Fatal(err)
	}
	client.URL = url
	if err = db.UpdateDownloadClient(context.Background(), client); err != nil {
		t.Fatal(err)
	}
}

func mustRecoveryPayload(t *testing.T, svc *Service, job RecoveryImport) string {
	t.Helper()
	path, err := svc.recoveryPayload(context.Background(), job.Recovery)
	if err != nil {
		t.Fatal(err)
	}
	return path
}
