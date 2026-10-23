package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestLibraryReportFlagIsSeparateAndGenerationScoped(t *testing.T) {
	svc, db, itemID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, _ := acq2AddFile(t, svc, itemID, 0, item.Path, "review.mkv", "")
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
		t.Fatal("file generation is empty")
	}
	before, err := db.GetFileQuality(ctx, fileID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.ReportLibraryFile(ctx, itemID, fileID, generation, "bad_media", "reported by user", nil); err != nil {
		t.Fatal(err)
	}
	flagged, err := db.HasOpenMediaFileReviewFlag(ctx, fileID, generation)
	if err != nil || !flagged {
		t.Fatalf("open report = %v, err %v", flagged, err)
	}
	wrongGeneration, err := db.HasOpenMediaFileReviewFlag(ctx, fileID, generation+"stale")
	if err != nil || wrongGeneration {
		t.Fatalf("stale generation matched report = %v, err %v", wrongGeneration, err)
	}
	after, err := db.GetFileQuality(ctx, fileID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Quality != after.Quality || before.Provenance != after.Provenance {
		t.Fatalf("report changed media quality/provenance: before=%+v after=%+v", before, after)
	}
}

func TestStableReleaseBlacklistDoesNotBecomeTitleWide(t *testing.T) {
	_, db, itemID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	if err := db.AddReleaseIdentityBlocklist(ctx, itemID, 42, "release-guid-1", "corrupt source"); err != nil {
		t.Fatal(err)
	}
	blockedReason, blocked, err := db.ReleaseIdentityBlocklistReason(ctx, itemID, 42, "release-guid-1")
	if err != nil || !blocked || blockedReason == "" {
		t.Fatalf("exact release identity was not blocked: reason=%q blocked=%v err=%v", blockedReason, blocked, err)
	}
	titleBlocked, err := db.IsBlocklisted(ctx, "Same.Title.1080p.WEB-DL", "indexer")
	if err != nil || titleBlocked {
		t.Fatalf("stable identity blacklist became title-wide: blocked=%v err=%v", titleBlocked, err)
	}
}

func TestBadReplacementPreservesBytesChangedAfterReview(t *testing.T) {
	svc, db, movieID := autoSetup(t, []ports.Release{rel("Test.Movie.2024.1080p.BluRay.x264-GOOD", 20)}, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, movieID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, path := acq2AddFile(t, svc, movieID, 0, item.Path, "reviewed.mkv", "Test.Movie.2024.720p.WEB-DL.x264-OLD")
	oldBytes := []byte("original reviewed bytes")
	if err := os.WriteFile(path, oldBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, movieID)
	if err != nil {
		t.Fatal(err)
	}
	var mediaFileGeneration string
	for _, file := range files {
		if file.ID == fileID {
			mediaFileGeneration = file.FileGeneration
		}
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='old-download-generation' WHERE id=?`, fileID); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(oldBytes)
	if _, err = db.W.ExecContext(ctx, `INSERT INTO media_file_placements(placement_id,media_file_id,file_generation,source_generation,path,sha256,committed_at) VALUES(?,?,?,?,?,?,?)`, "old-placement", fileID, mediaFileGeneration, "old-download-generation", path, hex.EncodeToString(digest[:]), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	blacklist := false
	observation, _, err := svc.StartReplacementFileObservation(ctx, movieID, fileID, mediaFileGeneration, "observe-bad-race")
	if err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	observation, err = svc.GetReplacementFileObservation(ctx, observation.ID)
	if err != nil || observation.State != "ready" {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
	changedBeforeClick := append([]byte(nil), oldBytes...)
	changedBeforeClick[0] = 'X'
	if err = os.WriteFile(path, changedBeforeClick, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = svc.CreateReplacement(ctx, ReplacementRequest{RequestID: "bad-race-before-click", SourceGeneration: "old-download-generation", Scope: "download", MediaItemID: movieID, SourceFileID: fileID, CopyID: 0, Intent: "replace_bad", BlacklistSourceRelease: &blacklist, ReasonCode: "bad_media", Reason: "reviewed", ObservedFiles: []ReplacementObservedFile{{ID: fileID, FileGeneration: mediaFileGeneration, ObservationID: observation.ID}}})
	if !errors.Is(err, ErrReplacementStale) {
		t.Fatalf("same-path same-size change before confirmation returned %v, want stale review", err)
	}
	if err = os.WriteFile(path, oldBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	observation, _, err = svc.StartReplacementFileObservation(ctx, movieID, fileID, mediaFileGeneration, "observe-bad-race-confirmed-bytes")
	if err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	observation, err = svc.GetReplacementFileObservation(ctx, observation.ID)
	if err != nil || observation.State != "ready" {
		t.Fatalf("fresh observation=%+v err=%v", observation, err)
	}
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{RequestID: "bad-race", SourceGeneration: "old-download-generation", Scope: "download", MediaItemID: movieID, SourceFileID: fileID, CopyID: 0, Intent: "replace_bad", BlacklistSourceRelease: &blacklist, ReasonCode: "bad_media", Reason: "reviewed", ObservedFiles: []ReplacementObservedFile{{ID: fileID, FileGeneration: mediaFileGeneration, ObservationID: observation.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	quarantine := filepath.Join(filepath.Dir(path), replacementQuarantinePrefix+op.ID+"-"+fmt.Sprint(fileID))
	conflictingQuarantine := []byte("unrelated quarantine file")
	if err := os.WriteFile(quarantine, conflictingQuarantine, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = svc.removeBadReplacementSources(ctx, op); err == nil {
		t.Fatal("conflicting quarantine path was overwritten or deleted")
	}
	if got, readErr := os.ReadFile(quarantine); readErr != nil || string(got) != string(conflictingQuarantine) {
		t.Fatalf("conflicting quarantine file changed: bytes=%q err=%v", got, readErr)
	}
	if err = os.Remove(quarantine); err != nil {
		t.Fatal(err)
	}
	changed := append([]byte(nil), oldBytes...)
	changed[0] = 'Y'
	if err = os.WriteFile(path, changed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = svc.removeBadReplacementSources(ctx, op); err == nil {
		t.Fatal("changed source bytes were deleted")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(changed) {
		t.Fatalf("source bytes changed after blocked cleanup: %q", got)
	}
	if _, err = db.GetFileQuality(ctx, fileID); err != nil {
		t.Fatalf("source library row was removed: %v", err)
	}
	if filepath.Dir(path) != item.Path {
		t.Fatalf("unexpected fixture path %q", path)
	}
}

func TestReplacementObservationCanBeFreshlyPreparedAfterMountReviewFailure(t *testing.T) {
	svc, db, itemID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, path := acq2AddFile(t, svc, itemID, 0, item.Path, "mount-review.mkv", "Mount.Review.2024.1080p.WEB-DL")
	if err := os.WriteFile(path, []byte("reviewable media bytes"), 0o644); err != nil {
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
	first, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, "mount-detached-observation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE replacement_file_observations SET storage_identity='detached-mount' WHERE observation_id=?`, first.ID); err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	failed, err := svc.GetReplacementFileObservation(ctx, first.ID)
	if err != nil || failed.State != "stale" {
		t.Fatalf("detached mount observation=%+v err=%v", failed, err)
	}
	second, replayed, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, "mount-restored-observation")
	if err != nil || replayed {
		t.Fatalf("fresh observation=%+v replayed=%v err=%v", second, replayed, err)
	}
	svc.processReplacementFileObservations(ctx)
	ready, err := svc.GetReplacementFileObservation(ctx, second.ID)
	if err != nil || ready.State != "ready" || ready.Size != int64(len("reviewable media bytes")) {
		t.Fatalf("restored mount observation=%+v err=%v", ready, err)
	}
}

func TestBadReplacementRemovesReviewedSourceAfterQuarantine(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, movieID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, path := acq2AddFile(t, svc, movieID, 0, item.Path, "bad.mkv", "Film.2024.720p.WEB-DL-GROUP")
	contents := []byte("reviewed bad media")
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, movieID)
	if err != nil {
		t.Fatal(err)
	}
	var generation string
	for _, file := range files {
		if file.ID == fileID {
			generation = file.FileGeneration
		}
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='bad-download-generation' WHERE id=?`, fileID); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	if _, err = db.W.ExecContext(ctx, `INSERT INTO media_file_placements(placement_id,media_file_id,file_generation,source_generation,path,sha256,committed_at) VALUES(?,?,?,?,?,?,?)`, "bad-placement", fileID, generation, "bad-download-generation", path, hex.EncodeToString(digest[:]), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	observation, _, err := svc.StartReplacementFileObservation(ctx, movieID, fileID, generation, "observe-normal-remove")
	if err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	observation, err = svc.GetReplacementFileObservation(ctx, observation.ID)
	if err != nil || observation.State != "ready" {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
	blacklist := false
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{RequestID: "normal-remove", SourceGeneration: "bad-download-generation", Scope: "download", MediaItemID: movieID, SourceFileID: fileID, Intent: "replace_bad", BlacklistSourceRelease: &blacklist, ReasonCode: "bad_media", Reason: "reviewed bad file", ObservedFiles: []ReplacementObservedFile{{ID: fileID, FileGeneration: generation, ObservationID: observation.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	// Operations are durable across Activity/process lifetime. Reopen SQLite
	// before the worker reads the accepted observation and removes the file.
	dbPath := db.Path
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err = reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc.db = reopened
	op, err = reopened.GetReplacementOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacementFiles, err := reopened.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(replacementFiles) != 1 || replacementFiles[0].Size != int64(len(contents)) {
		t.Fatalf("persisted reviewed replacement size=%+v err=%v", replacementFiles, err)
	}
	if err = svc.removeBadReplacementSources(ctx, op); err != nil {
		t.Fatalf("remove reviewed source: %v", err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reviewed source still exists: %v", err)
	}
	if _, err = reopened.GetFileQuality(ctx, fileID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("library row was not removed after source unlink: %v", err)
	}
}

func TestReplacementRejectsIncompleteCurrentSourceScope(t *testing.T) {
	svc, db, movieID := autoSetup(t, nil, &fakeClient{})
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, movieID)
	if err != nil {
		t.Fatal(err)
	}
	firstID, _ := acq2AddFile(t, svc, movieID, 0, item.Path, "one.mkv", "Test.Movie.2024.720p.WEB-DL.x264-OLD")
	secondID, _ := acq2AddFile(t, svc, movieID, 0, item.Path, "two.mkv", "Test.Movie.2024.720p.WEB-DL.x264-OLD")
	for _, id := range []int64{firstID, secondID} {
		if _, err := db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='source-generation',source_transfer='source-transfer',source_release='Test.Movie.2024.720p.WEB-DL.x264-OLD',source_indexer='old-indexer',source_guid='old-guid',source_indexer_id=8 WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	files, err := db.ListFilesForItem(ctx, movieID)
	if err != nil {
		t.Fatal(err)
	}
	var firstGeneration string
	for _, file := range files {
		if file.ID == firstID {
			firstGeneration = file.FileGeneration
		}
	}
	observation, _, err := svc.StartReplacementFileObservation(ctx, movieID, firstID, firstGeneration, "observe-only-one")
	if err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	observation, err = svc.GetReplacementFileObservation(ctx, observation.ID)
	if err != nil || observation.State != "ready" {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}
	blacklist := false
	_, _, err = svc.CreateReplacement(ctx, ReplacementRequest{
		RequestID: "incomplete-source-scope", Scope: "download", MediaItemID: movieID, SourceFileID: firstID,
		Intent: "replace_bad", BlacklistSourceRelease: &blacklist, ReasonCode: "bad_media", Reason: "bad release",
		ObservedFiles: []ReplacementObservedFile{{ID: firstID, FileGeneration: firstGeneration, ObservationID: observation.ID}},
	})
	if !errors.Is(err, ErrReplacementStale) {
		t.Fatalf("incomplete observed source scope error=%v, want stale review conflict", err)
	}
}

func observeReplacementFile(t *testing.T, svc *Service, itemID, fileID int64, generation, requestID string) sqlite.ReplacementFileObservation {
	t.Helper()
	ctx := context.Background()
	observation, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, requestID)
	if err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	observation, err = svc.GetReplacementFileObservation(ctx, observation.ID)
	if err != nil || observation.State != "ready" {
		t.Fatalf("replacement observation=%+v err=%v", observation, err)
	}
	return observation
}

func TestBadReplacementDeletesReviewedFileBeforeAdmittingAlternative(t *testing.T) {
	replacement := rel("Test.Movie.2024.1080p.WEB-DL.x264-NEW", 20)
	replacement.GUID, replacement.IndexerID = "new-guid", 42
	client := &fakeClient{}
	svc, db, itemID := autoSetup(t, []ports.Release{replacement}, client)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, path := acq2AddFile(t, svc, itemID, 0, item.Path, "bad.mkv", "Test.Movie.2024.720p.WEB-DL.x264-OLD")
	if err := os.WriteFile(path, []byte("bad source bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var fileGeneration string
	for _, file := range files {
		if file.ID == fileID {
			fileGeneration = file.FileGeneration
		}
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='old-source-generation',source_transfer='old-transfer',source_release='Test.Movie.2024.720p.WEB-DL.x264-OLD',source_indexer='old-indexer',source_guid='old-guid',source_indexer_id=7 WHERE id=?`, fileID); err != nil {
		t.Fatal(err)
	}
	observation := observeReplacementFile(t, svc, itemID, fileID, fileGeneration, "observe-bad-before-admit")
	blacklist := false
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{RequestID: "bad-before-admit", Scope: "download", MediaItemID: itemID,
		SourceFileID: fileID, Intent: "replace_bad", BlacklistSourceRelease: &blacklist, ReasonCode: "corrupt_media", Reason: "bad bytes",
		ObservedFiles: []ReplacementObservedFile{{ID: fileID, FileGeneration: fileGeneration, ObservationID: observation.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bad source still exists when replacement admission completed: %v", err)
	}
	if len(client.added) != 1 || client.added[0] != replacement.DownloadURL {
		t.Fatalf("replacement submissions=%v, want only %q after deletion", client.added, replacement.DownloadURL)
	}
	rows, err := db.ListReplacementDownloads(ctx, op.ID)
	if err != nil || len(rows) != 1 || rows[0].ReplacementOperationID != op.ID {
		t.Fatalf("operation downloads=%+v err=%v", rows, err)
	}
	final, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || final.State != "replacing" {
		t.Fatalf("operation=%+v err=%v, want replacing after exact alternative admission", final, err)
	}
}

func TestBadReplacementCanReconcileAbsenceAfterTrustedStoragePin(t *testing.T) {
	release := rel("Test.Movie.2024.1080p.WEB-DL.x264-REPLACEMENT", 20)
	release.GUID, release.IndexerID = "replacement-guid", 42
	client := &fakeClient{}
	svc, db, itemID := autoSetup(t, []ports.Release{release}, client)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, "already-missing.mkv")
	if err = os.WriteFile(path, []byte("historical bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, itemID, 0, path, int64(len("historical bytes")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='legacy-source-generation',source_transfer='legacy-source-transfer',source_release='Test.Movie.2024.720p.WEB-DL.OLD',source_indexer='idx' WHERE id=?`, fileID); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var fileGeneration string
	for _, file := range files {
		if file.ID == fileID {
			fileGeneration = file.FileGeneration
		}
	}
	trustedObservation := observeReplacementFile(t, svc, itemID, fileID, fileGeneration, "observe-present-before-absence")
	if trustedObservation.State != "ready" {
		t.Fatalf("present-file observation=%+v; expected verified bytes before absence", trustedObservation)
	}
	if _, err = db.GetReplacementStorageIdentity(ctx, filepath.Clean(item.Path)); err != nil {
		t.Fatalf("present observation did not durably pin storage identity: %v", err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	observation, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, fileGeneration, "observe-already-missing-library-file")
	if err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	observation, err = svc.GetReplacementFileObservation(ctx, observation.ID)
	if err != nil || observation.State != "absent" || observation.SHA256 != "" {
		t.Fatalf("missing file observation=%+v err=%v; expected an exact absence review without a fabricated digest", observation, err)
	}
	blacklist := false
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{RequestID: "already-missing-file-replacement", Scope: "download", MediaItemID: itemID,
		SourceFileID: fileID, Intent: "replace_bad", BlacklistSourceRelease: &blacklist, ReasonCode: "missing_media", Reason: "reconcile stale library record",
		ObservedFiles: []ReplacementObservedFile{{ID: fileID, FileGeneration: fileGeneration, ObservationID: observation.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if _, err = db.GetFileQuality(ctx, fileID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("stale library record remains after explicit absence action: err=%v", err)
	}
	if len(client.added) != 1 || client.added[0] != release.DownloadURL {
		t.Fatalf("replacement was not admitted after explicit missing-file reconciliation: %v", client.added)
	}
}

func TestMissingLegacyFileWithoutTrustedStorageIdentityFailsClosed(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := autoSetup(t, []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-REPLACEMENT", 20)}, client)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, "unverified-missing.mkv")
	if err = os.WriteFile(path, []byte("old bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, itemID, 0, path, int64(len("old bytes")))
	if err != nil {
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
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	observation, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, "observe-unverified-absence")
	if err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	observation, err = svc.GetReplacementFileObservation(ctx, observation.ID)
	if err != nil || observation.State != "stale" || observation.Error != "storage identity unverified; restore expected storage" {
		t.Fatalf("untrusted missing observation=%+v err=%v", observation, err)
	}
	if _, err = db.GetFileQuality(ctx, fileID); err != nil {
		t.Fatalf("missing file's library row was removed without trusted storage: %v", err)
	}
	if len(client.added) != 0 {
		t.Fatalf("replacement was admitted without trusted storage: %v", client.added)
	}
}

func TestMissingFileOnWrongAccessibleMountFailsTrustedIdentityCheck(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := autoSetup(t, []ports.Release{rel("Test.Movie.2024.1080p.WEB-DL.x264-REPLACEMENT", 20)}, client)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, "wrong-mount-missing.mkv")
	if err = os.WriteFile(path, []byte("verified bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, itemID, 0, path, int64(len("verified bytes")))
	if err != nil {
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
	if got := observeReplacementFile(t, svc, itemID, fileID, generation, "observe-mount-before-detach"); got.State != "ready" {
		t.Fatalf("trusted present observation=%+v", got)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	actualIdentity := replacementStorageIdentity
	replacementStorageIdentity = func(root string) (string, error) { return "different-accessible-storage", nil }
	t.Cleanup(func() { replacementStorageIdentity = actualIdentity })
	presentOnWrongMount, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, "observe-present-on-wrong-mount")
	if err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	presentOnWrongMount, err = svc.GetReplacementFileObservation(ctx, presentOnWrongMount.ID)
	if err != nil || presentOnWrongMount.State == "ready" || presentOnWrongMount.Error != "expected storage identity changed; restore expected storage" {
		t.Fatalf("observation on different accessible mount=%+v err=%v; must not appear ready", presentOnWrongMount, err)
	}
	observation, _, err := svc.StartReplacementFileObservation(ctx, itemID, fileID, generation, "observe-detached-mount")
	if err != nil {
		t.Fatal(err)
	}
	svc.processReplacementFileObservations(ctx)
	observation, err = svc.GetReplacementFileObservation(ctx, observation.ID)
	if err != nil || observation.State != "stale" || observation.Error != "expected storage identity changed; restore expected storage" {
		t.Fatalf("wrong-mount absence observation=%+v err=%v", observation, err)
	}
	if _, err = db.GetFileQuality(ctx, fileID); err != nil {
		t.Fatalf("wrong-mount review removed the library row: %v", err)
	}
	if len(client.added) != 0 {
		t.Fatalf("wrong-mount review admitted a replacement: %v", client.added)
	}
}

func TestPresentFileUnlinkReplayUsesFileIdentityOnlyBeforeRemoval(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := autoSetup(t, nil, client)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(item.Path, "unlinked-before-ledger-commit.mkv")
	contents := []byte("exact reviewed bytes")
	if err = os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, itemID, 0, path, int64(len(contents)))
	if err != nil {
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
	observation := observeReplacementFile(t, svc, itemID, fileID, generation, "observe-before-unlink-replay")
	blacklist := false
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{RequestID: "unlink-replay-op", Scope: "download", MediaItemID: itemID,
		SourceFileID: fileID, Intent: "replace_bad", BlacklistSourceRelease: &blacklist, ReasonCode: "bad_media", Reason: "reviewed bad bytes",
		ObservedFiles: []ReplacementObservedFile{{ID: fileID, FileGeneration: generation, ObservationID: observation.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// Reopen SQLite to model a crash after the anchored unlink but before the
	// corresponding library-row deletion was committed.
	dbPath := db.Path
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(filepath.Dir(dbPath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err = reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	svc.db = reopened
	operation, err := reopened.GetReplacementOperation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacementFiles, err := reopened.ListReplacementFiles(ctx, op.ID)
	if err != nil || len(replacementFiles) != 1 {
		t.Fatalf("replacement file rows=%+v err=%v", replacementFiles, err)
	}
	if err = svc.deleteMissingBadReplacementRecord(ctx, operation, replacementFiles[0], item.Path, replacementFiles[0].StorageIdentity); err != nil {
		t.Fatalf("present-file unlink replay was blocked: %v", err)
	}
	if _, err = reopened.GetFileQuality(ctx, fileID); !errors.Is(err, sqlite.ErrNotFound) {
		t.Fatalf("unlinked library record remains after replay: %v", err)
	}
}

func TestQualityReplacementCommitCleansOnlyObservedOldGeneration(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Seasons) != 1 || len(item.Seasons[0].Episodes) < 2 {
		t.Fatalf("fixture needs two episodes: %+v", item.Seasons)
	}
	episodeOne, episodeTwo := item.Seasons[0].Episodes[0].ID, item.Seasons[0].Episodes[1].ID
	oldPath := filepath.Join(item.Path, "Season 1", "old-episode-one.mkv")
	newerSiblingPath := filepath.Join(item.Path, "Season 1", "newer-episode-two.mkv")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("old selected episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newerSiblingPath, []byte("newer sibling must survive"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldID, err := db.UpsertFile(ctx, itemID, 0, oldPath, int64(len("old selected episode")))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReplaceFileEpisodeLinks(ctx, oldID, []int64{episodeOne}); err != nil {
		t.Fatal(err)
	}
	if err = db.SetFileQualityFrom(ctx, oldID, quality.Quality{Source: quality.SourceHDTV, Resolution: 720}, mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	siblingID, err := db.UpsertFile(ctx, itemID, 0, newerSiblingPath, int64(len("newer sibling must survive")))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReplaceFileEpisodeLinks(ctx, siblingID, []int64{episodeTwo}); err != nil {
		t.Fatal(err)
	}
	if err = db.SetFileQualityFrom(ctx, siblingID, quality.Quality{Source: quality.SourceWEBDL, Resolution: 1080}, mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var oldGeneration string
	for _, file := range files {
		if file.ID == oldID {
			oldGeneration = file.FileGeneration
		}
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='old-episode-one-source',source_transfer='old-transfer',source_release='Test.Show.S01E01.720p.HDTV-OLD',source_indexer='idx',source_guid='old-guid',source_indexer_id=8 WHERE id=?`, oldID); err != nil {
		t.Fatal(err)
	}
	observation := observeReplacementFile(t, svc, itemID, oldID, oldGeneration, "observe-quality-selected")
	blacklist := false
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{RequestID: "quality-exact-cleanup", Scope: "episodes", MediaItemID: itemID,
		SourceFileID: oldID, EpisodeIDs: []int64{episodeOne}, Intent: "replace_quality", BlacklistSourceRelease: &blacklist,
		ReasonCode: "quality_preference", Reason: "upgrade selected episode", ObservedFiles: []ReplacementObservedFile{{ID: oldID, FileGeneration: oldGeneration, ObservationID: observation.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	payload := t.TempDir()
	if err = os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.NEW.mkv"), []byte("replacement episode one"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := svc.importDownload(ctx, sqlite.Download{MediaItemID: itemID, ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL.NEW", Indexer: "idx", GenerationKey: "new-episode-one-source", ReplacementOperationID: op.ID}, payload, false)
	if err != nil || result.Imported != 1 {
		t.Fatalf("replacement import=%+v err=%v", result, err)
	}
	newFiles, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var newPath, newGeneration string
	var newFileID int64
	for _, file := range newFiles {
		if file.SourceGeneration == "new-episode-one-source" {
			newPath, newGeneration, newFileID = file.Path, file.FileGeneration, file.ID
		}
	}
	if newPath == "" || newGeneration == "" {
		t.Fatalf("replacement placement provenance missing: %+v", newFiles)
	}
	if err = db.CreateLibraryReviewFlag(ctx, itemID, newFileID, newGeneration, "suspected_corruption", "new replacement needs review", "{}"); err != nil {
		t.Fatal(err)
	}
	if err = svc.processReplacementOperation(ctx, op); err == nil {
		t.Fatal("expected current replacement review flag to block old-file cleanup")
	}
	if _, err = os.Stat(oldPath); err != nil {
		t.Fatalf("old usable file was retired while replacement had an open review flag: %v", err)
	}
	flags, err := db.ListOpenMediaFileReviewFlags(ctx, itemID)
	if err != nil || len(flags) != 1 {
		t.Fatalf("replacement review flags=%+v err=%v", flags, err)
	}
	if err = db.ResolveMediaFileReviewFlag(ctx, itemID, flags[0].ID, newGeneration); err != nil {
		t.Fatal(err)
	}
	newBytes, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(newPath); err != nil {
		t.Fatal(err)
	}
	if err = svc.processReplacementOperation(ctx, op); err == nil {
		t.Fatal("expected missing replacement bytes to block old-file cleanup")
	}
	if _, err = os.Stat(oldPath); err != nil {
		t.Fatalf("old usable file was retired while replacement bytes were missing: %v", err)
	}
	if err = os.WriteFile(newPath, newBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(oldPath); err != nil {
		t.Fatalf("old selected output was not retained until import commit, or cleanup lost it: %v", err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact superseded generation still exists after committed replacement: %v", err)
	}
	if _, err = os.Stat(newerSiblingPath); err != nil {
		t.Fatalf("newer unselected sibling was removed: %v", err)
	}
	finalFiles, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var hasNewEpisodeOne, hasEpisodeTwo bool
	for _, file := range finalFiles {
		for _, episodeID := range file.EpisodeIDs {
			hasNewEpisodeOne = hasNewEpisodeOne || (file.SourceGeneration == "new-episode-one-source" && episodeID == episodeOne)
			hasEpisodeTwo = hasEpisodeTwo || (file.ID == siblingID && episodeID == episodeTwo)
		}
	}
	if !hasNewEpisodeOne || !hasEpisodeTwo {
		t.Fatalf("post-replacement files=%+v, want new episode one and untouched newer episode two", finalFiles)
	}
	finalOp, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || finalOp.State != "complete" {
		t.Fatalf("replacement operation=%+v err=%v", finalOp, err)
	}
}

func TestAutomaticReportAndLateFailureDoNotAcquireOrBlacklist(t *testing.T) {
	client := &fakeClient{}
	release := rel("Test.Movie.2024.1080p.WEB-DL.x264-GOOD", 20)
	release.GUID, release.IndexerID = "good-guid", 42
	svc, db, itemID := autoSetup(t, []ports.Release{release}, client)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, path := acq2AddFile(t, svc, itemID, 0, item.Path, "existing.mkv", release.Title)
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
	if err = svc.ReportLibraryFile(ctx, itemID, fileID, generation, "suspected_corruption", "automated probe warning", json.RawMessage(`{"source":"probe"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("automatic report deleted existing media: %v", err)
	}
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("download clients=%v err=%v", clients, err)
	}
	download, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: itemID, ClientID: clients[0].ID, State: "imported",
		ReleaseTitle: release.Title, Indexer: release.Indexer, Protocol: "torrent", SourceGUID: release.GUID, SourceIndexerID: release.IndexerID})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.SetDownloadHandle(ctx, download.ID, "late-terminal-job", "late-terminal-transfer"); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Dir(db.Path)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	svc.db = reopened
	persisted, err := reopened.GetDownload(ctx, download.ID)
	if err != nil || persisted.GenerationKey != download.GenerationKey || persisted.State != "imported" {
		t.Fatalf("imported source did not survive restart: row=%+v err=%v", persisted, err)
	}
	clients, err = reopened.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("reopened clients=%v err=%v", clients, err)
	}
	svc.reconcileDownload(ctx, persisted, clients[0], ports.DownloadStatus{Handle: "late-terminal-job", State: ports.StateFailed, Message: "late terminal failure event"}, "event")
	if reason, blocked, err := reopened.ReleaseIdentityBlocklistReason(ctx, itemID, release.IndexerID, release.GUID); err != nil || blocked {
		t.Fatalf("late failure after report changed source blacklist: reason=%q blocked=%v err=%v", reason, blocked, err)
	}
	if got := len(client.added); got != 0 {
		t.Fatalf("report or late completion started acquisition: submissions=%v", client.added)
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatalf("report or late completion removed existing media: %v", err)
	}
	flags, err := reopened.ListOpenMediaFileReviewFlags(ctx, itemID)
	if err != nil || len(flags) != 1 || flags[0].State != "open" {
		t.Fatalf("review flags=%+v err=%v", flags, err)
	}
}

func TestReplacementExcludesOriginalSourceAndImportsOnlySelectedPackEpisode(t *testing.T) {
	client := &fakeClient{}
	originalPack := rel("Test.Show.S01.720p.HDTV.PACK", 10)
	originalPack.GUID, originalPack.IndexerID = "original-pack-guid", 8
	selectedPack := rel("Test.Show.S01.1080p.WEB-DL.PACK", 40)
	selectedPack.GUID, selectedPack.IndexerID = "alternative-pack-guid", 8
	svc, db, itemID := setup(t, []ports.Release{originalPack, selectedPack}, client)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	episodeID := item.Seasons[0].Episodes[0].ID
	oldPath := filepath.Join(item.Path, "Season 1", "old-episode-one.mkv")
	if err = os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(oldPath, []byte("original episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, itemID, 0, oldPath, int64(len("original episode")))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{episodeID}); err != nil {
		t.Fatal(err)
	}
	if err = db.SetFileQualityFrom(ctx, fileID, quality.Quality{Source: quality.SourceHDTV, Resolution: 720}, mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='original-generation',source_transfer='original-transfer',source_release=?,source_indexer='idx',source_guid=?,source_indexer_id=8 WHERE id=?`, originalPack.Title, originalPack.GUID, fileID); err != nil {
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
	observation := observeReplacementFile(t, svc, itemID, fileID, generation, "observe-original-pack")
	blacklist := false
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{RequestID: "selected-episode-pack-fallback", Scope: "episodes", MediaItemID: itemID,
		SourceFileID: fileID, EpisodeIDs: []int64{episodeID}, Intent: "replace_quality", BlacklistSourceRelease: &blacklist,
		ReasonCode: "quality_preference", Reason: "replace selected episode", ObservedFiles: []ReplacementObservedFile{{ID: fileID, FileGeneration: generation, ObservationID: observation.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if len(client.added) != 1 || client.added[0] != selectedPack.DownloadURL {
		t.Fatalf("request-local exclusion or selected pack fallback failed: submissions=%v want=%q", client.added, selectedPack.DownloadURL)
	}
	if reason, blocked, err := db.ReleaseIdentityBlocklistReason(ctx, itemID, originalPack.IndexerID, originalPack.GUID); err != nil || blocked {
		t.Fatalf("declining global blacklist blocked the original source elsewhere: reason=%q blocked=%v err=%v", reason, blocked, err)
	}
	if blocked, err := db.IsBlocklisted(ctx, originalPack.Title, originalPack.Indexer); err != nil || blocked {
		t.Fatalf("request-local exclusion became title blacklist: blocked=%v err=%v", blocked, err)
	}
	rows, err := db.ListReplacementDownloads(ctx, op.ID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("replacement downloads=%+v err=%v", rows, err)
	}
	payload := t.TempDir()
	if err = os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.mkv"), []byte("selected episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(payload, "Test.Show.S01E02.1080p.WEB-DL.mkv"), []byte("unselected sibling"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := svc.importDownload(ctx, rows[0], payload, false)
	if err != nil || result.Imported != 1 {
		t.Fatalf("pack import=%+v err=%v, want one selected episode", result, err)
	}
	if err = db.UpdateDownloadState(ctx, rows[0].ID, "imported", 100, ""); err != nil {
		t.Fatal(err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	finalFiles, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range finalFiles {
		if strings.Contains(filepath.Base(file.Path), "S01E02") {
			t.Fatalf("unselected pack sibling was imported: %+v", file)
		}
	}
	if _, err = os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quality replacement failed to clean exact superseded file after commit: %v", err)
	}
}

func TestFailedReplacementCleanupBlocksAcrossRestartUnlessRunnerReceiptKeepsSource(t *testing.T) {
	for _, keep := range []bool{false, true} {
		name := "cleanup pending survives restart"
		if keep {
			name = "explicit Runner Keep permits replacement while preserving bytes"
		}
		t.Run(name, func(t *testing.T) {
			replacement := rel("Test.Movie.2024.1080p.WEB-DL.x264-ALTERNATIVE", 15)
			client := &fakeClient{}
			svc, db, itemID := autoSetup(t, []ports.Release{replacement}, client)
			ctx := context.Background()
			var clientID int64
			if err := db.R.QueryRowContext(ctx, `SELECT id FROM download_clients LIMIT 1`).Scan(&clientID); err != nil {
				t.Fatal(err)
			}
			op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
				ID: "failed-attempt-cleanup-gate", RequestID: "failed-attempt-cleanup-gate", Fingerprint: "cleanup-gate-fingerprint",
				State: "searching", Intent: "replace_bad", Scope: "download", MediaItemID: itemID,
				ReasonCode: "bad_media", Reason: "replace failed release", CreatedAt: time.Now(),
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: itemID, ClientID: clientID,
				ReleaseTitle: "Test.Movie.2024.720p.WEB-DL.x264-FAILED", Indexer: "idx", Protocol: "torrent",
				State: "failed", ReplacementOperationID: op.ID, DeliveryExpected: true})
			if err != nil {
				t.Fatal(err)
			}
			dl.Transfer = "t-cleanup-gate"
			if err = db.SetDownloadHandle(ctx, dl.ID, "runner-job-1", dl.Transfer); err != nil {
				t.Fatal(err)
			}
			client.admissionFound = true
			client.admission = ports.RequestAdmission{ProtocolVersion: 1, RequestToken: dl.Transfer, JobID: "runner-job-1",
				Generation: "admission-generation", RunnerInstanceID: "runner-instance", Outcome: "failed",
				TerminalOutcome: "FAILURE/HEALTH", CustodyState: "runner_owned"}
			if _, err = db.W.ExecContext(ctx, `UPDATE downloads SET state='failed',cleanup_pending=1,delivery_expected=1 WHERE id=? AND generation_key=?`, dl.ID, dl.GenerationKey); err != nil {
				t.Fatal(err)
			}
			receipt, err := db.SaveDiscoveredDelivery(ctx, clientID, ports.RunnerDelivery{
				ProtocolVersion: 1, Sequence: 1, DeliveryID: "delivery-cleanup-gate", PublicationGeneration: "publication-generation",
				SourceArtifactID: "artifact", SourceGeneration: "source-generation", ManifestDigest: "manifest-digest",
				ArtifactID: "artifact", JobID: "runner-job-1", RequestToken: dl.Transfer, RunnerInstanceID: "runner-instance",
				PublicationPath: filepath.Join(t.TempDir(), "published"), KeepSourceOnFailure: keep, ReadyAt: time.Now(),
			}, filepath.Join(t.TempDir(), "published"))
			if err != nil {
				t.Fatal(err)
			}
			receipt.DownloadID, receipt.GenerationKey, receipt.DownloadHandle, receipt.MediaItemID = dl.ID, dl.GenerationKey, "runner-job-1", itemID
			receipt.CopyID = 0
			if err = db.AssociateDelivery(ctx, receipt); err != nil {
				t.Fatal(err)
			}
			dataDir := filepath.Dir(db.Path)
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := sqlite.Open(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			svc.db = reopened
			persisted, err := reopened.GetDownload(ctx, dl.ID)
			if err != nil || persisted.GenerationKey != dl.GenerationKey || persisted.ReplacementOperationID != op.ID || !persisted.CleanupPending {
				t.Fatalf("reopened failed attempt=%+v err=%v", persisted, err)
			}
			persistedOp, err := reopened.GetReplacementOperation(ctx, op.ID)
			if err != nil || persistedOp.ID != op.ID {
				t.Fatalf("reopened operation=%+v err=%v", persistedOp, err)
			}
			if err = svc.processReplacementOperation(ctx, persistedOp); err != nil {
				t.Fatal(err)
			}
			adds := len(client.added)
			if keep {
				if adds != 0 {
					t.Fatalf("stale publication Keep decision bypassed a current Keep-off state: adds=%v", client.added)
				}
				client.mu.Lock()
				client.admission.KeepSourceOnFailure = true
				client.admission.CustodyState = "kept_by_user"
				client.mu.Unlock()
				persistedOp, err = reopened.GetReplacementOperation(ctx, op.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err = svc.processReplacementOperation(ctx, persistedOp); err != nil {
					t.Fatal(err)
				}
				adds = len(client.added)
				if adds != 1 {
					t.Fatalf("explicit Keep did not permit alternative admission: adds=%v", client.added)
				}
				latest, err := reopened.DeliveryReceiptByID(ctx, receipt.ID)
				if err != nil || !latest.Delivery.KeepSourceOnFailure {
					t.Fatalf("Keep decision was not durable: receipt=%+v err=%v", latest, err)
				}
				updated, err := reopened.GetDownload(ctx, dl.ID)
				if err != nil || updated.PayloadRemoved {
					t.Fatalf("kept payload was marked removed: download=%+v err=%v", updated, err)
				}
			} else {
				if adds != 0 {
					t.Fatalf("replacement bypassed unresolved cleanup: adds=%v", client.added)
				}
				blocked, err := reopened.GetReplacementOperation(ctx, op.ID)
				if err != nil || blocked.State != "cleanup_blocked" || blocked.LastError == "" {
					t.Fatalf("cleanup gate is not visibly pending after restart: operation=%+v err=%v", blocked, err)
				}
			}
		})
	}
}

func TestReplacementCleanupGateUsesExactCurrentRunnerCustody(t *testing.T) {
	for _, tc := range []struct {
		name        string
		admission   ports.RequestAdmission
		wantBlocked bool
	}{
		{name: "runner still owns unresolved source", admission: ports.RequestAdmission{CustodyState: "runner_owned", SourceCleanupStatus: "pending"}, wantBlocked: true},
		{name: "runner cleanup settled", admission: ports.RequestAdmission{CustodyState: "runner_owned", SourceCleanupStatus: "removed"}},
		{name: "failed request had no payload", admission: ports.RequestAdmission{CustodyState: "no_payload", SourceCleanupStatus: "not_required"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeClient{admissionFound: true}
			svc, db, itemID := autoSetup(t, nil, client)
			ctx := context.Background()
			clients, err := db.ListDownloadClients(ctx)
			if err != nil || len(clients) == 0 {
				t.Fatalf("clients=%+v err=%v", clients, err)
			}
			dl, err := db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: itemID, ClientID: clients[0].ID, State: "failed",
				Transfer: "t-exact-current-custody", Handle: "runner-current-job", DeliveryExpected: true, CleanupPending: true})
			if err != nil {
				t.Fatal(err)
			}
			if err = db.SetDownloadHandle(ctx, dl.ID, dl.Handle, dl.Transfer); err != nil {
				t.Fatal(err)
			}
			client.admission = ports.RequestAdmission{ProtocolVersion: 1, RequestToken: dl.Transfer, JobID: dl.Handle,
				Generation: "exact-generation", RunnerInstanceID: "exact-runner", Outcome: "failed", TerminalOutcome: "FAILURE/HEALTH",
				CustodyState: tc.admission.CustodyState, SourceCleanupStatus: tc.admission.SourceCleanupStatus}
			blocked, _, err := svc.replacementAttemptCleanupBlock(ctx, dl)
			if err != nil || blocked != tc.wantBlocked {
				t.Fatalf("cleanup gate blocked=%v err=%v, want %v", blocked, err, tc.wantBlocked)
			}
		})
	}
}

func TestExplicitBadDeleteRemovesOnlySelectedFileAndPreservesKeptSibling(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	episodeOne, episodeTwo := item.Seasons[0].Episodes[0].ID, item.Seasons[0].Episodes[1].ID
	selectedPath := filepath.Join(item.Path, "Season 1", "selected-bad.mkv")
	keptSiblingPath := filepath.Join(item.Path, "Season 1", "kept-good-sibling.mkv")
	if err = os.MkdirAll(filepath.Dir(selectedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(selectedPath, []byte("selected bad episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keptSiblingPath, []byte("good sibling retained by user"), 0o644); err != nil {
		t.Fatal(err)
	}
	selectedID, err := db.UpsertFile(ctx, itemID, 0, selectedPath, int64(len("selected bad episode")))
	if err != nil {
		t.Fatal(err)
	}
	siblingID, err := db.UpsertFile(ctx, itemID, 0, keptSiblingPath, int64(len("good sibling retained by user")))
	if err != nil {
		t.Fatal(err)
	}
	for id, episodeIDs := range map[int64][]int64{selectedID: {episodeOne}, siblingID: {episodeTwo}} {
		if err = db.ReplaceFileEpisodeLinks(ctx, id, episodeIDs); err != nil {
			t.Fatal(err)
		}
		if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='kept-source-generation',source_transfer='kept-source-transfer',source_release='Test.Show.S01.720p.HDTV.PACK',source_indexer='idx',source_guid='kept-pack-guid',source_indexer_id=8 WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var selectedGeneration string
	for _, file := range files {
		if file.ID == selectedID {
			selectedGeneration = file.FileGeneration
		}
	}
	observation := observeReplacementFile(t, svc, itemID, selectedID, selectedGeneration, "observe-explicit-selected-delete")
	blacklist := false
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{RequestID: "explicit-selected-delete", Scope: "episodes", MediaItemID: itemID,
		SourceFileID: selectedID, EpisodeIDs: []int64{episodeOne}, Intent: "replace_bad", BlacklistSourceRelease: &blacklist,
		ReasonCode: "corrupt_media", Reason: "explicitly remove the selected bad episode", ObservedFiles: []ReplacementObservedFile{{ID: selectedID, FileGeneration: selectedGeneration, ObservationID: observation.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(selectedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit bad-delete did not remove the selected disposable file: %v", err)
	}
	if got, err := os.ReadFile(keptSiblingPath); err != nil || string(got) != "good sibling retained by user" {
		t.Fatalf("explicit selected delete exceeded its scope and changed kept sibling: bytes=%q err=%v", got, err)
	}
	if _, err = db.GetFileQuality(ctx, siblingID); err != nil {
		t.Fatalf("unselected good sibling library row was removed: %v", err)
	}
	if reason, blocked, err := db.ReleaseIdentityBlocklistReason(ctx, itemID, 8, "kept-pack-guid"); err != nil || blocked {
		t.Fatalf("declined blacklist choice changed the source blacklist: reason=%q blocked=%v err=%v", reason, blocked, err)
	}
}

func TestBadReplacementPreservesUnrelatedAndSharedSourceMappings(t *testing.T) {
	for _, scenario := range []string{"unrelated same-episode source", "shared target source"} {
		t.Run(scenario, func(t *testing.T) {
			svc, db, itemID := setup(t, nil, &fakeClient{})
			ctx := context.Background()
			item, err := db.GetMediaItemFull(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			episodeOne, episodeTwo := item.Seasons[0].Episodes[0].ID, item.Seasons[0].Episodes[1].ID
			type fixture struct {
				id         int64
				path       string
				generation string
			}
			createFile := func(name, body, generation string, episodes []int64) fixture {
				t.Helper()
				path := filepath.Join(item.Path, "Season 1", name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				id, err := db.UpsertFile(ctx, itemID, 0, path, int64(len(body)))
				if err != nil {
					t.Fatal(err)
				}
				if err = db.ReplaceFileEpisodeLinks(ctx, id, episodes); err != nil {
					t.Fatal(err)
				}
				if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation=?,source_transfer=?,source_release=?,source_indexer='idx',source_guid=?,source_indexer_id=8 WHERE id=?`, generation, "transfer-"+generation, "Test.Show.S01E01.720p.HDTV."+generation, "guid-"+generation, id); err != nil {
					t.Fatal(err)
				}
				files, err := db.ListFilesForItem(ctx, itemID)
				if err != nil {
					t.Fatal(err)
				}
				var fileGeneration string
				for _, file := range files {
					if file.ID == id {
						fileGeneration = file.FileGeneration
					}
				}
				return fixture{id: id, path: path, generation: fileGeneration}
			}
			var target fixture
			var other fixture
			selectedEpisodes := []int64{episodeOne}
			if scenario == "unrelated same-episode source" {
				target = createFile("target-bad.mkv", "target bad bytes", "target-source", []int64{episodeOne})
				other = createFile("alternate-good.mkv", "healthy alternate bytes", "unrelated-source", []int64{episodeOne})
			} else {
				target = createFile("target-shared.mkv", "shared selected and good bytes", "target-source", []int64{episodeOne, episodeTwo})
			}
			observed := make([]ReplacementObservedFile, 0, 2)
			fixtures := []fixture{target}
			if other.id != 0 {
				fixtures = append(fixtures, other)
			}
			for i, file := range fixtures {
				observation := observeReplacementFile(t, svc, itemID, file.id, file.generation, fmt.Sprintf("observe-preservation-%s-%d", strings.ReplaceAll(scenario, " ", "-"), i))
				observed = append(observed, ReplacementObservedFile{ID: file.id, FileGeneration: file.generation, ObservationID: observation.ID})
			}
			blacklist := false
			op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{RequestID: "preserve-" + strings.ReplaceAll(scenario, " ", "-"), Scope: "episodes", MediaItemID: itemID,
				SourceFileID: target.id, EpisodeIDs: selectedEpisodes, Intent: "replace_bad", BlacklistSourceRelease: &blacklist,
				ReasonCode: "corrupt_media", Reason: "remove only the selected bad source scope", ObservedFiles: observed})
			if err != nil {
				t.Fatal(err)
			}
			if err = svc.removeBadReplacementSources(ctx, op); err != nil {
				t.Fatal(err)
			}
			files, err := db.ListFilesForItem(ctx, itemID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "unrelated same-episode source" {
				if _, err = os.Stat(target.path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("selected bad target remains: %v", err)
				}
				if data, readErr := os.ReadFile(other.path); readErr != nil || string(data) != "healthy alternate bytes" {
					t.Fatalf("unrelated healthy source bytes changed: %q err=%v", data, readErr)
				}
				if len(files) != 1 || files[0].ID != other.id || len(files[0].EpisodeIDs) != 1 || files[0].EpisodeIDs[0] != episodeOne {
					t.Fatalf("unrelated healthy source mapping did not survive: %+v", files)
				}
			} else {
				if data, readErr := os.ReadFile(target.path); readErr != nil || string(data) != "shared selected and good bytes" {
					t.Fatalf("shared target bytes were removed: %q err=%v", data, readErr)
				}
				if len(files) != 1 || files[0].ID != target.id || len(files[0].EpisodeIDs) != 1 || files[0].EpisodeIDs[0] != episodeTwo {
					t.Fatalf("shared source should retain only the unselected good mapping: %+v", files)
				}
			}
		})
	}
}
