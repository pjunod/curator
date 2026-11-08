package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func TestEnsureDeliveryManifestRollsBackEarlierRowsOnImmutableConflict(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	receipt, err := db.SaveDiscoveredDelivery(ctx, 12, coverageDelivery("manifest-rollback"), "/mounted/release")
	if err != nil {
		t.Fatal(err)
	}
	original := DeliveryManifestFile{Path: "b.mkv", SizeBytes: 8, SHA256: "bbbb", FileGeneration: "file-gen-b"}
	if err := db.EnsureDeliveryFileOutcomes(ctx, receipt.ID, []DeliveryManifestFile{original}, nil, false); err != nil {
		t.Fatal(err)
	}
	changed := original
	changed.FileGeneration = "replacement-file-gen-b"
	files := []DeliveryManifestFile{
		{Path: "a.mkv", SizeBytes: 7, SHA256: "aaaa", FileGeneration: "file-gen-a"},
		changed,
	}
	if err := db.EnsureDeliveryFileOutcomes(ctx, receipt.ID, files, nil, false); err == nil {
		t.Fatal("changed immutable manifest was accepted")
	}
	rows, err := db.DeliveryFileOutcomes(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != original.Path || rows[0].FileGeneration != original.FileGeneration || rows[0].SHA256 != original.SHA256 {
		t.Fatalf("manifest conflict partially inserted/replaced ledger rows: %+v", rows)
	}
}

func TestQueueDeliveryReimportRollsBackDownloadWhenReceiptIdentityChanges(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Reimport rollback", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	delivery := coverageDelivery("reimport-late-conflict")
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: itemID, ClientID: 12, ReleaseTitle: "Reimport rollback", Protocol: "nzb", State: "imported", Handle: delivery.JobID, Transfer: delivery.RequestToken, Intent: "manual_additional", CopyID: 9})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := db.SaveDiscoveredDelivery(ctx, 12, delivery, "/mounted/release")
	if err != nil {
		t.Fatal(err)
	}
	receipt.DownloadID, receipt.GenerationKey, receipt.DownloadHandle = dl.ID, dl.GenerationKey, dl.Handle
	receipt.MediaItemID, receipt.CopyID = itemID, 9
	if err := db.AssociateDelivery(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	receipt.ConsumerReceiptID = "consumer-ack"
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
	// The download update is first in QueueDeliveryReimport's transaction. A
	// receipt-only identity change makes its second guarded update fail; neither
	// half of the re-open may then remain committed.
	if _, err := db.W.ExecContext(ctx, `UPDATE runner_delivery_receipts SET manifest_digest='changed-after-read' WHERE id=?`, receipt.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueueDeliveryReimport(ctx, expected, dl); !errors.Is(err, ErrDeliveryReceiptStale) {
		t.Fatalf("reimport with stale receipt generation error=%v", err)
	}
	currentDownload, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentReceipt, err := db.DeliveryReceiptByID(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentDownload.State != "imported" || len(currentDownload.Handoff) != 0 || currentReceipt.State != "imported" || currentReceipt.AckState != "pending" || currentReceipt.Delivery.ManifestDigest != "changed-after-read" {
		t.Fatalf("late identity conflict left a partial Resolve mutation: download=%+v receipt=%+v", currentDownload, currentReceipt)
	}
}

func TestManualSubmissionAcknowledgementRejectsStaleGenerationAndTransfer(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Manual admission identity", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: itemID, ClientID: 14, ReleaseTitle: "Manual admission identity", Protocol: "torrent", State: "grabbed", Intent: "manual_additional", SubmissionPhase: "pending", Transfer: "t-14-0123456789abcdef0123456789abcdef", RequestTokenIdempotent: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, generation, transfer string
	}{
		{name: "stale download generation", generation: "reused-download-generation", transfer: dl.Transfer},
		{name: "different request token", generation: dl.GenerationKey, transfer: "another-transfer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.CommitManualSubmission(ctx, dl.ID, tc.generation, "wrong-job", tc.transfer); !errors.Is(err, ErrManualSubmissionChanged) {
				t.Fatalf("mismatched acknowledgement error=%v", err)
			}
			current, err := db.GetDownload(ctx, dl.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.GenerationKey != dl.GenerationKey || current.SubmissionPhase != "pending" || current.Handle != "" || current.Transfer != dl.Transfer {
				t.Fatalf("mismatched acknowledgement changed manual admission custody: %+v", current)
			}
		})
	}
	if err := db.CommitManualSubmission(ctx, dl.ID, dl.GenerationKey, "exact-job", dl.Transfer); err != nil {
		t.Fatal(err)
	}
	committed, err := db.GetDownload(ctx, dl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committed.SubmissionPhase != "submitted" || committed.Handle != "exact-job" || committed.Transfer != dl.Transfer {
		t.Fatalf("valid exact acknowledgement did not persist as one submitted fact: %+v", committed)
	}
}

func TestReplacementAdmissionRollsBackOperationAndFileSnapshotOnStepFailure(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Replacement rollback", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(t.TempDir(), "reviewed.mkv")
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files(id,media_item_id,path,size,added_at,file_generation,source_generation) VALUES(?,?,?,?,?,?,?)`, 812, itemID, filePath, 123, time.Now().UnixMilli(), "reviewed-generation", "source-generation"); err != nil {
		t.Fatal(err)
	}
	op := ReplacementOperation{ID: "atomic-replacement", RequestID: "atomic-replacement-request", Fingerprint: "atomic-fingerprint", State: "accepted", Intent: "replace_bad", Scope: "download", MediaItemID: itemID, SourceGeneration: "source-generation", ReasonCode: "bad_media", Reason: "verified damaged source"}
	file := ReplacementFile{MediaFileID: 812, FileGeneration: "reviewed-generation", SourceGeneration: "source-generation", Path: filePath, Disposition: "selected", RemovalState: "protected"}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_replacement_step BEFORE INSERT ON replacement_operation_steps WHEN NEW.operation_id='atomic-replacement' BEGIN SELECT RAISE(ABORT,'injected step write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateReplacementOperation(ctx, op, []ReplacementFile{file}); err == nil {
		t.Fatal("replacement admission unexpectedly succeeded after its step journal failed")
	}
	var operations, snapshots, steps int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM replacement_operations WHERE operation_id=?`, op.ID).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM replacement_operation_files WHERE operation_id=?`, op.ID).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM replacement_operation_steps WHERE operation_id=?`, op.ID).Scan(&steps); err != nil {
		t.Fatal(err)
	}
	if operations != 0 || snapshots != 0 || steps != 0 {
		t.Fatalf("failed admission left a partial reservation: operations=%d snapshots=%d steps=%d", operations, snapshots, steps)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_replacement_step`); err != nil {
		t.Fatal(err)
	}
	if _, replay, err := db.CreateReplacementOperation(ctx, op, []ReplacementFile{file}); err != nil || replay {
		t.Fatalf("rolled-back request id remained poisoned after restartable retry: replay=%v err=%v", replay, err)
	}
	accepted, err := db.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(accepted) != 1 || accepted[0].FileGeneration != file.FileGeneration {
		t.Fatalf("successful retry did not reserve the exact reviewed generation: files=%+v err=%v", accepted, err)
	}
}

func TestReplacementEpisodeDetachRollsBackIfRemovalJournalCannotCommit(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Episode detach rollback", Path: t.TempDir()})
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
	filePath := filepath.Join(t.TempDir(), "episode.mkv")
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files(id,media_item_id,path,size,added_at,file_generation,source_generation) VALUES(?,?,?,?,?,?,?)`, 813, itemID, filePath, 321, time.Now().UnixMilli(), "episode-generation", "source-generation"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_file_episodes(media_file_id,episode_id) VALUES(?,?)`, 813, episodeID); err != nil {
		t.Fatal(err)
	}
	op := ReplacementOperation{ID: "detach-atomic", RequestID: "detach-atomic-request", Fingerprint: "detach-atomic-fingerprint", State: "removing", Intent: "replace_bad", Scope: "episodes", MediaItemID: itemID, SourceGeneration: "source-generation", EpisodeIDs: []int64{episodeID}, ReasonCode: "bad_media", Reason: "selected bad episode"}
	file := ReplacementFile{MediaFileID: 813, FileGeneration: "episode-generation", SourceGeneration: "source-generation", Path: filePath, EpisodeIDs: []int64{episodeID}, Disposition: "selected", RemovalState: "protected"}
	if _, _, err := db.CreateReplacementOperation(ctx, op, []ReplacementFile{file}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_detach_journal BEFORE UPDATE OF removal_state ON replacement_operation_files WHEN NEW.operation_id='detach-atomic' BEGIN SELECT RAISE(ABORT,'injected removal journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.DetachReplacementEpisodes(ctx, op.ID, file, []int64{episodeID}); err == nil {
		t.Fatal("episode link was detached although its durable removal transition failed")
	}
	var episodeLinks int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_episodes WHERE media_file_id=? AND episode_id=?`, file.MediaFileID, episodeID).Scan(&episodeLinks); err != nil {
		t.Fatal(err)
	}
	snapshot, err := db.ListReplacementFiles(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if episodeLinks != 1 || len(snapshot) != 1 || snapshot[0].RemovalState != "protected" {
		t.Fatalf("failed detach partially removed selected episode evidence: links=%d snapshot=%+v", episodeLinks, snapshot)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_detach_journal`); err != nil {
		t.Fatal(err)
	}
	stale := file
	stale.FileGeneration = "reused-id-generation"
	if err := db.DetachReplacementEpisodes(ctx, op.ID, stale, []int64{episodeID}); err == nil {
		t.Fatal("stale generation detached the replacement file's episode mapping")
	}
	if err := db.DetachReplacementEpisodes(ctx, op.ID, file, []int64{episodeID}); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_episodes WHERE media_file_id=? AND episode_id=?`, file.MediaFileID, episodeID).Scan(&episodeLinks); err != nil || episodeLinks != 0 {
		t.Fatalf("valid exact-generation detach was not committed: links=%d err=%v", episodeLinks, err)
	}
}
