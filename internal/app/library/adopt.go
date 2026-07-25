package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/matcher"
	"github.com/monarr-media/monarr/internal/domain/parser"
	"github.com/monarr-media/monarr/internal/ports"
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
	return out, nil
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
	p.Confidence = grade(kind, parsed, results)
	if won := clearing(kind, parsed, results); len(won) == 1 {
		// Put the winner first so the caller never has to re-derive it, and
		// so the UI's first chip is the one adoption would have picked.
		results = append(won, filterOut(results, won[0])...)
	}
	p.Candidates = trim(results, maxProposalCandidates)
	return p
}

// filterOut returns everything except one specific result.
func filterOut(in []ports.SearchResult, drop ports.SearchResult) []ports.SearchResult {
	out := make([]ports.SearchResult, 0, len(in))
	for _, r := range in {
		if r.Kind == drop.Kind && r.TMDBID == drop.TMDBID && r.OLID == drop.OLID {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (s *Service) searchTop(ctx context.Context, kind domain.MediaKind, query string) []ports.SearchResult {
	if strings.TrimSpace(query) == "" {
		return nil
	}
	res, err := s.Search(ctx, kind, query)
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
		key := fmt.Sprintf("%s|%d|%s", r.Kind, r.TMDBID, r.OLID)
		if r.TMDBID == 0 && r.OLID == "" {
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
	var exactYear, tolerated []ports.SearchResult
	for _, r := range results {
		if !clears(kind, parsed, r) {
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

// clears reports whether one result meets the bar for its kind.
func clears(kind domain.MediaKind, parsed parser.Parsed, r ports.SearchResult) bool {
	if matcher.NormalizeTitle(parsed.Title) != matcher.NormalizeTitle(r.Title) {
		return false
	}
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
	req := AddRequest{
		Kind:         win.Kind,
		TMDBID:       win.TMDBID,
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

	// Persist what needs a human so the review list survives a reload.
	s.saveReviewQueue(ctx, res.Review)

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
// The path must be one the review queue is actually offering. That is not
// ceremony: the endpoint writes a library item pointed at a directory, and
// accepting an arbitrary caller-supplied path would let anyone with a session
// aim the library anywhere on the host.
func (s *Service) AdoptOne(ctx context.Context, path string, pick ports.SearchResult, force bool) error {
	clean := filepath.Clean(path)
	queue := s.loadReviewQueue(ctx)
	var found *Proposal
	for i := range queue {
		if queue[i].Path == clean {
			found = &queue[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("%w: %s is not a folder awaiting review", ErrNotFound, clean)
	}
	if pick.Kind == "" {
		pick.Kind = found.Kind
	}
	if pick.Kind == "" {
		return fmt.Errorf("a media kind is required to adopt %s", clean)
	}

	p := *found
	p.Candidates = []ports.SearchResult{pick}
	p.Force = force
	if err := s.adoptOne(ctx, p); err != nil {
		return err
	}
	s.dropFromReviewQueue(ctx, clean)
	s.log.Info("adopt: folder adopted by hand",
		"path", clean, "kind", pick.Kind, "title", pick.Title, "year", pick.Year)
	return nil
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

	adopted := map[string]bool{}
	for _, p := range res.Adopted {
		adopted[p.Path] = true
	}
	kept := make([]Proposal, 0, len(queue))
	for _, p := range queue {
		if !adopted[p.Path] {
			kept = append(kept, p)
		}
	}
	s.saveReviewQueue(ctx, kept)
	s.log.Info("adopt: confident matches applied on request",
		"adopted", len(res.Adopted), "remaining", len(kept))
	return res, nil
}

// dropFromReviewQueue removes one entry, so an adopted folder stops being
// offered without waiting for the next scan.
func (s *Service) dropFromReviewQueue(ctx context.Context, path string) {
	queue := s.loadReviewQueue(ctx)
	kept := make([]Proposal, 0, len(queue))
	for _, p := range queue {
		if p.Path != path {
			kept = append(kept, p)
		}
	}
	s.saveReviewQueue(ctx, kept)
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
	default:
		id, err = s.db.GetMediaItemByKindTmdb(ctx, win.Kind, win.TMDBID)
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
