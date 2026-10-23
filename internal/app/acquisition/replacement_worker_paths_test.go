package acquisition

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// mutableWorkerIndexer lets the test move an operation through a real worker
// retry: the first search has a provider failure and the next sweep has a
// usable release. The factory is replaced on the Service, not the worker.
type mutableWorkerIndexer struct {
	mu       sync.Mutex
	releases []ports.Release
	err      error
}

func (i *mutableWorkerIndexer) Search(context.Context, domain.SearchQuery) ([]ports.Release, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]ports.Release(nil), i.releases...), i.err
}

func (*mutableWorkerIndexer) FetchRSS(context.Context) ([]ports.Release, error) { return nil, nil }
func (*mutableWorkerIndexer) Test(context.Context) error                        { return nil }

func (i *mutableWorkerIndexer) set(releases []ports.Release, err error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.releases, i.err = append([]ports.Release(nil), releases...), err
}

func TestReplacementWorkerRetriesSearchAfterProviderRecovery(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) == 0 {
		t.Fatalf("series fixture item=%+v err=%v", item, err)
	}
	indexer := &mutableWorkerIndexer{err: errors.New("indexer temporarily unavailable")}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return indexer }

	episodeID := item.Seasons[0].Episodes[0].ID
	op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
		ID: "worker-provider-retry", RequestID: "worker-provider-retry", Fingerprint: "worker-provider-retry-fingerprint",
		State: "searching", Intent: "replace_bad", Scope: "episodes", MediaItemID: itemID,
		EpisodeIDs: []int64{episodeID}, ReasonCode: "bad_media", Reason: "exercise worker search retry", CreatedAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatalf("worker should persist provider failure as a retryable state, not return a terminal error: %v", err)
	}
	waiting, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || waiting.State != "waiting_for_release" || !strings.Contains(waiting.LastError, "indexer searches failed") || waiting.NextAttemptAt.IsZero() {
		t.Fatalf("provider failure was not persisted as a retryable wait: operation=%+v err=%v", waiting, err)
	}
	if len(client.added) != 0 {
		t.Fatalf("worker submitted a release during the failed search: %v", client.added)
	}

	indexer.set([]ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-RECOVERED", 25)}, nil)
	if err = svc.processReplacementOperation(ctx, waiting); err != nil {
		t.Fatalf("worker did not retry after provider recovery: %v", err)
	}
	latest, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || latest.State != "replacing" || latest.LastError != "" {
		t.Fatalf("successful retry did not move the operation out of the wait state: operation=%+v err=%v", latest, err)
	}
	if len(client.added) != 1 || client.added[0] == "" {
		t.Fatalf("worker did not admit exactly one recovered release: %v", client.added)
	}
}

func TestReplacementWorkerStopsWhenSelectedEpisodeDisappears(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-NEW", 25)}, client)
	ctx := context.Background()
	op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
		ID: "worker-stale-episode", RequestID: "worker-stale-episode", Fingerprint: "worker-stale-episode-fingerprint",
		State: "searching", Intent: "replace_quality", Scope: "episodes", MediaItemID: itemID,
		EpisodeIDs: []int64{999999}, ReasonCode: "quality_preference", Reason: "exercise stale selected scope", CreatedAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	final, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || final.State != "failed" || final.LastError == "" {
		t.Fatalf("stale operation was not terminally recorded: operation=%+v err=%v", final, err)
	}
	if len(client.added) != 0 {
		t.Fatalf("worker admitted a release for a vanished episode scope: %v", client.added)
	}
}

func TestReplacementWorkerDoesNotDuplicateAnActiveAttempt(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-NEW", 25)}, client)
	ctx := context.Background()
	clients, err := db.ListDownloadClients(ctx)
	if err != nil || len(clients) == 0 {
		t.Fatalf("download clients=%+v err=%v", clients, err)
	}
	op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
		ID: "worker-active-attempt", RequestID: "worker-active-attempt", Fingerprint: "worker-active-attempt-fingerprint",
		State: "searching", Intent: "replace_bad", Scope: "download", MediaItemID: itemID,
		ReasonCode: "bad_media", Reason: "exercise active attempt guard", CreatedAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.InsertDownloadRecord(ctx, sqlite.Download{MediaItemID: itemID, ClientID: clients[0].ID,
		ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL.x264-IN-PROGRESS", Indexer: "idx", Protocol: "torrent",
		State: "downloading", ReplacementOperationID: op.ID}); err != nil {
		t.Fatal(err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatal(err)
	}
	latest, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || latest.State != "replacing" {
		t.Fatalf("active attempt did not keep operation in replacing state: operation=%+v err=%v", latest, err)
	}
	if len(client.added) != 0 {
		t.Fatalf("worker submitted a duplicate while the original attempt was active: %v", client.added)
	}
}

func TestReplacementWorkerSweepHonorsCancellation(t *testing.T) {
	client := &fakeClient{}
	svc, db, itemID := setup(t, []ports.Release{rel("Test.Show.S01E01.1080p.WEB-DL.x264-NEW", 25)}, client)
	ctx := context.Background()
	op, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{
		ID: "worker-cancelled-sweep", RequestID: "worker-cancelled-sweep", Fingerprint: "worker-cancelled-sweep-fingerprint",
		State: "searching", Intent: "replace_bad", Scope: "download", MediaItemID: itemID,
		ReasonCode: "bad_media", Reason: "exercise worker cancellation", CreatedAt: time.Now(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	svc.processReplacementOperations(cancelled)
	latest, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || latest.State != "searching" {
		t.Fatalf("cancelled sweep mutated open operation: operation=%+v err=%v", latest, err)
	}
	if len(client.added) != 0 {
		t.Fatalf("cancelled sweep admitted a release: %v", client.added)
	}
}

func TestReplacementWorkerContinuesToNextEpisodeAfterPartialCommit(t *testing.T) {
	client := &fakeClient{}
	secondEpisode := rel("Test.Show.S01E02.1080p.WEB-DL.x264-SECOND", 25)
	svc, db, itemID := setup(t, []ports.Release{secondEpisode}, client)
	ctx := context.Background()
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil || len(item.Seasons) == 0 || len(item.Seasons[0].Episodes) < 2 {
		t.Fatalf("series fixture item=%+v err=%v", item, err)
	}
	episodeOne, episodeTwo := item.Seasons[0].Episodes[0].ID, item.Seasons[0].Episodes[1].ID
	oldPath := filepath.Join(item.Path, "Season 1", "old-first-episode.mkv")
	if err = os.MkdirAll(filepath.Dir(oldPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(oldPath, []byte("old first episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldFileID, err := db.UpsertFile(ctx, itemID, 0, oldPath, int64(len("old first episode")))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReplaceFileEpisodeLinks(ctx, oldFileID, []int64{episodeOne}); err != nil {
		t.Fatal(err)
	}
	if err = db.SetFileQualityFrom(ctx, oldFileID, quality.Quality{Source: quality.SourceHDTV, Resolution: 720}, mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	oldSecondPath := filepath.Join(item.Path, "Season 1", "old-second-episode.mkv")
	if err = os.WriteFile(oldSecondPath, []byte("old second episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldSecondFileID, err := db.UpsertFile(ctx, itemID, 0, oldSecondPath, int64(len("old second episode")))
	if err != nil {
		t.Fatal(err)
	}
	if err = db.ReplaceFileEpisodeLinks(ctx, oldSecondFileID, []int64{episodeTwo}); err != nil {
		t.Fatal(err)
	}
	if err = db.SetFileQualityFrom(ctx, oldSecondFileID, quality.Quality{Source: quality.SourceHDTV, Resolution: 720}, mediainfo.ProvenanceFilename, mediainfo.ConfidenceNone); err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='old-first-episode',source_transfer='old-transfer' WHERE id=?`, oldFileID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE media_files SET source_generation='old-second-episode',source_transfer='old-transfer' WHERE id=?`, oldSecondFileID); err != nil {
		t.Fatal(err)
	}
	files, err := db.ListFilesForItem(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	var oldGeneration, oldSecondGeneration string
	for _, file := range files {
		if file.ID == oldFileID {
			oldGeneration = file.FileGeneration
		}
		if file.ID == oldSecondFileID {
			oldSecondGeneration = file.FileGeneration
		}
	}
	observation := observeReplacementFile(t, svc, itemID, oldFileID, oldGeneration, "worker-partial-commit-observation")
	secondObservation := observeReplacementFile(t, svc, itemID, oldSecondFileID, oldSecondGeneration, "worker-partial-commit-second-observation")
	blacklist := false
	op, _, err := svc.CreateReplacement(ctx, ReplacementRequest{
		RequestID: "worker-partial-commit", SourceGeneration: "old-first-episode", SourceTransfer: "old-transfer",
		Scope: "episodes", MediaItemID: itemID, SourceFileID: oldFileID, EpisodeIDs: []int64{episodeOne, episodeTwo}, Intent: "replace_bad",
		BlacklistSourceRelease: &blacklist, ReasonCode: "bad_media", Reason: "exercise remaining episode continuation",
		ObservedFiles: []ReplacementObservedFile{
			{ID: oldFileID, FileGeneration: oldGeneration, ObservationID: observation.ID},
			{ID: oldSecondFileID, FileGeneration: oldSecondGeneration, ObservationID: secondObservation.ID},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The first sweep retires only the reviewed bad sources, then waits because
	// the indexer currently has the later episode but not episode one.
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatalf("initial worker sweep did not settle reviewed source cleanup: %v", err)
	}
	op, err = db.GetReplacementOperation(ctx, op.ID)
	if err != nil || op.State != "waiting_for_release" {
		t.Fatalf("operation did not wait for episode one's release after cleanup: operation=%+v err=%v", op, err)
	}

	payload := t.TempDir()
	if err = os.WriteFile(filepath.Join(payload, "Test.Show.S01E01.1080p.WEB-DL.x264-FIRST.mkv"), []byte("verified first episode"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := svc.importDownload(ctx, sqlite.Download{MediaItemID: itemID, ReleaseTitle: "Test.Show.S01E01.1080p.WEB-DL.x264-FIRST", Indexer: "idx", GenerationKey: "first-episode-generation", ReplacementOperationID: op.ID}, payload, false)
	if err != nil || result.Imported != 1 {
		t.Fatalf("first selected episode import=%+v err=%v", result, err)
	}
	if err = svc.processReplacementOperation(ctx, op); err != nil {
		t.Fatalf("worker did not continue the scoped replacement after a partial commit: %v", err)
	}
	latest, err := db.GetReplacementOperation(ctx, op.ID)
	if err != nil || latest.State != "replacing" {
		t.Fatalf("remaining episode was not admitted: operation=%+v err=%v", latest, err)
	}
	if len(client.added) != 1 || client.added[0] != secondEpisode.DownloadURL {
		t.Fatalf("worker did not target the remaining selected episode: submissions=%v want=%q", client.added, secondEpisode.DownloadURL)
	}
	rows, err := db.ListReplacementDownloads(ctx, op.ID)
	if err != nil || len(rows) != 1 || rows[0].ReleaseTitle != secondEpisode.Title {
		t.Fatalf("remaining-episode download record=%+v err=%v", rows, err)
	}
}
