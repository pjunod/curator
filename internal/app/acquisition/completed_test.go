package acquisition

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	if got.Bytes != 35 || len(got.Entries) != 3 {
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

func TestCompletedInventoryHidesCachedStorageContainer(t *testing.T) {
	svc, db, _, root := completedFixture(t)
	ctx := context.Background()
	report := CompletedRoot{Path: root, Bytes: 42, Error: "mount unavailable", Entries: []CompletedEntry{
		{Path: filepath.Join(root, "completed"), Status: "storage"},
		{Path: filepath.Join(root, "completed", "release"), Status: "untracked", Bytes: 42, Files: 1},
	}}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCompletedScan(ctx, root, string(raw)); err != nil {
		t.Fatal(err)
	}
	inv, err := svc.CompletedInventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Roots) != 1 || len(inv.Roots[0].Entries) != 1 || inv.Bytes != 42 || inv.Attention != 1 || inv.Roots[0].Error != report.Error {
		t.Fatalf("cached container affected inventory: %+v", inv)
	}
}

func TestCompletedContainerDoesNotHideFilesOrSymlinksNamedCompleted(t *testing.T) {
	for _, kind := range []string{"empty directory", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			svc, _, _, root := completedFixture(t)
			path := filepath.Join(root, "completed")
			wantEntries, wantBytes := 1, int64(0)
			switch kind {
			case "empty directory":
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				wantEntries = 0
			case "file":
				writeCompleted(t, path, 7)
				wantBytes = 7
			case "symlink":
				if err := os.Symlink(t.TempDir(), path); err != nil {
					t.Fatal(err)
				}
			}
			if err := svc.ScanCompleted(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := inventoryRoot(t, svc, root)
			if len(got.Entries) != wantEntries || got.Bytes != wantBytes {
				t.Fatalf("incorrect container handling: %+v", got)
			}
		})
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

func TestChangedStorageMountPreservesEvidenceUntilExplicitlyAccepted(t *testing.T) {
	svc, _, _, root := completedFixture(t)
	ctx := context.Background()
	writeCompleted(t, filepath.Join(root, "payload"), 21)
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	before := inventoryRoot(t, svc, root)
	offline := root + "-offline"
	if err := os.Rename(root, offline); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root); _ = os.Rename(offline, root) })
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err == nil {
		t.Fatal("replacement mount passed")
	}
	after := inventoryRoot(t, svc, root)
	if after.Bytes != 21 || after.Error == "" || after.RootIdentity != before.RootIdentity || !after.LastCompleteAt.Equal(before.LastCompleteAt) {
		t.Fatalf("lost mount evidence: %+v", after)
	}
	if err := svc.SetCompletedRoots(ctx, root); err != nil {
		t.Fatal(err)
	}
	if inventoryRoot(t, svc, root).Bytes != 21 {
		t.Fatal("saving erased stale bytes")
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if got := inventoryRoot(t, svc, root); got.Bytes != 0 || got.Error != "" {
		t.Fatalf("replacement not accepted: %+v", got)
	}
}

func TestMissingPayloadParentDoesNotRecordRemoval(t *testing.T) {
	svc, db, item, root := completedFixture(t)
	ctx := context.Background()
	parent := filepath.Join(root, "completed")
	path := filepath.Join(parent, "payload")
	writeCompleted(t, filepath.Join(path, "film.mkv"), 8)
	dl := insertImportable(t, db, item, path)
	dl.State = "imported"
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(parent, parent+"-offline"); err != nil {
		t.Fatal(err)
	}
	svc.removeImportedDir(ctx, downloadRef{ID: dl.ID, ClientID: dl.ClientID, ImportPath: path})
	got, err := db.GetDownload(ctx, dl.ID)
	if err != nil || got.PayloadRemoved {
		t.Fatalf("missing parent claimed removal: %+v %v", got, err)
	}
	if err := os.Rename(parent+"-offline", parent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestBindAliasRootsCountOnceAndProtectLibrary(t *testing.T) {
	svc, db, item, root := completedFixture(t)
	ctx := context.Background()
	completed := filepath.Join(root, "completed")
	alias := t.TempDir()
	writeCompleted(t, filepath.Join(completed, "film", "keep.mkv"), 8)
	writeCompleted(t, filepath.Join(alias, "film", "keep.mkv"), 8)
	left, _ := os.Stat(completed)
	right, _ := os.Stat(alias)
	original := storageSameFile
	storageSameFile = func(a, b os.FileInfo) bool {
		return os.SameFile(a, b) || (os.SameFile(a, left) && os.SameFile(b, right)) || (os.SameFile(a, right) && os.SameFile(b, left))
	}
	t.Cleanup(func() { storageSameFile = original })
	if err := svc.SetCompletedRoots(ctx, root+"\n"+alias); err != nil {
		t.Fatal(err)
	}
	roots, err := svc.completedRoots(ctx, nil)
	if err != nil || len(roots) != 1 || roots[0] != canonicalCompletedPath(root) {
		t.Fatalf("duplicate aliases: %v %v", roots, err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if got := inventoryRoot(t, svc, root); got.Bytes != 8 {
		t.Fatalf("double counted: %+v", got)
	}
	if _, err := db.AddRootFolder(ctx, alias, domain.RootKind("movie")); err != nil {
		t.Fatal(err)
	}
	dl := insertImportable(t, db, item, filepath.Join(completed, "film"))
	ref := downloadRef{ID: dl.ID, ClientID: dl.ClientID, ImportPath: dl.ImportPath}
	if svc.safePayloadPath(ctx, ref) {
		t.Fatal("library bind alias allowed cleanup")
	}
	if err := svc.ScanCompleted(ctx); err == nil {
		t.Fatal("library alias scan was allowed")
	}
}

func TestManualAdmissionAfterDeletionLeavesNoForgottenJob(t *testing.T) {
	svc, db, item, root := completedFixture(t)
	ctx := context.Background()
	path := filepath.Join(root, "orphan")
	writeCompleted(t, filepath.Join(path, "film.mkv"), 8)
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	// Start an import request while an operator's deletion decision is in flight.
	decisionCtx, release := svc.lockStorageDecision(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := svc.QueueManualImport(ctx, ManualImportRequest{Path: path, MediaItemID: item})
		done <- err
	}()
	err := svc.DeleteCompletedEntry(decisionCtx, entry.Path, entry.Fingerprint)
	release()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("accepted deleted source")
	}
	rows, err := db.ListRecentDownloads(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("invalid admission left a job: %+v %v", rows, err)
	}
}

func TestStorageHealthAndRootReconfiguration(t *testing.T) {
	svc, db, _, root := completedFixture(t)
	ctx := context.Background()
	if svc.CompletedHealth(ctx).Status != health.StatusWarning {
		t.Fatal("unscanned storage healthy")
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if svc.CompletedHealth(ctx).Status != health.StatusOK {
		t.Fatal("empty scanned storage unhealthy")
	}
	report := inventoryRoot(t, svc, root)
	report.LastCompleteAt = time.Now().Add(-time.Hour)
	raw, _ := json.Marshal(report)
	if err := db.SaveCompletedScan(ctx, report.Path, string(raw)); err != nil {
		t.Fatal(err)
	}
	if svc.CompletedHealth(ctx).Status != health.StatusWarning {
		t.Fatal("stale storage healthy")
	}
	next := t.TempDir()
	if err := svc.SetCompletedRoots(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	inventory, err := svc.CompletedInventory(ctx)
	if err != nil || len(inventory.Roots) != 1 || inventory.Roots[0].Path != canonicalCompletedPath(next) {
		t.Fatalf("old scope retained: %+v %v", inventory, err)
	}
	if err := db.SaveCompletedScan(ctx, canonicalCompletedPath(next), "broken JSON"); err != nil {
		t.Fatal(err)
	}
	if svc.CompletedHealth(ctx).Status != health.StatusWarning {
		t.Fatal("corrupt inventory healthy")
	}
}

func TestStorageDeletionProtectsRecoveryAndUnlocatedDownloads(t *testing.T) {
	svc, db, item, root := completedFixture(t)
	ctx := context.Background()
	path := filepath.Join(root, "payload")
	writeCompleted(t, filepath.Join(path, "film.mkv"), 8)
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	if err := svc.SetRecoverySettings(ctx, RecoverySettings{LocalRoot: path}); err != nil {
		t.Fatal(err)
	}
	if svc.safePayloadPath(ctx, downloadRef{ImportPath: path}) {
		t.Fatal("recovery payload allowed automatic cleanup")
	}
	if err := svc.DeleteCompletedEntry(ctx, entry.Path, entry.Fingerprint); err == nil {
		t.Fatal("recovery source deleted")
	}
	if err := svc.SetRecoverySettings(ctx, RecoverySettings{LocalRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, State: "grabbed", Protocol: "usenet"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteCompletedEntry(ctx, entry.Path, entry.Fingerprint); err == nil {
		t.Fatal("unlocated active work ignored")
	}
	if err := svc.DeleteCompletedEntry(ctx, entry.Path, ""); err == nil {
		t.Fatal("missing review token accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestStorageReportsRetentionCleanupAndConflictingClaims(t *testing.T) {
	svc, db, item, root := completedFixture(t)
	ctx := context.Background()
	for _, state := range []string{"imported", "downloading", "downloaded"} {
		path := filepath.Join(root, state)
		writeCompleted(t, filepath.Join(path, "film.mkv"), 8)
		dl := insertImportable(t, db, item, path)
		dl.State = state
		if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, entry := range inventoryRoot(t, svc, root).Entries {
		statuses[filepath.Base(entry.Path)] = entry.Status
	}
	if statuses["imported"] != "cleanup_pending" || statuses["downloading"] != "active" || statuses["downloaded"] != "awaiting_import" {
		t.Fatal(statuses)
	}
	clients, err := db.ListDownloadClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	client := clients[0]
	client.RemoveCompleted = false
	if err := db.UpdateDownloadClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	for _, entry := range inventoryRoot(t, svc, root).Entries {
		if filepath.Base(entry.Path) == "imported" && entry.Status != "retained" {
			t.Fatalf("retention not explained: %+v", entry)
		}
	}
	dl := insertImportable(t, db, item, filepath.Join(root, "imported"))
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	for _, entry := range inventoryRoot(t, svc, root).Entries {
		if filepath.Base(entry.Path) == "imported" && entry.Status != "ambiguous" {
			t.Fatalf("conflicting claims hidden: %+v", entry)
		}
	}
}

func TestCompletedSelectedScanMatchesInventory(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"completed/first/video.mkv", "completed/second/video.mkv", "loose.rar", "other/video.mkv"} {
		writeCompleted(t, filepath.Join(root, name), 12)
	}
	all, _, err := scanCompletedRoot(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, original := range all {
		if original.Status == "storage" {
			continue
		}
		t.Run(filepath.Base(original.Path), func(t *testing.T) {
			selected, total, err := scanCompletedSelection(context.Background(), root, original.Path, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range selected {
				if entry.Status == "storage" {
					continue
				}
				if entry.Path != original.Path || entry.Fingerprint != original.Fingerprint || entry.Bytes != original.Bytes || entry.Files != original.Files {
					t.Fatalf("targeted scan changed fingerprint or visited a sibling: %+v, want %+v", entry, original)
				}
				found = true
			}
			if !found || total != original.Bytes {
				t.Fatalf("selected payload not fully accounted for: found=%v, bytes=%d", found, total)
			}
		})
	}
}

func TestCompletedSelectedScanHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "payload", "video.mkv")
	writeCompleted(t, path, 12)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := scanCompletedSelection(ctx, root, filepath.Dir(path), nil, nil); err != context.Canceled {
		t.Fatalf("canceled scan returned %v", err)
	}
}
