// Package recommendation contains deterministic interpretation and evidence rules.
package recommendation

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pjunod/monarr/internal/domain"
)

const RulesVersion = "theme-rules-v1"
const RankingVersion = "metadata-semantic-v1"

type Theme struct {
	Key, Label, Text                  string
	Phrases, Aliases, Lanes, Evidence []string
	Pattern, Central                  *regexp.Regexp
}

func rx(s string) *regexp.Regexp { return regexp.MustCompile(`(?i)` + s) }

var Themes = []Theme{
	{Key: "gay_male", Label: "Gay male stories", Text: "TV series about gay men, their lives, friendships and romantic relationships.", Phrases: []string{"gay themed", "gay romance", "gay relationship", "gay"}, Aliases: []string{"gay romance", "gay relationship", "gay", "lgbt", "gay theme", "boys' love (bl)"}, Lanes: []string{"gay theme", "boys' love (bl)", "lgbt"}, Evidence: []string{"gay romance", "gay relationship", "gay theme", "boys' love (bl)"}, Pattern: rx(`\b(?:gay (?:men|man|couple|life)|homosexual (?:men|man)|(?:romance|relationship) between two men|two men.{0,50}(?:love each other|fall in love with each other)|boys.? love|men in love|male couple)\b`), Central: rx(`^(?:.{0,90})\b(?:gay (?:men|man|couple)|romance between two men|two men.{0,50}(?:love each other|fall in love with each other)|boys.? love)\b`)},
	{Key: "lesbian", Label: "Lesbian stories", Text: "TV series about lesbian women, their lives and romantic relationships.", Phrases: []string{"lesbian themed", "lesbian romance", "lesbian relationship", "lesbian"}, Aliases: []string{"lesbian romance", "lesbian relationship", "lesbian", "lgbt"}, Lanes: []string{"lesbian romance", "lesbian"}, Evidence: []string{"lesbian romance", "lesbian relationship", "lesbian", "girls' love (gl)"}, Pattern: rx(`\b(?:lesbian|girls.? love|women in love|(?:romance|relationship) between two women|two women.{0,50}(?:love each other|fall in love with each other))\b`), Central: rx(`^.{0,90}\b(?:lesbian (?:women|woman|couple)|romance between two women|two women.{0,50}fall in love with each other)\b`)},
	{Key: "lgbtq", Label: "LGBTQ+ stories", Text: "TV series about LGBTQ people, their lives and relationships.", Phrases: []string{"lgbtq themed", "lgbt themed", "queer themed", "lgbtq", "lgbt", "queer"}, Aliases: []string{"lgbt", "gay theme", "lesbian", "transgender"}, Lanes: []string{"lgbt", "gay theme", "lesbian"}, Evidence: []string{"lgbt", "lgbt+", "lgbtq", "gay theme", "gay romance", "lesbian", "transgender", "boys' love (bl)", "girls' love (gl)", "queer", "bisexuality"}, Pattern: rx(`\b(?:gay (?:men|man|couple)|lesbian|queer (?:people|life|romance)|transgender|same.sex (?:romance|relationship)|boys.? love|girls.? love|lgbtq?)\b`), Central: rx(`^.{0,90}\b(?:gay (?:men|man|couple)|lesbian (?:women|woman|couple)|transgender|queer (?:people|life)|lgbtq?)\b`)},
	{Key: "coming_of_age", Label: "Coming of age", Text: "TV series about growing up, self-discovery and coming of age.", Phrases: []string{"coming of age"}, Aliases: []string{"coming of age", "adolescence", "growing up"}, Lanes: []string{"coming of age", "growing up"}, Evidence: []string{"coming of age", "growing up"}, Pattern: rx(`\b(?:coming.of.age|growing up|journey through adolescence)\b`), Central: rx(`^.{0,90}\b(?:coming.of.age|growing up|journey through adolescence)\b`)},
	{Key: "found_family", Label: "Found family", Text: "TV series about chosen families, belonging and close bonds beyond biological family.", Phrases: []string{"found family", "chosen family"}, Aliases: []string{"found family", "chosen family"}, Lanes: []string{"found family", "chosen family"}, Evidence: []string{"found family", "chosen family"}, Pattern: rx(`\b(?:found family|chosen family|makeshift family)\b`), Central: rx(`^.{0,90}\b(?:found family|chosen family|makeshift family)\b`)},
	{Key: "political_drama", Label: "Political drama", Text: "Drama about politics, power, government and political intrigue.", Phrases: []string{"political drama", "political intrigue"}, Aliases: []string{"political drama", "politics", "political corruption"}, Lanes: []string{"political drama", "politics"}, Evidence: []string{"political drama", "politics", "political intrigue", "government conspiracy", "political corruption"}, Pattern: rx(`\b(?:politic\w*|prime minister|parliament|government conspiracy|white house)\b`), Central: rx(`^.{0,90}\b(?:politic\w*|prime minister|parliament|government conspiracy|white house)\b`)},
	{Key: "space_exploration", Label: "Space exploration", Text: "TV series about exploring space, starship missions and voyages to new worlds.", Phrases: []string{"space exploration", "exploring space"}, Aliases: []string{"space exploration", "space travel", "astronaut", "outer space"}, Lanes: []string{"space exploration", "space travel"}, Evidence: []string{"space exploration", "space travel"}, Pattern: rx(`\b(?:explor\w* (?:new )?(?:worlds|planets)|space mission|interstellar voyage|starship.{0,70}(?:mission|explor\w*))\b`), Central: rx(`^.{0,90}\b(?:explor\w* (?:new )?(?:worlds|planets)|space mission|interstellar voyage|starship.{0,70}(?:mission|explor\w*))\b`)},
}

var Genres = map[int]string{10759: "Action & Adventure", 16: "Animation", 35: "Comedy", 80: "Crime", 99: "Documentary", 18: "Drama", 10751: "Family", 10762: "Kids", 9648: "Mystery", 10763: "News", 10764: "Reality", 10765: "Sci-Fi & Fantasy", 10766: "Soap", 10767: "Talk", 10768: "War & Politics", 37: "Western"}
var Languages = []string{"en", "ko", "ja", "zh", "th", "fr", "es", "de", "it", "pt", "hi", "tr", "ar", "nl", "sv", "da", "no", "fi", "pl", "ru", "he", "id", "vi", "tl", "ms", "uk", "cs", "el", "hu", "ro", "fa", "bn", "ta", "te"}

type Filters struct {
	Theme            *string `json:"theme"`
	CentralThemeOnly bool    `json:"centralThemeOnly"`
	Genres           []int   `json:"genres"`
	OriginalLanguage *string `json:"originalLanguage"`
	YearFrom         *int    `json:"yearFrom"`
	YearTo           *int    `json:"yearTo"`
	ExcludeTeenFocus bool    `json:"excludeTeenFocus"`
	HideInLibrary    bool    `json:"hideInLibrary"`
}

type Request struct {
	Kind     domain.MediaKind
	Query    string
	Seed     *domain.ExternalRef
	Filters  Filters
	Limit    int
	Explicit map[string]bool
}
type Applied struct {
	Query                 string              `json:"query"`
	RankingText           string              `json:"rankingText"`
	Seed                  *domain.ExternalRef `json:"seed,omitempty"`
	Filters               Filters             `json:"filters"`
	InterpretationVersion string              `json:"interpretationVersion"`
	Limit                 int                 `json:"limit"`
}
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type Evidence struct {
	State string
	Field string
	Value string
}
type Facts struct {
	IDs                                           domain.ExternalIDs
	Title, Overview, PosterPath, OriginalLanguage string
	Year                                          int
	GenreIDs                                      []int
	GenreNames, Keywords                          []string
	KeywordIDs                                    map[string]int64
	FetchedAt                                     time.Time
}

func FindTheme(key string) *Theme {
	for i := range Themes {
		if Themes[i].Key == key {
			return &Themes[i]
		}
	}
	return nil
}
func normalized(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("-", " ", "+", "", "’", "'").Replace(strings.ToLower(s))), " ")
}
func phrase(s, p string) bool { return rx(`\b` + regexp.QuoteMeta(p) + `\b`).MatchString(s) }

func Normalize(r Request, now time.Time) (Applied, []Warning, error) {
	a := Applied{Query: strings.TrimSpace(r.Query), Seed: r.Seed, Filters: r.Filters, InterpretationVersion: RulesVersion, Limit: r.Limit}
	if r.Kind != domain.KindSeries || utf8.RuneCountInString(a.Query) > 500 {
		return a, nil, fmt.Errorf("recommendations require series and a query of at most 500 characters")
	}
	if a.Limit == 0 {
		a.Limit = 20
	}
	if a.Limit < 1 || a.Limit > 20 {
		return a, nil, fmt.Errorf("limit must be 1–20")
	}
	if !r.Explicit["hideInLibrary"] {
		a.Filters.HideInLibrary = true
	}
	s := normalized(a.Query)
	residual := s
	var matched []string
	for _, t := range Themes {
		for _, p := range t.Phrases {
			if phrase(s, p) {
				matched = append(matched, t.Key)
				residual = rx(`\b`+regexp.QuoteMeta(p)+`\b`).ReplaceAllString(residual, " ")
				break
			}
		}
	}
	if !r.Explicit["theme"] && len(matched) == 1 {
		a.Filters.Theme = &matched[0]
	}
	teen := phrase(s, "no teen dramas") || phrase(s, "no teen drama")
	residual = rx(`\bno teen dramas?\b`).ReplaceAllString(residual, " ")
	if !r.Explicit["excludeTeenFocus"] {
		a.Filters.ExcludeTeenFocus = teen
	}
	for id, name := range Genres {
		p := normalized(name)
		if phrase(s, p) {
			if !r.Explicit["genres"] {
				a.Filters.Genres = append(a.Filters.Genres, id)
			}
			residual = rx(`\b`+regexp.QuoteMeta(p)+`\b`).ReplaceAllString(residual, " ")
		}
	}
	slices.Sort(a.Filters.Genres)
	a.Filters.Genres = slices.Compact(a.Filters.Genres)
	if len(a.Filters.Genres) > 3 {
		return a, nil, fmt.Errorf("choose at most three genres")
	}
	for _, id := range a.Filters.Genres {
		if _, ok := Genres[id]; !ok {
			return a, nil, fmt.Errorf("unsupported TV genre")
		}
	}
	if a.Filters.Theme != nil && FindTheme(*a.Filters.Theme) == nil {
		return a, nil, fmt.Errorf("unsupported theme")
	}
	if a.Filters.CentralThemeOnly && a.Filters.Theme == nil {
		return a, nil, fmt.Errorf("central theme requires a theme")
	}
	if a.Filters.OriginalLanguage != nil && !slices.Contains(Languages, *a.Filters.OriginalLanguage) {
		return a, nil, fmt.Errorf("unsupported original language")
	}
	for _, y := range []*int{a.Filters.YearFrom, a.Filters.YearTo} {
		if y != nil && (*y < 1900 || *y > now.Year()+5) {
			return a, nil, fmt.Errorf("year is outside supported range")
		}
	}
	if a.Filters.YearFrom != nil && a.Filters.YearTo != nil && *a.Filters.YearFrom > *a.Filters.YearTo {
		return a, nil, fmt.Errorf("year range is reversed")
	}
	residual = rx(`\b(?:tv|series|shows|show|themed)\b`).ReplaceAllString(residual, " ")
	residual = strings.Trim(strings.Join(strings.Fields(residual), " "), " ,.")
	var warnings []Warning
	if (!r.Explicit["theme"] && len(matched) > 1) || rx(`\b(?:not|without|except|no)\b`).MatchString(residual) {
		warnings = append(warnings, Warning{"needs_refinement", "Choose one theme and use the visible filters for exclusions."})
	}
	if a.Filters.Theme == nil && a.Seed == nil {
		warnings = append(warnings, Warning{"needs_refinement", "Choose a supported theme or select a series with More like this."})
	}
	if a.Filters.Theme != nil {
		a.RankingText = FindTheme(*a.Filters.Theme).Text
	}
	if residual != "" {
		a.RankingText = strings.TrimSpace(a.RankingText + " " + residual)
	}
	return a, warnings, nil
}

var teenPattern = rx(`\b(?:teenagers?|teens|high.school students?|teenage protagonist|schoolboys?|schoolgirls?)\b`)
var teenKeywords = []string{"teen drama", "teenager", "teenagers", "high school", "high school student", "high school students", "school romance", "school life", "lgbt teen", "teen coming of age"}

func TeenFocus(f Facts) bool {
	if slices.Contains(f.GenreIDs, 10762) {
		return true
	}
	for _, k := range f.Keywords {
		if slices.Contains(teenKeywords, normalized(k)) {
			return true
		}
	}
	return teenPattern.MatchString(f.Overview)
}

func Evaluate(f Facts, key string) Evidence {
	t := FindTheme(key)
	if t == nil {
		return Evidence{State: "unknown"}
	}
	if key == "political_drama" && !slices.Contains(f.GenreIDs, 18) {
		return Evidence{State: "unknown"}
	}
	for _, k := range f.Keywords {
		if slices.Contains(t.Evidence, normalized(k)) {
			if t.Central.MatchString(f.Overview) {
				return Evidence{"central", "overview", Excerpt(f.Overview, 320)}
			}
			return Evidence{"present", "keywords", k}
		}
	}
	contrary := map[string]string{"gay_male": `(?:not a gay (?:story|romance)|not gay romance)`, "lesbian": `not a lesbian (?:story|romance)`, "lgbtq": `not a queer (?:story|romance)`}
	if pattern := contrary[key]; pattern != "" && rx(pattern).MatchString(f.Overview) {
		return Evidence{State: "contradicted"}
	}
	if t.Central.MatchString(f.Overview) {
		return Evidence{"central", "overview", Excerpt(f.Overview, 320)}
	}
	for _, k := range f.Keywords {
		if slices.Contains(t.Evidence, normalized(k)) {
			return Evidence{"present", "keywords", k}
		}
	}
	if t.Pattern.MatchString(f.Overview) {
		return Evidence{"present", "overview", Excerpt(f.Overview, 320)}
	}
	return Evidence{State: "unknown"}
}

func Eligible(f Facts, filters Filters) (Evidence, bool) {
	e := Evidence{State: "unknown"}
	if filters.Theme != nil {
		e = Evaluate(f, *filters.Theme)
		if e.State != "central" && (e.State != "present" || filters.CentralThemeOnly) {
			return e, false
		}
	}
	if filters.OriginalLanguage != nil && f.OriginalLanguage != *filters.OriginalLanguage {
		return e, false
	}
	if filters.YearFrom != nil && (f.Year == 0 || f.Year < *filters.YearFrom) || filters.YearTo != nil && (f.Year == 0 || f.Year > *filters.YearTo) {
		return e, false
	}
	if len(filters.Genres) > 0 {
		found := false
		for _, g := range filters.Genres {
			found = found || slices.Contains(f.GenreIDs, g)
		}
		if !found {
			return e, false
		}
	}
	if filters.ExcludeTeenFocus && TeenFocus(f) {
		return e, false
	}
	return e, true
}

func Excerpt(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
func UTF8Prefix(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
func SummaryText(s string) string { return UTF8Prefix(strings.TrimSpace(s), 8192) }
func selectedKeywords(f Facts) []string {
	byName := map[string]string{}
	for _, k := range f.Keywords {
		byName[normalized(k)] = k
	}
	priority := []string{}
	for _, t := range Themes {
		priority = append(priority, t.Aliases...)
		priority = append(priority, t.Evidence...)
	}
	priority = append(priority, "male friendship", "female friendship", "friendship", "friends to lovers", "rivals to lovers", "enemies to lovers", "university", "family relationships", "coming out", "romance", "relationship", "exploration", "space opera")
	keys := []string{}
	for k := range byName {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	priority = append(priority, keys...)
	out := []string{}
	seen := map[string]bool{}
	for _, k := range priority {
		if value, ok := byName[k]; ok && !seen[k] {
			seen[k] = true
			out = append(out, UTF8Prefix(value, 128))
			if len(out) == 16 {
				break
			}
		}
	}
	return out
}
func Text(f Facts) string {
	genres := []string{}
	for _, g := range f.GenreNames[:min(16, len(f.GenreNames))] {
		genres = append(genres, UTF8Prefix(g, 64))
	}
	prefix := "Keywords: " + strings.Join(selectedKeywords(f), ", ") + ". Genres: " + strings.Join(genres, ", ") + ". Synopsis: "
	return prefix + UTF8Prefix(f.Overview, 8192-len(prefix))
}
func UsableSeed(f Facts) bool {
	return len(rx(`\b\w+\b`).FindAllString(f.Overview+" "+strings.Join(selectedKeywords(f), " "), -1)) >= 8
}
func SummaryScore(s, key string) int {
	if key != "gay_male" {
		t := FindTheme(key)
		if t == nil {
			return 0
		}
		score := 0
		if t.Pattern.MatchString(s) {
			score += 4
		}
		for _, alias := range t.Evidence {
			if phrase(normalized(s), normalized(alias)) {
				score += 2
				break
			}
		}
		return score
	}
	score := 0
	for i, p := range []string{`\b(?:gay|homosexual|same.sex|boys.? love|two men|two boys|male couple|men in love)\b`, `\b(?:queer|lgbtq?)\b`, `\b(?:romance|relationship|fall in love)\b`} {
		if rx(p).MatchString(s) {
			score += []int{4, 2, 1}[i]
		}
	}
	return score
}
