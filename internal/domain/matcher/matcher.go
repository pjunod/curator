// Package matcher decides whether a parsed release satisfies a wantable —
// the second of exactly two places media-kind knowledge lives
// (blueprint §4.1). Pure.
package matcher

import (
	"regexp"
	"strings"

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/parser"
)

var reNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// reSortName matches the inverted-article convention that media libraries
// have used forever: "Fall, The", "Lord of the Rings, The", "Thing, A".
// Plex, Emby, Kodi and countless rename scripts all produce it, so a library
// full of it would otherwise match nothing.
//
// Deliberately requires the comma. Dropping any trailing "the" would also
// rewrite titles that legitimately end in the word, and the comma is the
// thing that actually signals an inversion.
var reSortName = regexp.MustCompile(`,\s*(the|a|an)\s*$`)

// NormalizeTitle lowers, strips punctuation/articles, and collapses spacing
// so "The Office (US)" matches "the.office.us" — and so "Fall, The" matches
// "The Fall", which is the same title written the way a library sorts it.
func NormalizeTitle(t string) string {
	t = strings.ToLower(t)
	// Un-invert before punctuation goes, because the comma is the signal and
	// stripping punctuation destroys it.
	t = reSortName.ReplaceAllString(t, "")
	t = reNonAlnum.ReplaceAllString(t, " ")
	fields := strings.Fields(t)
	if len(fields) > 1 {
		switch fields[0] {
		case "the", "a", "an":
			fields = fields[1:]
		}
	}
	return strings.Join(fields, " ")
}

// Match is a confirmed release↔wantable pairing.
type Result struct {
	Wantable domain.Wantable
	// FullSeason marks a season pack matched against a SeasonWantable.
	FullSeason bool
}

// TitleMatches applies normalization plus year tolerance (±1 when both
// sides know a year — release years drift around festival/regional dates).
func TitleMatches(parsed parser.Parsed, title string, year int) bool {
	if NormalizeTitle(parsed.Title) != NormalizeTitle(title) {
		return false
	}
	if parsed.Year != 0 && year != 0 {
		d := parsed.Year - year
		if d < -1 || d > 1 {
			return false
		}
	}
	return true
}

// Match reports which candidates the parsed release covers. A season pack
// matches a SeasonWantable (fan-out to episodes happens at import); a
// multi-episode release matches each covered EpisodeWantable.
func Match(p parser.Parsed, candidates []domain.Wantable) []Result {
	var out []Result
	for _, w := range candidates {
		switch t := w.(type) {
		case domain.MovieWantable:
			if len(p.Episodes) == 0 && !p.SeasonPack && p.Daily == "" &&
				TitleMatches(p, t.Title, t.Year) {
				out = append(out, Result{Wantable: w})
			}
		case domain.EpisodeWantable:
			if !TitleMatches(p, t.Title, 0) {
				continue
			}
			if p.SeasonPack && p.Season == t.Season {
				out = append(out, Result{Wantable: w})
				continue
			}
			// Anime absolute numbering: "[Group] Show - 15" carries no
			// season; match on the episode's absolute number.
			if len(p.Absolute) > 0 && t.Absolute > 0 {
				for _, abs := range p.Absolute {
					if abs == t.Absolute {
						out = append(out, Result{Wantable: w})
						break
					}
				}
				continue
			}
			if p.Season == t.Season {
				for _, ep := range p.Episodes {
					if ep == t.Episode {
						out = append(out, Result{Wantable: w})
						break
					}
				}
			}
		case domain.SeasonWantable:
			if p.SeasonPack && p.Season == t.Season && TitleMatches(p, t.Title, 0) {
				out = append(out, Result{Wantable: w, FullSeason: true})
			}
		case domain.BookWantable:
			if len(p.Episodes) == 0 && !p.SeasonPack && p.Daily == "" &&
				bookMatches(p, t.Title, t.Author) {
				out = append(out, Result{Wantable: w})
			}
		}
	}
	return out
}

// bookMatches pairs a parsed book release with a book wantable. Book naming
// is looser than scene video naming, so matching is containment-based:
// the book's title must appear in the parsed title (or, when the parser's
// "Author - Title" order assumption was wrong, in the parsed author), and
// every token of the book's author name must appear somewhere in the blob.
// Years are ignored — editions republish freely.
func bookMatches(p parser.Parsed, title, author string) bool {
	nt := NormalizeTitle(title)
	if nt == "" {
		return false
	}
	pt, pa := NormalizeTitle(p.Title), NormalizeTitle(p.Author)
	titleIn, swapped := containsPhrase(pt, nt), containsPhrase(pa, nt)
	if !titleIn && !swapped {
		return false
	}
	if author == "" {
		return true
	}
	blob := pt + " " + pa
	for _, tok := range strings.Fields(NormalizeTitle(author)) {
		if !containsPhrase(blob, tok) {
			return false
		}
	}
	return true
}

// containsPhrase reports whether phrase appears in s on word boundaries.
func containsPhrase(s, phrase string) bool {
	if s == phrase {
		return true
	}
	return strings.Contains(" "+s+" ", " "+phrase+" ")
}
