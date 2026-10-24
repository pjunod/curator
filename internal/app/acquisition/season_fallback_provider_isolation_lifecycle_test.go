package acquisition

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func TestFailedSingleMakesOnlyItsEpisodeImportableFromPersistedSeasonPack(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	_, rows := admittedFixture(t, svc, db, itemID, "pack", "single")
	if len(rows) != 2 {
		t.Fatalf("planned downloads=%+v", rows)
	}
	pack, failedSingle := rows[0], rows[1]
	if len(pack.ReservedEpisodes) != 1 || len(failedSingle.ReservedEpisodes) != 1 {
		t.Fatalf("fixture did not start with split reservations: pack=%+v single=%+v", pack, failedSingle)
	}
	if _, err := db.ClaimPlannedSubmission(ctx, failedSingle.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSubmissionPhase(ctx, failedSingle.ID, "submitted"); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateDownloadState(ctx, failedSingle.ID, "failed", 0, "provider download failed"); err != nil {
		t.Fatal(err)
	}

	// The pack arrived after its originally selected episode was planned. The
	// failed exact single should extend that pack's authority to one episode,
	// while an unrelated extra in the payload remains outside the plan.
	payload := t.TempDir()
	for _, name := range []string{
		"Test.Show.S01E01.1080p.WEB-DL-PACK.mkv",
		"Test.Show.S01E02.1080p.WEB-DL-PACK.mkv",
		"Test.Show.S01E99.1080p.WEB-DL-PACK.mkv",
	} {
		if err := os.WriteFile(filepath.Join(payload, name), []byte("fixture video bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	result, err := svc.importDownload(ctx, pack, payload, false)
	if err != nil {
		t.Fatalf("automatic fallback pack import: %v; files=%+v", err, result.Files)
	}
	if result.Imported != 2 {
		t.Fatalf("pack import placed %d files, want original plus exact failed-single fallback: %+v", result.Imported, result.Files)
	}
	item, err := db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(item.Files) != 2 {
		t.Fatalf("fallback widened the pack to an unrelated episode: files=%+v", item.Files)
	}
	want := map[int64]bool{}
	for _, episode := range item.Seasons[0].Episodes {
		want[episode.ID] = true
	}
	for _, file := range item.Files {
		if file.CopyID != 0 || len(file.EpisodeIDs) != 1 || !want[file.EpisodeIDs[0]] {
			t.Fatalf("unexpected import scope after fallback: %+v", file)
		}
		delete(want, file.EpisodeIDs[0])
	}
	if len(want) != 0 {
		t.Fatalf("failed-single episode did not reach the primary copy: %v", want)
	}
	updatedPack, err := db.GetDownload(ctx, pack.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updatedPack.ReservedEpisodes) != 2 || len(updatedPack.WantableIDs) != 2 {
		t.Fatalf("fallback authority was not persisted as an exact two-episode reservation: %+v", updatedPack)
	}
	var fallbackHistory int
	if err := db.R.QueryRowContext(ctx, `SELECT count(*) FROM history_events WHERE type='planned_pack_fallback' AND media_item_id=?`, itemID).Scan(&fallbackHistory); err != nil {
		t.Fatal(err)
	}
	if fallbackHistory != 1 {
		t.Fatalf("fallback history rows=%d, want one exact episode extension", fallbackHistory)
	}
}

type authFailingSeasonIndexer struct{ searches int }

func (i *authFailingSeasonIndexer) Capabilities(context.Context) (ports.IndexerCapabilities, error) {
	return ports.IndexerCapabilities{}, &ports.RemoteError{Category: ports.RemoteAuth, Cause: errors.New("credentials expired")}
}

func (i *authFailingSeasonIndexer) Search(context.Context, domain.SearchQuery) ([]ports.Release, error) {
	i.searches++
	return nil, errors.New("search must not run after capability authentication failure")
}

func (*authFailingSeasonIndexer) FetchRSS(context.Context) ([]ports.Release, error) { return nil, nil }
func (*authFailingSeasonIndexer) Test(context.Context) error                        { return nil }

func TestSeasonComparisonDegradesOnlyFailedIndexerAndKeepsHealthyProviderSupply(t *testing.T) {
	ctx := context.Background()
	client := &fakeClient{}
	svc, db, itemID := setup(t, nil, client)
	initialIndexers, err := db.ListIndexers(ctx)
	if err != nil || len(initialIndexers) != 1 {
		t.Fatalf("initial indexer fixture=%+v err=%v", initialIndexers, err)
	}
	failedIndexerID := initialIndexers[0].ID
	goodIndexerID, err := db.AddIndexer(ctx, ports.IndexerConfig{
		Name: "healthy", URL: "http://healthy", Protocol: "torrent", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bad := &authFailingSeasonIndexer{}
	good := fakeIndexer{releases: []ports.Release{
		{Title: "Test.Show.S01E01.1080p.WEB-DL-HEALTHY", DownloadURL: "healthy-one", Indexer: "healthy", IndexerID: goodIndexerID, Protocol: "torrent", Size: 1000},
		{Title: "Test.Show.S01E02.1080p.WEB-DL-HEALTHY", DownloadURL: "healthy-two", Indexer: "healthy", IndexerID: goodIndexerID, Protocol: "torrent", Size: 1000},
	}}
	svc.newIndexer = func(cfg ports.IndexerConfig) ports.Indexer {
		if cfg.ID == goodIndexerID {
			return good
		}
		return bad
	}
	if err := svc.enqueueSeason(ctx, itemID, 0, 1, "test", true); err != nil {
		t.Fatal(err)
	}
	job, err := db.ClaimJob(ctx, "provider-isolation", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.handleSeasonSearch(ctx, job)
	var deferred *sqlite.BudgetDeferred
	if !errors.As(err, &deferred) || deferred.Reason != "new discovery epoch after genuine incomplete comparison" {
		t.Fatalf("partial provider comparison result=%T %v", err, err)
	}
	if bad.searches != 0 || len(client.added) != 2 {
		t.Fatalf("provider isolation: failed searches=%d, dispatched=%v", bad.searches, client.added)
	}
	rows, err := db.ListRecentDownloads(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("dispatched rows=%+v err=%v", rows, err)
	}
	want := map[string]bool{"healthy-one": true, "healthy-two": true}
	var plan sqlite.AcquisitionPlan
	for _, row := range rows {
		var execution plannedExecution
		if err := json.Unmarshal([]byte(row.ExecutionPayload), &execution); err != nil {
			t.Fatal(err)
		}
		if row.SourceIndexerID != goodIndexerID || !want[execution.URL] {
			t.Fatalf("failed indexer influenced admitted row: %+v", row)
		}
		delete(want, execution.URL)
		plan, err = db.GetAcquisitionPlan(ctx, row.PlanID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(want) != 0 {
		t.Fatalf("healthy exact supplies were not all admitted: %v", want)
	}
	var decisionFacts seasonDecision
	if err = json.Unmarshal(plan.Decision, &decisionFacts); err != nil {
		t.Fatal(err)
	}
	if len(decisionFacts.Scopes) != 2 {
		t.Fatalf("plan decision lost per-indexer scope evidence: %+v", decisionFacts.Scopes)
	}
	states := map[int64]discoveryScope{}
	for _, scope := range decisionFacts.Scopes {
		states[scope.IndexerID] = scope
	}
	indexers, err := db.ListIndexers(ctx)
	if err != nil || len(indexers) != 2 {
		t.Fatalf("indexer state lookup=%+v err=%v", indexers, err)
	}
	if failed := states[failedIndexerID]; !failed.Degraded || !failed.Complete || len(failed.Reasons) == 0 {
		t.Fatalf("authentication failure was not isolated as degraded evidence: %+v", failed)
	}
	if healthy := states[goodIndexerID]; !healthy.Complete || healthy.Degraded || len(healthy.Reasons) != 0 {
		t.Fatalf("healthy provider evidence was contaminated: %+v", healthy)
	}
}
