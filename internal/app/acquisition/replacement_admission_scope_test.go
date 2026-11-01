package acquisition

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestSeasonDispatcherHonorsReplacementScopeWithoutExcludingSibling(t *testing.T) {
	for _, scope := range []string{"selected episode", "sibling only", "whole pack"} {
		t.Run(scope, func(t *testing.T) {
			client := &fakeClient{}
			svc, db, item := setup(t, nil, client)
			ctx := context.Background()
			planID, rows := admittedFixture(t, svc, db, item, "single")
			row := rows[0]
			first, err := db.GetEpisodeID(ctx, item, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			second, err := db.GetEpisodeID(ctx, item, 1, 2)
			if err != nil {
				t.Fatal(err)
			}
			reserved := []int64{first}
			episode := 1
			if scope == "sibling only" {
				reserved = []int64{second}
				episode = 2
			}
			if scope == "whole pack" {
				reserved = []int64{}
			}
			reservedJSON, _ := json.Marshal(reserved)
			wants, _ := json.Marshal([]string{fmt.Sprintf("episode:%d:1:%d", item, episode)})
			if _, err := db.W.ExecContext(ctx, `UPDATE downloads SET release_title='Test.Show.S01.1080p.WEB-DL',reserved_episodes=?,wantables=? WHERE id=?`, string(reservedJSON), string(wants), row.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{ID: "scope-exclusion", RequestID: "scope-exclusion", Fingerprint: "scope-exclusion", State: "searching", Intent: "replace_bad", Scope: "episodes", MediaItemID: item, EpisodeIDs: []int64{first}, SourceRelease: "Test.Show.S01.1080p.WEB-DL", SourceGUID: row.SourceGUID, SourceIndexerID: row.SourceIndexerID, ReasonCode: "bad_media", Reason: "selected source was declined without global blacklist"}, nil); err != nil {
				t.Fatal(err)
			}
			err = svc.dispatchPlan(ctx, planID)
			if scope == "sibling only" {
				if err != nil || len(client.added) != 1 {
					t.Fatalf("unselected sibling scope wrongly excluded: adds=%v err=%v", client.added, err)
				}
			} else if err == nil || len(client.added) != 0 {
				t.Fatalf("declined source dispatched for selected scope: adds=%v err=%v", client.added, err)
			}
		})
	}
}
func TestRSSAndWantedSearchRespectOpenReplacementWithoutGlobalBlacklist(t *testing.T) {
	for _, engine := range []string{"rss", "wanted search"} {
		t.Run(engine, func(t *testing.T) {
			original := rel("Test.Movie.2024.1080p.WEB-DL.x264-ORIGINAL", 50)
			original.GUID = "original-guid"
			original.IndexerID = 1
			client := &fakeClient{}
			svc, db, item := autoSetup(t, []ports.Release{original}, client)
			ctx := context.Background()
			if _, _, err := db.CreateReplacementOperation(ctx, sqlite.ReplacementOperation{ID: "engine-exclusion", RequestID: "engine-exclusion", Fingerprint: "engine-exclusion", State: "waiting_for_release", Intent: "replace_bad", Scope: "download", MediaItemID: item, SourceRelease: original.Title, SourceIndexer: original.Indexer, SourceGUID: original.GUID, SourceIndexerID: original.IndexerID, ReasonCode: "bad_media", Reason: "exclude this request only"}, nil); err != nil {
				t.Fatal(err)
			}
			if engine == "rss" {
				if err := svc.SyncRSS(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := svc.AutoSearchItem(ctx, item); err != nil {
					t.Fatal(err)
				}
			}
			if len(client.added) != 0 {
				t.Fatalf("%s reacquired declined source: %v", engine, client.added)
			}
			blocked, err := db.IsBlocklisted(ctx, original.Title, original.Indexer)
			if err != nil || blocked {
				t.Fatalf("request exclusion globally blocklisted source: %v %v", blocked, err)
			}
			if err := db.TransitionReplacementOperation(ctx, "engine-exclusion", "cancelled", ""); err != nil {
				t.Fatal(err)
			}
			_, err = svc.Grab(ctx, GrabRequest{MediaItemID: item, Season: -1, Title: original.Title, DownloadURL: original.DownloadURL, Indexer: original.Indexer, SourceGUID: original.GUID, IndexerID: original.IndexerID, Protocol: original.Protocol})
			if err != nil || len(client.added) != 1 {
				t.Fatalf("settled request continued excluding source: %v %v", client.added, err)
			}
		})
	}
}
