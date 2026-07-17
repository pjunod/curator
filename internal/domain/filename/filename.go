// Package filename extracts season/episode structure from on-disk file
// names, for the Phase 1 disk scan and reconcile loop.
//
// This is deliberately NOT the release-name parser (Phase 2, blueprint §4.3):
// it answers one narrow question — "which episodes does this library file
// cover?" — for files that already live in a curated library layout. It is a
// pure function, table-tested, and will be subsumed by the full parser later.
package filename

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// VideoExtensions are the file extensions the scanner treats as media.
var VideoExtensions = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".m4v": true,
	".mov": true, ".wmv": true, ".ts": true, ".webm": true,
}

// IsVideo reports whether path has a known video extension.
func IsVideo(path string) bool {
	return VideoExtensions[strings.ToLower(filepath.Ext(path))]
}

// Episodes is the result of extraction: one season, one or more episodes.
type Episodes struct {
	Season   int
	Episodes []int
}

var (
	// S01E02, s01e02e03, S01E01-E03, S1E1
	sxxExx = regexp.MustCompile(`(?i)\bS(\d{1,2})[ ._-]?E(\d{1,3})((?:[ ._-]?E\d{1,3}|-\d{1,3})*)\b`)
	// trailing E03 / -03 continuations captured above, split here
	contin = regexp.MustCompile(`(?i)[E-](\d{1,3})`)
	// 1x02, 01x02-03
	nxx = regexp.MustCompile(`(?i)\b(\d{1,2})x(\d{2,3})(?:-(\d{2,3}))?\b`)
)

// Extract parses a file name (base name or full path) and returns the
// episodes it covers. ok is false when no season/episode pattern is found.
// Multi-episode files (S01E01E02, S01E01-03, 1x01-02) return every episode
// in the run.
func Extract(name string) (Episodes, bool) {
	base := filepath.Base(name)

	if m := sxxExx.FindStringSubmatch(base); m != nil {
		season, _ := strconv.Atoi(m[1])
		first, _ := strconv.Atoi(m[2])
		eps := []int{first}
		for _, c := range contin.FindAllStringSubmatch(m[3], -1) {
			n, _ := strconv.Atoi(c[1])
			eps = append(eps, n)
		}
		// A dash continuation like S01E01-03 means a range.
		eps = expandRuns(eps)
		return Episodes{Season: season, Episodes: eps}, true
	}

	if m := nxx.FindStringSubmatch(base); m != nil {
		season, _ := strconv.Atoi(m[1])
		first, _ := strconv.Atoi(m[2])
		eps := []int{first}
		if m[3] != "" {
			last, _ := strconv.Atoi(m[3])
			eps = append(eps, last)
			eps = expandRuns(eps)
		}
		return Episodes{Season: season, Episodes: eps}, true
	}

	return Episodes{}, false
}

// expandRuns turns [1,3] into [1,2,3] when the list is a plausible ascending
// range, and dedupes/sorts otherwise. Explicit lists like E01E02E05 are kept
// as-is (5 is not adjacent, so no fill).
func expandRuns(eps []int) []int {
	if len(eps) == 2 && eps[1] > eps[0]+1 && eps[1]-eps[0] <= 12 {
		out := make([]int, 0, eps[1]-eps[0]+1)
		for e := eps[0]; e <= eps[1]; e++ {
			out = append(out, e)
		}
		return out
	}
	seen := map[int]bool{}
	out := eps[:0]
	for _, e := range eps {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}
