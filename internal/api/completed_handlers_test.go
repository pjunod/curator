package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/app/acquisition"
)

func TestCompletedInventorySettingsAndCachedAPI(t *testing.T) {
	e := newAPIEnv(t)
	root := t.TempDir()
	body, _ := json.Marshal(map[string]string{"completedRoots": root})
	e.put(t, "/api/v1/settings", string(body)).expect(t, http.StatusNoContent)
	e.put(t, "/api/v1/settings", `{"completedRoots":"/"}`).expect(t, http.StatusBadRequest)
	if err := os.WriteFile(filepath.Join(root, "abandoned.rar"), []byte("leftover"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.deps.Acquisition.ScanCompleted(context.Background()); err != nil {
		t.Fatal(err)
	}
	rr := e.get(t, "/api/v1/downloads/inventory").expect(t, http.StatusOK)
	var inv acquisition.CompletedInventory
	if err := json.Unmarshal(rr.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	if inv.Bytes != 8 || inv.Attention != 1 {
		t.Fatalf("unaccounted payload: %+v", inv)
	}
	// Reads use the cached scan even if the file changes after it.
	if err := os.Remove(filepath.Join(root, "abandoned.rar")); err != nil {
		t.Fatal(err)
	}
	rr = e.get(t, "/api/v1/downloads/inventory").expect(t, http.StatusOK)
	if err := json.Unmarshal(rr.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	if inv.Bytes != 8 {
		t.Fatal("API performed unrequested disk traversal")
	}
}

func TestStorageDeletionAPIRequiresCurrentReview(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	if err := e.acq.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "orphan.rar")
	if err := os.WriteFile(path, []byte("payload"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := e.acq.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	inv, err := e.acq.CompletedInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entry := inv.Roots[0].Entries[0]
	endpoint := "/api/v1/downloads/inventory/entry"
	for _, body := range []string{"{", `{"path":"/","fingerprint":"x"}`} {
		rr := do(t, e.h, http.MethodDelete, endpoint, body)
		if rr.Code < 400 {
			t.Fatalf("invalid request accepted: %s", rr.Body.String())
		}
	}
	body, _ := json.Marshal(map[string]string{"path": entry.Path, "fingerprint": entry.Fingerprint})
	rr := do(t, e.h, http.MethodDelete, endpoint, string(body))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("payload survived: %v", err)
	}
	rr = do(t, e.h, http.MethodDelete, endpoint, string(body))
	if rr.Code != http.StatusConflict {
		t.Fatal("stale token accepted")
	}
	e.srv.deps.Acquisition = nil
	e.get(t, "/api/v1/downloads/inventory").expect(t, http.StatusOK)
	if rr := do(t, e.h, http.MethodDelete, endpoint, string(body)); rr.Code != http.StatusServiceUnavailable {
		t.Fatal("missing service accepted deletion")
	}
}

func TestStorageChangePreviewAndAcceptanceUseRealHTTPAndAtomicReview(t *testing.T) {
	e := newAPIEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	if err := e.acq.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "pre-change-payload.mkv")
	if err := os.WriteFile(file, []byte("review required bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.acq.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	inventory, err := e.acq.CompletedInventory(ctx)
	if err != nil || len(inventory.Roots) != 1 {
		t.Fatalf("inventory=%+v %v", inventory, err)
	}
	cached := inventory.Roots[0]
	observed := cached.StorageIdentity
	// Seed a historical expected source while retaining the actual live observer.
	// The HTTP endpoints and SQLite transaction remain the real product paths.
	cached.StorageIdentity = "nfs4\x00previous-server:/exports\x00/"
	cached.ObservedStorageIdentity = observed
	cached.Error = "storage source changed"
	raw, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.SaveCompletedScan(ctx, cached.Path, string(raw)); err != nil {
		t.Fatal(err)
	}
	request := acquisition.StorageChangeRequest{Root: cached.Path, ExpectedIdentity: cached.StorageIdentity, ObservedIdentity: observed}
	body, _ := json.Marshal(request)
	e.post(t, "/api/v1/downloads/inventory/storage-change/accept", string(body)).expect(t, http.StatusConflict)
	result := e.post(t, "/api/v1/downloads/inventory/storage-change/preview", string(body)).expect(t, http.StatusOK)
	var review acquisition.StorageChangeReview
	if err := json.Unmarshal(result.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	if review.ReviewToken == "" || review.ExistingPayloads != 1 {
		t.Fatalf("HTTP omitted exact impact review: %+v", review)
	}
	body, _ = json.Marshal(review.StorageChangeRequest)
	e.post(t, "/api/v1/downloads/inventory/storage-change/accept", string(body)).expect(t, http.StatusNoContent)
	e.post(t, "/api/v1/downloads/inventory/storage-change/accept", string(body)).expect(t, http.StatusConflict)
	held, err := e.db.LegacyCleanupReviewPath(ctx, file)
	if err != nil || !held {
		t.Fatalf("acceptance granted existing payload authority: %v %v", held, err)
	}
	actual, err := os.ReadFile(file)
	if err != nil || string(actual) != "review required bytes" {
		t.Fatalf("acceptance mutated existing payload: %q %v", actual, err)
	}
	var audits int
	if err := e.db.R.QueryRowContext(ctx, `SELECT count(*) FROM history_events WHERE type='storage_change_accepted'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("acceptance audit=%d %v", audits, err)
	}
}
