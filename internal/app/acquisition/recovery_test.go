package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func recoveryFixture(t *testing.T) (*Service, *sqlite.DB, RecoveryRequest, *atomic.Int32) {
	t.Helper()
	svc, db, itemID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	mount := t.TempDir()
	payload := filepath.Join(mount, "abc123", "payload")
	if err := os.MkdirAll(payload, 0755); err != nil {
		t.Fatal(err)
	}
	bytes := corpusFile(t, wholeCorpus1080)
	name := "Recovered.2024.1080p.WEB-DL.mkv"
	if err := os.WriteFile(filepath.Join(payload, name), bytes, 0644); err != nil {
		t.Fatal(err)
	}
	manifest := RunnerRecovery{ID: "abc123", Installation: "test-installation", Generation: "generation", State: "published", ManifestDigest: "sealed", Published: "/published/abc123", Files: []RecoveryFile{{ID: "file", Path: name, Bytes: int64(len(bytes)), SHA256: fmt.Sprintf("%x", sha256.Sum256(bytes))}}}
	receipts := &atomic.Int32{}
	var manifestMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		manifestMu.Lock()
		defer manifestMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.Header.Get("X-Recovery-Token") != "credential" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/claim"):
			manifest.State = "claimed"
		case strings.HasSuffix(r.URL.Path, "/receipt"):
			receipts.Add(1)
			manifest.State = "imported"
		case strings.HasSuffix(r.URL.Path, "/cancel"):
			manifest.State = "cancelled"
		}
		_ = json.NewEncoder(w).Encode(manifest)
	}))
	t.Cleanup(server.Close)
	clientID, err := db.AddDownloadClient(ctx, ports.ClientConfig{Type: "nzbd", Name: "Runner", URL: server.URL, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.SetRecoverySettings(ctx, RecoverySettings{LocalRoot: mount, RemoteRoot: "/published", ConsumerToken: "credential"}); err != nil {
		t.Fatal(err)
	}
	return svc, db, RecoveryRequest{ClientID: clientID, RecoveryID: manifest.ID, MediaItemID: itemID, FileIDs: []string{"file"}}, receipts
}

func TestRecoveryReceiptFollowsDurableLibraryCommit(t *testing.T) {
	svc, db, req, receipts := recoveryFixture(t)
	ctx := context.Background()
	preview, err := svc.PreviewRecovery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	if receipts.Load() != 0 {
		t.Fatal("receipt sent before import")
	}
	svc.recoverySweep(ctx)
	result, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "imported" {
		t.Fatalf("state %s: %s", result.State, result.Error)
	}
	files, err := db.ListFilesForItem(ctx, req.MediaItemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if _, err = os.Stat(files[0].Path); err != nil {
		t.Fatal(err)
	}
	if receipts.Load() != 1 {
		t.Fatalf("receipts=%d", receipts.Load())
	}
	svc.recoverySweep(ctx)
	if receipts.Load() != 1 {
		t.Fatal("delivered receipt resent")
	}
}
func TestRecoveryPreviewRejectsChangedBytesAndTarget(t *testing.T) {
	svc, db, req, _ := recoveryFixture(t)
	ctx := context.Background()
	preview, err := svc.PreviewRecovery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_items SET title='Changed' WHERE id=?`, req.MediaItemID); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.QueueRecovery(ctx, preview.Request); err == nil {
		t.Fatal("stale target accepted")
	}
	cfg, err := svc.RecoverySettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.LocalRoot, "abc123", "payload", preview.Files[0].Path)
	if err = os.WriteFile(path, []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.PreviewRecovery(ctx, req); err == nil {
		t.Fatal("changed staging accepted")
	}
}
func TestRecoveryCancelBeforeWorkerKeepsLibraryEmpty(t *testing.T) {
	svc, db, req, receipts := recoveryFixture(t)
	ctx := context.Background()
	preview, err := svc.PreviewRecovery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.CancelRecoveryImport(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	svc.recoverySweep(ctx)
	current, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "cancelled" {
		t.Fatalf("state=%s error=%s", current.State, current.Error)
	}
	files, err := db.ListFilesForItem(ctx, req.MediaItemID)
	if err != nil || len(files) != 0 {
		t.Fatalf("unexpected files=%v err=%v", files, err)
	}
	if receipts.Load() != 0 {
		t.Fatal("cancelled import delivered a receipt")
	}
}
func TestPlacementRestartCommitsPublishedBytesAndCleansRollback(t *testing.T) {
	svc, db, itemID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	root := t.TempDir()
	target := filepath.Join(root, "target.mkv")
	backup := filepath.Join(root, ".monarr-prior-intent")
	if err := os.WriteFile(target, []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backup, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	p := sqlite.Placement{ID: "intent", Target: target, Temporary: filepath.Join(root, ".monarr-stage-intent"), Backup: backup, ItemID: itemID, Size: 3, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("new"))), PreviousSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("old"))), State: "prepared"}
	if err := db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	svc.reconcilePlacements(ctx)
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if _, err = os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("rollback cleanup=%v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "new" {
		t.Fatalf("target=%q err=%v", got, err)
	}
}

func TestRecoveryPreviewJobPersistsAndRejectsChangedSelection(t *testing.T) {
	svc, _, req, _ := recoveryFixture(t)
	ctx := context.Background()
	task, err := svc.StartRecoveryPreview(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != "queued" || task.ID == "" {
		t.Fatalf("task=%+v", task)
	}
	svc.recoveryPreviewSweep(ctx)
	task, err = svc.RecoveryPreviewStatus(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.State != "ready" || task.Preview == nil {
		t.Fatalf("preview=%+v", task)
	}
	changed := task.Preview.Request
	changed.FileIDs = []string{"another-file"}
	if _, err = svc.QueueRecovery(ctx, changed); err == nil {
		t.Fatal("uninspected selection accepted")
	}
	job, err := svc.QueueRecovery(ctx, task.Preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "queued" {
		t.Fatalf("state=%s", job.State)
	}
}
func TestRecoveryLibraryEditDuringCopyPreventsReceipt(t *testing.T) {
	svc, db, req, receipts := recoveryFixture(t)
	ctx := context.Background()
	preview, err := svc.PreviewRecovery(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	job, err := svc.QueueRecovery(ctx, preview.Request)
	if err != nil {
		t.Fatal(err)
	}
	original := placeFile
	t.Cleanup(func() { placeFile = original })
	placeFile = func(ctx context.Context, src, dest string, progress func(int64, int64)) error {
		if err := original(ctx, src, dest, progress); err != nil {
			return err
		}
		_, err := db.W.ExecContext(ctx, `UPDATE media_items SET title='Concurrent edit' WHERE id=?`, req.MediaItemID)
		return err
	}
	svc.recoverySweep(ctx)
	result, err := svc.RecoveryImport(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "review" {
		t.Fatalf("state=%s error=%s", result.State, result.Error)
	}
	files, err := db.ListFilesForItem(ctx, req.MediaItemID)
	if err != nil || len(files) != 0 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	if receipts.Load() != 0 {
		t.Fatal("receipt sent for stale library target")
	}
}
