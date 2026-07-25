package matcher

import "testing"

// The inverted-article convention — "Fall, The" — is what Plex, Emby, Kodi
// and most rename scripts write. A library full of it used to match nothing:
// "Fall, The" normalized to "fall the" while TMDB's "The Fall" became "fall".
func TestNormalizeTitleHandlesSortNames(t *testing.T) {
	for _, tc := range []struct{ sortName, natural string }{
		{"Fall, The", "The Fall"},
		{"Matrix, The", "The Matrix"},
		{"Lord of the Rings, The", "The Lord of the Rings"},
		{"Thing, A", "A Thing"},
		{"Untouchables, The", "The Untouchables"},
		{"American Tail, An", "An American Tail"},
		// Casing and spacing around the comma vary by tool.
		{"Fall,The", "the fall"},
		{"FALL, THE", "The Fall"},
	} {
		if got, want := NormalizeTitle(tc.sortName), NormalizeTitle(tc.natural); got != want {
			t.Errorf("NormalizeTitle(%q) = %q, want it equal to NormalizeTitle(%q) = %q",
				tc.sortName, got, tc.natural, want)
		}
	}
}

// The comma is required, because a title can legitimately end in an article
// and rewriting those would invent matches that are not real.
func TestNormalizeTitleLeavesTrailingArticlesAloneWithoutAComma(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// Interior articles were never stripped and still are not.
		{"Nothing But the Truth", "nothing but the truth"},
		// No comma: "the" is the last word of the title itself, not an
		// inverted article, so it stays.
		{"Da 5 Bloods the", "da 5 bloods the"},
		{"Waiting for the", "waiting for the"},
	} {
		if got := NormalizeTitle(tc.in); got != tc.want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A bare article must not normalize to nothing — an empty title would match
// everything.
func TestNormalizeTitleKeepsBareArticles(t *testing.T) {
	for _, in := range []string{"The", "A", "An"} {
		if got := NormalizeTitle(in); got == "" {
			t.Errorf("NormalizeTitle(%q) = %q — an empty title matches everything", in, got)
		}
	}
}
