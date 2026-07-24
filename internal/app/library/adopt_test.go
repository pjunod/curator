package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// adoptProvider returns a controllable result set, so the confidence rules
// can be exercised without a live TMDB.
type adoptProvider struct {
	movies []ports.SearchResult
	series []ports.SearchResult
}

func (p adoptProvider) SearchMovies(context.Context, string) ([]ports.SearchResult, error) {
	return p.movies, nil
}

func (p adoptProvider) SearchSeries(context.Context, string) ([]ports.SearchResult, error) {
	return p.series, nil
}

func (p adoptProvider) GetMovie(_ context.Context, id int64) (domain.MediaItem, error) {
	for _, m := range p.movies {
		if m.TMDBID == id {
			return domain.MediaItem{
				Kind: domain.KindMovie, Title: m.Title, Year: m.Year,
				IDs: domain.ExternalIDs{TMDB: id},
			}, nil
		}
	}
	return domain.MediaItem{}, os.ErrNotExist
}

func (p adoptProvider) GetSeries(_ context.Context, id int64) (domain.MediaItem, error) {
	for _, s := range p.series {
		if s.TMDBID == id {
			return domain.MediaItem{
				Kind: domain.KindSeries, Title: s.Title, Year: s.Year,
				IDs: domain.ExternalIDs{TMDB: id},
			}, nil
		}
	}
	return domain.MediaItem{}, os.ErrNotExist
}

func movie(id int64, title string, year int) ports.SearchResult {
	return ports.SearchResult{Kind: domain.KindMovie, TMDBID: id, Title: title, Year: year}
}

func series(id int64, title string, year int) ports.SearchResult {
	return ports.SearchResult{Kind: domain.KindSeries, TMDBID: id, Title: title, Year: year}
}

func propose(t *testing.T, prov adoptProvider, rootKind domain.RootKind, folder string) Proposal {
	t.Helper()
	svc, _, _ := newService(t)
	svc.meta = prov
	ctx := context.Background()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, folder), 0o755); err != nil {
		t.Fatal(err)
	}
	rf, err := svc.AddRootFolder(ctx, root, rootKind)
	if err != nil {
		t.Fatal(err)
	}
	props, err := svc.ProposeAdoptions(ctx, []UnmatchedDir{
		{RootFolderID: rf.ID, Path: filepath.Join(root, folder), Name: folder},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(props) != 1 {
		t.Fatalf("want 1 proposal, got %d", len(props))
	}
	return props[0]
}

// A movie needs title AND year: remakes are the everyday case, and this is
// exactly where a fuzzy best-score match would confidently pick wrong.
func TestMovieBarRequiresYear(t *testing.T) {
	prov := adoptProvider{movies: []ports.SearchResult{movie(1, "Dune", 2021), movie(2, "Dune", 1984)}}

	if got := propose(t, prov, domain.RootKindOf(domain.KindMovie), "Dune (2021)"); got.Confidence != ConfidenceExact {
		t.Errorf("Dune (2021) against both Dunes should be exact, got %s", got.Confidence)
	} else if got.Candidates[0].Year != 2021 {
		t.Errorf("winner should be the 2021 Dune, got %d", got.Candidates[0].Year)
	}

	// Without a year in the folder name there is nothing to disambiguate.
	if got := propose(t, prov, domain.RootKindOf(domain.KindMovie), "Dune"); got.Confidence == ConfidenceExact {
		t.Error("a yearless movie folder matching two films must not auto-adopt")
	}
}

// A series may match on a unique title alone — TV folders routinely have no
// year, and requiring one would send every well-formed TV library to review.
func TestSeriesBarAllowsYearlessUniqueTitle(t *testing.T) {
	prov := adoptProvider{series: []ports.SearchResult{series(10, "Severance", 2022)}}
	got := propose(t, prov, domain.RootKindOf(domain.KindSeries), "Severance")
	if got.Confidence != ConfidenceExact {
		t.Fatalf("a unique series title should be exact, got %s", got.Confidence)
	}
}

func TestSeriesBarRejectsTwoTitleMatches(t *testing.T) {
	prov := adoptProvider{series: []ports.SearchResult{series(10, "The Office", 2005), series(11, "The Office", 2001)}}
	got := propose(t, prov, domain.RootKindOf(domain.KindSeries), "The Office")
	if got.Confidence != ConfidenceAmbiguous {
		t.Fatalf("two equally-good series matches must ask, got %s", got.Confidence)
	}
	if len(got.Candidates) != 2 {
		t.Errorf("both candidates should be offered for review, got %d", len(got.Candidates))
	}
}

// A mixed root always asks: resolving "which provider" is the ambiguity the
// bar refuses to guess through, and this is what makes typing a root pay off.
func TestMixedRootNeverAutoAdopts(t *testing.T) {
	prov := adoptProvider{movies: []ports.SearchResult{movie(1, "Arrival", 2016)}}
	got := propose(t, prov, domain.KindMixed, "Arrival (2016)")
	if got.Confidence == ConfidenceExact {
		t.Fatal("a mixed root must never auto-adopt")
	}
	if got.Kind != "" {
		t.Errorf("a mixed root's proposal should carry no kind, got %q", got.Kind)
	}
}

func TestNoProviderResultsIsConfidenceNone(t *testing.T) {
	got := propose(t, adoptProvider{}, domain.RootKindOf(domain.KindMovie), "Some Home Video")
	if got.Confidence != ConfidenceNone {
		t.Fatalf("confidence = %s, want none", got.Confidence)
	}
}

// The parse is what turns a folder name into a query — the piece ADR 0005
// promised and never wired up.
func TestProposalCarriesTheParsedTitleAndYear(t *testing.T) {
	prov := adoptProvider{movies: []ports.SearchResult{movie(1, "Arrival", 2016)}}
	got := propose(t, prov, domain.RootKindOf(domain.KindMovie), "Arrival.2016.1080p.BluRay.x264-GROUP")
	if got.ParsedTitle != "Arrival" || got.ParsedYear != 2016 {
		t.Fatalf("parsed = %q/%d, want Arrival/2016", got.ParsedTitle, got.ParsedYear)
	}
	if got.Confidence != ConfidenceExact {
		t.Errorf("a scene-named folder should still match, got %s", got.Confidence)
	}
}

// End to end: an exact match is adopted and pointed at the folder that
// already exists, so nothing is moved or renamed (the ADR 0005 promise).
func TestAdoptPointsTheItemAtTheExistingFolder(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(550, "Fight Club", 1999)}}
	ctx := context.Background()

	root := t.TempDir()
	folder := filepath.Join(root, "Fight Club (1999)")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	props, err := svc.ProposeAdoptions(ctx, []UnmatchedDir{
		{RootFolderID: rf.ID, Path: folder, Name: "Fight Club (1999)"},
	})
	if err != nil {
		t.Fatal(err)
	}

	res := svc.Adopt(ctx, props, false)
	if len(res.Adopted) != 1 || len(res.Failures) != 0 {
		t.Fatalf("adopted=%d review=%d failures=%v", len(res.Adopted), len(res.Review), res.Failures)
	}
	items, err := svc.List(ctx, domain.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 adopted item, got %d", len(items))
	}
	if items[0].Path != folder {
		t.Fatalf("item path = %q, want the folder that already exists (%q)", items[0].Path, folder)
	}
}

// A dry run proposes and writes nothing — what the first pass over a new
// root does, when the user is least able to spot a wrong match among
// hundreds.
func TestDryRunWritesNothing(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(550, "Fight Club", 1999)}}
	ctx := context.Background()

	root := t.TempDir()
	folder := filepath.Join(root, "Fight Club (1999)")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	props, _ := svc.ProposeAdoptions(ctx, []UnmatchedDir{
		{RootFolderID: rf.ID, Path: folder, Name: "Fight Club (1999)"},
	})

	res := svc.Adopt(ctx, props, true)
	if len(res.Adopted) != 1 {
		t.Fatalf("a dry run should still report what it would adopt, got %d", len(res.Adopted))
	}
	items, _ := svc.List(ctx, domain.KindMovie)
	if len(items) != 0 {
		t.Fatalf("a dry run must not write: found %d items", len(items))
	}
}

// The whole point, end to end: scan then adopt turns a list of prompts into
// an adopted library, and a first pass over a new root only proposes.
func TestRunAdoptionFirstPassProposesThenAdopts(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{
		movie(550, "Fight Club", 1999), movie(1, "Arrival", 2016),
	}}
	ctx := context.Background()

	root := t.TempDir()
	for _, name := range []string{"Fight Club (1999)", "Arrival (2016)"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	first, err := svc.RunAdoption(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Adopted) != 0 || len(first.Review) != 2 {
		t.Fatalf("first pass should propose only: adopted=%d review=%d", len(first.Adopted), len(first.Review))
	}

	// Confirm the root, and the same run now applies.
	if err := svc.ConfirmRootAdopted(ctx, rf.ID); err != nil {
		t.Fatal(err)
	}
	second, err := svc.RunAdoption(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Adopted) != 2 {
		t.Fatalf("after confirmation both should adopt: adopted=%d review=%d failures=%v",
			len(second.Adopted), len(second.Review), second.Failures)
	}
	items, _ := svc.List(ctx, domain.KindMovie)
	if len(items) != 2 {
		t.Fatalf("library should hold 2 adopted items, got %d", len(items))
	}
}
