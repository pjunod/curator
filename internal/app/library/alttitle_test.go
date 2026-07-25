package library

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/parser"
	"github.com/monarr-media/monarr/internal/ports"
)

// altProvider is an adoptProvider that also answers alternative-title
// lookups, and counts them — the cost of the second pass is part of what
// these tests are pinning down.
type altProvider struct {
	adoptProvider
	alts   map[int64][]string
	nLooks *int
}

func (p altProvider) AlternativeTitles(_ context.Context, _ domain.MediaKind, id int64) ([]string, error) {
	*p.nLooks++
	return p.alts[id], nil
}

func proposeWith(t *testing.T, prov ports.MetadataProvider, rootKind domain.RootKind, folder string) Proposal {
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

// The case that started this: a folder named after the release title of a
// film TMDB lists under a different one. Comparing only primary titles left
// the row saying "no match —" with the right answer one field away.
func TestAlternateTitleRanksTheRightCandidateFirst(t *testing.T) {
	looks := 0
	prov := altProvider{
		adoptProvider: adoptProvider{movies: []ports.SearchResult{
			movie(1, "Cunk on Life", 2024),
		}},
		alts:   map[int64][]string{1: {"Diane Morgan's Cunk on Life", "Cunk's Quest for Meaning"}},
		nLooks: &looks,
	}

	got := proposeWith(t, prov, domain.RootKindOf(domain.KindMovie), "Cunk's Quest for Meaning (2024)")

	if len(got.Candidates) == 0 {
		t.Fatal("a folder named after an alternate title should still get a candidate")
	}
	if got.Candidates[0].TMDBID != 1 {
		t.Errorf("want Cunk on Life first, got %q", got.Candidates[0].Title)
	}
	// The chip has to explain itself: it says "Cunk on Life" for a folder
	// that says something else, and only the matched name makes that legible.
	if want := []string{"Cunk's Quest for Meaning"}; len(got.Candidates[0].AltTitles) != 1 ||
		got.Candidates[0].AltTitles[0] != want[0] {
		t.Errorf("want the matched alternate name carried through, got %v", got.Candidates[0].AltTitles)
	}
	// ...and the unmatched forty regional retitles must not ride along.
	if looks != 1 {
		t.Errorf("want exactly one lookup for one candidate, got %d", looks)
	}
}

// An alternate-title match is offered, never applied. TMDB's alternate
// titles are crowd-maintained and include working titles and noise, so this
// is the conservative half of ADR 0010 §2 holding.
func TestAlternateTitleNeverAutoAdopts(t *testing.T) {
	looks := 0
	prov := altProvider{
		adoptProvider: adoptProvider{series: []ports.SearchResult{series(7, "Cunk on Earth", 2022)}},
		alts:          map[int64][]string{7: {"Charlie Brooker's Cunk on Earth"}},
		nLooks:        &looks,
	}

	got := proposeWith(t, prov, domain.RootKindOf(domain.KindSeries), "Charlie Brooker's Cunk on Earth")
	if got.Confidence == ConfidenceExact {
		t.Fatal("a match found only through an alternate title must ask first")
	}
	if len(got.Candidates) != 1 || got.Candidates[0].TMDBID != 7 {
		t.Fatalf("the candidate should still be offered, got %+v", got.Candidates)
	}

	svc, _, _ := newService(t)
	svc.meta = prov
	res := svc.Adopt(context.Background(), []Proposal{got}, false)
	if len(res.Adopted) != 0 || len(res.Review) != 1 {
		t.Errorf("want it held for review, got %d adopted / %d review",
			len(res.Adopted), len(res.Review))
	}
}

// Promotion is the load-bearing part: rows carry three candidates, and a
// retitled work lands wherever the provider's relevance ranking puts it.
func TestAlternateTitleSurvivesTheThreeCandidateTrim(t *testing.T) {
	looks := 0
	noise := []ports.SearchResult{}
	for i := int64(1); i <= 4; i++ {
		noise = append(noise, movie(i, fmt.Sprintf("Something Else %d", i), 2024))
	}
	noise = append(noise, movie(9, "Cunk on Life", 2024))

	prov := altProvider{
		adoptProvider: adoptProvider{movies: noise},
		alts:          map[int64][]string{9: {"Cunk's Quest for Meaning"}},
		nLooks:        &looks,
	}

	got := proposeWith(t, prov, domain.RootKindOf(domain.KindMovie), "Cunk's Quest for Meaning (2024)")
	if len(got.Candidates) == 0 || got.Candidates[0].TMDBID != 9 {
		t.Fatalf("the matching candidate must be promoted past the noise, got %+v", got.Candidates)
	}
}

// An alternate title gets a candidate over the title hurdle and no further:
// every other part of the bar — the year for films, the author for books —
// still has to be cleared on its own terms.
func TestAlternateTitleStillObeysTheRestOfTheBar(t *testing.T) {
	film := movie(1, "Cunk on Life", 2024)
	film.AltTitles = []string{"Cunk's Quest for Meaning"}
	results := []ports.SearchResult{film}

	agrees := parser.Parse("Cunk's Quest for Meaning (2024)")
	if got := clearingAlt(domain.KindMovie, agrees, results); len(got) != 1 {
		t.Fatalf("the same year should clear, got %d", len(got))
	}

	// A five-year gap is two different releases that happen to share a name.
	disagrees := parser.Parse("Cunk's Quest for Meaning (2019)")
	if got := clearingAlt(domain.KindMovie, disagrees, results); len(got) != 0 {
		t.Errorf("a five-year gap must not clear, got %d", len(got))
	}

	// And a film with no year on either side never clears, alternate name or
	// not — remakes are why that rule exists.
	yearless := movie(2, "Nosferatu", 0)
	yearless.AltTitles = []string{"Nosferatu: A Symphony of Horror"}
	if got := clearingAlt(domain.KindMovie, parser.Parse("Nosferatu A Symphony of Horror"),
		[]ports.SearchResult{yearless}); len(got) != 0 {
		t.Errorf("a yearless film must not clear, got %d", len(got))
	}
}

// A primary-title match must not pay for lookups it does not need. Adoption
// runs over hundreds of folders and this is one request each.
func TestPrimaryTitleMatchSkipsTheAltTitleLookup(t *testing.T) {
	looks := 0
	prov := altProvider{
		adoptProvider: adoptProvider{movies: []ports.SearchResult{movie(1, "Nosferatu", 2024)}},
		alts:          map[int64][]string{1: {"Nosferatu: A Symphony of Horror"}},
		nLooks:        &looks,
	}

	got := proposeWith(t, prov, domain.RootKindOf(domain.KindMovie), "Nosferatu (2024)")
	if got.Confidence != ConfidenceExact {
		t.Fatalf("want exact, got %s", got.Confidence)
	}
	if looks != 0 {
		t.Errorf("a title that already matched should trigger no lookups, got %d", looks)
	}
}

// The whole second pass is optional. A provider without the capability — and
// every test double written before it existed — must behave as it always did.
func TestAdoptionWorksWithoutTheAltTitleCapability(t *testing.T) {
	prov := adoptProvider{movies: []ports.SearchResult{movie(1, "Cunk on Life", 2024)}}
	got := proposeWith(t, prov, domain.RootKindOf(domain.KindMovie), "Cunk's Quest for Meaning (2024)")
	if got.Confidence == ConfidenceExact {
		t.Errorf("nothing matched by name, so nothing may be applied; got %s", got.Confidence)
	}
	// The row looks exactly as it did before the second pass existed: the
	// provider's suggestion, with no claim about why it is there.
	if len(got.Candidates) != 1 || len(got.Candidates[0].AltTitles) != 0 {
		t.Errorf("want a bare suggestion and no alternate names, got %+v", got.Candidates)
	}
}

// The original-language title rides along in every search response, so it
// costs nothing and should be honoured with no lookup at all.
func TestSearchSuppliedAlternateTitleIsHonoured(t *testing.T) {
	r := movie(1, "Godzilla Minus One", 2023)
	r.AltTitles = []string{"Gojira -1.0"}
	prov := adoptProvider{movies: []ports.SearchResult{r}}

	got := proposeWith(t, prov, domain.RootKindOf(domain.KindMovie), "Gojira -1.0 (2023)")
	if len(got.Candidates) == 0 || got.Candidates[0].TMDBID != 1 {
		t.Fatalf("the original-language title should match, got %+v", got.Candidates)
	}
	if got.Confidence == ConfidenceExact {
		t.Error("still an alternate-title match, so still one click")
	}
}

// The umbrella case, which the first version of alternate-title matching made
// worse rather than better: TMDB files the Cunk programmes under one series
// and lists every one of them among that series' alternate names, so a folder
// per programme produces a batch where every row leads with the same entry.
func TestUmbrellaTitleIsFlaggedAcrossTheBatch(t *testing.T) {
	looks := 0
	prov := altProvider{
		adoptProvider: adoptProvider{series: []ports.SearchResult{series(82712, "Cunk on...", 2018)}},
		alts: map[int64][]string{82712: {
			"Cunk on Britain", "Cunk on Earth", "Cunk on Christmas",
		}},
		nLooks: &looks,
	}

	svc, _, _ := newService(t)
	svc.meta = prov
	ctx := context.Background()
	root := t.TempDir()
	for _, name := range []string{"Cunk on Britain", "Cunk on Earth"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindSeries))
	if err != nil {
		t.Fatal(err)
	}
	props, err := svc.ProposeAdoptions(ctx, []UnmatchedDir{
		{RootFolderID: rf.ID, Path: filepath.Join(root, "Cunk on Britain"), Name: "Cunk on Britain"},
		{RootFolderID: rf.ID, Path: filepath.Join(root, "Cunk on Earth"), Name: "Cunk on Earth"},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range props {
		if len(p.SharedWith) != 1 {
			t.Errorf("%s should name the other folder it collides with, got %v", p.Name, p.SharedWith)
			continue
		}
		if p.SharedWith[0] == p.Path {
			t.Errorf("%s should not list itself", p.Name)
		}
		// Still offered — the provider's answer is information — but never
		// applied on its own.
		if p.Confidence == ConfidenceExact {
			t.Errorf("%s must not auto-adopt into a shared title", p.Name)
		}
	}
}

// Once one of them is adopted, the rest have to say who holds it. Without
// this the row shows a chip that looks correct and the conflict is only
// discoverable by clicking it.
func TestAnAlreadyHeldAlternateTitleNamesTheFolderThatHasIt(t *testing.T) {
	looks := 0
	prov := altProvider{
		adoptProvider: adoptProvider{series: []ports.SearchResult{series(82712, "Cunk on...", 2018)}},
		alts:          map[int64][]string{82712: {"Cunk on Britain", "Cunk on Earth"}},
		nLooks:        &looks,
	}

	svc, _, _ := newService(t)
	svc.meta = prov
	ctx := context.Background()
	root := t.TempDir()
	britain := filepath.Join(root, "Cunk on Britain")
	earth := filepath.Join(root, "Cunk on Earth")
	for _, d := range []string{britain, earth} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindSeries))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AdoptOne(ctx, britain, series(82712, "Cunk on...", 2018), false); err != nil {
		t.Fatal(err)
	}

	props, err := svc.ProposeAdoptions(ctx, []UnmatchedDir{
		{RootFolderID: rf.ID, Path: earth, Name: "Cunk on Earth"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if props[0].HeldBy != britain {
		t.Errorf("want the row to name %q as the holder, got %q", britain, props[0].HeldBy)
	}
}

// A primary-title match whose item points at a folder that is *not* there is
// the ordinary adoptable case — an item added by hand and never attached to
// files — and must not be flagged, or relinking becomes unreachable.
func TestAnItemWithNoRealFolderIsNotReportedAsHolding(t *testing.T) {
	looks := 0
	prov := altProvider{
		adoptProvider: adoptProvider{series: []ports.SearchResult{series(7, "Cunk on...", 2018)}},
		alts:          map[int64][]string{7: {"Cunk on Earth"}},
		nLooks:        &looks,
	}

	svc, _, _ := newService(t)
	svc.meta = prov
	ctx := context.Background()
	root := t.TempDir()
	earth := filepath.Join(root, "Cunk on Earth")
	if err := os.Mkdir(earth, 0o755); err != nil {
		t.Fatal(err)
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindSeries))
	if err != nil {
		t.Fatal(err)
	}
	// Added by title, pointed at a folder the naming rules invented.
	if _, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindSeries, TMDBID: 7, RootFolderID: rf.ID, Monitored: true, Monitor: "none",
	}); err != nil {
		t.Fatal(err)
	}

	props, err := svc.ProposeAdoptions(ctx, []UnmatchedDir{
		{RootFolderID: rf.ID, Path: earth, Name: "Cunk on Earth"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if props[0].HeldBy != "" {
		t.Errorf("an entry with no folder on disk is adoptable, not a conflict; got %q", props[0].HeldBy)
	}
}
