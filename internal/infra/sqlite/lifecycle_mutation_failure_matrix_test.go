package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

// These fault injections sit between durable rows in the lifecycle write
// paths. Each case proves that an interrupted transaction leaves no partial
// authority behind and that the same intent can be retried after the fault is
// removed.
func TestSaveDiscoveredDeliveryRollsBackRunnerPinWhenReceiptInsertFails(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	delivery := coverageDelivery("pin-before-receipt-rollback")
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_discovery_insert BEFORE INSERT ON runner_delivery_receipts WHEN NEW.delivery_id='pin-before-receipt-rollback' BEGIN SELECT RAISE(ABORT,'injected receipt insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveDiscoveredDelivery(ctx, 821, delivery, "/mounted/pinned-release"); err == nil {
		t.Fatal("receipt insert unexpectedly succeeded")
	}
	var pins, receipts int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_client_instances WHERE client_id=821`).Scan(&pins); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_delivery_receipts WHERE client_id=821 AND delivery_id=?`, delivery.DeliveryID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if pins != 0 || receipts != 0 {
		t.Fatalf("failed discovery left an installation pin or receipt: pins=%d receipts=%d", pins, receipts)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_discovery_insert`); err != nil {
		t.Fatal(err)
	}
	receipt, err := db.SaveDiscoveredDelivery(ctx, 821, delivery, "/mounted/pinned-release")
	if err != nil || receipt.ID == 0 || receipt.State != "unresolved" {
		t.Fatalf("same discovery did not retry cleanly: receipt=%+v err=%v", receipt, err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_client_instances WHERE client_id=821 AND runner_instance_id=?`, delivery.RunnerInstanceID).Scan(&pins); err != nil || pins != 1 {
		t.Fatalf("successful retry did not pin exact Runner instance: pins=%d err=%v", pins, err)
	}
}

func TestSaveDiscoveredDeliveryConflictMarkerFailureKeepsOriginalReceiptRetryable(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	original := coverageDelivery("conflict-marker-write-failure")
	receipt, err := db.SaveDiscoveredDelivery(ctx, 832, original, "/mounted/original")
	if err != nil {
		t.Fatal(err)
	}
	changed := original
	changed.PublicationPath = "/mounted/reused-id-with-new-path"
	conflictTrigger := fmt.Sprintf(`CREATE TRIGGER reject_delivery_conflict_marker BEFORE UPDATE OF state ON runner_delivery_receipts WHEN NEW.id=%d AND NEW.state='conflict' BEGIN SELECT RAISE(ABORT,'injected conflict marker failure'); END`, receipt.ID)
	if _, err := db.W.ExecContext(ctx, conflictTrigger); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveDiscoveredDelivery(ctx, 832, changed, "/mounted/new-mapping"); err == nil {
		t.Fatal("changed immutable delivery facts ignored a failed conflict journal write")
	}
	stored, err := db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil || stored.State != "unresolved" || stored.Delivery.PublicationPath != original.PublicationPath {
		t.Fatalf("failed conflict marker rewrote the original receipt: %+v err=%v", stored, err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_delivery_conflict_marker`); err != nil {
		t.Fatal(err)
	}
	stored, err = db.SaveDiscoveredDelivery(ctx, 832, changed, "/mounted/new-mapping")
	if err != nil || stored.State != "conflict" || stored.Delivery.PublicationPath != original.PublicationPath {
		t.Fatalf("changed delivery facts did not persist a conflict on retry: %+v err=%v", stored, err)
	}
}

func TestEnsureDeliveryFileOutcomesRollsBackSQLFailureAfterEarlierFileInsert(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	receipt, err := db.SaveDiscoveredDelivery(ctx, 822, coverageDelivery("outcome-insert-rollback"), "/mounted/outcomes")
	if err != nil {
		t.Fatal(err)
	}
	files := []DeliveryManifestFile{
		{Path: "episode-01.mkv", SizeBytes: 10, SHA256: "digest-01", FileGeneration: "generation-01"},
		{Path: "episode-02.mkv", SizeBytes: 20, SHA256: "digest-02", FileGeneration: "generation-02"},
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_second_outcome BEFORE INSERT ON runner_delivery_file_outcomes WHEN NEW.path='episode-02.mkv' BEGIN SELECT RAISE(ABORT,'injected second file failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureDeliveryFileOutcomes(ctx, receipt.ID, files, map[string]bool{"episode-01.mkv": true}, true); err == nil {
		t.Fatal("file ledger accepted a failed second insert")
	}
	rows, err := db.DeliveryFileOutcomes(ctx, receipt.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed ledger transaction kept a prefix of the manifest: rows=%+v err=%v", rows, err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_second_outcome`); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureDeliveryFileOutcomes(ctx, receipt.ID, files, map[string]bool{"episode-01.mkv": true}, true); err != nil {
		t.Fatalf("exact file manifest did not retry after rollback: %v", err)
	}
	rows, err = db.DeliveryFileOutcomes(ctx, receipt.ID)
	if err != nil || len(rows) != 2 || rows[0].Disposition != "pending" || rows[0].ManualAdditional != true || rows[1].Disposition != "unselected_recoverable" || !rows[1].ManualAdditional {
		t.Fatalf("retry did not persist exact selected scope: rows=%+v err=%v", rows, err)
	}
}

func TestQueueDeliveryReimportRollsBackDownloadUpdateOnReceiptWriteFailure(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Atomic reimport", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	delivery := coverageDelivery("reimport-write-failure")
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: itemID, ClientID: 823, ReleaseTitle: "atomic reimport", Protocol: "nzb", State: "imported", Handle: delivery.JobID, Transfer: delivery.RequestToken, Intent: "manual_additional", CopyID: 4})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := db.SaveDiscoveredDelivery(ctx, 823, delivery, "/mounted/reimport")
	if err != nil {
		t.Fatal(err)
	}
	receipt.DownloadID, receipt.GenerationKey, receipt.DownloadHandle = dl.ID, dl.GenerationKey, dl.Handle
	receipt.MediaItemID, receipt.CopyID = itemID, 4
	if err := db.AssociateDelivery(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	receipt.ConsumerReceiptID = "consumer-receipt-reimport"
	if err := db.PrepareDeliveryAcceptance(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	if err := db.SetDeliveryImported(ctx, receipt.ID); err != nil {
		t.Fatal(err)
	}
	expected, err := db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	dl, err = db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	dl.State = "importing"
	dl.Handoff = []HandoffEntry{{Step: "manual_import_queued", At: time.Now().UnixMilli(), Paths: []string{"Movie.mkv"}}}
	trigger := fmt.Sprintf(`CREATE TRIGGER reject_receipt_reopen BEFORE UPDATE OF state ON runner_delivery_receipts WHEN NEW.id=%d AND NEW.state='ready' BEGIN SELECT RAISE(ABORT,'injected receipt reopen failure'); END`, receipt.ID)
	if _, err := db.W.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	if err := db.QueueDeliveryReimport(ctx, expected, dl); err == nil {
		t.Fatal("reimport partially succeeded when receipt reopen failed")
	}
	currentDownload, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentReceipt, err := db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentDownload.State != "imported" || len(currentDownload.Handoff) != 0 || currentReceipt.State != "imported" || currentReceipt.AckState != "pending" {
		t.Fatalf("failed reimport split the download and receipt state: download=%+v receipt=%+v", currentDownload, currentReceipt)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_receipt_reopen`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueueDeliveryReimport(ctx, expected, dl); err != nil {
		t.Fatalf("exact selected Resolve request did not retry: %v", err)
	}
	currentDownload, err = db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentReceipt, err = db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil || currentDownload.State != "importing" || len(currentDownload.Handoff) != 1 || currentReceipt.State != "ready" {
		t.Fatalf("reimport retry did not commit as one state: download=%+v receipt=%+v err=%v", currentDownload, currentReceipt, err)
	}
}

func TestCommitPlacementRollsBackFileAndEpisodeRowsWhenPlacementJournalInsertFails(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Atomic placement", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO episodes(media_item_id,season_number,episode_number,title,monitored) VALUES(?,1,1,'Pilot',1)`, itemID); err != nil {
		t.Fatal(err)
	}
	episodeID, err := db.GetEpisodeID(ctx, itemID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	p := Placement{ID: "placement-index-rollback", ItemID: itemID, Target: "/library/Atomic.Placement.S01E01.mkv", Size: 17, SHA256: "verified-output-digest", EpisodeIDs: []int64{episodeID}, State: "prepared"}
	if err := db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_placement_index BEFORE INSERT ON media_file_placements WHEN NEW.placement_id='placement-index-rollback' BEGIN SELECT RAISE(ABORT,'injected placement index failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitPlacement(ctx, p); err == nil {
		t.Fatal("placement metadata committed without its outbox journal")
	}
	var files, links, placementIndexes int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE path=?`, p.Target).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_episodes WHERE media_file_id IN (SELECT id FROM media_files WHERE path=?)`, p.Target).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_placements WHERE placement_id=?`, p.ID).Scan(&placementIndexes); err != nil {
		t.Fatal(err)
	}
	stored, err := db.GetPlacement(ctx, p.ID)
	if err != nil || files != 0 || links != 0 || placementIndexes != 0 || stored.State != "prepared" {
		t.Fatalf("failed placement index write left partial authority: files=%d links=%d indexes=%d placement=%+v err=%v", files, links, placementIndexes, stored, err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_placement_index`); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.CommitPlacement(ctx, p)
	if err != nil {
		t.Fatalf("same prepared placement did not retry after rollback: %v", err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_placements p JOIN media_files f ON f.id=p.media_file_id JOIN media_file_episodes mfe ON mfe.media_file_id=f.id WHERE p.placement_id=? AND f.id=? AND f.file_generation=? AND mfe.episode_id=?`, p.ID, fileID, p.ID, episodeID).Scan(&placementIndexes); err != nil || placementIndexes != 1 {
		t.Fatalf("retry did not commit matching file, placement and episode rows: rows=%d err=%v", placementIndexes, err)
	}
}

func TestCreateReplacementOperationRollsBackWhenFileSnapshotInsertFails(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Atomic replacement snapshot", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	path := "/library/atomic-replacement-snapshot.mkv"
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files(id,media_item_id,path,size,added_at,file_generation,source_generation) VALUES(?,?,?,?,?,?,?)`, 829, itemID, path, 44, time.Now().UnixMilli(), "snapshot-file-generation", "snapshot-source-generation"); err != nil {
		t.Fatal(err)
	}
	op := ReplacementOperation{ID: "replacement-snapshot-write-failure", RequestID: "replacement-snapshot-write-failure-request", Fingerprint: "replacement-snapshot-write-failure-fingerprint", State: "accepted", Intent: "replace_bad", Scope: "download", MediaItemID: itemID, SourceGeneration: "snapshot-source-generation", ReasonCode: "bad_media", Reason: "verified corrupt source"}
	file := ReplacementFile{MediaFileID: 829, FileGeneration: "snapshot-file-generation", SourceGeneration: "snapshot-source-generation", Path: path, Disposition: "selected", RemovalState: "protected"}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_replacement_snapshot BEFORE INSERT ON replacement_operation_files WHEN NEW.operation_id='replacement-snapshot-write-failure' BEGIN SELECT RAISE(ABORT,'injected snapshot insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateReplacementOperation(ctx, op, []ReplacementFile{file}); err == nil {
		t.Fatal("operation admission succeeded without its approved file snapshot")
	}
	var operations, snapshots, steps int
	for query, target := range map[string]*int{
		`SELECT count(*) FROM replacement_operations WHERE operation_id=?`:      &operations,
		`SELECT count(*) FROM replacement_operation_files WHERE operation_id=?`: &snapshots,
		`SELECT count(*) FROM replacement_operation_steps WHERE operation_id=?`: &steps,
	} {
		if err := db.R.QueryRowContext(ctx, query, op.ID).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	if operations != 0 || snapshots != 0 || steps != 0 {
		t.Fatalf("snapshot failure left an unbounded replacement operation: operations=%d snapshots=%d steps=%d", operations, snapshots, steps)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_replacement_snapshot`); err != nil {
		t.Fatal(err)
	}
	created, replay, err := db.CreateReplacementOperation(ctx, op, []ReplacementFile{file})
	if err != nil || replay || created.ID != op.ID {
		t.Fatalf("exact replacement intent failed after rollback: op=%+v replay=%v err=%v", created, replay, err)
	}
	accepted, err := db.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(accepted) != 1 || accepted[0].FileGeneration != file.FileGeneration || accepted[0].Path != file.Path {
		t.Fatalf("retry did not reserve exact reviewed file snapshot: files=%+v err=%v", accepted, err)
	}
}

func TestRunnerAdmissionPollClaimRollsBackScheduleRowWhenDeadlineWriteFails(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Atomic poll schedule", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: itemID, ReleaseTitle: "uncertain admission", Protocol: "nzb", ClientID: 830, State: "grabbed", Transfer: "poll-schedule-transfer", RequestTokenIdempotent: true, SubmissionPhase: "uncertain"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_900_000_000, 0)
	trigger := fmt.Sprintf(`CREATE TRIGGER reject_poll_deadline BEFORE UPDATE OF next_attempt_at ON runner_admission_poll_schedule WHEN NEW.download_id=%d BEGIN SELECT RAISE(ABORT,'injected poll deadline failure'); END`, dl.ID)
	if _, err := db.W.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimRunnerAdmissionPoll(ctx, dl.ID, dl.GenerationKey, now); err == nil || claimed {
		t.Fatalf("poll claim ignored failed durable deadline write: claimed=%v err=%v", claimed, err)
	}
	var schedules int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_admission_poll_schedule WHERE download_id=?`, dl.ID).Scan(&schedules); err != nil || schedules != 0 {
		t.Fatalf("failed poll claim left a schedule without a deadline: rows=%d err=%v", schedules, err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_poll_deadline`); err != nil {
		t.Fatal(err)
	}
	if claimed, err := db.ClaimRunnerAdmissionPoll(ctx, dl.ID, dl.GenerationKey, now); err != nil || !claimed {
		t.Fatalf("poll schedule did not retry after rollback: claimed=%v err=%v", claimed, err)
	}
	var attempts int
	var next int64
	if err := db.R.QueryRowContext(ctx, `SELECT attempts,next_attempt_at FROM runner_admission_poll_schedule WHERE download_id=? AND generation_key=?`, dl.ID, dl.GenerationKey).Scan(&attempts, &next); err != nil || attempts != 1 || next != now.Add(30*time.Second).UnixMilli() {
		t.Fatalf("successful retry has incorrect atomic schedule: attempts=%d next=%d err=%v", attempts, next, err)
	}
}

func TestBindRunnerAdmissionRollsBackInstallationPinWhenDownloadBindingFails(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Atomic Runner bind", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: itemID, ReleaseTitle: "exact admission", Protocol: "nzb", ClientID: 831, State: "grabbed", Transfer: "bind-exact-transfer", RequestTokenIdempotent: true})
	if err != nil {
		t.Fatal(err)
	}
	trigger := fmt.Sprintf(`CREATE TRIGGER reject_download_binding BEFORE UPDATE OF runner_instance_id ON downloads WHEN NEW.id=%d BEGIN SELECT RAISE(ABORT,'injected download binding failure'); END`, dl.ID)
	if _, err := db.W.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	if err := db.BindRunnerAdmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "runner-atomic", "admission-generation-atomic"); err == nil {
		t.Fatal("Runner binding succeeded despite failed download write")
	}
	var pins int
	var runner, generation string
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM runner_client_instances WHERE client_id=?`, dl.ClientID).Scan(&pins); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT runner_instance_id,runner_admission_generation FROM downloads WHERE id=?`, dl.ID).Scan(&runner, &generation); err != nil {
		t.Fatal(err)
	}
	if pins != 0 || runner != "" || generation != "" {
		t.Fatalf("failed exact binding left partial installation authority: pins=%d runner=%q generation=%q", pins, runner, generation)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_download_binding`); err != nil {
		t.Fatal(err)
	}
	if err := db.BindRunnerAdmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "runner-atomic", "admission-generation-atomic"); err != nil {
		t.Fatalf("same exact binding did not retry after rollback: %v", err)
	}
	bound, err := db.GetDownload(ctx, dl.ID)
	if err != nil || bound.RunnerInstanceID != "runner-atomic" || bound.RunnerAdmissionGeneration != "admission-generation-atomic" {
		t.Fatalf("retry did not commit the complete pinned identity: row=%+v err=%v", bound, err)
	}
}

func TestManualSubmissionAcknowledgementFailureCannotPublishSubmittedWithoutHandle(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Atomic submission acknowledgement", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: itemID, ReleaseTitle: "manual exact ack", Protocol: "nzb", ClientID: 833, State: "grabbed", Transfer: "manual-exact-ack-transfer", Intent: "manual_additional", SubmissionPhase: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	trigger := fmt.Sprintf(`CREATE TRIGGER reject_manual_submission_ack BEFORE UPDATE OF submission_phase ON downloads WHEN NEW.id=%d AND NEW.submission_phase='submitted' BEGIN SELECT RAISE(ABORT,'injected acknowledgement failure'); END`, dl.ID)
	if _, err := db.W.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitManualSubmission(ctx, dl.ID, dl.GenerationKey, "accepted-client-handle", dl.Transfer); err == nil {
		t.Fatal("failed acknowledgement write left a submitted download")
	}
	current, err := db.GetDownload(ctx, dl.ID)
	if err != nil || current.SubmissionPhase != "pending" || current.Handle != "" || current.Transfer != dl.Transfer {
		t.Fatalf("failed acknowledgement changed exact pending request: row=%+v err=%v", current, err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_manual_submission_ack`); err != nil {
		t.Fatal(err)
	}
	if err := db.CommitManualSubmission(ctx, dl.ID, dl.GenerationKey, "accepted-client-handle", dl.Transfer); err != nil {
		t.Fatalf("exact acknowledgement did not retry: %v", err)
	}
	current, err = db.GetDownload(ctx, dl.ID)
	if err != nil || current.SubmissionPhase != "submitted" || current.Handle != "accepted-client-handle" || current.Transfer != dl.Transfer {
		t.Fatalf("acknowledgement retry did not commit exact accepted identity: row=%+v err=%v", current, err)
	}
}
