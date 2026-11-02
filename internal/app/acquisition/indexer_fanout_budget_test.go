package acquisition

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestExhaustedIndexerDoesNotStopAnotherIndexerInSearchFanout(t *testing.T) {
	for _, engine := range []string{"interactive", "wanted automatic"} {
		t.Run(engine, func(t *testing.T) {
			client := &fakeClient{}
			svc, db, item := autoSetup(t, nil, client)
			ctx := context.Background()
			configs, err := db.ListIndexers(ctx)
			if err != nil || len(configs) != 1 {
				t.Fatalf("indexers=%+v %v", configs, err)
			}
			exhaustedID := configs[0].ID
			if err := db.SetDailyRequestCap(ctx, exhaustedID, 10); err != nil {
				t.Fatal(err)
			}
			exhausted := false
			for i := 0; i < 20; i++ {
				err := db.ReserveIndexerRequest(ctx, exhaustedID, "interactive", false, time.Now())
				var deferred *sqlite.BudgetDeferred
				if errors.As(err, &deferred) {
					exhausted = true
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if !exhausted {
				t.Fatal("fixture did not exhaust the first indexer's real request budget")
			}
			if _, err := db.AddIndexer(ctx, ports.IndexerConfig{Name: "available-indexer", URL: "http://available.test", Protocol: "torrent", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			first := &recordingIndexer{}
			release := rel("Test.Movie.2024.1080p.WEB-DL.x264-AVAILABLE", 20)
			release.Indexer = "available-indexer"
			release.DownloadURL = "magnet:available-only"
			second := &recordingIndexer{releases: []ports.Release{release}}
			svc.newIndexer = func(cfg ports.IndexerConfig) ports.Indexer {
				if cfg.ID == exhaustedID {
					return first
				}
				return second
			}
			if engine == "interactive" {
				candidates, status, err := svc.SearchCopyDetailed(ctx, item, 0, 0, 0)
				if err != nil || len(candidates) != 1 || !status.Partial || candidates[0].Release.DownloadURL != release.DownloadURL {
					t.Fatalf("available result lost after neighbor exhausted: candidates=%+v status=%+v err=%v", candidates, status, err)
				}
			} else {
				outcome, err := svc.AutoSearchItem(ctx, item)
				if err != nil || outcome.Grabbed != 1 || len(client.added) != 1 || client.added[0] != release.DownloadURL {
					t.Fatalf("automatic fanout stopped at exhausted indexer: %+v adds=%v err=%v", outcome, client.added, err)
				}
			}
			if len(first.asked()) != 0 || len(second.asked()) == 0 {
				t.Fatalf("per-indexer isolation lost: exhausted calls=%v available calls=%v", first.asked(), second.asked())
			}
		})
	}
}
