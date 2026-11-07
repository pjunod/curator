package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestLegacySelectedDeletePreservesPayloadOwnedByActiveDownload(t *testing.T) {
	svc, db, itemID, root := completedFixture(t)
	ctx := context.Background()
	payload := filepath.Join(root, "active-run", "release")
	selectedPath := filepath.Join(payload, "selected.mkv")
	siblingPath := filepath.Join(payload, "sibling.mkv")
	writeCompleted(t, selectedPath, 31)
	writeCompleted(t, siblingPath, 37)
	dl := insertImportable(t, db, itemID, payload)
	aliasParent := filepath.Join(t.TempDir(), "download-root-alias")
	if err := os.Symlink(filepath.Dir(payload), aliasParent); err != nil {
		t.Fatalf("create explicit storage alias: %v", err)
	}
	aliasPayload := filepath.Join(aliasParent, filepath.Base(payload))
	dl.State, dl.ImportPath, dl.SavePath = "downloading", aliasPayload, aliasPayload
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	reservations, err := db.ListDownloadReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var reservationFound bool
	for _, reservation := range reservations {
		if reservation.ID == dl.ID && reservation.State == "downloading" && reservation.ImportPath == aliasPayload {
			reservationFound = true
		}
	}
	if !reservationFound {
		t.Fatalf("active payload reservation was not persisted: %+v", reservations)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES(?,?)`, dl.GenerationKey, payload); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	selection := completedSelectionForPath(t, &entry, selectedPath)
	reservations, err = db.ListDownloadReservations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reservations) == 0 {
		t.Fatal("completed inventory removed the active download reservation")
	}

	if err := svc.DeleteSelectedLegacyFiles(ctx, entry.Path, entry.Fingerprint, []CompletedFileSelection{selection}); err == nil {
		t.Fatalf("selected legacy deletion proceeded while a live download reserved payload=%q entry=%q reservations=%+v", payload, entry.Path, reservations)
	}
	for _, path := range []string{selectedPath, siblingPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("active download payload member %q was changed: %v", path, err)
		}
	}

	// An active job elsewhere must not block the next check, so the pending
	// placement below independently exercises alias-safe placement fencing.
	unrelated := filepath.Join(t.TempDir(), "another-release")
	dl.ImportPath, dl.SavePath = unrelated, unrelated
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}

	// An unresolved placement's source is another ownership fence. Store the
	// source through the explicit alias and ensure it protects these exact
	// payload bytes too.
	target := filepath.Join(t.TempDir(), "library", "target.mkv")
	placement := sqlite.Placement{
		ID: "legacy-delete-pending-placement", Source: aliasPayload, Target: target,
		Temporary: target + ".stage", Backup: target + ".prior", State: "prepared",
		ItemID: itemID, CopyID: 0, EpisodeIDs: []int64{1}, Release: "pending placement",
	}
	if err := db.PreparePlacement(ctx, placement); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedLegacyFiles(ctx, entry.Path, entry.Fingerprint, []CompletedFileSelection{selection}); err == nil {
		t.Fatal("selected deletion proceeded while a prepared placement retained the aliased payload")
	}
	for _, path := range []string{selectedPath, siblingPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("pending placement changed payload member %q: %v", path, err)
		}
	}
	if _, err := db.W.ExecContext(ctx, `DELETE FROM import_placements WHERE id=?`, placement.ID); err != nil {
		t.Fatal(err)
	}

	// With the unrelated active job and placement both outside this payload,
	// the exact unchanged review selection can proceed.
	if err := svc.DeleteSelectedLegacyFiles(ctx, entry.Path, entry.Fingerprint, []CompletedFileSelection{selection}); err != nil {
		t.Fatalf("unrelated active download blocked selected legacy deletion: %v", err)
	}
	if _, err := os.Stat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("reviewed selected file remains after unrelated reservation moved away: %v", err)
	}
	if _, err := os.Stat(siblingPath); err != nil {
		t.Fatalf("unselected sibling changed on retry: %v", err)
	}
}

func completedSelectionForPath(t *testing.T, entry *CompletedEntry, path string) CompletedFileSelection {
	t.Helper()
	for _, file := range entry.ReviewFiles {
		if canonicalCompletedPath(file.Path) == canonicalCompletedPath(path) {
			return CompletedFileSelection{Path: file.Path, SnapshotIdentity: file.SnapshotIdentity}
		}
	}
	t.Fatalf("review selection %q missing from entry %+v", path, entry)
	return CompletedFileSelection{}
}
