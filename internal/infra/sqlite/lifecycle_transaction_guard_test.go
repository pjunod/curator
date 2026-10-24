package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
)

func TestCommitPlacementRollsBackLateJournalFailureAndRetriesExactIntent(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Late placement rollback", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	p := Placement{ID: "late-placement-journal", ItemID: itemID, Target: "/library/Late placement rollback.mkv", Size: 42, SHA256: "accepted-digest", State: "prepared"}
	if err := db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_late_placement_commit BEFORE UPDATE OF state ON import_placements WHEN NEW.id='late-placement-journal' AND NEW.state='committed' BEGIN SELECT RAISE(ABORT,'injected final journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitPlacement(ctx, p); err == nil {
		t.Fatal("placement committed after the final journal write was rejected")
	}
	var files, placements int
	var state string
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE path=?`, p.Target).Scan(&files); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_placements WHERE placement_id=?`, p.ID).Scan(&placements); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT state FROM import_placements WHERE id=?`, p.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if files != 0 || placements != 0 || state != "prepared" {
		t.Fatalf("late failure left partial library state: files=%d placements=%d placement_state=%q", files, placements, state)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_late_placement_commit`); err != nil {
		t.Fatal(err)
	}
	fid, err := db.CommitPlacement(ctx, p)
	if err != nil {
		t.Fatalf("exact prepared intent did not retry after rollback: %v", err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files f JOIN media_file_placements p ON p.media_file_id=f.id WHERE p.placement_id=? AND f.id=? AND f.file_generation=? AND f.path=?`, p.ID, fid, p.ID, p.Target).Scan(&files); err != nil || files != 1 {
		t.Fatalf("retried placement did not publish its exact generation: rows=%d err=%v", files, err)
	}
}

func TestDeleteRecoverySupersededKeepsFileAcrossStaleAndFailedJournalTransitions(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Recovery superseded rollback", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.W.ExecContext(ctx, `INSERT INTO media_files(media_item_id,path,size,added_at,file_generation,source_generation) VALUES(?,?,?,?,?,?)`, itemID, "/library/old.mkv", 15, time.Now().UnixMilli(), "old-generation", "source-generation")
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO recovery_imports(id,state,data,updated_at) VALUES('superseded-rollback','importing',json_object('accepted_revision',(SELECT revision FROM lifecycle_library_revision WHERE id=1)),0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteRecoverySuperseded(ctx, fileID, "superseded-rollback"); err == nil {
		t.Fatal("superseded file was deleted using a stale recovery revision")
	}
	var remaining int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE id=? AND file_generation='old-generation'`, fileID).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("stale review changed the source file row: rows=%d err=%v", remaining, err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE recovery_imports SET data=json_set(data,'$.accepted_revision',(SELECT revision FROM lifecycle_library_revision WHERE id=1)) WHERE id='superseded-rollback'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER reject_recovery_advance BEFORE UPDATE OF data ON recovery_imports WHEN OLD.id='superseded-rollback' BEGIN SELECT RAISE(ABORT,'injected recovery revision failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteRecoverySuperseded(ctx, fileID, "superseded-rollback"); err == nil {
		t.Fatal("superseded file deletion survived failure to advance its recovery journal")
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE id=? AND file_generation='old-generation'`, fileID).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("failed recovery journal update committed file deletion: rows=%d err=%v", remaining, err)
	}
	if _, err := db.W.ExecContext(ctx, `DROP TRIGGER reject_recovery_advance`); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteRecoverySuperseded(ctx, fileID, "superseded-rollback"); err != nil {
		t.Fatalf("exact refreshed recovery intent did not retry: %v", err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE id=?`, fileID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("successful recovery retirement left source row: rows=%d err=%v", remaining, err)
	}
}

func TestCreateReplacementOperationRollsBackStaleReviewedFileSnapshot(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		generation string
		source     string
	}{
		{name: "file generation changed", generation: "replaced-file-generation", source: "source-generation"},
		{name: "source generation changed", generation: "reviewed-file-generation", source: "new-source-generation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _ := openLifecycleCoverageDB(t)
			defer func() { _ = db.Close() }()
			itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Stale replacement snapshot", Path: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			path := "/library/reviewed-source.mkv"
			if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files(id,media_item_id,path,size,added_at,file_generation,source_generation) VALUES(?,?,?,?,?,?,?)`, 910, itemID, path, 200, time.Now().UnixMilli(), "reviewed-file-generation", "source-generation"); err != nil {
				t.Fatal(err)
			}
			op := ReplacementOperation{ID: "stale-snapshot", RequestID: "stale-snapshot-request", Fingerprint: "stale-snapshot-fingerprint", State: "accepted", Intent: "replace_bad", Scope: "download", MediaItemID: itemID, SourceGeneration: "source-generation", ReasonCode: "bad_media", Reason: "reviewed file changed"}
			file := ReplacementFile{MediaFileID: 910, FileGeneration: tc.generation, SourceGeneration: tc.source, Path: path, Disposition: "selected", RemovalState: "protected"}
			if _, _, err := db.CreateReplacementOperation(ctx, op, []ReplacementFile{file}); err == nil {
				t.Fatal("replacement reserved a source file with changed review identity")
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
				t.Fatalf("stale review left a partial operation reservation: operations=%d snapshots=%d steps=%d", operations, snapshots, steps)
			}
		})
	}
}

func TestFinishReplacementObservationCannotAdoptReusedFileGeneration(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Replaced observation", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	path := "/library/observed.mkv"
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files(id,media_item_id,path,size,added_at,file_generation,source_generation) VALUES(?,?,?,?,?,?,?)`, 920, itemID, path, 200, time.Now().UnixMilli(), "reviewed-generation", "source-generation"); err != nil {
		t.Fatal(err)
	}
	observation, replay, err := db.StartReplacementFileObservation(ctx, ReplacementFileObservation{ID: "observation-reuse", RequestID: "observation-reuse-request", MediaItemID: itemID, MediaFileID: 920, FileGeneration: "reviewed-generation", StorageIdentity: "disk:library"})
	if err != nil || replay || observation.State != "pending" {
		t.Fatalf("start observation=%+v replay=%v err=%v", observation, replay, err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE media_files SET file_generation='replacement-generation',source_generation='new-source-generation' WHERE id=920`); err != nil {
		t.Fatal(err)
	}
	observation.State, observation.SHA256, observation.FileIdentity, observation.FilesystemGeneration, observation.Size = "ready", "reviewed-sha", "old-object", "old-filesystem-generation", 200
	if err := db.FinishReplacementFileObservation(ctx, observation); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale observation finish error=%v, want ErrNotFound", err)
	}
	stored, err := db.GetReplacementFileObservation(ctx, observation.ID)
	if err != nil || stored.State != "pending" || stored.SHA256 != "" || stored.FileIdentity != "" {
		t.Fatalf("replacement file inherited stale review evidence: %+v err=%v", stored, err)
	}
}

func TestBadReplacementPlacementWaitsForApprovedSourceRemoval(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindSeries, Title: "Scoped replacement", Path: t.TempDir()})
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
	sourcePath := "/library/Scoped replacement old.mkv"
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_files(id,media_item_id,path,size,added_at,file_generation,source_generation) VALUES(?,?,?,?,?,?,?)`, 930, itemID, sourcePath, 100, time.Now().UnixMilli(), "bad-source-generation", "payload-generation"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `INSERT INTO media_file_episodes(media_file_id,episode_id) VALUES(930,?)`, episodeID); err != nil {
		t.Fatal(err)
	}
	op := ReplacementOperation{ID: "bad-replacement-commit", RequestID: "bad-replacement-request", Fingerprint: "bad-replacement-fingerprint", State: "replacing", Intent: "replace_bad", Scope: "episodes", MediaItemID: itemID, SourceGeneration: "payload-generation", EpisodeIDs: []int64{episodeID}, ReasonCode: "bad_media", Reason: "selected bad episode"}
	file := ReplacementFile{MediaFileID: 930, FileGeneration: "bad-source-generation", SourceGeneration: "payload-generation", Path: sourcePath, EpisodeIDs: []int64{episodeID}, Disposition: "selected", RemovalState: "protected"}
	if _, _, err := db.CreateReplacementOperation(ctx, op, []ReplacementFile{file}); err != nil {
		t.Fatal(err)
	}
	p := Placement{ID: "replacement-output-generation", ReplacementOperationID: op.ID, ItemID: itemID, Target: "/library/Scoped replacement new.mkv", Size: 300, SHA256: "replacement-payload-digest", EpisodeIDs: []int64{episodeID}, State: "prepared"}
	if err := db.PreparePlacement(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitPlacement(ctx, p); err == nil {
		t.Fatal("bad replacement published while its exact selected source remained protected")
	}
	var targetRows, sourceRows int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE path=?`, p.Target).Scan(&targetRows); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_files WHERE id=930 AND file_generation=?`, file.FileGeneration).Scan(&sourceRows); err != nil {
		t.Fatal(err)
	}
	if targetRows != 0 || sourceRows != 1 {
		t.Fatalf("protected replacement mutated library: target=%d source=%d", targetRows, sourceRows)
	}
	if err := db.SetReplacementFileRemovalState(ctx, op.ID, file, "removed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `DELETE FROM media_files WHERE id=930 AND file_generation=?`, file.FileGeneration); err != nil {
		t.Fatal(err)
	}
	fid, err := db.CommitPlacement(ctx, p)
	if err != nil {
		t.Fatalf("exact selected replacement failed after its source was removed: %v", err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM media_file_placements p JOIN media_file_episodes mfe ON mfe.media_file_id=p.media_file_id JOIN media_files f ON f.id=p.media_file_id WHERE p.placement_id=? AND f.id=? AND f.path=? AND mfe.episode_id=?`, p.ID, fid, p.Target, episodeID).Scan(&targetRows); err != nil || targetRows != 1 {
		t.Fatalf("replacement did not commit the approved episode output: rows=%d err=%v", targetRows, err)
	}
}

func TestRunnerSubmissionCASKeepsExactAdmissionState(t *testing.T) {
	ctx := context.Background()
	db, _ := openLifecycleCoverageDB(t)
	defer func() { _ = db.Close() }()
	itemID, err := db.CreateMediaItem(ctx, domain.MediaItem{Kind: domain.KindMovie, Title: "Submission CAS", Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	dl, err := db.InsertDownloadRecord(ctx, Download{MediaItemID: itemID, ReleaseTitle: "submission", Protocol: "nzb", ClientID: 22, State: "grabbed", Transfer: "opaque-transfer-token", Intent: "automatic", RequestTokenIdempotent: true, SubmissionPhase: "uncertain"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateRunnerSubmissionWarning(ctx, dl.ID, dl.GenerationKey, "Runner lookup deferred"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDownloadSubmission(ctx, dl.ID, "stale-generation", dl.Transfer, "job-31"); !errors.Is(err, ErrRunnerAdmissionIdentityConflict) {
		t.Fatalf("stale generation submission error=%v", err)
	}
	var state, phase, handle, detail string
	load := func() {
		t.Helper()
		if err := db.R.QueryRowContext(ctx, `SELECT state,submission_phase,handle,error FROM downloads WHERE id=?`, dl.ID).Scan(&state, &phase, &handle, &detail); err != nil {
			t.Fatal(err)
		}
	}
	load()
	if state != "grabbed" || phase != "uncertain" || handle != "" || detail != "Runner lookup deferred" {
		t.Fatalf("stale submission changed the pending row: state=%q phase=%q handle=%q detail=%q", state, phase, handle, detail)
	}
	if err := db.RecordDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "job-31"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "reused-job-31"); !errors.Is(err, ErrRunnerAdmissionIdentityConflict) {
		t.Fatalf("conflicting handle replay error=%v", err)
	}
	load()
	if state != "grabbed" || phase != "submitted" || handle != "job-31" {
		t.Fatalf("conflicting replay overwrote accepted handle: state=%q phase=%q handle=%q", state, phase, handle)
	}
	if err := db.RejectDownloadSubmission(ctx, dl.ID, dl.GenerationKey, dl.Transfer, "explicit client refusal"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateRunnerSubmissionWarning(ctx, dl.ID, dl.GenerationKey, "late warning"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected request accepted a late warning: %v", err)
	}
	load()
	if state != "failed" || phase != "rejected" || handle != "job-31" || detail != "explicit client refusal" {
		t.Fatalf("terminal refusal was overwritten by stale work: state=%q phase=%q handle=%q detail=%q", state, phase, handle, detail)
	}
}
