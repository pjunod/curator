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
	automatic, err := queriesForIndexer(context.Background(), indexer, want, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(automatic) != 3 || automatic[0].ID == nil || automatic[0].ID.Provider != "tvdb" {
		t.Fatalf("automatic queries = %+v", automatic)
	}
	interactive, err := queriesForIndexer(context.Background(), indexer, want, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(interactive) != 5 || interactive[1].ID == nil || interactive[1].ID.Provider != "imdb" {
		t.Fatalf("interactive queries = %+v", interactive)
	}
}

func TestUnknownCapabilitiesPermitCanonicalGenericOnly(t *testing.T) {
	want := domain.MovieWantable{Item: 1, Identity: domain.MediaIdentity{Title: "Fight Club", Year: 1999, IDs: domain.ExternalIDs{IMDB: "tt0137523"}}}
	queries, err := queriesForIndexer(context.Background(), capableIndexer{}, want, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || queries[0].Mode != "generic" || queries[0].ID != nil {
		t.Fatalf("queries = %+v", queries)
	}
}
