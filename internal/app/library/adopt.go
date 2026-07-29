package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/matcher"
	"github.com/pjunod/monarr/internal/domain/parser"
	"github.com/pjunod/monarr/internal/ports"
)

// Confidence is how sure adoption is about a proposed match.
type Confidence string

// The three confidence levels. There is deliberately no "probably": a
// proposal is either safe to apply without a human, or it is not.
const (
	// ConfidenceExact clears the per-kind bar in §2 of ADR 0010 and may be
	// adopted without asking.
	ConfidenceExact Confidence = "exact"
	// ConfidenceAmbiguous means there were plausible matches but more than
	// one, or the year disagreed. A human picks.
	ConfidenceAmbiguous Confidence = "ambiguous"
	// ConfidenceNone means the provider returned nothing usable.
	ConfidenceNone Confidence = "none"
)

// Proposal is a candidate directory with what adoption thinks it is.
//
// The value of this type over a bare folder name: even when nothing is
// auto-adopted, review becomes "confirm a pre-filled answer" instead of
// "search for each of 600 titles". Those are different orders of work.
type Proposal struct {
	RootFolderID int64                `json:"rootFolderId"`
	Path         string               `json:"path"`
	Name         string               `json:"name"`
	ParsedTitle  string               `json:"parsedTitle"`
	ParsedYear   int                  `json:"parsedYear"`
	Kind         domain.MediaKind     `json:"kind,omitempty"` // "" in a mixed root
	Confidence   Confidence           `json:"confidence"`
	Candidates   []ports.SearchResult `json:"candidates"`
	// HeldBy is the folder that already holds the leading candidate, set only
	// when that candidate answers to this folder through an alternate title.
	//
	// It is the umbrella case: a provider that files a whole franchise under
	// one title lists every entry in it as an alternate name, so several
	// folders match one entry and only the first can have it. Saying so on the
	// row is the whole point — the alternative is the user clicking a chip
	// that looks correct and reading a conflict error afterwards.
	HeldBy string `json:"heldBy,omitempty"`
	// SharedWith names the other folders in this review batch whose leading
	// candidate is the same entry, by the same alternate-title route. Two
	// folders answering to one title is the signal that the title covers both
	// rather than being either one's answer.
	SharedWith []string `json:"sharedWith,omitempty"`
	// Force re-points an existing library entry at this folder even when it
	// already points at a directory that exists. Only ever set by an
	// explicit single-folder request — bulk adoption must never take a
	// folder away from another entry on its own initiative.
	Force bool `json:"-"`
}

// maxProposalCandidates is how many options a review row carries. Three is
// enough to choose from and few enough to read; the ranking beyond that is
// not trustworthy anyway.
const maxProposalCandidates = 3

// ProposeAdoptions turns unmatched directories into proposals: parse the
// folder name, search the provider for the root's kind, and score what comes
// back (ADR 0010 §1).
//
// A mixed root never proposes a single answer, because resolving "which
// provider" is exactly the ambiguity the auto-adopt bar refuses to guess
// through (§3). That is deliberate: it makes typing a root pay off visibly
// rather than being a setting the user is nagged about.
func (s *Service) ProposeAdoptions(ctx context.Context, dirs []UnmatchedDir) ([]Proposal, error) {
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return nil, err
	}
	kindOf := map[int64]domain.RootKind{}
	for _, rf := range roots {
		kindOf[rf.ID] = rf.Kind
	}

	out := make([]Proposal, 0, len(dirs))
	for _, d := range dirs {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		out = append(out, s.propose(ctx, d, kindOf[d.RootFolderID]))
	}
	markAltCollisions(out)
	return out, nil
}

// markAltCollisions records, on each proposal, the other folders in this batch
// that lead with the same alternate-title match.
//
// One entry answering to several folders is how a provider says "these are all
// one title" — TMDB files the Cunk programmes under a single series and lists
// every one of them among its alternate names, so a folder per programme
// produces a batch where three rows all point at one entry. Adopting the first
// is fine; the rest then collide with it, and without this the collision is
// only discoverable by clicking and reading the error.
func markAltCollisions(props []Proposal) {
	byEntry := map[string][]int{}
	for i, p := range props {
		if key, ok := altLead(p); ok {
			byEntry[key] = append(byEntry[key], i)
		}
	}
	for _, idx := range byEntry {
		if len(idx) < 2 {
			continue
		}
		for _, i := range idx {
			for _, j := range idx {
				if i != j {
					props[i].SharedWith = append(props[i].SharedWith, props[j].Path)
				}
			}
		}
	}
}

// altLead reports the identity of the leading candidate when the folder answers
// to it only through an alternate title.
func altLead(p Proposal) (string, bool) {
	if len(p.Candidates) == 0 {
		return "", false
	}
	lead := p.Candidates[0]
	if len(lead.AltTitles) == 0 ||
		matcher.NormalizeTitle(lead.Title) == matcher.NormalizeTitle(p.ParsedTitle) {
		return "", false
	}
	return identity(lead), true
}

// propose builds one proposal.
func (s *Service) propose(ctx context.Context, d UnmatchedDir, rootKind domain.RootKind) Proposal {
	parsed := parser.Parse(d.Name)
	p := Proposal{
		RootFolderID: d.RootFolderID,
		Path:         d.Path,
		Name:         d.Name,
		ParsedTitle:  parsed.Title,
		ParsedYear:   parsed.Year,
		Confidence:   ConfidenceNone,
		Candidates:   []ports.SearchResult{},
	}
	if p.ParsedTitle == "" {
		p.ParsedTitle = d.Name
	}

	kind, typed := rootKind.Media()
	if !typed {
		// Mixed root: offer across kinds and always ask.
		var mixed []ports.SearchResult
		for _, k := range []domain.MediaKind{domain.KindMovie, domain.KindSeries} {
			mixed = append(mixed, s.searchTop(ctx, k, p.ParsedTitle)...)
		}
		p.Candidates = trim(dedupeResults(mixed), maxProposalCandidates)
		if len(p.Candidates) > 0 {
			p.Confidence = ConfidenceAmbiguous
		}
		return p
	}

	p.Kind = kind
	results := dedupeResults(s.searchTop(ctx, kind, p.ParsedTitle))

	switch won := clearing(kind, parsed, results); {
	case len(won) == 1:
		// Put the winner first so the caller never has to re-derive it, and
		// so the UI's first chip is the one adoption would have picked.
		results = append(won, filterOut(results, won[0])...)
	case len(won) == 0:
		// Nothing matched on its primary title, so there are two fallbacks
		// and the order between them is the whole point.
		//
		// The chain goes first (ADR 0011 §1). A standalone record from
		// another provider beats an alternate name on the first provider's
		// umbrella entry, and the umbrella case produces both: TMDB lists
		// "Cunk on Earth" among the alternate titles of "Cunk on…", so
		// trying alternates first settles for the entry that *cannot hold
		// the folder* — and settles confidently enough to stop looking.
		// It is also the cheaper probe: one search against one provider,
		// versus one request per candidate for alternate titles.
		//
		// The trigger is "nothing clears", not "no results". TMDB answers
		// these folders with something; a chain keyed on emptiness would
		// never engage.
		if kind == domain.KindSeries {
			if chained := s.chainProposals(ctx, parsed, results); len(chained) > 0 {
				results = chained
				break
			}
		}
		// No other provider has it either. Ask what else the first
		// provider's candidates are called and rank any that answer to the
		// folder's name — a work released under two names is ordinary, and
		// the one the user wants is often at position nine of a list that
		// gets trimmed to three.
		results = s.withAltTitles(ctx, kind, results)
		if alt := clearingAlt(kind, parsed, results); len(alt) > 0 {
			results = append(alt, filterOutAll(results, alt)...)
		}
	}

	// Graded on the final set, not the first one: a match found through the
	// chain clears the same bar any other match does, and grading before the
	// chain ran would file it as ambiguous for no reason. Alternate-title
	// matches are unaffected — clearing() compares primary titles, so they
	// stay ambiguous by construction (ADR 0010 §2).
	p.Confidence = grade(kind, parsed, results)

	p.Candidates = trim(results, maxProposalCandidates)
	// Keep only the alternate names that are why a candidate is here, so a
	// row can say *why* "Cunk on Life" is offered for a folder called
	// something else, without shipping TMDB's forty regional retitles.
	for i := range p.Candidates {
		p.Candidates[i].AltTitles = matchingAlts(parsed, p.Candidates[i])
	}
	if _, isAlt := altLead(p); isAlt {
		p.HeldBy = s.folderHolding(ctx, p.Candidates[0], p.Path)
	}
	return p
}

// folderHolding returns the existing folder of a library item matching this
// candidate, when there is one, it is not `self`, and it exists on disk.
//
// Only the last condition makes this worth checking: an item pointed at a
// directory that is not there is the ordinary adoptable case (added by hand,
// never attached to files), and relinking it is exactly right. An item pointed
// at a folder that *does* exist is a different situation, and one the user
// should hear about before clicking rather than after.
func (s *Service) folderHolding(ctx context.Context, c ports.SearchResult, self string) string {
	var id int64
	var err error
	switch {
	case c.OLID != "":
		id, err = s.db.GetMediaItemByKindOlid(ctx, c.Kind, c.OLID)
	case c.TMDBID != 0:
		id, err = s.db.GetMediaItemByKindTmdb(ctx, c.Kind, c.TMDBID)
	case c.TVDBID != 0:
		id, err = s.db.GetMediaItemByKindTvdb(ctx, c.Kind, c.TVDBID)
	default:
		return ""
	}
	if err != nil {
		return ""
	}
	item, err := s.Get(ctx, id)
	if err != nil || item.Path == "" || item.Path == self {
		return ""
	}
	if _, statErr := os.Stat(item.Path); statErr != nil {
		return ""
	}
	return item.Path
}

// chainProposals asks the rest of the series chain about a folder the first
// link could not place, and returns a result set with anything that clears
// the bar ranked first.
//
// Returns nil when no link produces a clearing match, so the caller keeps
// the original candidates: a provider that merely has *different* wrong
// answers should not displace the ones already on screen.
func (s *Service) chainProposals(
	ctx context.Context, parsed parser.Parsed, existing []ports.SearchResult,
) []ports.SearchResult {
	for _, p := range s.series {
		if ctx.Err() != nil {
			return nil
		}
		res, err := p.SearchSeries(ctx, parsed.Title)
		if err != nil {
			s.log.Debug("adopt: series provider search failed",
				"provider", p.Name(), "query", parsed.Title, "err", err)
			continue
		}
		res = dedupeResults(res)
		won := clearing(domain.KindSeries, parsed, res)
		if len(won) == 0 {
			continue
		}
		s.log.Info("adopt: matched through the series chain",
			"provider", p.Name(), "folder", parsed.Title,
			"title", won[0].Title, "tvdb", won[0].TVDBID)
		// Winner first, then this provider's runners-up, then whatever the
		// earlier link offered — which is still worth showing, because the
		// user may know better than either.
		rest := filterOutAll(res, won)
		return append(append(won, rest...), existing...)
	}
	return nil
}

// maxAltTitleLookups bounds the second pass. Adoption runs over hundreds of
// folders and this costs one request per candidate, so it buys back the top
// few results — where a retitled work realistically lands — and no more.
const maxAltTitleLookups = 5

// withAltTitles fills in AltTitles for the leading candidates.
//
// The provider capability is optional (ports.AltTitleProvider): without it,
// or when a lookup fails, the candidates come back exactly as they went in
// and the folder lands in review. That is the pre-existing behaviour, which
// is the right thing for an enrichment step to degrade to.
func (s *Service) withAltTitles(ctx context.Context, kind domain.MediaKind, in []ports.SearchResult) []ports.SearchResult {
	prov, ok := s.meta.(ports.AltTitleProvider)
	if !ok || len(in) == 0 {
		return in
	}
	out := make([]ports.SearchResult, len(in))
	copy(out, in)
	for i := range out {
		if i >= maxAltTitleLookups || ctx.Err() != nil {
			break
		}
		if out[i].TMDBID == 0 {
			continue
		}
		alts, err := prov.AlternativeTitles(ctx, kind, out[i].TMDBID)
		if err != nil {
			s.log.Debug("adopt: alternative titles unavailable",
				"kind", kind, "tmdb", out[i].TMDBID, "err", err)
			continue
		}
		out[i].AltTitles = append(append([]string{}, out[i].AltTitles...), alts...)
	}
	return out
}

// matchingAlts returns the alternate names of r that the folder was actually
// named after.
func matchingAlts(parsed parser.Parsed, r ports.SearchResult) []string {
	want := matcher.NormalizeTitle(parsed.Title)
	if want == "" {
		return nil
	}
	var out []string
	for _, alt := range r.AltTitles {
		if matcher.NormalizeTitle(alt) == want {
			out = append(out, alt)
		}
	}
	return out
}

// filterOut returns everything except one specific result.
func filterOut(in []ports.SearchResult, drop ports.SearchResult) []ports.SearchResult {
	return filterOutAll(in, []ports.SearchResult{drop})
}

// filterOutAll returns everything not in drop.
func filterOutAll(in, drop []ports.SearchResult) []ports.SearchResult {
	gone := make(map[string]bool, len(drop))
	for _, d := range drop {
		gone[identity(d)] = true
	}
	out := make([]ports.SearchResult, 0, len(in))
	for _, r := range in {
		if gone[identity(r)] {
			continue
		}
		out = append(out, r)
	}
	return out
}

func identity(r ports.SearchResult) string {
	// Every id space, not just TMDB's: a chain result carries a TVDB id and
	// zeros elsewhere, so a key without it collapses every one of them onto
	// each other (which reads as "the provider returned one result").
	return fmt.Sprintf("%s|%d|%d|%s", r.Kind, r.TMDBID, r.TVDBID, r.OLID)
}

func (s *Service) searchTop(ctx context.Context, kind domain.MediaKind, query string) []ports.SearchResult {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	// The first link only. The rest of the chain is reached from propose,
	// and only for a folder nothing here could place — see chainProposals.
	res, err := s.searchPrimary(ctx, kind, query)
	if err != nil {
		s.log.Debug("adopt: provider search failed", "kind", kind, "query", query, "err", err)
		return nil
	}
	return res
}

// dedupeResults removes candidates that are the same title twice.
//
// Providers really do return duplicates — a TMDB search for "Daniel Sloss
// Can't" comes back with the same film twice — and because the bar is
// "exactly one result clears", a duplicate turns a perfect match into an
// ambiguous one. That failure is invisible from the outside: the user sees
// two identical chips and no explanation for why nothing was adopted.
//
// Identity is the provider id where there is one, and normalized title plus
// year otherwise, so two rows for the same film collapse even when their ids
// differ.
func dedupeResults(in []ports.SearchResult) []ports.SearchResult {
	seen := map[string]bool{}
	out := make([]ports.SearchResult, 0, len(in))
	for _, r := range in {
		key := identity(r)
		if r.TMDBID == 0 && r.TVDBID == 0 && r.OLID == "" {
			key = fmt.Sprintf("%s|%s|%d", r.Kind, matcher.NormalizeTitle(r.Title), r.Year)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// grade applies the per-kind bar. The asymmetry is the point: a wrong
// auto-match writes a path assignment and metadata that look right until
// someone notices the poster, while an unmatched folder is merely visible
// and annoying. So the bar is set where false positives are close to
// impossible, and more folders land in review as a result (ADR 0010 §2).
func grade(kind domain.MediaKind, parsed parser.Parsed, results []ports.SearchResult) Confidence {
	if len(results) == 0 {
		return ConfidenceNone
	}
	if len(clearing(kind, parsed, results)) == 1 {
		return ConfidenceExact
	}
	return ConfidenceAmbiguous
}

// clearing returns every candidate that meets the bar, with one refinement
// that matters a great deal in practice: **an exact year beats a tolerated
// one.**
//
// The ±1 tolerance exists for release-date drift, where a folder says 2024
// and the provider says 2025 for the same film. But applied blindly it makes
// Nosferatu (2024) ambiguous against Nosferatu (2025) — two different films,
// one of which matches the folder exactly. So when any candidate agrees on
// the year outright, only those candidates are eligible; the tolerance is a
// fallback for when nothing matches exactly, not a widening of the target.
func clearing(kind domain.MediaKind, parsed parser.Parsed, results []ports.SearchResult) []ports.SearchResult {
	return clearingWith(kind, parsed, results, primaryTitle)
}

// clearingAlt returns candidates that clear every other part of the bar but
// answer to the folder's name only through an *alternate* title.
//
// Deliberately not fed into grade: an alternate-title match is good enough to
// offer as the first chip and not good enough to write without asking. TMDB's
// alternate titles are crowd-maintained and include working titles, dubbed
// retitles and outright noise, so a yearless series folder could clear on a
// coincidence. Ranking it first turns a "no match —" row into one click; the
// click is the part that stays (ADR 0010 §2).
func clearingAlt(kind domain.MediaKind, parsed parser.Parsed, results []ports.SearchResult) []ports.SearchResult {
	return clearingWith(kind, parsed, results, alternateTitle)
}

// titleRule says which of a candidate's names may answer to the folder.
type titleRule func(parser.Parsed, ports.SearchResult) bool

func primaryTitle(parsed parser.Parsed, r ports.SearchResult) bool {
	return matcher.NormalizeTitle(parsed.Title) == matcher.NormalizeTitle(r.Title)
}

func alternateTitle(parsed parser.Parsed, r ports.SearchResult) bool {
	return !primaryTitle(parsed, r) && len(matchingAlts(parsed, r)) > 0
}

func clearingWith(
	kind domain.MediaKind, parsed parser.Parsed, results []ports.SearchResult, titleOK titleRule,
) []ports.SearchResult {
	var exactYear, tolerated []ports.SearchResult
	for _, r := range results {
		if !titleOK(parsed, r) || !clearsRest(kind, parsed, r) {
			continue
		}
		if parsed.Year != 0 && r.Year == parsed.Year {
			exactYear = append(exactYear, r)
			continue
		}
		tolerated = append(tolerated, r)
	}
	if len(exactYear) > 0 {
		return exactYear
	}
	return tolerated
}

// clearsRest reports whether one result meets everything the bar asks for
// besides the title, which its caller has already decided.
func clearsRest(kind domain.MediaKind, parsed parser.Parsed, r ports.SearchResult) bool {
	switch kind {
	case domain.KindMovie:
		// Movies need a year on both sides. Movie folders almost always
		// carry one, and without it remakes and same-title films are a
		// coin flip — Dune (1984) and Dune (2021) are the everyday case.
		if parsed.Year == 0 || r.Year == 0 {
			return false
		}
		d := parsed.Year - r.Year
		return d >= -1 && d <= 1
	case domain.KindSeries:
		// Series may match on a unique title alone: TV folders routinely
		// have no year (Severance, Andor), so requiring one would send
		// every well-formed TV library to manual review. When both sides
		// do know a year, a disagreement still disqualifies.
		if parsed.Year != 0 && r.Year != 0 {
			d := parsed.Year - r.Year
			return d >= -1 && d <= 1
		}
		return true
	case domain.KindBook:
		// Books need an author: title collisions are endemic, and the
		// parser already extracts one when the name carries it.
		if parsed.Author == "" || r.Author == "" {
			return false
		}
		return matcher.NormalizeTitle(parsed.Author) == matcher.NormalizeTitle(r.Author)
	}
	return false
}

// trim copies rather than reslices. The copy is not incidental: propose
// reorders the result to put the winner first, and a provider that returns
// a cached or shared slice would otherwise have its own data permutated
// under it — one folder's adoption silently rewriting another's candidates.
func trim[T any](in []T, n int) []T {
	if n > len(in) {
		n = len(in)
	}
	out := make([]T, n)
	copy(out, in[:n])
	return out
}

// AdoptResult reports what an adoption run did.
type AdoptResult struct {
	Adopted  []Proposal `json:"adopted"`
	Review   []Proposal `json:"review"`
	Failures []string   `json:"failures"`
}

// Adopt applies every proposal that clears the bar and returns the rest for
// review. dryRun proposes without writing, which is what the first adoption
// of a root uses: hundreds of candidates at once is the moment a user is
// least able to spot a wrong match and most likely to lose trust over one
// (ADR 0010 §5).
func (s *Service) Adopt(ctx context.Context, proposals []Proposal, dryRun bool) AdoptResult {
	res := AdoptResult{Adopted: []Proposal{}, Review: []Proposal{}, Failures: []string{}}
	for _, p := range proposals {
		if p.Confidence != ConfidenceExact || len(p.Candidates) == 0 {
			res.Review = append(res.Review, p)
			continue
		}
		if dryRun {
			res.Adopted = append(res.Adopted, p)
			continue
		}
		if err := s.adoptOne(ctx, p); err != nil {
			if errors.Is(err, ErrAlreadyExists) {
				// The title is already in the library under another
				// folder. Not a failure, but not something to guess
				// about either — a human decides which folder wins.
				res.Review = append(res.Review, p)
				continue
			}
			res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", p.Path, err))
			continue
		}
		res.Adopted = append(res.Adopted, p)
	}
	return res
}

// adoptOne adds the winning candidate and points it at the folder that is
// already on disk, so nothing is moved or renamed — the promise ADR 0005
// made about adopting existing layouts.
func (s *Service) adoptOne(ctx context.Context, p Proposal) error {
	win := p.Candidates[0]

	// One folder, one item. Without this, a folder can be adopted twice under
	// two identities that share no id — a TMDB record with no tvdb_id and a
	// TVmaze record keyed on TVDB have nothing to collide on, so the
	// per-id-space duplicate check (ADR 0011 §4) passes both. What lands is
	// two cards for one show, of which at most one can hold the folder; the
	// other keeps the invented naming-rule path and is linked to nothing.
	//
	// The folder is the thing that cannot be shared, so it is the thing to
	// check. Whoever holds it already IS the answer for this folder.
	if holder, ok := s.itemHolding(ctx, p.Path); ok {
		s.log.Info("adopt: folder already held, not adding a second entry",
			"path", p.Path, "held by", holder.Title, "id", holder.ID)
		return nil
	}

	req := AddRequest{
		Kind:         win.Kind,
		TMDBID:       win.TMDBID,
		TVDBID:       win.TVDBID,
		OLID:         win.OLID,
		RootFolderID: p.RootFolderID,
		Monitored:    true,
		Monitor:      "none", // adopting is not a request to go download things
	}
	item, err := s.Add(ctx, req)
	if errors.Is(err, ErrAlreadyExists) {
		// The title is already in the library. That is not a dead end — it
		// is usually the *interesting* case: an item added by hand, or by an
		// earlier import, that has never been pointed at the files it
		// describes. Adopting the folder is exactly how you say "this
		// folder is that item".
		return s.relinkExisting(ctx, p, win, p.Force)
	}
	if err != nil {
		return err
	}
	// Add derives a folder from the naming rules; adoption must use the
	// folder that exists instead, or the item points at a path with no
	// files in it.
	path := p.Path
	if _, err := s.UpdateItem(ctx, item.ID, UpdateRequest{Path: &path}); err != nil {
		return fmt.Errorf("point %d at %s: %w", item.ID, p.Path, err)
	}
	return nil
}

// itemHolding returns the library item already pointed at path, if any.
func (s *Service) itemHolding(ctx context.Context, path string) (domain.MediaItem, bool) {
	if path == "" {
		return domain.MediaItem{}, false
	}
	items, err := s.db.ListMediaItems(ctx, "")
	if err != nil {
		s.log.Warn("adopt: could not check whether the folder is held", "err", err)
		return domain.MediaItem{}, false
	}
	for _, it := range items {
		if it.Path == path {
			return it, true
		}
	}
	return domain.MediaItem{}, false
}

// adoptionStateKey records which roots have been adopted at least once.
const adoptionStateKey = "adopted_roots"

// RunAdoption is the queued entry point: propose matches for everything the
// last scan left unmatched, then apply the ones that clear the bar.
//
// First adoption of a root is a dry run — proposals are persisted for review
// and nothing is written — while a root that has been adopted before applies
// automatically. These are genuinely different situations (ADR 0010 §5): a
// first pass is hundreds of candidates at the moment the user is least able
// to spot a wrong match, and an incremental pass is one new folder that is
// reviewable in the activity feed.
func (s *Service) RunAdoption(ctx context.Context) (AdoptResult, error) {
	report, ok, err := s.LastScanReport(ctx)
	if err != nil {
		return AdoptResult{}, err
	}
	if !ok || len(report.UnmatchedDirs) == 0 {
		return AdoptResult{Adopted: []Proposal{}, Review: []Proposal{}, Failures: []string{}}, nil
	}

	proposals, err := s.ProposeAdoptions(ctx, report.UnmatchedDirs)
	if err != nil {
		return AdoptResult{}, err
	}

	adoptedRoots := s.adoptedRoots(ctx)
	var auto, first []Proposal
	firstRoots := map[int64]bool{}
	for _, p := range proposals {
		if adoptedRoots[p.RootFolderID] {
			auto = append(auto, p)
			continue
		}
		firstRoots[p.RootFolderID] = true
		first = append(first, p)
	}

	res := s.Adopt(ctx, auto, false)
	preview := s.Adopt(ctx, first, true)
	// A first pass proposes but does not write, so its would-be adoptions
	// are reported as review rather than as done.
	res.Review = append(res.Review, preview.Adopted...)
	res.Review = append(res.Review, preview.Review...)
	res.Failures = append(res.Failures, preview.Failures...)

	// Persist what needs a human so the review list survives a reload...
	s.saveReviewQueue(ctx, res.Review)
	// ...and take what was adopted out of the scan report, so the panel stops
	// counting work that is done. The automatic path is the common one, and
	// it was leaving the report untouched.
	adoptedPaths := make([]string, 0, len(res.Adopted))
	for _, p := range res.Adopted {
		adoptedPaths = append(adoptedPaths, p.Path)
	}
	s.dropFromOutstanding(ctx, adoptedPaths...)

	if len(firstRoots) > 0 {
		s.log.Info("adopt: first pass over new roots proposed for review",
			"roots", len(firstRoots), "proposals", len(first))
	}
	s.log.Info("adopt: complete",
		"adopted", len(res.Adopted), "review", len(res.Review), "failures", len(res.Failures))
	return res, nil
}

// ConfirmRootAdopted marks a root as reviewed, so subsequent scans adopt
// into it without asking.
func (s *Service) ConfirmRootAdopted(ctx context.Context, rootID int64) error {
	return s.SetRootAutoAdopt(ctx, rootID, true)
}

// SetRootAutoAdopt turns automatic adoption on or off for one root.
//
// Reversible on purpose: confirming a root is a trust decision, and a
// setting you can only ever switch on is one users are right to hesitate
// over. Turning it back off returns that root to propose-only.
func (s *Service) SetRootAutoAdopt(ctx context.Context, rootID int64, on bool) error {
	seen := s.adoptedRoots(ctx)
	if on {
		seen[rootID] = true
	} else {
		delete(seen, rootID)
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, fmt.Sprint(id))
	}
	return s.db.SetMeta(ctx, adoptionStateKey, strings.Join(ids, ","))
}

// AutoAdoptRoots reports which roots adopt without asking, so the UI can
// show the state rather than describing it in prose nobody can act on.
func (s *Service) AutoAdoptRoots(ctx context.Context) map[int64]bool {
	return s.adoptedRoots(ctx)
}

func (s *Service) adoptedRoots(ctx context.Context) map[int64]bool {
	out := map[int64]bool{}
	raw, err := s.db.GetMeta(ctx, adoptionStateKey)
	if err != nil {
		return out
	}
	for _, part := range strings.Split(raw, ",") {
		var id int64
		if _, err := fmt.Sscan(strings.TrimSpace(part), &id); err == nil && id != 0 {
			out[id] = true
		}
	}
	return out
}

// AdoptOne adopts a single folder to a caller-chosen candidate.
//
// This is what a click on a proposed match should do. It used to be a link
// into the Add page, which meant re-running the search, clicking Add next to
// the title you had already chosen, and landing on the item's page — three
// steps and a lost place in the queue, to accept an answer the queue had
// already worked out.
//
// The path is validated against the root folders and the disk, NOT against
// the review queue. The security property wanted here is "this is a folder
// inside a library root", and that is a fact about the filesystem. The queue
// is a snapshot that a rescan or a previous adoption rewrites underneath an
// open window, so checking membership in it rejected clicks on rows the user
// could plainly see — reported as "not a folder awaiting review", which is
// both wrong and impossible to act on. The queue is still consulted for the
// parse it already did; it is just no longer the gatekeeper.
func (s *Service) AdoptOne(ctx context.Context, path string, pick ports.SearchResult, force bool) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("adoption path must be absolute")
	}
	clean := filepath.Clean(path)

	root, err := s.rootFolderFor(ctx, clean)
	if err != nil {
		return err
	}
	info, statErr := os.Stat(clean)
	if statErr != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory on disk", ErrNotFound, clean)
	}

	// Use the queue entry when there is one — it carries the parsed title —
	// and otherwise synthesise from the folder name.
	p := Proposal{RootFolderID: root.ID, Path: clean, Name: filepath.Base(clean)}
	for _, q := range s.loadReviewQueue(ctx) {
		if q.Path == clean {
			p = q
			break
		}
	}
	p.RootFolderID = root.ID

	if pick.Kind == "" {
		pick.Kind = p.Kind
	}
	if pick.Kind == "" {
		// A mixed root resolves nothing, so the caller has to say which kind
		// the chosen candidate is.
		if k, typed := root.Kind.Media(); typed {
			pick.Kind = k
		}
	}
	if pick.Kind == "" {
		return fmt.Errorf("a media kind is required to adopt %s", clean)
	}
	if !root.Kind.Accepts(pick.Kind) {
		return fmt.Errorf("%w: %s holds %s", ErrRootKindMismatch, root.Path, root.Kind)
	}

	p.Candidates = []ports.SearchResult{pick}
	p.Force = force
	if err := s.adoptOne(ctx, p); err != nil {
		return err
	}
	s.dropFromOutstanding(ctx, clean)
	s.log.Info("adopt: folder adopted by hand",
		"path", clean, "kind", pick.Kind, "title", pick.Title, "year", pick.Year)
	return nil
}

// rootFolderFor returns the registered root that directly contains path.
//
// Requiring a *direct* child is deliberate and is the whole security check:
// scan only ever offers depth-1 children of a root, so anything else is
// either a mistake or someone probing. The library must not be aimable at
// arbitrary places on the host by a caller who can post JSON.
func (s *Service) rootFolderFor(ctx context.Context, path string) (domain.RootFolder, error) {
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return domain.RootFolder{}, err
	}
	parent := filepath.Dir(path)
	for _, rf := range roots {
		if filepath.Clean(rf.Path) == parent {
			return rf, nil
		}
	}
	return domain.RootFolder{}, fmt.Errorf(
		"%w: %s is not inside a registered root folder", ErrNotFound, path)
}

// AdoptExact applies every proposal in the queue that clears the bar,
// regardless of whether its root has been confirmed.
//
// ADR 0010 §5 holds the first pass over a new root for review, which is
// right: hundreds of unseen matches at once is where trust gets lost. But
// the user pressing this button *is* the review — so the guard has to be
// releasable from the screen where the reviewing happens, or it stops being
// a safety rail and becomes a dead end.
func (s *Service) AdoptExact(ctx context.Context) (AdoptResult, error) {
	queue := s.loadReviewQueue(ctx)
	if len(queue) == 0 {
		return AdoptResult{Adopted: []Proposal{}, Review: []Proposal{}, Failures: []string{}}, nil
	}
	var exact []Proposal
	for _, p := range queue {
		if p.Confidence == ConfidenceExact && len(p.Candidates) > 0 {
			exact = append(exact, p)
		}
	}
	res := s.Adopt(ctx, exact, false)

	paths := make([]string, 0, len(res.Adopted))
	for _, p := range res.Adopted {
		paths = append(paths, p.Path)
	}
	s.dropFromOutstanding(ctx, paths...)
	s.log.Info("adopt: confident matches applied on request",
		"adopted", len(res.Adopted), "remaining", len(queue)-len(res.Adopted))
	return res, nil
}

// dropFromOutstanding removes adopted folders from BOTH the review queue and
// the last scan report's unmatched list.
//
// Pruning only the queue left the two disagreeing: the report still counted
// the folder as unmatched, so the settings panel kept advertising work that
// was done, and the queue's fallback — which derives from that report — could
// resurrect an already-adopted folder. Two stores of the same fact have to be
// updated together or one of them is a lie.
func (s *Service) dropFromOutstanding(ctx context.Context, paths ...string) {
	if len(paths) == 0 {
		return
	}
	gone := make(map[string]bool, len(paths))
	for _, p := range paths {
		gone[p] = true
	}

	queue := s.loadReviewQueue(ctx)
	kept := make([]Proposal, 0, len(queue))
	for _, p := range queue {
		if !gone[p.Path] {
			kept = append(kept, p)
		}
	}
	s.saveReviewQueue(ctx, kept)

	report, ok, err := s.LastScanReport(ctx)
	if err != nil || !ok {
		return
	}
	keptDirs := make([]UnmatchedDir, 0, len(report.UnmatchedDirs))
	for _, d := range report.UnmatchedDirs {
		if !gone[d.Path] {
			keptDirs = append(keptDirs, d)
		}
	}
	if len(keptDirs) == len(report.UnmatchedDirs) {
		return // nothing to rewrite
	}
	report.UnmatchedDirs = keptDirs
	// Recompute rather than decrement: the report's list is the population
	// this count describes, and arithmetic against a possibly-stale total is
	// how counts drift out of step with the thing they count.
	report.UnmatchedTotal = len(keptDirs)
	if raw, err := json.Marshal(report); err == nil {
		if err := s.db.SetMeta(ctx, scanReportKey, string(raw)); err != nil {
			s.log.Warn("adopt: could not prune the scan report", "err", err)
		}
	}
}

// relinkExisting points an item that is already in the library at the folder
// being adopted.
//
// It refuses when the existing item already points at a real directory that
// is not this one, because that is a genuine conflict — two folders claiming
// one title — and silently moving the pointer would detach whatever files the
// other folder holds. The error names the other path, so the choice is
// informed rather than a mystery.
func (s *Service) relinkExisting(ctx context.Context, p Proposal, win ports.SearchResult, force bool) error {
	var id int64
	var err error
	switch {
	case win.OLID != "":
		id, err = s.db.GetMediaItemByKindOlid(ctx, win.Kind, win.OLID)
	case win.TMDBID != 0:
		id, err = s.db.GetMediaItemByKindTmdb(ctx, win.Kind, win.TMDBID)
	default:
		id, err = s.db.GetMediaItemByKindTvdb(ctx, win.Kind, win.TVDBID)
	}
	if err != nil {
		// Already-exists was reported but the row cannot be found: report the
		// original condition rather than inventing a new one.
		return ErrAlreadyExists
	}
	existing, err := s.Get(ctx, id)
	if err != nil {
		return ErrAlreadyExists
	}

	if existing.Path == p.Path {
		// Already pointed here. Nothing to do, and not an error — this is
		// what a re-run looks like.
		return nil
	}
	if existing.Path != "" && !force {
		if _, statErr := os.Stat(existing.Path); statErr == nil {
			// Two folders claiming one title. Which one is right is a
			// question only the user can answer, so name both paths
			// verbatim — the difference is often a single character
			// (a hyphen against an en dash) that prose cannot show.
			return fmt.Errorf(
				"%w: %q is already in the library at %q, and this is %q. "+
					"Point the entry at this folder, or dismiss it",
				ErrFolderConflict, existing.Title, existing.Path, p.Path)
		}
	}

	path := p.Path
	root := p.RootFolderID
	if _, err := s.UpdateItem(ctx, existing.ID, UpdateRequest{Path: &path, RootFolderID: &root}); err != nil {
		return fmt.Errorf("point %q at %s: %w", existing.Title, p.Path, err)
	}
	s.log.Info("adopt: existing library item pointed at its folder",
		"title", existing.Title, "path", p.Path, "was", existing.Path)
	return nil
}
