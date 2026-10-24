package acquisition

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func TestSelectedDeliveryDeleteReplaysAfterUnlinkAndLedgerWriteFailure(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, protocol, map[string][]byte{
		"operator-notes.txt": []byte("keep this sibling"),
	})
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	receipt := receipts[0]
	if _, err := svc.verifyDeliveryManifest(ctx, cfg, receipt); err != nil {
		t.Fatalf("verify manifest: %v", err)
	}
	receipt.ConsumerReceiptID = "selected-delete-replay"
	receipt.NextAttemptAt = time.Now()
	if err := db.PrepareDeliveryAcceptance(ctx, receipt); err != nil {
		t.Fatalf("prepare local acceptance: %v", err)
	}
	dl.ImportPath, dl.State = publication, "downloaded"
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatalf("associate publication: %v", err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := completedEntryForPath(t, svc, publication)
	originalFingerprint := entry.Fingerprint
	selectedPath := filepath.Join(publication, epRelease+".mkv")
	selected := completedSelectionForPath(t, entry, selectedPath)

	// Fail only the post-unlink ledger transition. PrepareExplicitDeliveryDeleteBatch
	// must commit first so the crash leaves a replayable, exact-file obligation.
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_explicit_delete_stamp
		BEFORE UPDATE OF cleanup_state ON runner_delivery_file_outcomes
		WHEN NEW.cleanup_state='removed'
		BEGIN SELECT RAISE(ABORT, 'explicit delete ledger unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	deleteErr := svc.DeleteSelectedDeliveryFiles(ctx, entry.Path, entry.Fingerprint, []CompletedFileSelection{selected})
	if deleteErr == nil || !containsText(deleteErr.Error(), "explicit delete ledger unavailable") {
		t.Fatalf("injected post-unlink failure was not reached: %v", deleteErr)
	}
	if _, err := os.Lstat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("fault did not occur after selected file unlink: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(publication, "operator-notes.txt")); err != nil || string(got) != "keep this sibling" {
		t.Fatalf("selected delete changed its unselected sibling: %q err=%v", got, err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, epRelease+".mkv")
	if err != nil || row.CleanupState != "retry" || row.CleanupError == "" || row.CleanupNextAttemptAt.IsZero() {
		t.Fatalf("failure did not retain the exact pending delete obligation: %+v err=%v", row, err)
	}
	if _, err := os.Stat(publication); err != nil {
		t.Fatalf("publication directory disappeared while the explicit batch was pending: %v", err)
	}
	if _, err := svc.removeEmptyDeliveryDirectories(ctx, receipt); err == nil {
		t.Fatal("directory cleanup ignored the pending explicit deletion batch")
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_explicit_delete_stamp`); err != nil {
		t.Fatal(err)
	}
	// A separate generic cleanup worker can stamp this already-unlinked row
	// removed before the explicit batch records its audit event. That generic
	// state is not proof that this batch completed.
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_state='removed' WHERE receipt_id=? AND path=?`, receipt.ID, epRelease+".mkv"); err != nil {
		t.Fatal(err)
	}
	if err := svc.cleanupDeliveryFiles(ctx, receipt); err != nil {
		t.Fatalf("generic cleanup failed while an exact batch was pending: %v", err)
	}
	var historyRecorded int
	if err := db.R.QueryRowContext(ctx, `SELECT history_recorded FROM runner_delivery_explicit_delete_files WHERE batch_id=(SELECT batch_id FROM runner_delivery_explicit_delete_batches WHERE receipt_id=?) AND path=?`, receipt.ID, epRelease+".mkv").Scan(&historyRecorded); err != nil || historyRecorded != 0 {
		t.Fatalf("generic cleanup incorrectly completed the explicit audit: history=%d err=%v", historyRecorded, err)
	}
	if _, err := os.Stat(publication); err != nil {
		t.Fatalf("generic cleanup removed the publication directory during explicit replay: %v", err)
	}

	// Reopen SQLite and rebuild the inventory as a fresh process would. The
	// selected file is absent, but the receipt and sibling remain reviewable.
	reopened := reopenLifecycleDB(t, db)
	restarted := New(reopened, svc.bus, svc.log, svc.newIndexer, svc.newClient)
	if err := restarted.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	completedEntryForPath(t, restarted, publication)
	if err := restarted.DeleteSelectedDeliveryFiles(ctx, publication, originalFingerprint, []CompletedFileSelection{selected}); err != nil {
		t.Fatalf("restart could not settle the already-unlinked exact selection: %v", err)
	}
	row, err = reopened.DeliveryFileOutcomeByPath(ctx, receipt.ID, epRelease+".mkv")
	if err != nil || row.CleanupState != "removed" {
		t.Fatalf("restart did not settle explicit deletion: %+v err=%v", row, err)
	}
	if got, err := os.ReadFile(filepath.Join(publication, "operator-notes.txt")); err != nil || string(got) != "keep this sibling" {
		t.Fatalf("replay changed the unselected sibling: %q err=%v", got, err)
	}
}

func TestSelectedDeliveryDeleteRejectsChangedSnapshotThenAllowsFreshReview(t *testing.T) {
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixture(t, protocol)
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	receipt := receipts[0]
	if _, err := svc.verifyDeliveryManifest(ctx, cfg, receipt); err != nil {
		t.Fatalf("verify manifest: %v", err)
	}
	receipt.ConsumerReceiptID = "changed-snapshot-delete-review"
	receipt.NextAttemptAt = time.Now()
	if err := db.PrepareDeliveryAcceptance(ctx, receipt); err != nil {
		t.Fatalf("prepare local acceptance: %v", err)
	}
	dl.ImportPath, dl.State = publication, "downloaded"
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatalf("associate publication: %v", err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry := completedEntryForPath(t, svc, publication)
	selectedPath := filepath.Join(publication, epRelease+".mkv")
	oldSelection := completedSelectionForPath(t, entry, selectedPath)
	if err := os.WriteFile(selectedPath, []byte("same path, different reviewed bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, entry.Path, entry.Fingerprint, []CompletedFileSelection{oldSelection}); err == nil {
		t.Fatal("selected deletion accepted a path whose snapshot changed after review")
	}
	if got, err := os.ReadFile(selectedPath); err != nil || string(got) != "same path, different reviewed bytes" {
		t.Fatalf("stale selection changed current bytes: %q err=%v", got, err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, epRelease+".mkv")
	if err == nil && row.CleanupState == "removed" {
		t.Fatalf("stale selection changed cleanup authority: %+v", row)
	}
	if err != nil && !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("unexpected ledger read after stale selection: %+v err=%v", row, err)
	}

	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	entry = completedEntryForPath(t, svc, publication)
	fresh := completedSelectionForPath(t, entry, selectedPath)
	if fresh.SnapshotIdentity == oldSelection.SnapshotIdentity {
		t.Fatal("fresh scan did not rotate the selected-file review identity")
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, entry.Path, entry.Fingerprint, []CompletedFileSelection{fresh}); err != nil {
		t.Fatalf("fresh exact review could not delete the changed payload: %v", err)
	}
	if _, err := os.Lstat(selectedPath); !os.IsNotExist(err) {
		t.Fatalf("freshly reviewed selected file remains: %v", err)
	}
}

func TestSelectedDeliveryDeletePersistsWholeBatchBeforeFirstUnlinkAndReplaysAfterRestart(t *testing.T) {
	ctx := context.Background()
	svc, db, receipt, publication := reviewedDeliveryDeleteFixture(t, map[string][]byte{
		"episode-02.mkv": []byte("selected second file"),
		"keep.txt":       []byte("unselected sibling"),
	})
	entry := completedEntryForPath(t, svc, publication)
	fingerprint := entry.Fingerprint
	firstPath := filepath.Join(publication, epRelease+".mkv")
	secondPath := filepath.Join(publication, "episode-02.mkv")
	selections := []CompletedFileSelection{
		completedSelectionForPath(t, entry, firstPath),
		completedSelectionForPath(t, entry, secondPath),
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_first_selected_delete
		BEFORE UPDATE OF cleanup_state ON runner_delivery_file_outcomes
		WHEN NEW.cleanup_state='removed'
		BEGIN SELECT RAISE(ABORT,'injected selected delete stamp failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, selections); err == nil || !containsText(err.Error(), "injected selected delete stamp failure") {
		t.Fatalf("did not reach the intended first-file post-unlink failure: %v", err)
	}
	if _, err := os.Lstat(firstPath); !os.IsNotExist(err) {
		t.Fatalf("first selected file was not unlinked before the injected stamp failure: %v", err)
	}
	if got, err := os.ReadFile(secondPath); err != nil || string(got) != "selected second file" {
		t.Fatalf("batch failure touched a later selected file before it was processed: %q err=%v", got, err)
	}
	var batchCount, fileCount int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_explicit_delete_batches WHERE receipt_id=?`, receipt.ID).Scan(&batchCount); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_explicit_delete_files f JOIN runner_delivery_explicit_delete_batches b ON b.batch_id=f.batch_id WHERE b.receipt_id=?`, receipt.ID).Scan(&fileCount); err != nil {
		t.Fatal(err)
	}
	if batchCount != 1 || fileCount != len(selections) {
		t.Fatalf("the complete selection was not persisted atomically before unlink: batches=%d files=%d", batchCount, fileCount)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_first_selected_delete`); err != nil {
		t.Fatal(err)
	}

	// A subset is not a replay of the approved batch and cannot widen or
	// rewrite the durable intent after the inventory fingerprint changed.
	restartedDB := reopenLifecycleDB(t, db)
	restarted := New(restartedDB, svc.bus, svc.log, svc.newIndexer, svc.newClient)
	if err := restarted.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, selections[:1]); err == nil {
		t.Fatalf("mixed/subset replay was not refused: %v", err)
	}
	freshEntry := completedEntryForPath(t, restarted, publication)
	mixed := append(append([]CompletedFileSelection(nil), selections...), completedSelectionForPath(t, freshEntry, filepath.Join(publication, "keep.txt")))
	if err := restarted.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, mixed); err == nil {
		t.Fatal("replay widened the original batch with a newly selected file")
	}
	if got, err := os.ReadFile(secondPath); err != nil || string(got) != "selected second file" {
		t.Fatalf("rejected subset replay changed the unprocessed file: %q err=%v", got, err)
	}
	if err := restarted.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, selections); err != nil {
		t.Fatalf("exact full batch did not resume after DB reopen/fresh scan: %v", err)
	}
	for _, path := range []string{firstPath, secondPath} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("exact resumed batch left selected path %q: %v", path, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(publication, "keep.txt")); err != nil || string(got) != "unselected sibling" {
		t.Fatalf("exact resumed batch changed unselected sibling: %q err=%v", got, err)
	}
}

func TestSelectedDeliveryDeleteReplayPreservesRecreatedPathAndRejectsGenericPending(t *testing.T) {
	t.Run("recreated file", func(t *testing.T) {
		ctx := context.Background()
		svc, db, _, publication := reviewedDeliveryDeleteFixture(t, nil)
		entry := completedEntryForPath(t, svc, publication)
		fingerprint := entry.Fingerprint
		path := filepath.Join(publication, epRelease+".mkv")
		selection := completedSelectionForPath(t, entry, path)
		if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_recreated_path_stamp
			BEFORE UPDATE OF cleanup_state ON runner_delivery_file_outcomes
			WHEN NEW.cleanup_state='removed'
			BEGIN SELECT RAISE(ABORT,'injected post-unlink stamp failure'); END`); err != nil {
			t.Fatal(err)
		}
		if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, []CompletedFileSelection{selection}); err == nil {
			t.Fatal("expected injected post-unlink failure")
		}
		if err := os.WriteFile(path, []byte("recreated different generation"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_recreated_path_stamp`); err != nil {
			t.Fatal(err)
		}
		if err := svc.ScanCompleted(ctx); err != nil {
			t.Fatal(err)
		}
		if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, []CompletedFileSelection{selection}); err == nil {
			t.Fatal("replay deleted a recreated path with a different reviewed identity")
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "recreated different generation" {
			t.Fatalf("recreated path was changed: %q err=%v", got, err)
		}
	})

	t.Run("generic pending is not an explicit intent", func(t *testing.T) {
		ctx := context.Background()
		svc, db, receipt, publication := reviewedDeliveryDeleteFixture(t, map[string][]byte{"keep.txt": []byte("keep")})
		entry := completedEntryForPath(t, svc, publication)
		fingerprint := entry.Fingerprint
		path := filepath.Join(publication, epRelease+".mkv")
		selection := completedSelectionForPath(t, entry, path)
		var manifest sqlite.DeliveryManifestFile
		for _, file := range receipt.Delivery.Files {
			if file.Path == epRelease+".mkv" {
				info, statErr := os.Lstat(path)
				if statErr != nil {
					t.Fatal(statErr)
				}
				manifest = sqlite.DeliveryManifestFile{Path: file.Path, SizeBytes: file.SizeBytes, SHA256: file.SHA256, FileGeneration: deliveryFileGeneration(info)}
			}
		}
		if manifest.Path == "" {
			t.Fatal("selected manifest file missing")
		}
		if err := db.EnsureExplicitDeliveryFileOutcome(ctx, receipt.ID, manifest); err != nil {
			t.Fatal(err)
		}
		// Model a legacy/incomplete generic pending marker with deliberately no
		// exact-delete batch. Such a marker is not valid operator authority to
		// replay an unlink after the source disappeared.
		res, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_file_outcomes SET cleanup_state='pending',cleanup_error='legacy unjournaled selection',cleanup_next_attempt_at=0 WHERE receipt_id=? AND path=? AND file_generation!=''`, receipt.ID, epRelease+".mkv")
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := res.RowsAffected(); err != nil || changed != 1 {
			t.Fatalf("legacy unjournaled pending fixture changed %d rows err=%v, want one exact outcome", changed, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := svc.ScanCompleted(ctx); err != nil {
			t.Fatal(err)
		}
		if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, []CompletedFileSelection{selection}); err == nil {
			t.Fatal("generic pending cleanup state authorized absent-file deletion replay")
		}
		if got, err := os.ReadFile(filepath.Join(publication, "keep.txt")); err != nil || string(got) != "keep" {
			t.Fatalf("refused generic-pending replay changed sibling: %q err=%v", got, err)
		}
	})
}

func TestSelectedDeliveryDeleteHistoryFailureIsIdempotentAfterOutcomeCommit(t *testing.T) {
	ctx := context.Background()
	svc, db, receipt, publication := reviewedDeliveryDeleteFixture(t, nil)
	entry := completedEntryForPath(t, svc, publication)
	fingerprint := entry.Fingerprint
	path := filepath.Join(publication, epRelease+".mkv")
	selection := completedSelectionForPath(t, entry, path)
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_explicit_delete_history
		BEFORE INSERT ON history_events WHEN NEW.type='delivery_file_deleted'
		BEGIN SELECT RAISE(ABORT,'injected history outage'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, []CompletedFileSelection{selection}); err == nil || !containsText(err.Error(), "injected history outage") {
		t.Fatalf("did not reach the history failure after committed removal: %v", err)
	}
	row, err := db.DeliveryFileOutcomeByPath(ctx, receipt.ID, selectionPathRelative(t, publication, selection.Path))
	if err != nil || row.CleanupState != "retry" || row.CleanupError == "" || row.CleanupNextAttemptAt.IsZero() {
		t.Fatalf("history failure did not preserve a replayable pending outcome: %+v err=%v", row, err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("selected source was not removed before history failure: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_explicit_delete_history`); err != nil {
		t.Fatal(err)
	}
	restarted := New(db, svc.bus, svc.log, svc.newIndexer, svc.newClient)
	if err := restarted.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := restarted.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, []CompletedFileSelection{selection}); err != nil {
		t.Fatalf("exact replay after transactional history failure was not idempotent: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("idempotent replay touched the already-removed path: %v", err)
	}
	var historyCount int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_events WHERE type='delivery_file_deleted'`).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 1 {
		t.Fatalf("retry did not write exactly one visible deletion history event: %d", historyCount)
	}
}

func TestExplicitDeliveryDeleteBatchResumesFromLocalWorkerAfterRestartAndClientOutage(t *testing.T) {
	ctx := context.Background()
	svc, db, receipt, publication := reviewedDeliveryDeleteFixture(t, map[string][]byte{
		"keep.txt": []byte("unselected sibling"),
	})
	entry := completedEntryForPath(t, svc, publication)
	path := filepath.Join(publication, epRelease+".mkv")
	selection := completedSelectionForPath(t, entry, path)
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_batch_outcome
		BEFORE UPDATE OF cleanup_state ON runner_delivery_file_outcomes
		WHEN NEW.cleanup_state='removed'
		BEGIN SELECT RAISE(ABORT,'simulated local database outage'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, entry.Fingerprint, []CompletedFileSelection{selection}); err == nil || !containsText(err.Error(), "simulated local database outage") {
		t.Fatalf("initial explicit deletion did not reach the injected post-unlink failure: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("expected a durable batch with the source already unlinked: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_batch_outcome`); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDownloadState(ctx, receipt.DownloadID, "imported", 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClearImportedDownloads(ctx); err != nil {
		t.Fatal(err)
	}
	var activityDismissed int
	if err := db.R.QueryRowContext(ctx, `SELECT activity_dismissed FROM downloads WHERE id=?`, receipt.DownloadID).Scan(&activityDismissed); err != nil || activityDismissed != 1 {
		t.Fatalf("test did not dismiss the visible Activity row while retaining custody: dismissed=%d err=%v", activityDismissed, err)
	}
	// Reopen without carrying the original browser selection or any live
	// Runner session. The local scheduler must reconstruct only the saved batch.
	reopened := reopenLifecycleDB(t, db)
	if _, err := reopened.W.ExecContext(ctx, `UPDATE runner_delivery_explicit_delete_batches SET retry_next_attempt_at=0 WHERE receipt_id=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, svc.bus, svc.log, svc.newIndexer, svc.newClient)
	workerCtx, cancel := context.WithCancel(ctx)
	restarted.StartImporters(workerCtx)
	defer func() {
		cancel()
		restarted.WaitImporters()
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var recorded int
		if err := reopened.R.QueryRowContext(ctx, `SELECT history_recorded FROM runner_delivery_explicit_delete_files WHERE batch_id=(SELECT batch_id FROM runner_delivery_explicit_delete_batches WHERE receipt_id=?) AND path=?`, receipt.ID, epRelease+".mkv").Scan(&recorded); err == nil && recorded == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var recorded int
	if err := reopened.R.QueryRowContext(ctx, `SELECT history_recorded FROM runner_delivery_explicit_delete_files WHERE batch_id=(SELECT batch_id FROM runner_delivery_explicit_delete_batches WHERE receipt_id=?) AND path=?`, receipt.ID, epRelease+".mkv").Scan(&recorded); err != nil || recorded != 1 {
		t.Fatalf("offline local worker did not finish the persisted delete batch: recorded=%d err=%v", recorded, err)
	}
	var historyCount int
	if err := reopened.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM history_events WHERE type='delivery_file_deleted' AND media_item_id=?`, receipt.MediaItemID).Scan(&historyCount); err != nil || historyCount != 1 {
		t.Fatalf("replayed deletion did not write exactly one event for the library item: count=%d err=%v", historyCount, err)
	}
	if got, err := os.ReadFile(filepath.Join(publication, "keep.txt")); err != nil || string(got) != "unselected sibling" {
		t.Fatalf("worker changed the unselected sibling: %q err=%v", got, err)
	}
}

func TestExplicitDeliveryDeleteWorkerReplaysExactAliasedReviewPathsAfterRestart(t *testing.T) {
	ctx := context.Background()
	svc, db, receipt, publication := reviewedDeliveryDeleteFixture(t, map[string][]byte{
		"episode-02.mkv": []byte("second selected file"),
		"keep.txt":       []byte("unselected sibling"),
	})
	entry := completedEntryForPath(t, svc, publication)
	// Keep a distinct lexical spelling that resolves to the same directory.
	// This reproduces /var versus /private/var aliases without relying on a
	// platform-specific mount alias or introducing a symlink into the tree.
	parent := filepath.Dir(publication)
	aliasedPayload := parent + string(filepath.Separator) + "." + string(filepath.Separator) + filepath.Base(publication)
	if canonicalCompletedPath(aliasedPayload) != canonicalCompletedPath(publication) || aliasedPayload == publication {
		t.Fatalf("fixture did not produce a distinct alias: %q => %q, original %q", aliasedPayload, canonicalCompletedPath(aliasedPayload), publication)
	}
	selections := make([]CompletedFileSelection, 0, 2)
	for _, name := range []string{epRelease + ".mkv", "episode-02.mkv"} {
		path := aliasedPayload + string(filepath.Separator) + name
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		selections = append(selections, CompletedFileSelection{Path: path, SnapshotIdentity: completedReviewFileIdentity(path, info)})
	}
	if _, err := db.W.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER fail_first_aliased_delete
		BEFORE UPDATE OF cleanup_state ON runner_delivery_file_outcomes
		WHEN NEW.cleanup_state='removed' AND NEW.path='%s'
		BEGIN SELECT RAISE(ABORT,'injected alias replay failure'); END`, epRelease+".mkv")); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, entry.Fingerprint, selections); err == nil || !containsText(err.Error(), "injected alias replay failure") {
		t.Fatalf("initial delete did not reach the intended post-unlink failure: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(publication, epRelease+".mkv")); !os.IsNotExist(err) {
		t.Fatalf("first selected file was not unlinked before failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(publication, "episode-02.mkv")); err != nil {
		t.Fatalf("failure touched the later selected file: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_first_aliased_delete`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_explicit_delete_batches SET retry_next_attempt_at=0 WHERE receipt_id=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	reopened := reopenLifecycleDB(t, db)
	restarted := New(reopened, svc.bus, svc.log, svc.newIndexer, svc.newClient)
	workerCtx, cancel := context.WithCancel(ctx)
	restarted.StartImporters(workerCtx)
	defer func() {
		cancel()
		restarted.WaitImporters()
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var recorded int
		_ = reopened.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_explicit_delete_files WHERE batch_id=(SELECT batch_id FROM runner_delivery_explicit_delete_batches WHERE receipt_id=?) AND history_recorded=1`, receipt.ID).Scan(&recorded)
		if recorded == len(selections) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	var recorded int
	if err := reopened.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_explicit_delete_files WHERE batch_id=(SELECT batch_id FROM runner_delivery_explicit_delete_batches WHERE receipt_id=?) AND history_recorded=1`, receipt.ID).Scan(&recorded); err != nil || recorded != len(selections) {
		t.Fatalf("worker did not finish exact aliased batch: recorded=%d err=%v", recorded, err)
	}
	for _, name := range []string{epRelease + ".mkv", "episode-02.mkv"} {
		if _, err := os.Lstat(filepath.Join(publication, name)); !os.IsNotExist(err) {
			t.Fatalf("worker left selected path %q: %v", name, err)
		}
	}
	if got, err := os.ReadFile(filepath.Join(publication, "keep.txt")); err != nil || string(got) != "unselected sibling" {
		t.Fatalf("alias replay changed unselected sibling: %q err=%v", got, err)
	}
}

func TestExplicitDeliveryDeleteWorkerPersistsRetryWhenReviewedAliasRetargets(t *testing.T) {
	ctx := context.Background()
	svc, db, receipt, publication := reviewedDeliveryDeleteFixture(t, map[string][]byte{
		"episode-02.mkv": []byte("later selected source remains"),
	})
	entry := completedEntryForPath(t, svc, publication)
	alias := filepath.Join(filepath.Dir(publication), "reviewed-publication-alias")
	if err := os.Symlink(publication, alias); err != nil {
		t.Fatal(err)
	}
	selections := make([]CompletedFileSelection, 0, 2)
	for _, name := range []string{epRelease + ".mkv", "episode-02.mkv"} {
		path := filepath.Join(alias, name)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		selections = append(selections, CompletedFileSelection{Path: path, SnapshotIdentity: completedReviewFileIdentity(path, info)})
	}
	if _, err := db.W.ExecContext(ctx, fmt.Sprintf(`CREATE TRIGGER fail_alias_replay
		BEFORE UPDATE OF cleanup_state ON runner_delivery_file_outcomes
		WHEN NEW.cleanup_state='removed' AND NEW.path='%s'
		BEGIN SELECT RAISE(ABORT,'injected alias replay setup failure'); END`, epRelease+".mkv")); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, entry.Fingerprint, selections); err == nil {
		t.Fatal("expected first delete attempt to fail after journaling")
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_alias_replay`); err != nil {
		t.Fatal(err)
	}
	var batchID string
	if err := db.R.QueryRowContext(ctx, `SELECT batch_id FROM runner_delivery_explicit_delete_batches WHERE receipt_id=?`, receipt.ID).Scan(&batchID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	other := publication + ".replacement"
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, epRelease+".mkv"), []byte("replacement alias target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "episode-02.mkv"), []byte("replacement alias later target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_explicit_delete_batches SET retry_next_attempt_at=0 WHERE batch_id=?`, batchID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.resumeExplicitDeliveryDeleteBatches(ctx, receipt); err == nil {
		t.Fatal("worker accepted a retargeted reviewed alias")
	}
	batch, err := db.ExplicitDeliveryDeleteBatch(ctx, batchID)
	if err != nil || batch.RetryAttempts < 2 || batch.RetryError == "" || batch.RetryNextAttemptAt.IsZero() {
		t.Fatalf("alias retarget failure was not recorded against the original batch: %+v err=%v", batch, err)
	}
	if got, err := os.ReadFile(filepath.Join(publication, "episode-02.mkv")); err != nil || string(got) != "later selected source remains" {
		t.Fatalf("retargeted alias failure changed the later reviewed source: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(other, epRelease+".mkv")); err != nil || string(got) != "replacement alias target" {
		t.Fatalf("retargeted alias failure changed its new target: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(other, "episode-02.mkv")); err != nil || string(got) != "replacement alias later target" {
		t.Fatalf("retargeted alias failure changed its later target: %q err=%v", got, err)
	}
}

func TestSelectedDeliveryDeleteReplayRejectsReplacedPublicationDirectory(t *testing.T) {
	ctx := context.Background()
	svc, db, _, publication := reviewedDeliveryDeleteFixture(t, map[string][]byte{"keep.txt": []byte("original sibling")})
	entry := completedEntryForPath(t, svc, publication)
	fingerprint := entry.Fingerprint
	path := filepath.Join(publication, epRelease+".mkv")
	selection := completedSelectionForPath(t, entry, path)
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_publication_replay_stamp
		BEFORE UPDATE OF cleanup_state ON runner_delivery_file_outcomes
		WHEN NEW.cleanup_state='removed'
		BEGIN SELECT RAISE(ABORT,'injected post-unlink failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, []CompletedFileSelection{selection}); err == nil {
		t.Fatal("expected post-unlink failure before directory replacement")
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_publication_replay_stamp`); err != nil {
		t.Fatal(err)
	}
	oldDirectory := publication + ".held"
	if err := os.Rename(publication, oldDirectory); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(publication, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new publication at reused path"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, fingerprint, []CompletedFileSelection{selection}); err == nil {
		t.Fatal("replay accepted a replacement publication directory at the same path")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new publication at reused path" {
		t.Fatalf("replay touched the replacement publication: %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(oldDirectory, "keep.txt")); err != nil || string(got) != "original sibling" {
		t.Fatalf("replay touched the old publication sibling: %q err=%v", got, err)
	}
}

func selectionPathRelative(t *testing.T, payload, selected string) string {
	t.Helper()
	rel, err := filepath.Rel(canonicalCompletedPath(payload), canonicalCompletedPath(selected))
	if err != nil {
		t.Fatal(err)
	}
	return filepath.ToSlash(rel)
}

func TestSelectedDeliveryDeleteIntentTransactionFailureLeavesEverySelectedFile(t *testing.T) {
	ctx := context.Background()
	svc, db, receipt, publication := reviewedDeliveryDeleteFixture(t, map[string][]byte{"episode-02.mkv": []byte("second")})
	entry := completedEntryForPath(t, svc, publication)
	selections := []CompletedFileSelection{
		completedSelectionForPath(t, entry, filepath.Join(publication, epRelease+".mkv")),
		completedSelectionForPath(t, entry, filepath.Join(publication, "episode-02.mkv")),
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_second_explicit_intent
		BEFORE INSERT ON runner_delivery_explicit_delete_files
		WHEN NEW.path='episode-02.mkv'
		BEGIN SELECT RAISE(ABORT,'injected intent transaction failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, entry.Fingerprint, selections); err == nil || !containsText(err.Error(), "injected intent transaction failure") {
		t.Fatalf("did not reach atomic intent failure: %v", err)
	}
	for _, selection := range selections {
		if _, err := os.Lstat(selection.Path); err != nil {
			t.Fatalf("intent transaction failure unlinked %q: %v", selection.Path, err)
		}
	}
	var batchCount int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM runner_delivery_explicit_delete_batches WHERE receipt_id=?`, receipt.ID).Scan(&batchCount); err != nil {
		t.Fatal(err)
	}
	if batchCount != 0 {
		t.Fatalf("failed transaction retained partial batch header: %d", batchCount)
	}
}

func TestExplicitDeliveryDeleteRetryCadenceAndDueBatchFairness(t *testing.T) {
	ctx := context.Background()
	svc, db, receipt, publication := reviewedDeliveryDeleteFixture(t, nil)
	entry := completedEntryForPath(t, svc, publication)
	selection := completedSelectionForPath(t, entry, filepath.Join(publication, epRelease+".mkv"))
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_retry_cadence
		BEFORE UPDATE OF cleanup_state ON runner_delivery_file_outcomes
		WHEN NEW.cleanup_state='removed'
		BEGIN SELECT RAISE(ABORT,'temporary delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteSelectedDeliveryFiles(ctx, publication, entry.Fingerprint, []CompletedFileSelection{selection}); err == nil {
		t.Fatal("expected first delete attempt to fail after journaling")
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER fail_retry_cadence`); err != nil {
		t.Fatal(err)
	}
	var batchID string
	if err := db.R.QueryRowContext(ctx, `SELECT batch_id FROM runner_delivery_explicit_delete_batches WHERE receipt_id=?`, receipt.ID).Scan(&batchID); err != nil {
		t.Fatal(err)
	}
	checkDelay := func(expected time.Duration) {
		t.Helper()
		batch, err := db.ExplicitDeliveryDeleteBatch(ctx, batchID)
		if err != nil {
			t.Fatal(err)
		}
		remaining := time.Until(batch.RetryNextAttemptAt)
		if remaining < expected-2*time.Second || remaining > expected+2*time.Second {
			t.Fatalf("retry delay=%s, want %s (batch=%+v)", remaining, expected, batch)
		}
	}
	checkDelay(30 * time.Second)
	for _, delay := range []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		if err := db.ScheduleExplicitDeliveryDeleteRetry(ctx, batchID, "still blocked"); err != nil {
			t.Fatal(err)
		}
		checkDelay(delay)
	}
	// A large future backlog must not occupy the bounded due query ahead of a
	// later eligible batch. The SQL filters due status before applying LIMIT.
	now := time.Now().UnixMilli()
	for i := 0; i < 130; i++ {
		id := fmt.Sprintf("future-explicit-%03d", i)
		if _, err := db.W.ExecContext(ctx, `INSERT INTO runner_delivery_explicit_delete_batches
			(batch_id,receipt_id,payload_path,inventory_fingerprint,publication_directory_identity,storage_identity,generation_key,request_token,publication_generation,retry_next_attempt_at,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, receipt.ID, canonicalCompletedPath(publication), "future", receipt.PublicationDirectoryIdentity, "storage-id", receipt.GenerationKey, receipt.Delivery.RequestToken, receipt.Delivery.PublicationGeneration, now+int64(time.Hour/time.Millisecond), now+int64(i)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.W.ExecContext(ctx, `INSERT INTO runner_delivery_explicit_delete_files(batch_id,path,reviewed_path,snapshot_identity,parent_identity) VALUES(?,?,?,?,?)`, id, fmt.Sprintf("future-%03d.mkv", i), filepath.Join(publication, fmt.Sprintf("future-%03d.mkv", i)), "snapshot", "parent"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO runner_delivery_explicit_delete_batches
		(batch_id,receipt_id,payload_path,inventory_fingerprint,publication_directory_identity,storage_identity,generation_key,request_token,publication_generation,retry_next_attempt_at,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, "eligible-explicit", receipt.ID, canonicalCompletedPath(publication), "eligible", receipt.PublicationDirectoryIdentity, "storage-id", receipt.GenerationKey, receipt.Delivery.RequestToken, receipt.Delivery.PublicationGeneration, 0, now+int64(2*time.Hour/time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO runner_delivery_explicit_delete_files(batch_id,path,reviewed_path,snapshot_identity,parent_identity) VALUES(?,?,?,?,?)`, "eligible-explicit", "eligible.mkv", filepath.Join(publication, "eligible.mkv"), "snapshot", "parent"); err != nil {
		t.Fatal(err)
	}
	due, err := db.ExplicitDeliveryDeleteBatchesDue(ctx, receipt.ID, 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != "eligible-explicit" {
		t.Fatalf("future journal rows starved the eligible batch: due=%+v", due)
	}
	dueReceipts, err := db.DeliveryFileCleanupReceipts(ctx, 0, 32)
	if err != nil {
		t.Fatal(err)
	}
	foundReceipt := false
	for _, id := range dueReceipts {
		if id == receipt.ID {
			foundReceipt = true
			break
		}
	}
	if !foundReceipt {
		t.Fatalf("global cleanup sweep did not discover receipt with a due batch behind delayed batches: %v", dueReceipts)
	}
}

func reviewedDeliveryDeleteFixture(t *testing.T, extras map[string][]byte) (*Service, *sqlite.DB, sqlite.DeliveryReceipt, string) {
	t.Helper()
	ctx := context.Background()
	protocol := &deliveryImportClient{}
	svc, db, dl, cfg, publication := deliveryImportFixtureWithFiles(t, protocol, extras)
	receipts, err := db.DeliveryReceiptsForClient(ctx, dl.ClientID, 1)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("receipt lookup=%+v err=%v", receipts, err)
	}
	receipt := receipts[0]
	if _, err := svc.verifyDeliveryManifest(ctx, cfg, receipt); err != nil {
		t.Fatalf("verify manifest: %v", err)
	}
	receipt.ConsumerReceiptID = "selected-delete-batch"
	receipt.NextAttemptAt = time.Now()
	if err := db.PrepareDeliveryAcceptance(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	dl.ImportPath, dl.State = publication, "downloaded"
	if err := db.UpdateDownloadHandoff(ctx, dl); err != nil {
		t.Fatal(err)
	}
	if err := svc.ScanCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	return svc, db, receipt, publication
}

func completedEntryForPath(t *testing.T, svc *Service, path string) *CompletedEntry {
	t.Helper()
	inventory, err := svc.CompletedInventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range inventory.Roots {
		for j := range inventory.Roots[i].Entries {
			entry := &inventory.Roots[i].Entries[j]
			if canonicalCompletedPath(entry.Path) == canonicalCompletedPath(path) {
				return entry
			}
		}
	}
	t.Fatalf("completed entry %q missing from inventory: %+v", path, inventory)
	return nil
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

func containsText(haystack, needle string) bool { return strings.Contains(haystack, needle) }
