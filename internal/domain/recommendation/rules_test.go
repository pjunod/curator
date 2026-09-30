package recommendation

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pjunod/monarr/internal/domain"
)

func TestInterpretationAndExplicitClears(t *testing.T) {
	for _, tc := range []struct {
		query, key string
		refine     bool
	}{{"gay-themed TV series", "gay_male", false}, {"queer TV series", "lgbtq", false}, {"lesbian romance", "lesbian", false}, {"gay stories without romance", "gay_male", true}, {"gay coming of age", "", true}, {"space exploration", "space_exploration", false}} {
		a, w, e := Normalize(Request{Kind: domain.KindSeries, Query: tc.query}, time.Now())
		if e != nil {
			t.Fatal(e)
		}
		if (len(w) > 0) != tc.refine {
			t.Errorf("%q warnings %+v", tc.query, w)
		}
		if tc.key != "" && (a.Filters.Theme == nil || *a.Filters.Theme != tc.key) {
			t.Errorf("%q applied %+v", tc.query, a)
		}
	}
	a, w, e := Normalize(Request{Kind: domain.KindSeries, Query: "gay TV series, no teen dramas", Seed: &domain.ExternalRef{Provider: "tmdb", Value: "1"}, Explicit: map[string]bool{"theme": true, "excludeTeenFocus": true}}, time.Now())
	if e != nil || len(w) > 0 || a.Filters.Theme != nil || a.Filters.ExcludeTeenFocus || a.RankingText != "" {
		t.Fatalf("explicit clear: %+v %+v %v", a, w, e)
	}
}

func TestPoliticalThemeRequiresDramaAndSpecificEvidenceWins(t *testing.T) {
	f := Facts{Keywords: []string{"politics"}, Overview: "A political discussion show."}
	if got := Evaluate(f, "political_drama"); got.State != "unknown" {
		t.Fatal(got)
	}
	f.GenreIDs = []int{18}
	if got := Evaluate(f, "political_drama"); got.State == "unknown" {
		t.Fatal(got)
	}
	f = Facts{Keywords: []string{"gay theme"}, Overview: "A detective says this is not gay romance."}
	if got := Evaluate(f, "gay_male"); got.State != "present" {
		t.Fatal(got)
	}
	if got := Evaluate(Facts{Overview: "Two women fall in love with the same man."}, "lesbian"); got.State != "unknown" {
		t.Fatal(got)
	}
}

func TestSummaryTextIsUTF8BoundedAndPrioritizesThemeKeywords(t *testing.T) {
	f := Facts{Overview: strings.Repeat("世界", 5000)}
	for i := 0; i < 20; i++ {
		f.Keywords = append(f.Keywords, strings.Repeat("a", 130))
	}
	f.Keywords = append(f.Keywords, "gay theme")
	text := Text(f)
	if len(text) > 8192 || !utf8.ValidString(text) || !strings.Contains(text, "gay theme") {
		t.Fatalf("bounded text: %d bytes", len(text))
	}
	politics := SummaryScore("A prime minister faces a government conspiracy.", "political_drama")
	if politics <= SummaryScore("Gay men fall in love.", "political_drama") {
		t.Fatal("political preselection uses unrelated evidence")
	}
}
func TestEvidenceCannotBeEstablishedByBroadKeywordsOrDemographics(t *testing.T) {
	for _, f := range []Facts{{Keywords: []string{"lgbt"}, Overview: "A detective investigates a theft."}, {Overview: "Two men fall in love with the same woman."}, {Overview: "A gay actor plays a detective."}, {Overview: "This is not gay romance."}} {
		if e := Evaluate(f, "gay_male"); e.State == "present" || e.State == "central" {
			t.Fatalf("false evidence %+v: %+v", f, e)
		}
	}
	f := Facts{Keywords: []string{"gay theme"}, Overview: "Three friends navigate life as gay men in San Francisco."}
	if e := Evaluate(f, "gay_male"); e.State != "central" {
		t.Fatalf("central synopsis %+v", e)
	}
	f.Overview = "A mystery in a coastal town."
	if e := Evaluate(f, "gay_male"); e.State != "present" {
		t.Fatal(e)
	}
	key := "gay_male"
	if _, ok := Eligible(f, Filters{Theme: &key, CentralThemeOnly: true}); ok {
		t.Fatal("keyword alone claimed central")
	}
}
func TestMissingFactsFailRequiredFilters(t *testing.T) {
	language := "ko"
	year := 2020
	for _, f := range []Filters{{OriginalLanguage: &language}, {YearFrom: &year}, {Genres: []int{18}}} {
		if _, ok := Eligible(Facts{}, f); ok {
			t.Fatalf("missing facts admitted %+v", f)
		}
	}
}

func TestInvalidExplicitFiltersAndTeenEvidence(t *testing.T) {
	badTheme, badLanguage := "unsupported", "unsupported"
	old, future, from, to := 1899, 2100, 2024, 2020
	for _, filter := range []Filters{
		{Genres: []int{18, 35, 80, 99}}, {Genres: []int{-1}},
		{Theme: &badTheme}, {CentralThemeOnly: true}, {OriginalLanguage: &badLanguage},
		{YearFrom: &old}, {YearTo: &future}, {YearFrom: &from, YearTo: &to},
	} {
		if _, _, err := Normalize(Request{Kind: domain.KindSeries, Filters: filter}, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
			t.Fatalf("invalid filter accepted: %+v", filter)
		}
	}
	for _, facts := range []Facts{{GenreIDs: []int{10762}}, {Keywords: []string{"high school"}}, {Overview: "Teenagers navigate their first romance."}} {
		if !TeenFocus(facts) {
			t.Fatalf("teen evidence missed: %+v", facts)
		}
		if _, ok := Eligible(facts, Filters{ExcludeTeenFocus: true}); ok {
			t.Fatal("excluded teen focus admitted")
		}
	}
	if TeenFocus(Facts{Overview: "Adults revisit memories of their childhood."}) {
		t.Fatal("adult story excluded without teen evidence")
	}
}
