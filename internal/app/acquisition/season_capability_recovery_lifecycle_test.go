package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

type changingCapabilityIndexer struct {
	caps     ports.IndexerCapabilities
	releases []ports.Release
	calls    int
	searches int
}

func (i *changingCapabilityIndexer) Capabilities(context.Context) (ports.IndexerCapabilities, error) {
	i.calls++
	return i.caps, nil
}

func (i *changingCapabilityIndexer) Search(context.Context, domain.SearchQuery) ([]ports.Release, error) {
	i.searches++
	return i.releases, nil
}

func (i *changingCapabilityIndexer) FetchRSS(context.Context) ([]ports.Release, error) {
	return nil, nil
}

func (i *changingCapabilityIndexer) Test(context.Context) error { return nil }

func TestSeasonDiscoveryRecoversPersistedUnsupportedCapabilityScopeAfterRestart(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{}
	svc, db, item := setup(t, nil, client)
	indexer := &changingCapabilityIndexer{caps: ports.IndexerCapabilities{
		Generic: ports.IndexerSearchCapability{Known: true, Available: true, Parameters: map[string]bool{"cat": true}},
		TV:      ports.IndexerSearchCapability{Known: true, Available: false},
	}}
	svc.newIndexer = func(ports.IndexerConfig) ports.Indexer { return indexer }
	if err := svc.enqueueSeason(ctx, item, 0, 1, "manual_additional_copy", true); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimJob(ctx, "capability-recovery", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.handleSeasonSearch(ctx, job)
	var deferred *sqlite.BudgetDeferred
	if !errors.As(err, &deferred) || deferred.Reason != "retrying capability scope" {
		t.Fatalf("unsupported capability snapshot error=%T %v", err, err)
	}
	if indexer.calls != 1 || indexer.searches != 0 {
		t.Fatalf("unsupported capability response did not prevent wire searches: capabilities=%d searches=%d", indexer.calls, indexer.searches)
	}
	if len(client.added) != 0 {
		t.Fatalf("unsupported scope dispatched releases: %v", client.added)
	}

	// The failure checkpoint is the durable handoff between provider capability
	// snapshots. Move its retry time forward instead of sleeping through it.
	job, err = db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint seasonCheckpoint
	if err = json.Unmarshal([]byte(job.Payload), &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Started.IsZero() || len(checkpoint.Scopes) != 1 || checkpoint.Scopes[0].Failures != 1 || checkpoint.Scopes[0].Complete {
		t.Fatalf("unsupported provider response did not preserve a retryable scope: %+v", checkpoint)
	}
	checkpoint.Scopes[0].RetryAt = time.Time{}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.W.ExecContext(ctx, `UPDATE jobs SET payload=? WHERE id=?`, string(raw), job.ID); err != nil {
		t.Fatal(err)
	}
	indexers, err := db.ListIndexers(ctx)
	if err != nil || len(indexers) != 1 {
		t.Fatalf("indexer fixture: %+v %v", indexers, err)
	}
	if err = db.SetIndexerRetryAt(ctx, indexers[0].ID, time.Time{}); err != nil {
		t.Fatal(err)
	}

	// A fresh Service instance represents worker recovery after restart. The
	// provider now advertises the exact TV query modes and returns valid singles.
	indexer.caps = ports.IndexerCapabilities{TV: ports.IndexerSearchCapability{
		Known: true, Available: true,
		Parameters: map[string]bool{"q": true, "tmdbid": true, "season": true, "ep": true},
	}}
	indexer.releases = []ports.Release{
		{Title: "Test.Show.S01E01.1080p.WEB-DL-ONE", DownloadURL: "one", Indexer: "idx", Protocol: "torrent", Size: 1000},
		{Title: "Test.Show.S01E02.1080p.WEB-DL-TWO", DownloadURL: "two", Indexer: "idx", Protocol: "torrent", Size: 1000},
	}
	restarted := New(db, nil, nil,
		func(ports.IndexerConfig) ports.Indexer { return indexer },
		func(ports.ClientConfig) ports.DownloadClient { return client },
	)
	job.Payload = string(raw)
	if err = restarted.handleSeasonSearch(ctx, job); err != nil {
		t.Fatalf("same persisted scope failed to recover: %v", err)
	}
	if len(client.added) != 2 {
		t.Fatalf("recovered comparison did not dispatch both exact singles: %v", client.added)
	}
	rows, err := db.ListRecentDownloads(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("persisted recovery rows=%+v err=%v", rows, err)
	}
	wantIDs := map[int64]bool{}
	for _, episode := range []int{1, 2} {
		id, lookupErr := db.GetEpisodeID(ctx, item, 1, episode)
		if lookupErr != nil {
			t.Fatal(lookupErr)
		}
		wantIDs[id] = true
	}
	for _, row := range rows {
		if row.ReleaseTitle == "" || len(row.ReservedEpisodes) != 1 || len(row.WantableIDs) != 1 || !wantIDs[row.ReservedEpisodes[0]] {
			t.Fatalf("recovery admitted a non-exact candidate: %+v", row)
		}
		delete(wantIDs, row.ReservedEpisodes[0])
	}
	if len(wantIDs) != 0 {
		t.Fatalf("recovery failed to reserve the complete exact scope: %v", wantIDs)
	}
	job, err = db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(job.Payload), &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.CooldownUntil.IsZero() || len(checkpoint.Scopes) == 0 {
		t.Fatalf("recovered complete evidence was not checkpointed: %+v", checkpoint)
	}
}
