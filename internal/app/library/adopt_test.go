package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
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

// Accepting a match must happen in place: no trip to the Add page, no
// re-search, no losing your position in the queue.
func TestAdoptOneAcceptsAChosenCandidate(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{
		movie(1, "Nosferatu", 2024), movie(2, "Nosferatu", 1922),
	}}
	ctx := context.Background()

	root := t.TempDir()
	folder := filepath.Join(root, "Nosferatu (2024)")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunAdoption(ctx); err != nil {
		t.Fatal(err)
	}

	// The 1922 version, deliberately not the one adoption would have picked:
	// the user's choice has to win over the proposal's ranking.
	if err := svc.AdoptOne(ctx, folder, movie(2, "Nosferatu", 1922), false); err != nil {
		t.Fatal(err)
	}

	items, err := svc.List(ctx, domain.KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 adopted item, got %d", len(items))
	}
	if items[0].Path != folder {
		t.Errorf("item path = %q, want the existing folder %q", items[0].Path, folder)
	}
	// And it leaves the queue immediately, without waiting for a rescan.
	if got := svc.ReviewQueue(ctx, "", "", 25, 0).Total; got != 0 {
		t.Errorf("review queue still holds %d entries after adopting", got)
	}
}

// A path outside every registered root is refused: this endpoint writes a
// library item pointed at a directory, so an arbitrary path would let a
// session aim the library anywhere on the host.
func TestAdoptOneRefusesArbitrarySystemPaths(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(1, "Anything", 2020)}}
	err := svc.AdoptOne(context.Background(), "/etc", movie(1, "Anything", 2020), false)
	if err == nil {
		t.Fatal("want a refusal for a path outside every root folder")
	}
}

// The first-pass guard has to be releasable from the screen where reviewing
// happens, or it is a dead end rather than a safety rail.
func TestAdoptExactAppliesConfidentMatchesInUnconfirmedRoots(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{
		movie(550, "Fight Club", 1999),
		movie(1, "Arrival", 2016),
		movie(2, "Ambiguous", 2020),
		movie(3, "Ambiguous", 2020),
	}}
	ctx := context.Background()

	root := t.TempDir()
	for _, n := range []string{"Fight Club (1999)", "Arrival (2016)", "Ambiguous (2020)"} {
		if err := os.Mkdir(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	// First pass over an unconfirmed root: everything is held for review.
	first, err := svc.RunAdoption(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Adopted) != 0 {
		t.Fatalf("first pass should write nothing, adopted %d", len(first.Adopted))
	}

	res, err := svc.AdoptExact(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Adopted) != 2 {
		t.Fatalf("adopted %d, want the 2 confident ones: %+v", len(res.Adopted), res.Failures)
	}
	// The genuinely ambiguous one stays put.
	page := svc.ReviewQueue(ctx, "", "", 25, 0)
	if page.Total != 1 || page.Items[0].Name != "Ambiguous (2020)" {
		t.Fatalf("queue should hold only the ambiguous folder, got %+v", page.Items)
	}
}

// A folder whose match is already in the library is the interesting case,
// not an error: it usually means an item added by hand has never been
// pointed at the files it describes. This was a dead end — "item already in
// library", with no way to ever accept the folder.
func TestAdoptOneRelinksAnItemThatHasNoFolder(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(550, "Fight Club", 1999)}}
	ctx := context.Background()

	// Added by hand, with no root folder: exactly what a pre-adoption
	// library looks like.
	added, err := svc.Add(ctx, AddRequest{Kind: domain.KindMovie, TMDBID: 550, Monitored: true})
	if err != nil {
		t.Fatal(err)
	}
	if added.Path != "" {
		t.Fatalf("precondition: expected no path, got %q", added.Path)
	}

	root := t.TempDir()
	folder := filepath.Join(root, "Fight Club (1999)")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunAdoption(ctx); err != nil {
		t.Fatal(err)
	}

	if err := svc.AdoptOne(ctx, folder, movie(550, "Fight Club", 1999), false); err != nil {
		t.Fatalf("adopting a folder for an item that has no folder should relink it: %v", err)
	}

	got, err := svc.Get(ctx, added.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != folder {
		t.Fatalf("item path = %q, want it pointed at %q", got.Path, folder)
	}
	// And exactly one item — relinking must not create a duplicate.
	items, _ := svc.List(ctx, domain.KindMovie)
	if len(items) != 1 {
		t.Fatalf("want 1 item after relink, got %d", len(items))
	}
}

// But a genuine conflict — two folders claiming one title, both on disk —
// must not silently move the pointer and detach the other folder's files.
func TestAdoptOneRefusesToStealAFolderThatExists(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(550, "Fight Club", 1999)}}
	ctx := context.Background()

	root := t.TempDir()
	first := filepath.Join(root, "Fight Club (1999)")
	second := filepath.Join(root, "Fight Club (1999) [remux]")
	for _, d := range []string{first, second} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	// The item already lives in the first folder, which exists.
	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunAdoption(ctx); err != nil {
		t.Fatal(err)
	}

	err = svc.AdoptOne(ctx, second, movie(550, "Fight Club", 1999), false)
	if err == nil {
		t.Fatal("want a refusal: the title already has a folder that exists")
	}
	if !errors.Is(err, ErrFolderConflict) {
		t.Errorf("want ErrFolderConflict so the UI can offer a resolution, got %v", err)
	}
	// Both paths must appear: the difference is often one character (a
	// hyphen against an en dash) and prose cannot show that.
	if !strings.Contains(err.Error(), first) || !strings.Contains(err.Error(), second) {
		t.Errorf("the error must name both folders (%q and %q), got %v", first, second, err)
	}
	// The original placement is untouched.
	got, _ := svc.Get(ctx, item.ID)
	if got.Path != first {
		t.Errorf("item path moved to %q; it should still be %q", got.Path, first)
	}

	// ...until the user says so explicitly, which is the resolution the UI
	// offers on that error.
	if err := svc.AdoptOne(ctx, second, movie(550, "Fight Club", 1999), true); err != nil {
		t.Fatalf("an explicit force should move the entry: %v", err)
	}
	moved, _ := svc.Get(ctx, item.ID)
	if moved.Path != second {
		t.Errorf("after force the item should sit at %q, got %q", second, moved.Path)
	}
	// Still one item — forcing must not duplicate.
	if items, _ := svc.List(ctx, domain.KindMovie); len(items) != 1 {
		t.Errorf("want 1 item after a forced move, got %d", len(items))
	}
}

// The reported failure: a click on a row the user can plainly see, rejected
// as "not a folder awaiting review". The queue is a snapshot that a rescan or
// an earlier adoption rewrites underneath an open window, so membership in it
// was never the right gate.
func TestAdoptOneWorksWhenTheQueueSnapshotIsStale(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{series: []ports.SearchResult{
		series(10, "The Rehearsal", 2022), series(11, "The Rehearsal", 2020),
	}}
	ctx := context.Background()

	root := t.TempDir()
	folder := filepath.Join(root, "The Rehearsal")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindSeries)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunAdoption(ctx); err != nil {
		t.Fatal(err)
	}

	// Simulate the window going stale: the persisted queue is emptied while
	// the browser still shows the row.
	svc.saveReviewQueue(ctx, nil)

	if err := svc.AdoptOne(ctx, folder, series(10, "The Rehearsal", 2022), false); err != nil {
		t.Fatalf("a stale window must not block a legitimate click: %v", err)
	}
	items, _ := svc.List(ctx, domain.KindSeries)
	if len(items) != 1 || items[0].Path != folder {
		t.Fatalf("want the folder adopted, got %+v", items)
	}
}

// The security property has to survive that relaxation: only a direct child
// of a registered root is adoptable, because that is all a scan ever offers.
func TestAdoptOneRefusesPathsOutsideRoots(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(1, "Anything", 2020)}}
	ctx := context.Background()

	root := t.TempDir()
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}
	// A real directory, outside every root.
	outside := t.TempDir()
	if err := svc.AdoptOne(ctx, outside, movie(1, "Anything", 2020), false); err == nil {
		t.Error("a directory outside every root must be refused")
	}
	// Nested deeper than a scan would ever offer.
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.AdoptOne(ctx, deep, movie(1, "Anything", 2020), false); err == nil {
		t.Error("a grandchild of a root must be refused — scan only offers direct children")
	}
	// A path that does not exist at all.
	if err := svc.AdoptOne(ctx, filepath.Join(root, "ghost"), movie(1, "Anything", 2020), false); err == nil {
		t.Error("a path that is not on disk must be refused")
	}
}

// A typed root still refuses the wrong kind, even by hand.
func TestAdoptOneRefusesWrongKindForTheRoot(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(1, "Thing", 2020)}}
	ctx := context.Background()

	root := t.TempDir()
	folder := filepath.Join(root, "Thing (2020)")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindSeries)); err != nil {
		t.Fatal(err)
	}
	err := svc.AdoptOne(ctx, folder, movie(1, "Thing", 2020), false)
	if !errors.Is(err, ErrRootKindMismatch) {
		t.Fatalf("want ErrRootKindMismatch putting a movie in a series root, got %v", err)
	}
}

// Adopting must update both stores of "what is outstanding". Pruning only
// the review queue left the scan report still advertising the folder, so the
// settings panel counted work that was done — and the queue's fallback, which
// derives from that report, could resurrect an adopted folder.
func TestAdoptingPrunesBothTheQueueAndTheReport(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{
		movie(550, "Fight Club", 1999), movie(1, "Arrival", 2016),
	}}
	ctx := context.Background()

	root := t.TempDir()
	for _, n := range []string{"Fight Club (1999)", "Arrival (2016)"} {
		if err := os.Mkdir(filepath.Join(root, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunAdoption(ctx); err != nil {
		t.Fatal(err)
	}

	before, _, _ := svc.LastScanReport(ctx)
	if len(before.UnmatchedDirs) != 2 {
		t.Fatalf("precondition: report should list 2 unmatched, got %d", len(before.UnmatchedDirs))
	}

	folder := filepath.Join(root, "Fight Club (1999)")
	if err := svc.AdoptOne(ctx, folder, movie(550, "Fight Club", 1999), false); err != nil {
		t.Fatal(err)
	}

	after, ok, err := svc.LastScanReport(ctx)
	if err != nil || !ok {
		t.Fatalf("report: ok=%v err=%v", ok, err)
	}
	for _, d := range after.UnmatchedDirs {
		if d.Path == folder {
			t.Error("an adopted folder must not still be listed as unmatched")
		}
	}
	if after.UnmatchedTotal != 1 {
		t.Errorf("unmatchedTotal = %d, want 1 — the panel's count comes from this", after.UnmatchedTotal)
	}
	if got := svc.ReviewQueue(ctx, "", "", 25, 0).Total; got != 1 {
		t.Errorf("review queue total = %d, want 1", got)
	}
}

// Accept-all prunes both stores for everything it took.
func TestAcceptAllPrunesTheReport(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(550, "Fight Club", 1999)}}
	ctx := context.Background()

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "Fight Club (1999)"), 0o755); err != nil {
		t.Fatal(err)
	}
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ConfirmRootAdopted(ctx, rf.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RunAdoption(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AdoptExact(ctx); err != nil {
		t.Fatal(err)
	}
	report, ok, _ := svc.LastScanReport(ctx)
	if ok && len(report.UnmatchedDirs) != 0 {
		t.Errorf("report still lists %d unmatched after accept-all: %+v",
			len(report.UnmatchedDirs), report.UnmatchedDirs)
	}
}

// The missing-folder list is a claim about the filesystem, and it goes stale
// the moment the user acts on it. A list that keeps naming items you have
// already dealt with is worse than no list.
func TestMissingListSelfHeals(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{
		movie(550, "Fight Club", 1999), movie(1, "Arrival", 2016),
	}}
	ctx := context.Background()

	root := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	// Two items added by title with a root: their folders are derived from
	// the naming rules and do not exist. This is what the Add page produces,
	// and it is where the reported phantoms came from.
	gone, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixable, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 1, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := svc.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.MissingItems) != 2 {
		t.Fatalf("precondition: want 2 missing, got %d", len(report.MissingItems))
	}

	// Resolve one by removing the entry, the other by pointing it somewhere
	// real — the two things a user can actually do.
	if err := svc.Delete(ctx, gone.ID); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "Arrival (2016)")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateItem(ctx, fixable.ID, UpdateRequest{Path: &real}); err != nil {
		t.Fatal(err)
	}

	// Without a rescan, the report must already agree.
	after, ok, err := svc.LastScanReport(ctx)
	if err != nil || !ok {
		t.Fatalf("report: ok=%v err=%v", ok, err)
	}
	if len(after.MissingItems) != 0 {
		t.Fatalf("missing list should be empty, still holds %+v", after.MissingItems)
	}
	if len(after.MissingPaths) != 0 {
		t.Errorf("missingPaths should agree with missingItems, got %+v", after.MissingPaths)
	}
}

// A moved item is reported at its current path, not the one recorded when the
// scan ran.
func TestMissingListReportsTheCurrentPath(t *testing.T) {
	svc, _, _ := newService(t)
	svc.meta = adoptProvider{movies: []ports.SearchResult{movie(550, "Fight Club", 1999)}}
	ctx := context.Background()

	root := t.TempDir()
	rf, err := svc.AddRootFolder(ctx, root, domain.RootKindOf(domain.KindMovie))
	if err != nil {
		t.Fatal(err)
	}
	item, err := svc.Add(ctx, AddRequest{
		Kind: domain.KindMovie, TMDBID: 550, RootFolderID: rf.ID, Monitored: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	// Point it at a different folder that also does not exist.
	moved := filepath.Join(root, "somewhere else")
	if _, err := svc.UpdateItem(ctx, item.ID, UpdateRequest{Path: &moved}); err != nil {
		t.Fatal(err)
	}
	after, _, err := svc.LastScanReport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.MissingItems) != 1 || after.MissingItems[0].Path != moved {
		t.Fatalf("want the current path %q reported, got %+v", moved, after.MissingItems)
	}
}
