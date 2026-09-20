package domain

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pjunod/monarr/internal/domain/quality"
)

// WantableID is a stable identity: "movie:42", "episode:42:2:5", "season:42:2".
type WantableID string

// copySuffix appends the copy discriminator to a wantable id: the primary
// stays "movie:42", a copy's want is "movie:42:c3" — distinct identities
// so in-flight suppression and grab bookkeeping never cross copies.
func copySuffix(base string, copyID int64) string {
	if copyID == 0 {
		return base
	}
	return fmt.Sprintf("%s:c%d", base, copyID)
}

// WantableCopy reports which media copy a wantable hunts for (0 = primary).
func WantableCopy(w Wantable) int64 {
	switch t := w.(type) {
	case MovieWantable:
		return t.Copy
	case EpisodeWantable:
		return t.Copy
	case SeasonWantable:
		return t.Copy
	case BookWantable:
		return t.Copy
	}
	return 0
}

// WantableCopyName is the copy's display label ("" for the primary).
func WantableCopyName(w Wantable) string {
	switch t := w.(type) {
	case MovieWantable:
		return t.CopyName
	case EpisodeWantable:
		return t.CopyName
	case SeasonWantable:
		return t.CopyName
	case BookWantable:
		return t.CopyName
	}
	return ""
}

// Wantable is "a thing the system wants on disk at a given quality" — the
// contract the entire acquisition pipeline is built against (blueprint §4.1).
type Wantable interface {
	ID() WantableID
	MediaItemID() int64
	ProfileID() int64
	Monitored() bool
	// CurrentQuality is the best KNOWN quality already on disk; ok=false
	// when nothing is on disk OR when what is there could not be measured.
	// Use OnDisk to tell those two apart — they are different states and
	// treating them alike is what made monarr hunt files it already had.
	CurrentQuality() (quality.Quality, bool)
	// OnDisk reports whether files exist for this wantable at all,
	// regardless of whether their quality could be determined (ADR 0013 §5:
	// "a file whose quality could not be determined still exists").
	OnDisk() bool
	// SourceVerified reports whether the SOURCE axis of CurrentQuality is
	// trustworthy enough to justify replacing the file. False for a
	// medium/low-confidence inference — the don't-churn rule.
	SourceVerified() bool
}

// MovieWantable is the degenerate case: one item, one file.
type MovieWantable struct {
	Item    int64
	Profile int64
	Mon     bool
	Have    *quality.Quality
	// Files reports whether this copy has any file on disk, even one whose
	// quality is unknown. Have==nil && Files==true is "on disk, unverified".
	Files    bool
	Verified bool
	Title    string
	Year     int
	Identity MediaIdentity
	// Copy is the media copy this wantable hunts for (0 = the primary).
	// Copies carry their own profile and their own on-disk file set.
	Copy     int64
	CopyName string
}

// ID implements Wantable.
func (m MovieWantable) ID() WantableID {
	return WantableID(copySuffix(fmt.Sprintf("movie:%d", m.Item), m.Copy))
}

// MediaItemID implements Wantable.
func (m MovieWantable) MediaItemID() int64 { return m.Item }

// ProfileID implements Wantable.
func (m MovieWantable) ProfileID() int64 { return m.Profile }

// Monitored implements Wantable.
func (m MovieWantable) Monitored() bool { return m.Mon }

// CurrentQuality implements Wantable.
func (m MovieWantable) CurrentQuality() (quality.Quality, bool) {
	if m.Have == nil {
		return quality.Quality{}, false
	}
	return *m.Have, true
}

// OnDisk implements Wantable.
func (m MovieWantable) OnDisk() bool { return m.Files || m.Have != nil }

// SourceVerified implements Wantable.
func (m MovieWantable) SourceVerified() bool { return m.Verified }

// EpisodeWantable is one episode of a series.
type EpisodeWantable struct {
	Item      int64
	EpisodeID int64
	Profile   int64
	Mon       bool
	Have      *quality.Quality
	Files     bool
	Verified  bool
	Title     string // series title
	Year      int
	Identity  MediaIdentity
	Season    int
	Episode   int
	Absolute  int // anime absolute number; 0 = unknown
	Copy      int64
	CopyName  string
}

// ID implements Wantable.
func (e EpisodeWantable) ID() WantableID {
	return WantableID(copySuffix(fmt.Sprintf("episode:%d:%d:%d", e.Item, e.Season, e.Episode), e.Copy))
}

// MediaItemID implements Wantable.
func (e EpisodeWantable) MediaItemID() int64 { return e.Item }

// ProfileID implements Wantable.
func (e EpisodeWantable) ProfileID() int64 { return e.Profile }

// Monitored implements Wantable.
func (e EpisodeWantable) Monitored() bool { return e.Mon }

// CurrentQuality implements Wantable.
func (e EpisodeWantable) CurrentQuality() (quality.Quality, bool) {
	if e.Have == nil {
		return quality.Quality{}, false
	}
	return *e.Have, true
}

// OnDisk implements Wantable.
func (e EpisodeWantable) OnDisk() bool { return e.Files || e.Have != nil }

// SourceVerified implements Wantable.
func (e EpisodeWantable) SourceVerified() bool { return e.Verified }

// SeasonWantable is the season-pack search target: one download that
// satisfies many episode wantables at import time — Sonarr's hardest
// structural feature, modeled from day one (blueprint §4.1).
type SeasonWantable struct {
	Item     int64
	Profile  int64
	Mon      bool
	Title    string
	Year     int
	Identity MediaIdentity
	Season   int
	Episodes []EpisodeWantable // the episodes a pack would satisfy
	Copy     int64
	CopyName string
}

// ID implements Wantable.
func (s SeasonWantable) ID() WantableID {
	return WantableID(copySuffix(fmt.Sprintf("season:%d:%d", s.Item, s.Season), s.Copy))
}

// MediaItemID implements Wantable.
func (s SeasonWantable) MediaItemID() int64 { return s.Item }

// ProfileID implements Wantable.
func (s SeasonWantable) ProfileID() int64 { return s.Profile }

// Monitored implements Wantable.
func (s SeasonWantable) Monitored() bool { return s.Mon }

// CurrentQuality implements Wantable: the WORST quality across the season's
// episodes (a pack upgrades the season only if it beats the weakest link);
// ok=false if any episode is missing entirely.
func (s SeasonWantable) CurrentQuality() (quality.Quality, bool) {
	var worst *quality.Quality
	for _, e := range s.Episodes {
		q, ok := e.CurrentQuality()
		if !ok {
			return quality.Quality{}, false
		}
		if worst == nil || quality.Better(*worst, q) {
			qq := q
			worst = &qq
		}
	}
	if worst == nil {
		return quality.Quality{}, false
	}
	return *worst, true
}

// OnDisk implements Wantable: a season is on disk only when every one of its
// episodes is. One missing episode makes the season an incomplete thing to
// hunt, whatever the others are.
func (s SeasonWantable) OnDisk() bool {
	if len(s.Episodes) == 0 {
		return false
	}
	for _, e := range s.Episodes {
		if !e.OnDisk() {
			return false
		}
	}
	return true
}

// SourceVerified implements Wantable: same weakest-link rule CurrentQuality
// uses. A season is only as verified as its least-verified episode.
func (s SeasonWantable) SourceVerified() bool {
	if len(s.Episodes) == 0 {
		return false
	}
	for _, e := range s.Episodes {
		if !e.SourceVerified() {
			return false
		}
	}
	return true
}

// BookWantable is one book (ADR 0006): like a movie, one item and one file,
// but matched by author+title and graded on format instead of resolution.
type BookWantable struct {
	Item     int64
	Profile  int64
	Mon      bool
	Have     *quality.Quality
	Files    bool
	Verified bool
	Title    string
	Author   string
	Year     int
	BookType quality.BookType
	// Copy is the independently curated book edition target. Zero is the
	// primary edition stored on the media item; non-zero identifies the other
	// edition in media_copies (ADR 0018).
	Copy     int64
	CopyName string
}

// ID implements Wantable.
func (b BookWantable) ID() WantableID {
	return WantableID(copySuffix(fmt.Sprintf("book:%d", b.Item), b.Copy))
}

// MediaItemID implements Wantable.
func (b BookWantable) MediaItemID() int64 { return b.Item }

// ProfileID implements Wantable.
func (b BookWantable) ProfileID() int64 { return b.Profile }

// Monitored implements Wantable.
func (b BookWantable) Monitored() bool { return b.Mon }

// CurrentQuality implements Wantable.
func (b BookWantable) CurrentQuality() (quality.Quality, bool) {
	if b.Have == nil {
		return quality.Quality{}, false
	}
	return *b.Have, true
}

// OnDisk implements Wantable.
func (b BookWantable) OnDisk() bool { return b.Files || b.Have != nil }

// SourceVerified implements Wantable.
func (b BookWantable) SourceVerified() bool { return b.Verified }

// SearchQuery is what a planner emits for indexers (blueprint §4.1).
type SearchQuery struct {
	Q string
	// Mode is selected after capability filtering: generic, tv, or movie.
	// Empty preserves the legacy adapter behavior for older callers.
	Mode       string
	Season     int // retained for compatibility; use SeasonSet for season zero
	Episode    int // retained for compatibility; use EpisodeSet for presence
	SeasonSet  bool
	EpisodeSet bool
	ID         *ExternalRef
	// Kind steers indexer category selection (books → Torznab 7000s/3030).
	Kind MediaKind
	// BookType narrows book searches to ebook or audiobook categories. Empty
	// preserves the broad legacy query for callers without a resolved profile.
	BookType quality.BookType
}

// SearchPlan separates exact-ID alternatives from bounded title fallbacks.
type SearchPlan struct {
	IDs    []SearchQuery
	Titles []SearchQuery
}

// PlanVideoSearch builds deterministic alternatives. Capability filtering
// and the automatic/interactive budgets live in the executor.
func PlanVideoSearch(w Wantable) SearchPlan {
	var ident MediaIdentity
	var canonical SearchQuery
	switch t := w.(type) {
	case MovieWantable:
		ident = t.Identity
		if ident.Title == "" {
			ident.Title, ident.Year, ident.IDs = t.Title, t.Year, t.Identity.IDs
		}
		q := ident.Title
		if ident.Year > 0 {
			q = fmt.Sprintf("%s %d", ident.Title, ident.Year)
		}
		canonical = SearchQuery{Q: q, Kind: KindMovie}
	case EpisodeWantable:
		ident = t.Identity
		if ident.Title == "" {
			ident.Title, ident.Year = t.Title, t.Year
		}
		canonical = SearchQuery{Q: fmt.Sprintf("%s S%02dE%02d", ident.Title, t.Season, t.Episode), Season: t.Season, Episode: t.Episode, SeasonSet: true, EpisodeSet: true, Kind: KindSeries}
	case SeasonWantable:
		ident = t.Identity
		if ident.Title == "" {
			ident.Title, ident.Year = t.Title, t.Year
		}
		canonical = SearchQuery{Q: fmt.Sprintf("%s S%02d", ident.Title, t.Season), Season: t.Season, SeasonSet: true, Kind: KindSeries}
	default:
		return SearchPlan{}
	}
	plan := SearchPlan{Titles: []SearchQuery{canonical}}
	appendID := func(provider, value string) {
		if value == "" || value == "0" {
			return
		}
		q := canonical
		q.Q = ""
		q.ID = &ExternalRef{Provider: provider, Value: value}
		plan.IDs = append(plan.IDs, q)
	}
	if canonical.Kind == KindSeries {
		if ident.IDs.TVDB > 0 {
			appendID("tvdb", fmt.Sprintf("%d", ident.IDs.TVDB))
		}
		appendID("imdb", ident.IDs.IMDB)
		if ident.IDs.TMDB > 0 {
			appendID("tmdb", fmt.Sprintf("%d", ident.IDs.TMDB))
		}
	} else {
		appendID("imdb", ident.IDs.IMDB)
		if ident.IDs.TMDB > 0 {
			appendID("tmdb", fmt.Sprintf("%d", ident.IDs.TMDB))
		}
	}
	aliases := append([]TitleAlias(nil), ident.Aliases...)
	aliasPriority := func(alias TitleAlias) int {
		if alias.Role == "manual" || alias.Source == "manual" {
			return 0
		}
		if alias.Role == "original" {
			return 1
		}
		return 2
	}
	sort.SliceStable(aliases, func(i, j int) bool {
		pi, pj := aliasPriority(aliases[i]), aliasPriority(aliases[j])
		if pi != pj {
			return pi < pj
		}
		return strings.ToLower(aliases[i].Title) < strings.ToLower(aliases[j].Title)
	})
	seen := map[string]bool{canonical.Q: true}
	for _, a := range aliases {
		if !a.Searchable || a.Scope != "work" || a.Title == "" {
			continue
		}
		q := canonical
		switch t := w.(type) {
		case EpisodeWantable:
			q.Q = fmt.Sprintf("%s S%02dE%02d", a.Title, t.Season, t.Episode)
		case SeasonWantable:
			q.Q = fmt.Sprintf("%s S%02d", a.Title, t.Season)
		default:
			q.Q = a.Title
		}
		if canonical.Kind == KindMovie && ident.Year > 0 {
			q.Q = fmt.Sprintf("%s %d", a.Title, ident.Year)
		}
		if seen[q.Q] {
			continue
		}
		plan.Titles = append(plan.Titles, q)
		seen[q.Q] = true
	}
	return plan
}

// PlanSearch builds the indexer queries for a wantable — one of exactly two
// places media-kind knowledge lives (the other is the matcher).
func PlanSearch(w Wantable) []SearchQuery {
	switch t := w.(type) {
	case MovieWantable:
		return PlanVideoSearch(t).Titles[:1]
	case EpisodeWantable:
		return PlanVideoSearch(t).Titles[:1]
	case SeasonWantable:
		return PlanVideoSearch(t).Titles[:1]
	case BookWantable:
		if t.Author == "" {
			return []SearchQuery{{Q: t.Title, Kind: KindBook, BookType: t.BookType}}
		}
		// Author+title is the discriminating query; title-only casts wider.
		return []SearchQuery{
			{Q: fmt.Sprintf("%s %s", t.Author, t.Title), Kind: KindBook, BookType: t.BookType},
			{Q: t.Title, Kind: KindBook, BookType: t.BookType},
		}
	default:
		return nil
	}
}
