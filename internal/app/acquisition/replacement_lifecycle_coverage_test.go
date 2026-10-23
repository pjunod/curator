package acquisition

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func reviewedBadReplacementFixture(t *testing.T) (*Service, *sqlite.DB, context.Context, sqlite.ReplacementOperation, sqlite.ReplacementFile, string) {
	t.Helper()
	ctx := context.Background()
	svc, db, itemID := autoSetup(t, nil, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, path := acq2AddFile(t, svc, itemID, 0, item.Path, "reviewed-bad.mkv", "Test.Movie.2024.720p.WEB-DL.OLD")
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='review-source-generation',source_transfer='review-transfer' WHERE id=?`, fileID); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var generation string
	for _, file := range files {
		if file.ID == fileID {
			generation = file.FileGeneration
		}
	}
	if generation == "" {
		t.Fatal("fixture library file has no persisted generation")
	}
	observation := observeReplacementFile(t, svc, itemID, fileID, generation, "coverage-observation")
	blacklist := false
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{
		RequestID: "coverage-replacement", SourceGeneration: "review-source-generation", SourceTransfer: "review-transfer",
		Scope: "download", MediaItemID: itemID, SourceFileID: fileID, Intent: "replace_bad",
		BlacklistSourceRelease: &blacklist, ReasonCode: "bad_media", Reason: "exercise durable reviewed cleanup",
		ObservedFiles: []ReplacementObservedFile{{ID: fileID, FileGeneration: generation, ObservationID: observation.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	operationFiles, err := db.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(operationFiles) != 1 {
		t.Fatalf("persisted reviewed replacement files=%+v err=%v", operationFiles, err)
	}
	return svc, db, ctx, op, operationFiles[0], path
}

func TestReplacementLifecycleReadsReturnExactPersistedReviewAndMissingState(t *testing.T) {
	svc, db, ctx, created, reviewed, _ := reviewedBadReplacementFixture(t)
	got, files, err := svc.GetReplacement(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || got.RequestID != created.RequestID || got.State != "accepted" || len(files) != 1 {
		t.Fatalf("replacement detail=%+v files=%+v", got, files)
	}
	if files[0].MediaFileID != reviewed.MediaFileID || files[0].FileGeneration != reviewed.FileGeneration ||
		files[0].ObservationID != reviewed.ObservationID || files[0].SHA256 == "" || files[0].StorageIdentity == "" || files[0].RemovalState != "pending" {
		t.Fatalf("detail did not retain the exact reviewed file snapshot: got=%+v want=%+v", files[0], reviewed)
	}
	listed, err := svc.ReplacementFiles(ctx, created.ID)
	if err != nil || len(listed) != 1 || listed[0].Path != reviewed.Path {
		t.Fatalf("replacement file list=%+v err=%v", listed, err)
	}
	if _, _, err = svc.GetReplacement(ctx, "missing-replacement"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("missing replacement detail error=%v, want not found", err)
	}
	if rows, err := svc.ReplacementFiles(ctx, "missing-replacement"); err != nil || len(rows) != 0 {
		t.Fatalf("missing replacement file list=%+v err=%v, want empty without error", rows, err)
	}
	if _, _, err = svc.CreateReplacement(ctx, ReplacementRequest{}); !errors.Is(err, ErrReplacementInvalid) {
		t.Fatalf("empty replacement request error=%v, want typed validation error", err)
	}
	if _, err := db.GetReplacementOperationByRequestID(ctx, "not-created"); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("missing request-id lookup error=%v", err)
	}
}

func TestReplacementReservationPersistsScopeExclusionAndRetryState(t *testing.T) {
	ctx := context.Background()
	_, db, itemID := seriesSetup(t, &fakeClient{})
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	episodeID := item.Seasons[0].Episodes[0].ID
	first := sqlite.ReplacementOperation{ID: "reservation-first", RequestID: "reservation-request-first", Fingerprint: "fingerprint-first",
		State: "searching", Intent: "replace_quality", Scope: "episodes", MediaItemID: itemID,
		EpisodeIDs: []int64{episodeID}, ReasonCode: "quality_preference", Reason: "replace one episode"}
	created, replay, err := db.CreateReplacementOperation(ctx, first, nil)
	if err != nil || replay || created.ID != first.ID {
		t.Fatalf("create operation=%+v replay=%v err=%v", created, replay, err)
	}
	byID, err := db.GetReplacementOperation(ctx, first.ID)
	if err != nil || byID.RequestID != first.RequestID || len(byID.EpisodeIDs) != 1 || byID.EpisodeIDs[0] != episodeID {
		t.Fatalf("durable operation lookup=%+v err=%v", byID, err)
	}
	byRequest, err := db.GetReplacementOperationByRequestID(ctx, first.RequestID)
	if err != nil || byRequest.ID != first.ID {
		t.Fatalf("request-id lookup=%+v err=%v", byRequest, err)
	}
	if _, replay, err = db.CreateReplacementOperation(ctx, first, nil); err != nil || !replay {
		t.Fatalf("same durable request did not replay: replay=%v err=%v", replay, err)
	}
	changed := first
	changed.Fingerprint = "changed-fingerprint"
	if _, _, err = db.CreateReplacementOperation(ctx, changed, nil); !errors.Is(err, sqlite.ErrReplacementRequestConflict) {
		t.Fatalf("changed replay error=%v, want request conflict", err)
	}
	second := first
	second.ID, second.RequestID, second.Fingerprint = "reservation-overlap", "reservation-request-overlap", "fingerprint-overlap"
	if _, _, err = db.CreateReplacementOperation(ctx, second, nil); !errors.Is(err, sqlite.ErrReplacementScopeConflict) {
		t.Fatalf("overlapping active episode request error=%v, want scope conflict", err)
	}
	operations, err := db.ListReplacementOperationsForItem(ctx, itemID, 0)
	if err != nil || len(operations) != 1 || operations[0].ID != first.ID {
		t.Fatalf("item replacement history=%+v err=%v", operations, err)
	}
	if err = db.TransitionReplacementOperation(ctx, first.ID, "cleanup_blocked", "selected storage is unavailable"); err != nil {
		t.Fatal(err)
	}
	blocked, err := db.GetReplacementOperation(ctx, first.ID)
	if err != nil || blocked.State != "cleanup_blocked" || blocked.RetryAttempts != 1 || blocked.BlockedSince.IsZero() || blocked.NextAttemptAt.IsZero() || blocked.LastError == "" {
		t.Fatalf("durable blocked state=%+v err=%v", blocked, err)
	}
	if err = db.TransitionReplacementOperation(ctx, first.ID, "searching", ""); err != nil {
		t.Fatal(err)
	}
	recovered, err := db.GetReplacementOperation(ctx, first.ID)
	if err != nil || recovered.State != "searching" || !recovered.BlockedSince.IsZero() || !recovered.NextAttemptAt.IsZero() || recovered.LastError != "" {
		t.Fatalf("recovered operation state=%+v err=%v", recovered, err)
	}
}

func TestReplacementQuarantineRecoveryAfterRestartCompletesOnlyReviewedUnlink(t *testing.T) {
	_, db, ctx, op, reviewed, path := reviewedBadReplacementFixture(t)
	root := filepath.Dir(path)
	quarantineName := replacementQuarantinePrefix + op.ID + "-" + itoa(reviewed.MediaFileID)
	quarantinePath := filepath.Join(filepath.Dir(path), quarantineName)
	if err := os.Link(path, quarantinePath); err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	quarantineInfo, err := os.Stat(quarantinePath)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate process death after the exclusive hard-link and directory sync,
	// before the original library name was unlinked. Reopen the database so the
	// only recovery authority is the persisted operation and observation.
	dataDir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := New(reopened, nil, nil, nil, nil)
	persistedOperation, err := reopened.GetReplacementOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedFiles, err := reopened.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(persistedFiles) != 1 {
		t.Fatalf("recovered replacement files=%+v err=%v", persistedFiles, err)
	}
	if err := restarted.removeVerifiedReplacementQuarantine(ctx, persistedOperation, persistedFiles[0], root,
		persistedFiles[0].StorageIdentity, ".", filepath.Base(path), quarantineName, parentInfo, quarantineInfo); err != nil {
		t.Fatalf("recovered hard-link quarantine: %v", err)
	}
	for _, candidate := range []string{path, quarantinePath} {
		if _, err := os.Lstat(candidate); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reviewed replacement name %q survived successful recovery: %v", candidate, err)
		}
	}
	if _, err := reopened.GetFileQuality(ctx, reviewed.MediaFileID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("library row remained after exact quarantine recovery: %v", err)
	}
}

func TestReplacementQuarantineRecoveryRetainsTamperedHardLinkAndSource(t *testing.T) {
	svc, _, ctx, op, reviewed, path := reviewedBadReplacementFixture(t)
	quarantineName := replacementQuarantinePrefix + op.ID + "-" + itoa(reviewed.MediaFileID)
	quarantinePath := filepath.Join(filepath.Dir(path), quarantineName)
	if err := os.Link(path, quarantinePath); err != nil {
		t.Fatal(err)
	}
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	reviewedInfo, err := os.Stat(quarantinePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(quarantinePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(quarantinePath, []byte("replacement object at quarantine name"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = svc.removeVerifiedReplacementQuarantine(ctx, op, reviewed, filepath.Dir(path), reviewed.StorageIdentity,
		".", filepath.Base(path), quarantineName, parentInfo, reviewedInfo)
	if err == nil {
		t.Fatal("tampered hard-link quarantine was accepted")
	}
	for _, candidate := range []string{path, quarantinePath} {
		if _, statErr := os.Lstat(candidate); statErr != nil {
			t.Fatalf("tampered bytes were removed from %q after stale verification: %v", candidate, statErr)
		}
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "bytes" {
		t.Fatalf("original reviewed source changed during quarantine substitution: bytes=%q err=%v", got, err)
	}
}

func TestReplacementPackRegradeUsesSelectedEpisodeAndRetainsBlacklistedReason(t *testing.T) {
	svc, db, itemID := seriesSetup(t, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) == 0 {
		t.Fatal("series fixture has no selected episode")
	}
	op := sqlite.ReplacementOperation{ID: "selected-episode-regrade", Scope: "episodes", MediaItemID: itemID,
		EpisodeIDs: []int64{item.Seasons[0].Episodes[0].ID}}
	valid := Candidate{Release: rel("Test.Show.S01.1080p.WEB-DL.x264-NEW", 20)}
	blocked := Candidate{Release: rel("Test.Show.S01.1080p.WEB-DL.x264-BLOCKED", 10), BlacklistReason: "excluded for this replacement request"}
	unrelated := Candidate{Release: rel("Different.Show.S01.1080p.WEB-DL.x264-OTHER", 50)}
	got, err := svc.regradeReplacementPacks(ctx, op, item, 1, 1, []Candidate{valid, blocked, unrelated})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("regraded pack candidates=%d, want all candidates retained for review", len(got))
	}
	if !got[0].Match.Matched || got[0].Token == "" {
		t.Fatalf("season pack was not matched and tokenized against selected episode: %+v", got[0])
	}
	if got[1].Accepted || len(got[1].Rejections) == 0 || got[1].Rejections[len(got[1].Rejections)-1].Code != "blacklisted" {
		t.Fatalf("request-local blacklist was lost while regrading: %+v", got[1])
	}
	if got[2].Match.Matched || got[2].Accepted || got[2].Token == "" {
		t.Fatalf("unrelated pack was not retained as a tokenized rejected candidate: %+v", got[2])
	}
	if op.EpisodeIDs[0] != item.Seasons[0].Episodes[0].ID {
		t.Fatalf("operation scope changed during regrade: %+v", op.EpisodeIDs)
	}
}

func TestReplacementWorkerResumesAcceptedBadFileCleanupAfterDatabaseRestart(t *testing.T) {
	svc, db, ctx, op, reviewed, path := reviewedBadReplacementFixture(t)
	if op.State != "accepted" || reviewed.RemovalState != "pending" {
		t.Fatalf("fixture lacks a durable accepted cleanup obligation: operation=%+v file=%+v", op, reviewed)
	}
	dataDir := filepath.Dir(db.Path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc.db = reopened

	// The normal startup sweep, rather than a test-only direct cleanup call,
	// resumes the persisted approval after process restart. The source must be
	// retired before the worker advances the operation to replacement search.
	svc.processReplacementOperations(ctx)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("restarted worker did not retire the exact reviewed file: %v", err)
	}
	persisted, err := reopened.GetReplacementOperation(ctx, op.ID)
	if err != nil || persisted.State != "waiting_for_release" {
		t.Fatalf("restarted worker did not preserve the operation for search: %+v err=%v", persisted, err)
	}
	files, err := reopened.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(files) != 1 || files[0].MediaFileID != reviewed.MediaFileID || files[0].FileGeneration != reviewed.FileGeneration || files[0].RemovalState != "removed" {
		t.Fatalf("durable reviewed source outcome=%+v err=%v", files, err)
	}
}

func TestReplacementWorkerSkipsPreviouslyFailedExactSources(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := autoSetup(t, nil, client)
	ctx := context.Background()
	exactFailed := rel("Test.Movie.2024.1080p.WEB-DL.x264.EXACT", 40)
	exactFailed.GUID, exactFailed.IndexerID = "failed-guid", 71
	legacyFailed := rel("Test.Movie.2024.1080p.WEB-DL.x264.FALLBACK", 35)
	alternative := rel("Test.Movie.2024.1080p.WEB-DL.x264.ALTERNATIVE", 30)
	alternative.GUID, alternative.IndexerID = "alternative-guid", 72
	indexer := &mutableWorkerIndexer{releases: []ports.Release{exactFailed, legacyFailed}}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return indexer }
	op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
		ID: "failed-candidate-filter", RequestID: "failed-candidate-filter", Fingerprint: "failed-candidate-filter-fingerprint",
		State: "searching", Intent: "replace_bad", Scope: "download", MediaItemID: itemID,
		ReasonCode: "bad_media", Reason: "exercise exact failed-source suppression", CreatedAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var clientID int64
	if err := db.R.QueryRowContext(ctx, `SELECT id FROM download_clients LIMIT 1`).Scan(&clientID); err != nil {
		t.Fatal(err)
	}
	failedRows := []sqlite.Download{
		{MediaItemID: itemID, ClientID: clientID, ReleaseTitle: exactFailed.Title, Indexer: exactFailed.Indexer,
			SourceGUID: exactFailed.GUID, SourceIndexerID: exactFailed.IndexerID, Protocol: "torrent", State: "failed", ReplacementOperationID: op.ID},
		{MediaItemID: itemID, ClientID: clientID, ReleaseTitle: "test movie 2024 1080p web dl x264 fallback", Indexer: "IDX",
			Protocol: "torrent", State: "failed", ReplacementOperationID: op.ID},
	}
	for _, failed := range failedRows {
		if _, err := db.InsertDownloadRecord(ctx, failed); err != nil {
			t.Fatal(err)
		}
	}
	svc.processReplacementOperations(ctx)
	waiting, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || waiting.State != "waiting_for_release" || len(client.added) != 0 {
		t.Fatalf("worker re-admitted an exact previously failed source: operation=%+v submissions=%v err=%v", waiting, client.added, err)
	}

	indexer.set([]ports.Release{exactFailed, legacyFailed, alternative}, nil)
	if _, err := db.W.ExecContext(ctx, `UPDATE replacement_operations SET next_attempt_at=0 WHERE operation_id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	svc.processReplacementOperations(ctx)
	latest, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || latest.State != "replacing" || len(client.added) != 1 || client.added[0] != alternative.DownloadURL {
		t.Fatalf("worker did not advance to a different source after suppressing failures: operation=%+v submissions=%v err=%v", latest, client.added, err)
	}
}

func TestBadReplacementRemovalCancellationRetainsReviewedSource(t *testing.T) {
	svc, _, ctx, op, reviewed, path := reviewedBadReplacementFixture(t)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err := svc.removeBadReplacementFile(cancelled, op, reviewed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reviewed-file removal error=%v, want context canceled", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("cancelled cleanup removed reviewed source: %v", err)
	}
	quarantine := filepath.Join(filepath.Dir(path), replacementQuarantinePrefix+op.ID+"-"+itoa(reviewed.MediaFileID))
	if _, err := os.Lstat(quarantine); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled hashing created or retained a quarantine object: %v", err)
	}
}

func TestReplacementWorkerBlocksChangedReviewedBytes(t *testing.T) {
	svc, db, ctx, op, reviewed, path := reviewedBadReplacementFixture(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := []byte(strings.Repeat("x", int(info.Size())))
	if err = os.WriteFile(path, changed, 0o644); err != nil {
		t.Fatal(err)
	}

	// The scheduled worker must revalidate the approved byte snapshot. A
	// same-path replacement cannot inherit the user's earlier destructive
	// approval, and the operation remains visible for a fresh review.
	svc.processReplacementOperations(ctx)
	current, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || current.State != "cleanup_blocked" || !strings.Contains(current.LastError, "changed since review") {
		t.Fatalf("changed reviewed bytes did not block replacement cleanup: operation=%+v err=%v", current, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, changed) {
		t.Fatalf("worker modified changed source bytes: got=%q err=%v", got, err)
	}
	files, err := db.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(files) != 1 || files[0].MediaFileID != reviewed.MediaFileID || files[0].RemovalState == "removed" {
		t.Fatalf("changed source was incorrectly marked retired: files=%+v err=%v", files, err)
	}
}

func TestReplacementWorkerHonorsCancellationBeforeProcessingOpenRows(t *testing.T) {
	svc, db, ctx, op, _, _ := reviewedBadReplacementFixture(t)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	svc.processReplacementOperations(cancelled)

	current, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || current.State != "accepted" {
		t.Fatalf("worker changed an open operation after cancellation: operation=%+v err=%v", current, err)
	}
}
