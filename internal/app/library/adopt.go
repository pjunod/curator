package library

import (
	"context"
	"errors"
	"fmt"
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
		for _, k := range []domain.MediaKind{domain.KindMovie, domain.KindSeries} {
			p.Candidates = append(p.Candidates, s.searchTop(ctx, k, p.ParsedTitle)...)
		}
		p.Candidates = trim(p.Candidates, maxProposalCandidates)
		if len(p.Candidates) > 0 {
			p.Confidence = ConfidenceAmbiguous
		}
		return p
	}

	p.Kind = kind
	results := s.searchTop(ctx, kind, p.ParsedTitle)
	p.Candidates = trim(results, maxProposalCandidates)
	p.Confidence = grade(kind, parsed, results)
	if p.Confidence == ConfidenceExact {
		// Put the winner first so the caller never has to re-derive it.
		for i, r := range p.Candidates {
			if clears(kind, parsed, r) {
				p.Candidates[0], p.Candidates[i] = p.Candidates[i], p.Candidates[0]
				break
			}
		}
	}
	return p
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

// grade applies the per-kind bar. The asymmetry is the point: a wrong
// auto-match writes a path assignment and metadata that look right until
// someone notices the poster, while an unmatched folder is merely visible
// and annoying. So the bar is set where false positives are close to
// impossible, and more folders land in review as a result (ADR 0010 §2).
func grade(kind domain.MediaKind, parsed parser.Parsed, results []ports.SearchResult) Confidence {
	if len(results) == 0 {
		return ConfidenceNone
	}
	clear := 0
	for _, r := range results {
		if clears(kind, parsed, r) {
			clear++
		}
	}
	if clear == 1 {
		return ConfidenceExact
	}
	return ConfidenceAmbiguous
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
	seen := s.adoptedRoots(ctx)
	seen[rootID] = true
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, fmt.Sprint(id))
	}
	return s.db.SetMeta(ctx, adoptionStateKey, strings.Join(ids, ","))
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
