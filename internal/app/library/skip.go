package library

import (
	"context"
	"path"
	"strings"
)

// SkipPatternsSetting is the app_meta key holding the user's extra skip
// patterns, one per line. Built-in defaults always apply on top of it.
const SkipPatternsSetting = "scan_skip_patterns"

// defaultSkipPatterns are the directories real libraries actually contain
// that are never media. These are matched against the directory *name*, not
// its path, and are case-insensitive.
//
// The NAS entries matter more than they look: @eaDir (Synology thumbnails)
// and #recycle appear inside every share on the hardware a large share of
// homelab libraries run on, and without them every scan offers them forever.
var defaultSkipPatterns = []string{
	"@eaDir",
	"#recycle",
	"$RECYCLE.BIN",
	"System Volume Information",
	"lost+found",
	"extras",
	"featurettes",
	"behind the scenes",
	"deleted scenes",
	"sample",
	"samples",
	"subs",
	"subtitles",
	"trailers",
}

// skipMatcher decides whether a directory name should be skipped outright,
// before it is ever offered as an adoption candidate (ADR 0009 §4, the
// prospective half). Dot-directories are always skipped: they are
// configuration and metadata by convention on every platform Monarr runs on.
type skipMatcher struct {
	patterns []string // lowercased; glob syntax per path.Match
}

// newSkipMatcher combines the built-in defaults with user patterns. User
// input is one pattern per line; blanks and # comments are ignored so the
// setting can be annotated.
func newSkipMatcher(userPatterns string) skipMatcher {
	m := skipMatcher{patterns: make([]string, 0, len(defaultSkipPatterns)+8)}
	for _, p := range defaultSkipPatterns {
		m.patterns = append(m.patterns, strings.ToLower(p))
	}
	for _, line := range strings.Split(userPatterns, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m.patterns = append(m.patterns, strings.ToLower(line))
	}
	return m
}

// skip reports whether a directory with this name should be ignored.
func (m skipMatcher) skip(name string) bool {
	if name == "" {
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	lower := strings.ToLower(name)
	for _, p := range m.patterns {
		if p == lower {
			return true
		}
		// path.Match errors only on malformed patterns; a bad user pattern
		// simply never matches rather than breaking the scan.
		if ok, err := path.Match(p, lower); err == nil && ok {
			return true
		}
	}
	return false
}

// loadSkipMatcher reads the user's patterns from app_meta. A missing or
// unreadable setting yields the defaults alone — a scan must never fail
// because a preference could not be read.
func (s *Service) loadSkipMatcher(ctx context.Context) skipMatcher {
	raw, err := s.db.GetMeta(ctx, SkipPatternsSetting)
	if err != nil {
		return newSkipMatcher("")
	}
	return newSkipMatcher(raw)
}
