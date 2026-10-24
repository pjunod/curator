package acquisition

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestLegacyManualImportConsentExpiresWhenCopyTargetChanges(t *testing.T) {
	svc, db, itemID, root := completedFixture(t)
	ctx := context.Background()
	copyPath := filepath.Join(t.TempDir(), "copy-before-review")
	if err := os.MkdirAll(copyPath, 0o755); err != nil {
		t.Fatal(err)
	}
	copyID, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: itemID, Name: "Reviewed copy", Path: copyPath, QualityProfileID: 1, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(root, "legacy-copy-payload")
	selected := filepath.Join(payload, "Reviewed.Show.S01E01.1080p.WEB-DL.mkv")
	writeCompleted(t, selected, 28)
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES('',?)`, payload); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	if len(entry.ReviewFiles) != 1 {
		t.Fatalf("copy review inventory=%+v", entry.ReviewFiles)
	}
	selection := CompletedFileSelection{Path: entry.ReviewFiles[0].Path, SnapshotIdentity: entry.ReviewFiles[0].SnapshotIdentity}

	svc.importMu.Lock()
	svc.importCh = make(chan importJob, 1)
	svc.importCtx = ctx
	svc.importJobs = map[int64]*importTicket{}
	svc.importMu.Unlock()
	id, err := svc.QueueManualImport(ctx, ManualImportRequest{
		Path: payload, Paths: []string{selection.Path}, MediaItemID: itemID, CopyID: copyID,
		ReviewLegacy: true, ReviewLegacyFingerprint: entry.Fingerprint, ReviewLegacyFiles: []CompletedFileSelection{selection},
	})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := db.GetDownload(ctx, id)
	if err != nil || len(queued.Handoff) == 0 || queued.Handoff[len(queued.Handoff)-1].TargetIdentity == "" {
		t.Fatalf("queued approval lost its copy target identity: handoff=%+v err=%v", queued.Handoff, err)
	}
	approvedIdentity := queued.Handoff[len(queued.Handoff)-1].TargetIdentity
	updated, err := db.GetMediaCopy(ctx, itemID, copyID)
	if err != nil {
		t.Fatal(err)
	}
	updated.Monitored = false
	if err := db.UpdateMediaCopy(ctx, updated); err != nil {
		t.Fatal(err)
	}
	currentIdentity, err := legacyReviewTargetIdentity(ctx, db, itemID, copyID)
	if err != nil || currentIdentity == "" || currentIdentity == approvedIdentity {
		t.Fatalf("current copy target identity=%q err=%v", currentIdentity, err)
	}
	dl, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.runImport(ctx, dl)
	if err == nil || !strings.Contains(err.Error(), "legacy payload review") {
		t.Fatalf("queued legacy approval followed a moved copy target: %v", err)
	}
	if got, err := os.ReadFile(selected); err != nil || len(got) != 28 {
		t.Fatalf("expired copy-target approval changed source bytes: len=%d err=%v", len(got), err)
	}
	var rows int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE media_item_id=? AND COALESCE(copy_id,0)=?`, itemID, copyID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("expired approval placed files in moved copy: rows=%d err=%v", rows, err)
	}
}

func TestCompletedDeleteHistoryWriteFailurePreservesPayloadAndCanRetry(t *testing.T) {
	svc, db, _, root := completedFixture(t)
	ctx := context.Background()
	payload := filepath.Join(root, "finished-release")
	media := filepath.Join(payload, "Movie.2026.1080p.WEB-DL.mkv")
	writeCompleted(t, media, 64)
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	if entry.Path != canonicalCompletedPath(payload) {
		t.Fatalf("inventory entry path=%q, payload=%q", entry.Path, payload)
	}

	// Simulate the audit store becoming unavailable at the exact destructive
	// boundary. DeleteCompletedEntry must not remove bytes until its durable
	// request event has been written.
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_delete_request BEFORE INSERT ON history_events
		WHEN NEW.type='download_files_delete_requested' BEGIN SELECT RAISE(ABORT, 'history unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	err := svc.DeleteCompletedEntry(ctx, entry.Path, entry.Fingerprint)
	if err == nil || !strings.Contains(err.Error(), "history unavailable") {
		t.Fatalf("delete did not report audit-store failure: %v", err)
	}
	if got, readErr := os.ReadFile(media); readErr != nil || len(got) != 64 {
		t.Fatalf("payload changed despite failed delete intent: bytes=%d err=%v", len(got), readErr)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_delete_request`); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteCompletedEntry(ctx, entry.Path, entry.Fingerprint); err != nil {
		t.Fatalf("same reviewed selection could not retry after history recovered: %v", err)
	}
	if _, err := os.Stat(payload); !os.IsNotExist(err) {
		t.Fatalf("successful retry left payload behind: %v", err)
	}
	var requested, completed int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM history_events WHERE type='download_files_delete_requested'`).Scan(&requested); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM history_events WHERE type='download_files_deleted'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if requested != 1 || completed != 1 {
		t.Fatalf("delete audit events requested=%d completed=%d, want one each", requested, completed)
	}
}

func TestCompletedDeleteWaitsForPlacementAuthorityToSettle(t *testing.T) {
	svc, db, _, root := completedFixture(t)
	ctx := context.Background()
	payload := filepath.Join(root, "placement-owned-release")
	media := filepath.Join(payload, "Movie.2026.1080p.WEB-DL.mkv")
	writeCompleted(t, media, 48)
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	if err := db.PreparePlacement(ctx, sqlite.Placement{
		ID: "delete-guard-pending-placement", Source: media, Target: filepath.Join(t.TempDir(), "target.mkv"),
		State: "prepared",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteCompletedEntry(ctx, entry.Path, entry.Fingerprint); err == nil || !strings.Contains(err.Error(), "pending placement") {
		t.Fatalf("payload deletion ignored an unresolved placement: %v", err)
	}
	if got, err := os.ReadFile(media); err != nil || len(got) != 48 {
		t.Fatalf("placement refusal changed payload bytes: len=%d err=%v", len(got), err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE import_placements SET state='rolled_back' WHERE id=?`, "delete-guard-pending-placement"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteCompletedEntry(ctx, entry.Path, entry.Fingerprint); err != nil {
		t.Fatalf("settled placement kept an unrelated payload undeletable: %v", err)
	}
	if _, err := os.Stat(payload); !os.IsNotExist(err) {
		t.Fatalf("retry after placement settled left payload behind: %v", err)
	}
}

func TestQueueManualImportRejectsStaleLegacyConsentBeforeAdmission(t *testing.T) {
	svc, db, itemID, root := completedFixture(t)
	ctx := context.Background()
	payload := filepath.Join(root, "legacy-approved")
	selected := filepath.Join(payload, "Reviewed.Show.S01E01.1080p.WEB-DL.mkv")
	writeCompleted(t, selected, 32)
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES('',?)`, payload); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	if len(entry.ReviewFiles) != 1 {
		t.Fatalf("review inventory files=%+v", entry.ReviewFiles)
	}
	approved := CompletedFileSelection{Path: entry.ReviewFiles[0].Path, SnapshotIdentity: entry.ReviewFiles[0].SnapshotIdentity}

	// The selected path still exists, but now contains a different generation.
	// A stale confirmation must fail before reserving an import row; the user
	// can make a new decision from a fresh inventory afterward.
	if err := os.WriteFile(selected, []byte("different reviewed generation"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := db.ListRecentDownloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.QueueManualImport(ctx, ManualImportRequest{
		Path: payload, Paths: []string{approved.Path}, MediaItemID: itemID, ReviewLegacy: true,
		ReviewLegacyFingerprint: entry.Fingerprint, ReviewLegacyFiles: []CompletedFileSelection{approved},
	})
	if err == nil || (!strings.Contains(err.Error(), "inventory") && !strings.Contains(err.Error(), "changed")) {
		t.Fatalf("stale legacy consent was not rejected clearly: %v", err)
	}
	after, err := db.ListRecentDownloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("stale consent created a durable import row: before=%d after=%d", len(before), len(after))
	}
	if got, readErr := os.ReadFile(selected); readErr != nil || string(got) != "different reviewed generation" {
		t.Fatalf("stale consent changed source bytes: %q err=%v", got, readErr)
	}

	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	fresh := inventoryRoot(t, svc, root).Entries[0]
	if len(fresh.ReviewFiles) != 1 {
		t.Fatalf("fresh review inventory files=%+v", fresh.ReviewFiles)
	}
	if fresh.Fingerprint == entry.Fingerprint || fresh.ReviewFiles[0].SnapshotIdentity == approved.SnapshotIdentity {
		t.Fatal("fresh scan failed to rotate the reviewed inventory identity")
	}
}

func TestSelectedLegacyDeleteReadFailurePreservesAllFilesAndCanRetry(t *testing.T) {
	svc, db, itemID, root := completedFixture(t)
	ctx := context.Background()
	payload := filepath.Join(root, "legacy-query-outage")
	first := filepath.Join(payload, "first.mkv")
	second := filepath.Join(payload, "second.mkv")
	writeCompleted(t, first, 21)
	writeCompleted(t, second, 22)
	dl := insertImportable(t, db, itemID, payload)
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES(?,?)`, dl.GenerationKey, payload); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDownloadState(ctx, dl.ID, "failed", 0, "held for review"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	selections := make([]CompletedFileSelection, 0, len(entry.ReviewFiles))
	for _, file := range entry.ReviewFiles {
		selections = append(selections, CompletedFileSelection{Path: file.Path, SnapshotIdentity: file.SnapshotIdentity})
	}
	if len(selections) != 2 {
		t.Fatalf("review selection=%+v", selections)
	}

	// The unresolved-placement query is part of the deletion authority check.
	// Make that read fail after the exact file identities have been reviewed;
	// neither selected member may be removed on an incomplete proof.
	if _, err := db.W.ExecContext(ctx, `ALTER TABLE import_placements RENAME TO import_placements_temporarily_unavailable`); err != nil {
		t.Fatal(err)
	}
	err := svc.DeleteSelectedLegacyFiles(ctx, entry.Path, entry.Fingerprint, selections)
	if err == nil {
		t.Fatal("selected legacy deletion proceeded without the placement authority query")
	}
	for _, path := range []string{first, second} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("read failure removed a reviewed file %s: %v", path, statErr)
		}
	}
	if _, err := db.W.ExecContext(ctx, `ALTER TABLE import_placements_temporarily_unavailable RENAME TO import_placements`); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedLegacyFiles(ctx, entry.Path, entry.Fingerprint, selections); err != nil {
		t.Fatalf("same exact reviewed selection could not retry after DB read recovered: %v", err)
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("successful retry left selected file %s: %v", path, err)
		}
	}
}

func TestQueueManualImportDatabaseReadFailureDoesNotReserveLegacyImport(t *testing.T) {
	svc, db, itemID, root := completedFixture(t)
	ctx := context.Background()
	payload := filepath.Join(root, "legacy-read-outage")
	selected := filepath.Join(payload, "Reviewed.Show.S01E01.1080p.WEB-DL.mkv")
	writeCompleted(t, selected, 24)
	if _, err := db.W.ExecContext(ctx, `INSERT INTO legacy_cleanup_review(generation_key,path) VALUES('',?)`, payload); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := inventoryRoot(t, svc, root).Entries[0]
	selection := CompletedFileSelection{Path: entry.ReviewFiles[0].Path, SnapshotIdentity: entry.ReviewFiles[0].SnapshotIdentity}
	// Hold queued work so this test measures admission without invoking the
	// importer as a side effect.
	svc.importMu.Lock()
	svc.importCh = make(chan importJob, 1)
	svc.importCtx = ctx
	svc.importJobs = map[int64]*importTicket{}
	svc.importMu.Unlock()
	before, err := db.ListRecentDownloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `ALTER TABLE legacy_cleanup_review RENAME TO legacy_cleanup_review_temporarily_unavailable`); err != nil {
		t.Fatal(err)
	}
	_, admissionErr := svc.QueueManualImport(ctx, ManualImportRequest{
		Path: payload, Paths: []string{selection.Path}, MediaItemID: itemID, ReviewLegacy: true,
		ReviewLegacyFingerprint: entry.Fingerprint, ReviewLegacyFiles: []CompletedFileSelection{selection},
	})
	if admissionErr == nil {
		t.Fatal("legacy import was admitted while its durable review fence was unreadable")
	}
	if _, err := db.W.ExecContext(ctx, `ALTER TABLE legacy_cleanup_review_temporarily_unavailable RENAME TO legacy_cleanup_review`); err != nil {
		t.Fatal(err)
	}
	after, err := db.ListRecentDownloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("failed legacy-fence read left an import reservation: before=%d after=%d", len(before), len(after))
	}
	if got, err := os.ReadFile(selected); err != nil || len(got) != 24 {
		t.Fatalf("failed admission changed source bytes: len=%d err=%v", len(got), err)
	}
	if _, err := svc.QueueManualImport(ctx, ManualImportRequest{
		Path: payload, Paths: []string{selection.Path}, MediaItemID: itemID, ReviewLegacy: true,
		ReviewLegacyFingerprint: entry.Fingerprint, ReviewLegacyFiles: []CompletedFileSelection{selection},
	}); err != nil {
		t.Fatalf("exact approval could not be retried after DB read recovered: %v", err)
	}
}

func TestQueueManualImportHandoffWriteFailureDoesNotLeaveOrphanRow(t *testing.T) {
	svc, db, itemID, root := completedFixture(t)
	ctx := context.Background()
	svc.importMu.Lock()
	svc.importCh = make(chan importJob, 1)
	svc.importCtx = ctx
	svc.importJobs = map[int64]*importTicket{}
	svc.importMu.Unlock()
	payload := filepath.Join(root, "manual-admission")
	selected := filepath.Join(payload, "Movie.2026.1080p.WEB-DL.mkv")
	writeCompleted(t, selected, 26)
	before, err := db.ListRecentDownloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_manual_handoff BEFORE UPDATE OF handoff_log ON downloads
		WHEN NEW.protocol='manual' BEGIN SELECT RAISE(ABORT, 'handoff store unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	_, admissionErr := svc.QueueManualImport(ctx, ManualImportRequest{
		Path: payload, Paths: []string{selected}, MediaItemID: itemID,
	})
	if admissionErr == nil || !strings.Contains(admissionErr.Error(), "handoff store unavailable") {
		t.Fatalf("manual admission did not report durable handoff failure: %v", admissionErr)
	}
	after, err := db.ListRecentDownloads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("failed atomic admission left an orphan row: before=%d after=%d new=%+v", len(before), len(after), after)
	}
	if len(svc.importCh) != 0 {
		t.Fatal("failed durable admission entered the import queue")
	}
	if _, err := os.Stat(selected); err != nil {
		t.Fatalf("failed admission changed source bytes: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_manual_handoff`); err != nil {
		t.Fatal(err)
	}
	id, err := svc.QueueManualImport(ctx, ManualImportRequest{Path: payload, Paths: []string{selected}, MediaItemID: itemID})
	if err != nil {
		t.Fatalf("manual admission could not retry after handoff storage recovered: %v", err)
	}
	after, err = db.ListRecentDownloads(ctx)
	if err != nil || len(after) != len(before)+1 || after[0].ID != id || len(after[0].Handoff) == 0 {
		t.Fatalf("successful retry did not create one complete admission: rows=%+v err=%v", after, err)
	}
	if len(svc.importCh) != 1 {
		t.Fatalf("successful durable admission queued %d imports, want exactly one", len(svc.importCh))
	}
}

func TestExistingManualImportAdmissionKeepsOldSelectionOnHandoffFailure(t *testing.T) {
	svc, db, itemID, root := completedFixture(t)
	ctx := context.Background()
	svc.importMu.Lock()
	svc.importCh = make(chan importJob, 2)
	svc.importCtx = ctx
	svc.importJobs = map[int64]*importTicket{}
	svc.importMu.Unlock()
	firstPath := filepath.Join(root, "first-manual-import")
	firstFile := filepath.Join(firstPath, "Movie.2026.1080p.WEB-DL.mkv")
	writeCompleted(t, firstFile, 30)
	id, err := svc.QueueManualImport(ctx, ManualImportRequest{
		Path: firstPath, Paths: []string{firstFile}, MediaItemID: itemID,
	})
	if err != nil {
		t.Fatal(err)
	}
	// No worker consumes this fixture's queue; release the slot before retrying
	// the same row with a new explicit selection.
	<-svc.importCh
	svc.importMu.Lock()
	delete(svc.importJobs, id)
	svc.importMu.Unlock()
	before, err := db.GetDownload(ctx, id)
	if err != nil || len(before.Handoff) != 1 {
		t.Fatalf("initial manual import row=%+v err=%v", before, err)
	}
	secondPath := filepath.Join(root, "second-manual-import")
	secondFile := filepath.Join(secondPath, "Movie.2026.1080p.WEB-DL.mkv")
	writeCompleted(t, secondFile, 31)
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_manual_handoff_update BEFORE UPDATE OF handoff_log ON downloads
		WHEN NEW.protocol='manual' BEGIN SELECT RAISE(ABORT, 'handoff store unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	_, err = svc.QueueManualImport(ctx, ManualImportRequest{
		Path: secondPath, Paths: []string{secondFile}, MediaItemID: itemID, DownloadID: id,
	})
	if err == nil || !strings.Contains(err.Error(), "handoff store unavailable") {
		t.Fatalf("existing-row admission did not report atomic handoff failure: %v", err)
	}
	afterFailure, err := db.GetDownload(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if afterFailure.ImportPath != before.ImportPath || len(afterFailure.Handoff) != len(before.Handoff) {
		t.Fatalf("failed admission partially replaced the existing row: before=%+v after=%+v", before, afterFailure)
	}
	if _, err := os.Stat(firstFile); err != nil {
		t.Fatalf("failed admission changed previous source: %v", err)
	}
	if _, err := os.Stat(secondFile); err != nil {
		t.Fatalf("failed admission changed requested source: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_manual_handoff_update`); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.QueueManualImport(ctx, ManualImportRequest{
		Path: secondPath, Paths: []string{secondFile}, MediaItemID: itemID, DownloadID: id,
	}); err != nil || got != id {
		t.Fatalf("existing-row admission could not retry: id=%d err=%v", got, err)
	}
	afterRetry, err := db.GetDownload(ctx, id)
	if err != nil || afterRetry.ImportPath != secondPath || len(afterRetry.Handoff) != len(before.Handoff)+1 {
		t.Fatalf("successful retry did not commit target+handoff together: row=%+v err=%v", afterRetry, err)
	}
}
