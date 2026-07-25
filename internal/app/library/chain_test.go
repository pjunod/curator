package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// chainProvider is a stand-in for TVmaze: series keyed on TheTVDB ids, with
// a call counter, because "was the chain consulted at all" is half of what
// these tests are about.
type chainProvider struct {
	name     string
	results  []ports.SearchResult
	searches *int
	hydrates *int
}

func (p chainProvider) Name() string { return p.name }

func (p chainProvider) SearchSeries(context.Context, string) ([]ports.SearchResult, error) {
	if p.searches != nil {
		*p.searches++
	}
	return p.results, nil
}

func (p chainProvider) GetSeriesByTVDB(_ context.Context, id int64) (domain.MediaItem, error) {
	if p.hydrates != nil {
		*p.hydrates++
	}
	for _, r := range p.results {
		if r.TVDBID == id {
			return domain.MediaItem{
				Kind:  domain.KindSeries,
				Title: r.Title,
				Year:  r.Year,
				IDs:   domain.ExternalIDs{TVDB: id},
				Seasons: []domain.Season{{Number: 1, Monitored: true, Episodes: []domain.Episode{
					{SeasonNumber: 1, EpisodeNumber: 1, Title: "One", Monitored: true},
				}}},
			}, nil
		}
	}
	return domain.MediaItem{}, os.ErrNotExist
}

func chained(id int64, title string, year int) ports.SearchResult {
	return ports.SearchResult{
		Kind: domain.KindSeries, TVDBID: id, Source: "tvmaze", Title: title, Year: year,
	}
}

// The case this was all for: TMDB answers with the umbrella entry, which is
// a result but not a match, so a chain keyed on "no results" would never
// engage. The trigger is "nothing clears the bar".
func TestChainEngagesWhenTheFirstProviderHasNoMatchingRecord(t *testing.T) {
	searches := 0
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{series: []ports.SearchResult{series(79063, "Cunk on...", 2018)}}
	svc.series = []ports.SeriesProvider{chainProvider{
		name:     "tvmaze",
		results:  []ports.SearchResult{chained(414217, "Cunk on Earth", 2022)},
		searches: &searches,
	}}
	ctx := context.Background()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Cunk on Earth"), 0o755); err != nil {
		t.Fatal(err)
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindSeries))
	if err != nil {
		t.Fatal(err)
	}
	props, err := svc.ProposeAdoptions(ctx, []UnmatchedDir{
		{RootFolderID: rf.ID, Path: filepath.Join(root, "Cunk on Earth"), Name: "Cunk on Earth"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := props[0]
	if searches != 1 {
		t.Errorf("the chain should have been asked exactly once, got %d", searches)
	}
	if got.Confidence != ConfidenceExact {
		t.Fatalf("a unique title match from the chain clears the same bar as any other, got %s",
			got.Confidence)
	}
	if got.Candidates[0].TVDBID != 414217 {
		t.Errorf("want the chain's candidate first, got %+v", got.Candidates[0])
	}
	if got.Candidates[0].Source != "tvmaze" {
		t.Errorf("the candidate should say where it came from, got %q", got.Candidates[0].Source)
	}
	// TMDB's suggestion is still offered — the user may know better than
	// either provider — just not first.
	var sawUmbrella bool
	for _, c := range got.Candidates {
		if c.TMDBID == 79063 {
			sawUmbrella = true
		}
	}
	if !sawUmbrella {
		t.Error("the first provider's answer should still be on the row")
	}
}

// The expensive half of "fallback only": a folder TMDB places correctly must
// not cost a second provider call, and must not have its answer displaced.
func TestChainIsNotConsultedWhenTheFirstProviderMatches(t *testing.T) {
	searches := 0
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{series: []ports.SearchResult{series(10, "Severance", 2022)}}
	svc.series = []ports.SeriesProvider{chainProvider{
		name:     "tvmaze",
		results:  []ports.SearchResult{chained(371980, "Severance", 2022)},
		searches: &searches,
	}}
	ctx := context.Background()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Severance"), 0o755); err != nil {
		t.Fatal(err)
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindSeries))
	if err != nil {
		t.Fatal(err)
	}
	props, err := svc.ProposeAdoptions(ctx, []UnmatchedDir{
		{RootFolderID: rf.ID, Path: filepath.Join(root, "Severance"), Name: "Severance"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if searches != 0 {
		t.Errorf("a folder the first provider placed must not reach the chain, got %d calls", searches)
	}
	if props[0].Candidates[0].TMDBID != 10 {
		t.Errorf("the first provider's match must stand, got %+v", props[0].Candidates[0])
	}
}

// A chain provider that has nothing useful must not displace what is already
// on the row — different wrong answers are not an improvement.
func TestChainKeepsTheOriginalCandidatesWhenItAlsoFails(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{series: []ports.SearchResult{series(79063, "Cunk on...", 2018)}}
	svc.series = []ports.SeriesProvider{chainProvider{
		name:    "tvmaze",
		results: []ports.SearchResult{chained(1, "Something Unrelated", 2019)},
	}}
	ctx := context.Background()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Cunk on Earth"), 0o755); err != nil {
		t.Fatal(err)
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindSeries))
	if err != nil {
		t.Fatal(err)
	}
	props, err := svc.ProposeAdoptions(ctx, []UnmatchedDir{
		{RootFolderID: rf.ID, Path: filepath.Join(root, "Cunk on Earth"), Name: "Cunk on Earth"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(props[0].Candidates) == 0 || props[0].Candidates[0].TMDBID != 79063 {
		t.Errorf("want the original candidate untouched, got %+v", props[0].Candidates)
	}
}

// Adding a chain-identified series has to hydrate through the chain and key
// the row on its TVDB id — that is what makes the library re-pointable at a
// TVDB adapter later without re-matching anything (ADR 0011 §4).
func TestAddBySeriesChainKeysTheItemOnItsTVDBID(t *testing.T) {
	hydrates := 0
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{}
	svc.series = []ports.SeriesProvider{chainProvider{
		name:     "tvmaze",
		results:  []ports.SearchResult{chained(414217, "Cunk on Earth", 2022)},
		hydrates: &hydrates,
	}}
	ctx := context.Background()

	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindSeries, TVDBID: 414217, Monitored: true, Monitor: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if hydrates != 1 {
		t.Errorf("want one hydrate through the chain, got %d", hydrates)
	}
	if item.IDs.TVDB != 414217 || item.IDs.TMDB != 0 {
		t.Errorf("want the row keyed on tvdb only, got %+v", item.IDs)
	}

	// And adding it again is a duplicate, even though there is no TMDB id to
	// check — the id-collapse rule has to cover every space.
	if _, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindSeries, TVDBID: 414217, Monitored: true,
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("want ErrAlreadyExists on a second add, got %v", err)
	}
}

// Two series that neither provider has a TVDB id for must not collapse onto
// each other. A zero id means "unknown", and treating it as a value is how
// every un-keyed series becomes the same row.
func TestZeroTVDBIDIsNeverAMatch(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{series: []ports.SearchResult{
		series(1, "First Show", 2020), series(2, "Second Show", 2021),
	}}
	ctx := context.Background()

	for _, id := range []int64{1, 2} {
		if _, err := svc.Add(ctx, AddRequest{
			Kind: domain.KindSeries, TMDBID: id, Monitored: true, Monitor: "none",
		}); err != nil {
			t.Fatalf("adding tmdb %d: %v", id, err)
		}
	}
	items, err := svc.List(ctx, domain.KindSeries)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("two distinct series with no tvdb id must both exist, got %d", len(items))
	}
}

// Interactive search merges rather than falling back: the user is looking
// for something, and a provider that has it should not stay silent because
// an earlier one returned a different show.
func TestInteractiveSearchMergesTheChainAndDropsDuplicates(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{series: []ports.SearchResult{
		series(79063, "Cunk on...", 2018),
		series(10, "Severance", 2022),
	}}
	svc.series = []ports.SeriesProvider{chainProvider{
		name: "tvmaze",
		results: []ports.SearchResult{
			chained(414217, "Cunk on Earth", 2022),
			chained(371980, "Severance", 2022), // same show the first link has
		},
	}}

	res, err := svc.Search(context.Background(), domain.KindSeries, "cunk")
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, r := range res {
		titles = append(titles, r.Title)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 results (Severance collapsed once), got %d: %v", len(res), titles)
	}
	// The first link keeps its position; the chain's extras follow.
	if res[0].Title != "Cunk on..." || res[len(res)-1].Title != "Cunk on Earth" {
		t.Errorf("want the first provider first and the chain's addition last, got %v", titles)
	}
}

// Order between the two fallbacks. The umbrella entry offers an alternate
// title that matches the folder exactly, and it is still the wrong answer:
// a standalone record from the next link wins, and the alternate-title
// lookup should not even be paid for.
func TestChainBeatsAnAlternateTitleOnTheUmbrellaEntry(t *testing.T) {
	looks := 0
	umbrella := altProvider{
		adoptProvider: adoptProvider{series: []ports.SearchResult{series(79063, "Cunk on...", 2018)}},
		alts:          map[int64][]string{79063: {"Cunk on Earth", "Cunk on Britain"}},
		nLooks:        &looks,
	}

	svc, _, _ := newService(t)
	svc.meta = umbrella
	svc.series = []ports.SeriesProvider{chainProvider{
		name:    "tvmaze",
		results: []ports.SearchResult{chained(414217, "Cunk on Earth", 2022)},
	}}
	ctx := context.Background()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Cunk on Earth"), 0o755); err != nil {
		t.Fatal(err)
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindSeries))
	if err != nil {
		t.Fatal(err)
	}
	props, err := svc.ProposeAdoptions(ctx, []UnmatchedDir{
		{RootFolderID: rf.ID, Path: filepath.Join(root, "Cunk on Earth"), Name: "Cunk on Earth"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := props[0]
	if got.Candidates[0].TVDBID != 414217 {
		t.Errorf("want the standalone record, got %+v", got.Candidates[0])
	}
	if got.Confidence != ConfidenceExact {
		t.Errorf("a standalone primary-title match is exact, got %s", got.Confidence)
	}
	if got.HeldBy != "" || len(got.SharedWith) != 0 {
		t.Errorf("no umbrella collision to report once the real record is found: %+v", got)
	}
	if looks != 0 {
		t.Errorf("the alternate-title lookup should not be reached, got %d", looks)
	}
}
