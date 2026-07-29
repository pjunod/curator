// Package matcher decides whether a parsed release satisfies a wantable —
// the second of exactly two places media-kind knowledge lives
// (blueprint §4.1). Pure.
package matcher

import (
	"regexp"
	"strings"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/parser"
)

var reNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// reElision matches the apostrophe family, which is DELETED rather than
// turned into a separator.
//
// This distinction is the whole difference between matching a film and not.
// An apostrophe inside a word is elision, not a word break: "The Carpenter's
// Son" is two words plus a possessive, and scene names write it
// "The.Carpenters.Son". Spacing the apostrophe gives "carpenter s son" on one
// side and "carpenters son" on the other, and forty perfectly good releases
// come back marked "does not match".
//
// The curly forms are not decoration either — TMDB overviews and titles are
// full of U+2019, so the straight-quote-only version of this fix would miss
// half the cases.
//
// Note this stays correct for apostrophes that ARE at a word boundary:
// "Rock 'n' Roll" keeps its surrounding spaces, so deleting the quotes still
// yields "rock n roll".
var reElision = regexp.MustCompile("['’‘ʼ´`]+")

// reAmpersand unifies the three ways a title says "and" before anything else
// runs. Metadata writes "Tom & Jerry"; releases write ".and." or drop the word
// entirely. Mapping & to the word and then dropping the word (see
// NormalizeTitle) makes all three spellings agree, where the old behaviour of
// spacing the ampersand agreed with only one of them.
var reAmpersand = regexp.MustCompile(`&`)

// reSortName matches the inverted-article convention that media libraries
// have used forever: "Fall, The", "Lord of the Rings, The", "Thing, A".
// Plex, Emby, Kodi and countless rename scripts all produce it, so a library
// full of it would otherwise match nothing.
//
// Deliberately requires the comma. Dropping any trailing "the" would also
// rewrite titles that legitimately end in the word, and the comma is the
// thing that actually signals an inversion.
var reSortName = regexp.MustCompile(`,\s*(the|a|an)\s*$`)

// NormalizeTitle lowers, folds, strips punctuation/articles, and collapses
// spacing so "The Office (US)" matches "the.office.us" — and so "Fall, The"
// matches "The Fall", which is the same title written the way a library sorts
// it.
//
// The governing idea: a release name is an ASCII transliteration of a title
// typed by someone who had a full keyboard. Everything here is a rule for
// undoing one way that transliteration is lossy. Order matters and each step
// says why it has to run where it does.
func NormalizeTitle(t string) string {
	t = strings.ToLower(t)
	// Un-invert before punctuation goes, because the comma is the signal and
	// stripping punctuation destroys it.
	t = reSortName.ReplaceAllString(t, "")
	// Fold accents before the alnum filter, or "amélie" loses its é to a
	// space and becomes "am lie" while the release stays "amelie".
	t = foldASCII(t)
	// Delete elisions; do NOT space them. See reElision.
	t = reElision.ReplaceAllString(t, "")
	t = reAmpersand.ReplaceAllString(t, " and ")
	t = reNonAlnum.ReplaceAllString(t, " ")
	fields := strings.Fields(t)
	if len(fields) > 1 {
		switch fields[0] {
		case "the", "a", "an":
			fields = fields[1:]
		}
	}
	// Drop "and" wherever it appears, which is what makes the three spellings
	// of an ampersand title agree: "Tom & Jerry", "Tom.and.Jerry" and
	// "Tom.Jerry" all reduce to "tom jerry". Dropping a word is a real loss of
	// precision, and it is affordable here only because two titles that differ
	// by nothing but a standalone "and" are not a thing that exists — and
	// movies carry a year check behind this anyway.
	out := fields[:0]
	for _, f := range fields {
		if f == "and" {
			continue
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		out = fields // a title that is only the word "and" keeps it
	}
	return strings.Join(out, " ")
}

// asciiFold maps the Latin letters a release name cannot carry onto the ones
// it writes instead. Lowercase only: NormalizeTitle folds after ToLower.
//
// A table rather than Unicode NFD because the two halves of the problem need
// different treatment and only one of them decomposes. "é" is e + a combining
// mark and NFD handles it; "ø", "æ", "ß" and "ð" are atomic letters with no
// decomposition at all, so they need a mapping either way. Given that, one
// table does the whole job with no dependency, which keeps this package
// stdlib-only like the parser it sits beside.
var asciiFold = map[rune]string{
	'à': "a", 'á': "a", 'â': "a", 'ã': "a", 'ä': "a", 'å': "a", 'ā': "a", 'ă': "a", 'ą': "a",
	'æ': "ae",
	'ç': "c", 'ć': "c", 'ĉ': "c", 'ċ': "c", 'č': "c",
	'ď': "d", 'đ': "d", 'ð': "d",
	'è': "e", 'é': "e", 'ê': "e", 'ë': "e", 'ē': "e", 'ĕ': "e", 'ė': "e", 'ę': "e", 'ě': "e",
	'ĝ': "g", 'ğ': "g", 'ġ': "g", 'ģ': "g",
	'ĥ': "h", 'ħ': "h",
	'ì': "i", 'í': "i", 'î': "i", 'ï': "i", 'ĩ': "i", 'ī': "i", 'ĭ': "i", 'į': "i", 'ı': "i",
	'ĵ': "j",
	'ķ': "k",
	'ĺ': "l", 'ļ': "l", 'ľ': "l", 'ł': "l",
	'ñ': "n", 'ń': "n", 'ņ': "n", 'ň': "n",
	'ò': "o", 'ó': "o", 'ô': "o", 'õ': "o", 'ö': "o", 'ø': "o", 'ō': "o", 'ŏ': "o", 'ő': "o",
	'œ': "oe",
	'ŕ': "r", 'ŗ': "r", 'ř': "r",
	'ś': "s", 'ŝ': "s", 'ş': "s", 'š': "s",
	'ß': "ss",
	'ţ': "t", 'ť': "t", 'ŧ': "t",
	'ù': "u", 'ú': "u", 'û': "u", 'ü': "u", 'ũ': "u", 'ū': "u", 'ŭ': "u", 'ů': "u", 'ű': "u", 'ų': "u",
	'ŵ': "w",
	'ý': "y", 'ÿ': "y", 'ŷ': "y",
	'ź': "z", 'ż': "z", 'ž': "z",
	'þ': "th",
}

// foldASCII rewrites accented Latin letters as the ASCII a release name uses.
// Anything not in the table is left alone — the alnum filter downstream turns
// it into a separator, which is the right answer for a character that carries
// no letter (a bullet, a CJK glyph, an emoji).
func foldASCII(t string) string {
	// Fast path: the overwhelming majority of titles are already ASCII, and
	// this runs on every release from every indexer against every wantable.
	ascii := true
	for _, r := range t {
		if r >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return t
	}
	var b strings.Builder
	b.Grow(len(t))
	for _, r := range t {
		if sub, ok := asciiFold[r]; ok {
			b.WriteString(sub)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
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
