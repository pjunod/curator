package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type revisionRaceIndexer struct {
	fakeIndexer
	mutate  func()
	changed atomic.Bool
}

func (i *revisionRaceIndexer) Search(ctx context.Context, q domain.SearchQuery) ([]ports.Release, error) {
	if i.changed.CompareAndSwap(false, true) {
		i.mutate()
	}
	return i.fakeIndexer.Search(ctx, q)
}

func TestSeasonSnapshotRaceDefersWithoutExhaustingAttempts(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{}
	s, db, item := setup(t, nil, client)
	idx := &revisionRaceIndexer{fakeIndexer: fakeIndexer{releases: []ports.Release{
		{Title: "Test.Show.S01E01.1080p.WEB-DL-GRP", DownloadURL: "one", Indexer: "idx", Protocol: "torrent", Seeders: 10, Size: 1000},
		{Title: "Test.Show.S01E02.1080p.WEB-DL-GRP", DownloadURL: "two", Indexer: "idx", Protocol: "torrent", Seeders: 10, Size: 1000},
	}}, mutate: func() {
		if _, err := db.W.ExecContext(ctx, `UPDATE lifecycle_library_revision SET revision=revision+1 WHERE id=1`); err != nil {
			t.Fatal(err)
		}
	}}
	s.newIndexer = func(ports.IndexerConfig) ports.Indexer { return idx }
	if err := s.enqueueSeason(ctx, item, 0, 1, "backlog", true); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimJob(ctx, "snapshot-race", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	err = s.handleSeasonSearch(ctx, job)
	var deferred *sqlite.BudgetDeferred
	if !errors.As(err, &deferred) {
		t.Fatalf("snapshot race became terminal: %v", err)
	}
	if len(client.added) != 0 {
		t.Fatal("stale snapshot dispatched a download")
	}
	if err = db.DeferClaimedJob(ctx, job, deferred.Error(), time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	saved, err := db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var p seasonCheckpoint
	if err = json.Unmarshal([]byte(saved.Payload), &p); err != nil {
		t.Fatal(err)
	}
	if saved.Attempts != 0 || p.Revision != 0 || !p.Started.IsZero() || len(p.Releases) != 0 || len(p.Scopes) != 0 {
		t.Fatal("snapshot retry retained stale evidence or spent an attempt")
	}
	runQueuedSeason(t, s, db)
	if len(client.added) != 2 {
		t.Fatalf("fresh retry did not acquire missing episodes: %v", client.added)
	}
}

func failedSeasonSearch(t *testing.T, db *sqlite.DB, item int64, recoveries int, age time.Duration) int64 {
	t.Helper()
	ctx := context.Background()
	p := seasonCheckpoint{Version: 1, ItemID: item, Season: 1, Trigger: "backlog", SearchRetryCount: recoveries, Manual: true, ManualGeneration: 4, Started: time.Now().Add(-time.Hour), Releases: []ports.Release{{Title: "stale"}}}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.EnqueueJob(ctx, domain.Job{Kind: SeasonSearchJobKind, Payload: string(raw), DedupeKey: seasonJobKey(item, 0, 1), Priority: 50, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.FailJob(ctx, id, "acquisition snapshot changed"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE jobs SET attempts=3,updated_at=?,finished_at=? WHERE id=?`, time.Now().Add(-age).UnixMilli(), time.Now().Add(-age).UnixMilli(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestFailedMissingSeasonSearchRecoversAsFreshBackgroundWork(t *testing.T) {
	s, db, item, _ := seriesFixture(t)
	ctx := context.Background()
	failed := failedSeasonSearch(t, db, item, 0, 6*time.Minute)
	s.reconcileSeasonPlans(ctx) // Same path used at startup and every 30 seconds.
	job, err := db.ClaimJob(ctx, "recovery", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if job.ID == failed || job.Priority != 50 {
		t.Fatalf("failed history was overwritten or retry lost priority: %+v", job)
	}
	var p seasonCheckpoint
	if err = json.Unmarshal([]byte(job.Payload), &p); err != nil {
		t.Fatal(err)
	}
	if p.Manual || p.ManualGeneration != 0 || p.SearchRetryCount != 1 || p.Trigger != "failed_search" || !p.Started.IsZero() || len(p.Releases) != 0 {
		t.Fatalf("retry reused old discovery/manual authority: %+v", p)
	}
	s.reconcileSeasonPlans(ctx)
	var n int
	if err = db.R.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE kind=?`, SeasonSearchJobKind).Scan(&n); err != nil || n != 2 {
		t.Fatalf("duplicate retry: %d %v", n, err)
	}
}

func TestFailedSeasonRecoveryKeepsBackoffAndMonitoring(t *testing.T) {
	for _, mode := range []string{"too soon", "unmonitored", "newer completed", "active provider deadline", "upgrade only", "offloaded", "reserved", "regrab capped", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			s, db, item, _ := seriesFixture(t)
			ctx := context.Background()
			age := 6 * time.Minute
			count := 0
			if mode == "too soon" {
				count = 4
			}
			failed := failedSeasonSearch(t, db, item, count, age)
			switch mode {
			case "malformed":
				if _, err := db.W.ExecContext(ctx, `UPDATE jobs SET payload='"broken"' WHERE id=?`, failed); err != nil {
					t.Fatal(err)
				}
			case "offloaded":
				if _, err := db.W.ExecContext(ctx, `UPDATE episodes SET offloaded_at=1 WHERE media_item_id=?`, item); err != nil {
					t.Fatal(err)
				}
			case "reserved":
				if _, err := s.Grab(ctx, GrabRequest{MediaItemID: item, Season: 1, Title: "Test.Show.S01.1080p.WEB-DL-GRP", DownloadURL: "pack", Protocol: "torrent", Indexer: "idx", Size: 1000}); err != nil {
					t.Fatal(err)
				}
			case "regrab capped":
				for i := 0; i < reGrabLimit; i++ {
					id, err := db.InsertDownload(ctx, sqlite.Download{MediaItemID: item, WantableIDs: []string{fmt.Sprintf("episode:%d:1:1", item), fmt.Sprintf("episode:%d:1:2", item)}, ReleaseTitle: fmt.Sprintf("failed-%d", i), Protocol: "torrent", State: "grabbed", Size: 1000})
					if err != nil {
						t.Fatal(err)
					}
					dl, err := db.GetDownload(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					dl.State, dl.Progress = "failed", 1
					if err = db.UpdateDownloadHandoff(ctx, dl); err != nil {
						t.Fatal(err)
					}
				}
			case "unmonitored":
				if err := db.SetSeasonMonitored(ctx, item, 1, false); err != nil {
					t.Fatal(err)
				}
			case "newer completed", "active provider deadline":
				p, _ := json.Marshal(seasonCheckpoint{Version: 1, ItemID: item, Season: 1})
				id, err := db.EnqueueJob(ctx, domain.Job{Kind: SeasonSearchJobKind, Payload: string(p), DedupeKey: seasonJobKey(item, 0, 1), RunAfter: time.Now().Add(time.Hour)})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "newer completed" {
					if err = db.CompleteJob(ctx, id); err != nil {
						t.Fatal(err)
					}
				}
			case "upgrade only":
				full, err := db.GetMediaItemFull(ctx, item)
				if err != nil {
					t.Fatal(err)
				}
				for _, ep := range full.Seasons[0].Episodes {
					id, err := db.UpsertFile(ctx, item, 0, fmt.Sprintf("/library/episode-%d.mkv", ep.ID), 1000)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = db.W.ExecContext(ctx, `INSERT INTO media_file_episodes(media_file_id,episode_id) VALUES(?,?)`, id, ep.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			before := int64(0)
			if err := db.R.QueryRowContext(ctx, `SELECT max(id) FROM jobs`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := s.recoverFailedSeasonSearches(ctx); err != nil {
				t.Fatal(err)
			}
			var after int64
			if err := db.R.QueryRowContext(ctx, `SELECT max(id) FROM jobs`).Scan(&after); err != nil || after != before {
				t.Fatalf("ineligible failure %d retried: before=%d after=%d err=%v", failed, before, after, err)
			}
		})
	}
}

func TestFailedJobReplacementDoesNotResurrectAfterAConcurrentCompletedRequest(t *testing.T) {
	_, db, item, _ := seriesFixture(t)
	ctx := context.Background()
	failed := failedSeasonSearch(t, db, item, 0, time.Hour)
	next := domain.Job{Kind: SeasonSearchJobKind, Payload: `{}`, DedupeKey: seasonJobKey(item, 0, 1), Priority: 50, MaxAttempts: 3}
	id, err := db.EnqueueJob(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CompleteJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	added, err := db.EnqueueFailedJobReplacement(ctx, failed, next)
	if err != nil || added {
		t.Fatalf("stale failure resurrected after newer request completed: added=%v err=%v", added, err)
	}
}

func TestFailedSeasonRetryBackoffIsBounded(t *testing.T) {
	for i, want := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour} {
		if got := failedSeasonRetryDelay(i); got != want {
			t.Fatalf("recovery %d: %v want %v", i, got, want)
		}
	}
	if got := failedSeasonRetryDelay(1000); got != time.Hour {
		t.Fatalf("retry ceiling overflowed: %v", got)
	}
}

func TestFailedSeasonRecoveryRotatesPastIneligibleHistory(t *testing.T) {
	s, db, item, _ := seriesFixture(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		failedSeasonSearch(t, db, 900000+int64(i), 0, time.Hour)
	}
	failedSeasonSearch(t, db, item, 0, time.Hour)
	if err := s.recoverFailedSeasonSearches(ctx); err != nil {
		t.Fatal(err)
	}
	var queued int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE state='queued'`).Scan(&queued); err != nil || queued != 0 {
		t.Fatalf("invalid scopes were revived: %d %v", queued, err)
	}
	if err := s.recoverFailedSeasonSearches(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE state='queued'`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("later eligible failure was starved: %d %v", queued, err)
	}
}

func TestFailedJobReplacementRejectsMismatchedScope(t *testing.T) {
	_, db, item, _ := seriesFixture(t)
	ctx := context.Background()
	failed := failedSeasonSearch(t, db, item, 0, time.Hour)
	added, err := db.EnqueueFailedJobReplacement(ctx, failed, domain.Job{Kind: SeasonSearchJobKind, Payload: `{}`, DedupeKey: seasonJobKey(item, 0, 2), Priority: 50, MaxAttempts: 3})
	if err != nil || added {
		t.Fatalf("recovery substituted a different season: added=%v err=%v", added, err)
	}
}
