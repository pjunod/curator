package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func stageCleanupFixture(t *testing.T) (*Service, *sqlite.DB, sqlite.ImportAttemptKey, sqlite.Placement) {
	t.Helper()
	svc, db, _ := setup(t, nil, &fakeClient{})
	ctx := context.Background()
	id := grabEpisode(t, svc)
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	dl.State = "failed"
	dl.ImportPath = filepath.Join(t.TempDir(), "source")
	if err = db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	a, err := db.EnsureImportAttempt(ctx, id, dl.GenerationKey)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(t.TempDir(), "library")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(parent, ".monarr-stage-review")
	target := filepath.Join(parent, "library.mkv")
	if err = os.WriteFile(stage, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(target, []byte("existing library"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(stage)
	if err != nil {
		t.Fatal(err)
	}
	storage, parentID, err := placementParentIdentity(target)
	if err != nil {
		t.Fatal(err)
	}
	epoch := a.Epoch
	p := sqlite.Placement{ID: "review", DownloadID: id, ImportEpoch: &epoch, SourceGeneration: dl.GenerationKey, ItemID: dl.MediaItemID, Source: dl.ImportPath, Target: target, Temporary: stage, Backup: filepath.Join(parent, ".monarr-prior-review"), State: "prepared", Size: 1000, SHA256: "not-yet-copied", TemporaryObjectIdentity: deliveryFileStableIdentity(info), StorageIdentity: storage, TargetParentIdentity: parentID}
	if err = db.SealImportAttempt(ctx, a.ImportAttemptKey, []sqlite.ImportAttemptFile{{Source: p.Source, Kind: "media"}}, false); err != nil {
		t.Fatal(err)
	}
	if err = db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	result, err := svc.CancelImportAttempt(ctx, id, &a.ImportAttemptKey)
	if err != nil || result.Status != "cancelled" {
		t.Fatalf("cancel: %+v %v", result, err)
	}
	return svc, db, a.ImportAttemptKey, p
}

func TestStageCleanupDisposesOnlyExplicitEmptyOwnedStage(t *testing.T) {
	svc, db, key, p := stageCleanupFixture(t)
	ctx := context.Background()
	ids := []string{"stage:" + p.ID}
	previews, err := svc.PreviewImportStageCleanup(ctx, key, ids)
	if err != nil {
		t.Fatal(err)
	}
	fp := map[string]string{ids[0]: previews[0].Fingerprint}
	if _, err = svc.CleanupImportStages(ctx, key, ids, fp, false); err == nil {
		t.Fatal("acknowledgement bypassed")
	}
	if _, err = svc.CleanupImportStages(ctx, key, ids, map[string]string{}, true); err == nil {
		t.Fatal("snapshot bypassed")
	}
	a, err := svc.CleanupImportStages(ctx, key, ids, fp, true)
	if err != nil || a.HeldStages != 0 || a.Outcome != "cancelled" {
		t.Fatalf("cleanup: %+v %v", a, err)
	}
	if _, err = os.Lstat(p.Temporary); !os.IsNotExist(err) {
		t.Fatal("stage remains", err)
	}
	body, err := os.ReadFile(p.Target)
	if err != nil || string(body) != "existing library" {
		t.Fatal("library target touched", err)
	}
	saved, err := db.GetPlacement(ctx, p.ID)
	if err != nil || saved.State != "abandoned" {
		t.Fatalf("ledger: %+v %v", saved, err)
	}
}

func TestStageCleanupRefusesChangedNonemptySymlinkAndBackup(t *testing.T) {
	for _, kind := range []string{"nonempty", "replacement", "symlink", "backup", "storage"} {
		t.Run(kind, func(t *testing.T) {
			svc, db, key, p := stageCleanupFixture(t)
			ctx := context.Background()
			switch kind {
			case "nonempty":
				_ = os.WriteFile(p.Temporary, []byte("keep"), 0600)
			case "replacement":
				_ = os.Rename(p.Temporary, p.Temporary+".old")
				_ = os.WriteFile(p.Temporary, nil, 0600)
			case "symlink":
				_ = os.Remove(p.Temporary)
				_ = os.Symlink(p.Target, p.Temporary)
			case "backup":
				_ = os.WriteFile(p.Backup, []byte("prior library"), 0600)
			case "storage":
				_, _ = db.W.ExecContext(ctx, `UPDATE import_placements SET data=json_set(data,'$.storage_identity','wrong') WHERE id=?`, p.ID)
			}
			if _, err := svc.PreviewImportStageCleanup(ctx, key, []string{"stage:" + p.ID}); err == nil {
				t.Fatal("unsafe stage offered for cleanup")
			}
			if body, err := os.ReadFile(p.Target); err != nil || string(body) != "existing library" {
				t.Fatal("target changed", err)
			}
		})
	}
}

func TestStageCleanupRechecksFileImmediatelyBeforeUnlink(t *testing.T) {
	svc, _, key, p := stageCleanupFixture(t)
	ctx := context.Background()
	ids := []string{"stage:" + p.ID}
	previews, err := svc.PreviewImportStageCleanup(ctx, key, ids)
	if err != nil {
		t.Fatal(err)
	}
	old := stageCleanupUnlinkHook
	t.Cleanup(func() { stageCleanupUnlinkHook = old })
	stageCleanupUnlinkHook = func() {
		if err := os.Rename(p.Temporary, p.Temporary+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.Temporary, []byte("replacement"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = svc.CleanupImportStages(ctx, key, ids, map[string]string{ids[0]: previews[0].Fingerprint}, true); err == nil {
		t.Fatal("replacement unlinked")
	}
	body, err := os.ReadFile(p.Temporary)
	if err != nil || string(body) != "replacement" {
		t.Fatal("replacement not preserved", err)
	}
}

func TestStageCleanupCrashAfterUnlinkLeavesRetryableHeldLedger(t *testing.T) {
	svc, db, key, p := stageCleanupFixture(t)
	ctx := context.Background()
	ids := []string{"stage:" + p.ID}
	previews, err := svc.PreviewImportStageCleanup(ctx, key, ids)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `CREATE TRIGGER fail_dispose BEFORE UPDATE ON import_placements BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.CleanupImportStages(ctx, key, ids, map[string]string{ids[0]: previews[0].Fingerprint}, true); err == nil {
		t.Fatal("ledger failure ignored")
	}
	saved, err := db.GetPlacement(ctx, p.ID)
	if err != nil || saved.State != "prepared" {
		t.Fatalf("lost held ledger: %+v %v", saved, err)
	}
	if _, err = os.Lstat(p.Temporary); !os.IsNotExist(err) {
		t.Fatal("physical step did not finish", err)
	}
	if _, err = db.W.ExecContext(ctx, `DROP TRIGGER fail_dispose`); err != nil {
		t.Fatal(err)
	}
	previews, err = svc.PreviewImportStageCleanup(ctx, key, ids)
	if err != nil || !previews[0].Absent {
		t.Fatal("absence not previewed", err)
	}
	if _, err = svc.CleanupImportStages(ctx, key, ids, map[string]string{ids[0]: previews[0].Fingerprint}, true); err != nil {
		t.Fatal(err)
	}
}

func TestStageCleanupHonoursWriterEpochAndTitleFences(t *testing.T) {
	svc, _, key, p := stageCleanupFixture(t)
	ctx := context.Background()
	ids := []string{"stage:" + p.ID}
	stale := key
	stale.Epoch++
	if _, err := svc.PreviewImportStageCleanup(ctx, stale, ids); err == nil {
		t.Fatal("stale epoch accepted")
	}
	unlock := svc.lockDownload(key.DownloadID)
	if _, err := svc.PreviewImportStageCleanup(ctx, key, ids); err == nil {
		t.Fatal("download owner bypassed")
	}
	unlock()
	_, release := svc.lockImportTarget(ctx, p.ItemID)
	if _, err := svc.PreviewImportStageCleanup(ctx, key, ids); err == nil {
		t.Fatal("title owner bypassed")
	}
	release()
	if _, err := svc.PreviewImportStageCleanup(ctx, key, append(ids, ids...)); err == nil {
		t.Fatal("duplicate stage accepted")
	}
}

func TestStageCleanupLateBackupPreservesStageAndPlacementCustody(t *testing.T) {
	svc, db, key, p := stageCleanupFixture(t)
	ctx := context.Background()
	ids := []string{"stage:" + p.ID}
	previews, err := svc.PreviewImportStageCleanup(ctx, key, ids)
	if err != nil {
		t.Fatal(err)
	}
	old := stageCleanupUnlinkHook
	t.Cleanup(func() { stageCleanupUnlinkHook = old })
	stageCleanupUnlinkHook = func() {
		if err := os.WriteFile(p.Backup, []byte("prior library bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = svc.CleanupImportStages(ctx, key, ids, map[string]string{ids[0]: previews[0].Fingerprint}, true); err == nil {
		t.Fatal("late backup lost its placement custody")
	}
	if _, err = os.Lstat(p.Temporary); err != nil {
		t.Fatal("stage unlinked", err)
	}
	saved, err := db.GetPlacement(ctx, p.ID)
	if err != nil || saved.State != "prepared" {
		t.Fatalf("lost backup custody: %+v %v", saved, err)
	}
}

func TestStageCleanupAbsenceRetryRequiresSuccessfulDirectorySync(t *testing.T) {
	svc, db, key, p := stageCleanupFixture(t)
	ctx := context.Background()
	ids := []string{"stage:" + p.ID}
	previews, err := svc.PreviewImportStageCleanup(ctx, key, ids)
	if err != nil {
		t.Fatal(err)
	}
	old := stageCleanupSyncDirectory
	t.Cleanup(func() { stageCleanupSyncDirectory = old })
	calls := 0
	stageCleanupSyncDirectory = func(*os.Root, string) error { calls++; return errors.New("injected directory sync failure") }
	if _, err = svc.CleanupImportStages(ctx, key, ids, map[string]string{ids[0]: previews[0].Fingerprint}, true); err == nil {
		t.Fatal("sync failure ignored")
	}
	previews, err = svc.PreviewImportStageCleanup(ctx, key, ids)
	if err != nil || !previews[0].Absent {
		t.Fatal("missing retry evidence", err)
	}
	if _, err = svc.CleanupImportStages(ctx, key, ids, map[string]string{ids[0]: previews[0].Fingerprint}, true); err == nil {
		t.Fatal("absent retry skipped durable directory sync")
	}
	if calls != 2 {
		t.Fatalf("sync calls=%d want2", calls)
	}
	saved, err := db.GetPlacement(ctx, p.ID)
	if err != nil || saved.State != "prepared" {
		t.Fatal("non-durable absence was attested", err)
	}
	stageCleanupSyncDirectory = old
	if _, err = svc.CleanupImportStages(ctx, key, ids, map[string]string{ids[0]: previews[0].Fingerprint}, true); err != nil {
		t.Fatal(err)
	}
}

func TestStageCleanupParentReplacementCannotRedirectUnlink(t *testing.T) {
	svc, _, key, p := stageCleanupFixture(t)
	ctx := context.Background()
	ids := []string{"stage:" + p.ID}
	previews, err := svc.PreviewImportStageCleanup(ctx, key, ids)
	if err != nil {
		t.Fatal(err)
	}
	old := stageCleanupUnlinkHook
	t.Cleanup(func() { stageCleanupUnlinkHook = old })
	parent := filepath.Dir(p.Temporary)
	stageCleanupUnlinkHook = func() {
		if err := os.Rename(parent, parent+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(parent, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.Temporary, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = svc.CleanupImportStages(ctx, key, ids, map[string]string{ids[0]: previews[0].Fingerprint}, true); err == nil {
		t.Fatal("replacement parent was accepted")
	}
	for _, path := range []string{p.Temporary, filepath.Join(parent+".old", filepath.Base(p.Temporary))} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatal("stage removed through wrong parent", err)
		}
	}
}
