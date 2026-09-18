package matcher

import "testing"

func TestTitleVariants(t *testing.T) {
	tests := []struct {
		in       string
		base     string
		country  string
		conflict bool
	}{
		{"Have I Got News for You US", "Have I Got News for You", "US", false},
		{"Have I Got News For You U. S.", "Have I Got News For You", "US", false},
		{"US Have I Got News for You", "Have I Got News for You", "US", false},
		{"Have I Got News [US] for You", "Have I Got News for You", "US", false},
		{"Show (United Kingdom)", "Show", "GB", false},
		{"Show (US) (UK)", "Show", "", true},
	}
	for _, tt := range tests {
		found := false
		for _, got := range TitleVariants(tt.in) {
			if got.Base == tt.base && got.Country == tt.country && got.Conflict == tt.conflict {
				found = true
			}
		}
		if !found {
			t.Errorf("TitleVariants(%q) = %+v", tt.in, TitleVariants(tt.in))
		}
	}
}

func TestTitleVariantsKeepsLiteralCountryWords(t *testing.T) {
	for _, title := range []string{"Us", "This Is Us", "The Last of Us", "Made in America", "Have I Got US News for You"} {
		got := TitleVariants(title)
		if len(got) == 0 || got[0].Rule != "literal" || got[0].Base != title {
			t.Errorf("literal %q was not preserved: %+v", title, got)
		}
	}
}
