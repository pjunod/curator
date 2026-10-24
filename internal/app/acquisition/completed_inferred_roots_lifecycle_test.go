package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/ports"
)

// When no explicit completed roots have been saved, Curator combines the
// download client's local mapping with durable payload receipt parents. This
// keeps older non-manual payloads visible after Activity is dismissed while
// avoiding the unsafe assumption that a manual path identifies a shared root.
func TestCompletedInventoryInfersMappedAndReceiptRootsWithoutManualRoot(t *testing.T) {
	ctx := context.Background()
	svc, db, itemID := setupWithClientType(t, &fakeClient{}, "sabnzbd")
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) != 1 {
		t.Fatalf("download clients: %+v, %v", clients, err)
	}
	mappedRoot := t.TempDir()
	clients[0].PathMappings = []ports.PathMapping{{Remote: "/remote/completed", Local: mappedRoot}}
	if err := db.UpdateDownloadClient(ctx, clients[0]); err != nil {
		t.Fatal(err)
	}

	// This second root is discoverable only from its persisted receipt.
	receiptRoot := t.TempDir()
	manualRoot := t.TempDir()
	for _, file := range []string{
		filepath.Join(mappedRoot, "mapped-payload", "mapped.mkv"),
		filepath.Join(receiptRoot, "receipt-payload", "receipt.mkv"),
		filepath.Join(manualRoot, "manual-payload", "manual.mkv"),
	} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("media"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	persistPayload := func(path, protocol string) {
		t.Helper()
		dl := insertImportable(t, db, itemID, path)
		dl.State = "imported"
		if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
			t.Fatal(err)
		}
		if protocol != dl.Protocol {
			if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET protocol=? WHERE id=?`, protocol, dl.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	persistPayload(filepath.Join(receiptRoot, "receipt-payload"), "sabnzbd")
	persistPayload(filepath.Join(manualRoot, "manual-payload"), "manual")
	if _, err := db.ClearImportedDownloads(ctx); err != nil {
		t.Fatalf("dismiss imported Activity rows: %v", err)
	}

	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	inventory, err := svc.CompletedInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gotRoots := map[string]CompletedRoot{}
	for _, root := range inventory.Roots {
		gotRoots[root.Path] = root
	}
	for _, want := range []string{mappedRoot, receiptRoot} {
		canonical := canonicalCompletedPath(want)
		if _, ok := gotRoots[canonical]; !ok {
			t.Fatalf("inferred root %q missing from %+v", canonical, inventory.Roots)
		}
	}
	if _, ok := gotRoots[canonicalCompletedPath(manualRoot)]; ok {
		t.Fatalf("manual receipt incorrectly expanded completed-root scope: %+v", inventory.Roots)
	}
	if inventory.Bytes != int64(len("media")*2) {
		t.Fatalf("inventory accounted unexpected roots/bytes: %+v", inventory)
	}
	if _, err := os.Stat(filepath.Join(manualRoot, "manual-payload", "manual.mkv")); err != nil {
		t.Fatalf("root inference changed manual payload: %v", err)
	}
}
