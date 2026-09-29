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
