package acquisition

import (
	"context"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

type capableIndexer struct{ caps ports.IndexerCapabilities }

func (c capableIndexer) Capabilities(context.Context) (ports.IndexerCapabilities, error) {
	return c.caps, nil
}
func (capableIndexer) Search(context.Context, domain.SearchQuery) ([]ports.Release, error) {
	return nil, nil
}
func (capableIndexer) FetchRSS(context.Context) ([]ports.Release, error) { return nil, nil }
func (capableIndexer) Test(context.Context) error                        { return nil }

func TestQueryBudgetsAndCapabilityFiltering(t *testing.T) {
	params := map[string]bool{"q": true, "tvdbid": true, "imdbid": true, "season": true, "ep": true}
	indexer := capableIndexer{caps: ports.IndexerCapabilities{
		Generic: ports.IndexerSearchCapability{Known: true, Available: true, Parameters: map[string]bool{"q": true}},
		TV:      ports.IndexerSearchCapability{Known: true, Available: true, Parameters: params},
	}}
	want := domain.EpisodeWantable{Item: 1, Season: 5, Episode: 1, Identity: domain.MediaIdentity{
		Title: "Have I Got News for You", IDs: domain.ExternalIDs{TVDB: 453187, IMDB: "tt33096993", TMDB: 250261},
		Aliases: []domain.TitleAlias{
			{Title: "Have I Got News for You US", Scope: "work", Role: "manual", Source: "manual", Searchable: true},
			{Title: "Have I Got News For You U. S.", Scope: "work", Role: "alternate", Source: "tvmaze", Searchable: true},
			{Title: "Ignored Alias", Scope: "work", Role: "alternate", Source: "tvmaze", Searchable: true},
		},
	}}
	automatic, _, err := queriesForIndexer(context.Background(), indexer, want, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(automatic) != 3 || automatic[0].ID == nil || automatic[0].ID.Provider != "tvdb" {
		t.Fatalf("automatic queries = %+v", automatic)
	}
	interactive, _, err := queriesForIndexer(context.Background(), indexer, want, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(interactive) != 5 || interactive[1].ID == nil || interactive[1].ID.Provider != "imdb" {
		t.Fatalf("interactive queries = %+v", interactive)
	}
}

func TestUnknownCapabilitiesPermitCanonicalGenericOnly(t *testing.T) {
	want := domain.MovieWantable{Item: 1, Identity: domain.MediaIdentity{Title: "Fight Club", Year: 1999, IDs: domain.ExternalIDs{IMDB: "tt0137523"}}}
	queries, _, err := queriesForIndexer(context.Background(), capableIndexer{}, want, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || queries[0].Mode != "generic" || queries[0].ID != nil {
		t.Fatalf("queries = %+v", queries)
	}
}

func TestDeduplicateReleasesUsesGUIDAndRetainsConflictingEvidence(t *testing.T) {
	releases := []ports.Release{
		{GUID: "same", Indexer: "one", Title: "Shared Title", DownloadURL: "first", IDs: domain.ExternalIDs{TVDB: 10}},
		{GUID: "same", Indexer: "one", Title: "Shared Title", DownloadURL: "second", IDs: domain.ExternalIDs{TVDB: 20}},
		{GUID: "different", Indexer: "one", Title: "Shared Title", DownloadURL: "third"},
	}
	got := deduplicateReleases(releases)
	if len(got) != 2 {
		t.Fatalf("deduplicated releases = %+v", got)
	}
	if len(got[0].IDIssues) != 1 || got[0].IDIssues[0].Code != "conflicting_ids" {
		t.Fatalf("merged evidence = %+v", got[0].IDIssues)
	}
}

func TestCandidateTokenBindsServerEvidenceToExactGrab(t *testing.T) {
	service := &Service{}
	release := ports.Release{Title: "Example.S01E01", DownloadURL: "https://indexer.invalid/1", Indexer: "one"}
	evidence := domain.MatchEvidence{Version: 1, Matched: true, Method: "id", Reason: "matched TVDB"}
	token := service.issueCandidateToken(7, 0, 1, 1, release, evidence)
	if token == "" {
		t.Fatal("empty candidate token")
	}
	if _, ok := service.consumeCandidateToken(GrabRequest{
		MediaItemID: 7, Season: 1, Episode: 1, Title: release.Title,
		DownloadURL: release.DownloadURL, Indexer: "forged", CandidateToken: token,
	}); ok {
		t.Fatal("token accepted for a different indexer")
	}
}

func TestCandidateTokenAcceptsWholeItemGrabWithoutSeason(t *testing.T) {
	service := &Service{}
	release := ports.Release{Title: "Example.2016.2160p", DownloadURL: "https://indexer.invalid/2", Indexer: "one"}
	evidence := domain.MatchEvidence{Version: 1, Matched: true, Method: "id", Reason: "matched TMDB"}
	// A movie search has no season, so the token is issued with 0 …
	token := service.issueCandidateToken(9, 0, 0, 0, release, evidence)
	// … and the grab handler spells an omitted season as -1.
	got, ok := service.consumeCandidateToken(GrabRequest{
		MediaItemID: 9, Season: -1, Title: release.Title,
		DownloadURL: release.DownloadURL, Indexer: release.Indexer, CandidateToken: token,
	})
	if !ok || got.Reason != evidence.Reason {
		t.Fatalf("whole-item grab lost its search evidence: ok=%v evidence=%+v", ok, got)
	}
	token = service.issueCandidateToken(9, 0, 2, 0, release, evidence)
	if _, ok := service.consumeCandidateToken(GrabRequest{
		MediaItemID: 9, Season: -1, Title: release.Title,
		DownloadURL: release.DownloadURL, Indexer: release.Indexer, CandidateToken: token,
	}); ok {
		t.Fatal("a season-pack token was accepted for a whole-item grab")
	}
}
