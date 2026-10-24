package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestReplacementWorkerRegradesSeasonPackAgainstSelectedCopyProfile(t *testing.T) {
	client := &fakeClient{}
	pack := rel("Test.Show.S01.720p.HDTV.x264-LOW", 30)
	svc, db, itemID := seriesSetup(t, client)
	ctx := context.Background()
	indexer := &mutableWorkerIndexer{releases: []ports.Release{pack}}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return indexer }

	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) == 0 {
		t.Fatalf("series fixture item=%+v err=%v", item, err)
	}
	copyPath := filepath.Join(t.TempDir(), "1080p-copy")
	if err = os.MkdirAll(copyPath, 0o755); err != nil {
		t.Fatal(err)
	}
	copyID, err := db.AddMediaCopy(ctx, domain.MediaCopy{
		MediaItemID: itemID, Name: "1080p copy", Path: copyPath, QualityProfileID: 2, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
		ID: "copy-profile-pack", RequestID: "copy-profile-pack", Fingerprint: "copy-profile-pack-fingerprint",
		State: "searching", Intent: "replace_quality", Scope: "episodes", MediaItemID: itemID, CopyID: copyID,
		EpisodeIDs: []int64{item.Seasons[0].Episodes[0].ID}, ReasonCode: "quality_preference",
		Reason: "replacement must meet the selected copy profile", CreatedAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	latest, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || latest.State != "waiting_for_release" {
		t.Fatalf("below-profile pack did not remain a durable wait: operation=%+v err=%v", latest, err)
	}
	if len(client.added) != 0 {
		t.Fatalf("worker admitted a pack below the selected copy's HD-1080p profile: %v", client.added)
	}
	rows, err := db.ListReplacementDownloads(ctx, op.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("profile-rejected pack created a replacement attempt: rows=%+v err=%v", rows, err)
	}
}

func TestReplacementWorkerPreservesUnselectedEpisodeOnSharedBadSource(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := seriesSetup(t, client)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) < 2 {
		t.Fatalf("series fixture item=%+v err=%v", item, err)
	}
	episodeOne, episodeTwo := item.Seasons[0].Episodes[0].ID, item.Seasons[0].Episodes[1].ID
	path := filepath.Join(item.Path, "Season 1", "shared-bad.mkv")
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	const body = "bad selected episode with a good unselected episode"
	if err = os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	fileID, err := db.UpsertFile(ctx, itemID, 0, path, int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReplaceFileEpisodeLinks(ctx, fileID, []int64{episodeOne, episodeTwo}); err != nil {
		t.Fatal(err)
	}
	if err = db.SetFileQualityFrom(ctx, fileID, quality.Quality{Source: quality.SourceHDTV, Resolution: 720}, mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='worker-shared-generation',source_transfer='worker-shared-transfer',source_release='Test.Show.S01.720p.HDTV.PACK',source_indexer='idx',source_guid='worker-shared-guid',source_indexer_id=8 WHERE id=?`, fileID); err != nil {
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
		t.Fatal("shared source has no durable file generation")
	}
	observation := observeReplacementFile(t, svc, itemID, fileID, generation, "worker-shared-source")
	blacklist := false
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{
		RequestID: "worker-shared-source", SourceGeneration: "worker-shared-generation", SourceTransfer: "worker-shared-transfer",
		Scope: "episodes", MediaItemID: itemID, SourceFileID: fileID, EpisodeIDs: []int64{episodeOne}, Intent: "replace_bad",
		BlacklistSourceRelease: &blacklist, ReasonCode: "bad_media", Reason: "replace only the selected episode",
		ObservedFiles: []ReplacementObservedFile{{ID: fileID, FileGeneration: generation, ObservationID: observation.ID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	if got, readErr := os.ReadFile(path); readErr != nil || string(got) != body {
		t.Fatalf("shared source bytes were removed while replacing one episode: bytes=%q err=%v", got, readErr)
	}
	remaining, err := db.ListFilesForItem(ctx, itemID)
	if err != nil || len(remaining) != 1 || remaining[0].ID != fileID || len(remaining[0].EpisodeIDs) != 1 || remaining[0].EpisodeIDs[0] != episodeTwo {
		t.Fatalf("worker did not preserve only the unselected episode mapping: files=%+v err=%v", remaining, err)
	}
	latest, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || latest.State != "waiting_for_release" {
		t.Fatalf("worker did not preserve the selected replacement obligation: operation=%+v err=%v", latest, err)
	}
	if len(client.added) != 0 {
		t.Fatalf("worker should wait when no selected-scope release is available: %v", client.added)
	}
}

func TestReplacementWorkerDoesNotReacquireTheReviewedSource(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := seriesSetup(t, client)
	ctx := context.Background()
	source := rel("Test.Show.S01E01.1080p.WEB-DL.x264-ORIGINAL", 30)
	source.GUID, source.IndexerID = "reviewed-release-guid", 41
	indexer := &mutableWorkerIndexer{releases: []ports.Release{source}}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return indexer }
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) == 0 {
		t.Fatalf("series fixture item=%+v err=%v", item, err)
	}
	op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
		ID: "same-reviewed-source", RequestID: "same-reviewed-source", Fingerprint: "same-reviewed-source-fingerprint",
		State: "searching", Intent: "replace_bad", Scope: "episodes", MediaItemID: itemID,
		EpisodeIDs: []int64{item.Seasons[0].Episodes[0].ID}, SourceRelease: source.Title,
		SourceIndexer: source.Indexer, SourceGUID: source.GUID, SourceIndexerID: source.IndexerID,
		ReasonCode: "bad_media", Reason: "replacement must not reacquire the reviewed source", CreatedAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	latest, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || latest.State != "waiting_for_release" {
		t.Fatalf("worker did not persist a wait after suppressing the reviewed release: operation=%+v err=%v", latest, err)
	}
	if len(client.added) != 0 {
		t.Fatalf("worker reacquired the exact reviewed source: %v", client.added)
	}
	rows, err := db.ListReplacementDownloads(ctx, op.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("same-source suppression created a download attempt: rows=%+v err=%v", rows, err)
	}
}

func TestCreateReplacementRejectsEpisodeScopeForMovieBeforeQueueing(t *testing.T) {
	client := &fakeClient{}
	svc, _, movieID := autoSetup(t, nil, client)
	blacklist := false
	_, _, err := svc.CreateReplacement(context.Background(), ReplacementRequest{
		RequestID: "movie-episode-scope", Scope: "episodes", MediaItemID: movieID,
		SourceFileID: 1, EpisodeIDs: []int64{1}, Intent: "replace_bad", BlacklistSourceRelease: &blacklist,
		ReasonCode: "bad_media", Reason: "episode scope is not supported for movies",
		ObservedFiles: []ReplacementObservedFile{{ID: 1, FileGeneration: "observed-generation", ObservationID: "observed-file"}},
	})
	if !errors.Is(err, ErrReplacementInvalid) {
		t.Fatalf("episode-scoped replacement for a movie error=%v, want invalid request", err)
	}
	if len(client.added) != 0 {
		t.Fatalf("invalid movie episode scope queued a download: %v", client.added)
	}
}
