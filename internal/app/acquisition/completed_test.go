package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func completedFixture(t *testing.T) (*Service, *sqlite.DB, int64, string) {
	t.Helper()
	svc, db, item := setupWithClientType(t, &fakeClient{}, "sabnzbd")
	root := t.TempDir()
	if err := svc.SetCompletedRoots(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	return svc, db, item, root
}

func writeCompleted(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func inventoryRoot(t *testing.T, svc *Service, root string) CompletedRoot {
	t.Helper()
	inv, err := svc.CompletedInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	root = canonicalCompletedPath(root)
	for _, r := range inv.Roots {
		if r.Path == root {
			return r
		}
	}
	t.Fatalf("root %s missing from %+v", root, inv)
	return CompletedRoot{}
}

func TestCompletedInventoryAccountsForFilesWithoutAnyQueue(t *testing.T) {
	svc, _, _, root := completedFixture(t)
	writeCompleted(t, filepath.Join(root, "Forgotten", "video.mkv"), 12)
	writeCompleted(t, filepath.Join(root, ".loose.rar"), 8)
	outside := filepath.Join(t.TempDir(), "external.mkv")
	writeCompleted(t, outside, 99)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := inventoryRoot(t, svc, root)
	if r.Bytes != 20 || len(r.Entries) != 3 {
		t.Fatalf("unaccounted bytes/entries: %+v", r)
	}
	for _, e := range r.Entries {
		if e.Status != "untracked" && e.Status != "symlink" {
			t.Fatalf("unexpected ownership: %+v", e)
		}
	}
	if svc.CompletedHealth(context.Background()).Status != health.StatusWarning {
		t.Fatal("unknown files should warn")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("scanner changed external files")
	}
}

func TestCompletedReceiptsSurviveClearingActivity(t *testing.T) {
	svc, db, item, root := completedFixture(t)
	ctx := context.Background()
	path := filepath.Join(root, "known")
	writeCompleted(t, filepath.Join(path, "video.mkv"), 15)
	dl := insertImportable(t, db, item, path)
	dl.State = "failed"
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if got := inventoryRoot(t, svc, root).Entries[0].Status; got != "failed" {
		t.Fatal(got)
	}
	if _, err := db.ClearFailedDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	e := inventoryRoot(t, svc, root).Entries[0]
	if e.Bytes != 15 || e.Status != "untracked" || len(e.Receipts) != 1 || e.Receipts[0].Live {
		t.Fatalf("cleared queue erased association: %+v", e)
	}
	// A new service has exactly the same durable view before it scans.
	again := New(db, svc.bus, svc.log, svc.newIndexer, svc.newClient)
	if got := inventoryRoot(t, again, root); got.Bytes != 15 {
		t.Fatalf("restart lost inventory: %+v", got)
	}
}

func TestIncompleteCompletedScanKeepsLastKnownBytes(t *testing.T) {
	svc, _, _, root := completedFixture(t)
	ctx := context.Background()
	writeCompleted(t, filepath.Join(root, "payload"), 21)
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	before := inventoryRoot(t, svc, root)
	// An unavailable root must not look like an empty healthy directory.
	if err := os.Rename(root, root+"-offline"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Rename(root+"-offline", root) })
	if err := svc.ScanCompleted(ctx); err == nil {
		t.Fatal("missing root passed")
	}
	after := inventoryRoot(t, svc, root)
	if after.Error == "" || after.Bytes != 21 || !after.LastCompleteAt.Equal(before.LastCompleteAt) {
		t.Fatalf("lost stale evidence: %+v", after)
	}
	if err := os.Rename(root+"-offline", root); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "payload")); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if got := inventoryRoot(t, svc, root); got.Bytes != 0 || len(got.Entries) != 0 || got.Error != "" {
		t.Fatalf("external removal not reconciled: %+v", got)
	}
}

func TestCompletedInventoryShowsImportedFilesDespiteRemovalFlag(t *testing.T) {
	svc, db, item, root := completedFixture(t)
	ctx := context.Background()
	path := filepath.Join(root, "leftover")
	writeCompleted(t, filepath.Join(path, "video.mkv"), 33)
	dl := insertImportable(t, db, item, path)
	dl.State = "imported"
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkPayloadRemoved(ctx, dl.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if got := inventoryRoot(t, svc, root).Entries[0]; got.Status != "untracked" || got.Bytes != 33 {
		t.Fatalf("false receipt hides files: %+v", got)
	}
	if err := svc.CleanupPayloads(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cleanup deleted a path whose ownership had ended: %v", err)
	}
}

func TestCompletedRootsAndLibraryAliasesAreProtected(t *testing.T) {
	svc, db, item, root := completedFixture(t)
	ctx := context.Background()
	if _, err := db.AddRootFolder(ctx, t.TempDir(), domain.RootKind("movie")); err != nil {
		t.Fatal(err)
	}
	roots, err := db.ListRootFolders(ctx)
	if err != nil || len(roots) == 0 {
		t.Fatalf("fixture roots: %v", err)
	}
	alias := filepath.Join(root, "library-alias")
	if err := os.Symlink(roots[0].Path, alias); err != nil {
		t.Fatal(err)
	}
	writeCompleted(t, filepath.Join(roots[0].Path, "film", "keep.mkv"), 4)
	clients, _ := db.ListDownloadClients(ctx)
	for _, path := range []string{root, filepath.Join(alias, "film")} {
		svc.removeImportedDir(ctx, downloadRef{ID: 999, ClientID: clients[0].ID, MediaItemID: item, ImportPath: path})
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("deleted protected path %s: %v", path, err)
		}
	}
}

func TestWholeWorkingTreeSplitsCompletedAndCountsSiblings(t *testing.T) {
	svc, _, _, root := completedFixture(t)
	writeCompleted(t, filepath.Join(root, "completed", "one", "film.mkv"), 10)
	writeCompleted(t, filepath.Join(root, "completed", "two", "archive.rar"), 20)
	writeCompleted(t, filepath.Join(root, "forgotten.tmp"), 5)
	if err := svc.ScanCompleted(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := inventoryRoot(t, svc, root)
	if got.Bytes != 35 || len(got.Entries) != 4 {
		t.Fatalf("working tree not fully accounted: %+v", got)
	}
	count := 0
	for _, e := range got.Entries {
		if e.Status == "untracked" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("completed contents were collapsed: %+v", got)
	}
}

func TestRunnerSuccessMustRemoveBytesOrRemainPending(t *testing.T) {
	svc, db, item := setupUsenet(t, &fakeClient{})
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "payload")
	writeCompleted(t, filepath.Join(path, "file.mkv"), 10)
	dl := insertImportable(t, db, item, path)
	dl.State = "imported"
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	ref := downloadRef{ID: dl.ID, ClientID: dl.ClientID, Handle: dl.Handle, ImportPath: path}
	if svc.removePayload(ctx, ref) {
		t.Fatal("client acknowledgement claimed files gone")
	}
	svc.removeImportedDir(ctx, ref)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("deleted Runner-owned storage outside Curator's root")
	}
	if err := svc.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	svc.removeImportedDir(ctx, ref)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("Curator could not clean its own storage: %v", err)
	}
}

func TestReviewedStorageDeletionRejectsChangedFilesAndLiveWork(t *testing.T) {
	svc, db, item, root := completedFixture(t)
	ctx := context.Background()
	path := filepath.Join(root, "orphan", "video.mkv")
	writeCompleted(t, path, 10)
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	writeCompleted(t, path, 11)
	if err := svc.DeleteCompletedEntry(ctx, entry.Path, entry.Fingerprint); err == nil {
		t.Fatal("deleted changed files")
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry = inventoryRoot(t, svc, root).Entries[0]
	dl := insertImportable(t, db, item, filepath.Dir(path))
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteCompletedEntry(ctx, entry.Path, entry.Fingerprint); err == nil {
		t.Fatal("deleted active download")
	}
	if err := db.UpdateDownloadState(ctx, dl.ID, "failed", 0, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteCompletedEntry(ctx, entry.Path, entry.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("explicit deletion left bytes")
	}
	if got := inventoryRoot(t, svc, root); got.Bytes != 0 || len(got.Entries) != 0 {
		t.Fatalf("stale accounting after delete: %+v", got)
	}
}

func TestReviewedStorageDeletionNeverFollowsSymlink(t *testing.T) {
	svc, _, _, root := completedFixture(t)
	outside := filepath.Join(t.TempDir(), "keep.mkv")
	writeCompleted(t, outside, 50)
	if err := os.Symlink(outside, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(context.Background()); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	if err := svc.DeleteCompletedEntry(context.Background(), entry.Path, entry.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("deleted symlink target")
	}
}
