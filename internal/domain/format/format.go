// Package format is the custom-format scoring engine (Phase 5): named
// regex specifications with scores, summed over a release title. Scores
// order otherwise-equal releases (and can bury bad ones with negatives) —
// the Sonarr/Radarr model, minus the per-profile score gates for now.
package format

import (
	"regexp"
	"sync"
)

// CustomFormat is one stored specification.
type CustomFormat struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Pattern string `json:"pattern"` // case-insensitive RE2
	Score   int    `json:"score"`
}

var cache sync.Map // pattern → *regexp.Regexp (nil for invalid)

func compiled(pattern string) *regexp.Regexp {
	if v, ok := cache.Load(pattern); ok {
		re, _ := v.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		re = nil // invalid patterns score nothing, forever
	}
	cache.Store(pattern, re)
	return re
}

// Score sums the scores of every format whose pattern matches the title.
func Score(title string, formats []CustomFormat) int {
	total := 0
	for _, f := range formats {
		if re := compiled(f.Pattern); re != nil && re.MatchString(title) {
			total += f.Score
		}
	}
	return total
}

// Matches returns the names of formats that hit, for UI display.
func Matches(title string, formats []CustomFormat) []string {
	var out []string
	for _, f := range formats {
		if re := compiled(f.Pattern); re != nil && re.MatchString(title) {
			out = append(out, f.Name)
		}
	}
	return out
}
