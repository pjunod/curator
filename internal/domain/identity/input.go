// Package identity parses exact external-ID input without network access.
package identity

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/pjunod/monarr/internal/domain"
)

var (
	ErrInvalidExternalID = errors.New("invalid external id")
	ErrWrongMediaKind    = errors.New("external id is not valid for this media kind")
)

// ParseInput returns a validated external reference, whether the input used
// recognized ID syntax, and an error. recognized remains true for malformed
// prefixed input so callers never fall back to a text search.
func ParseInput(raw string, kind domain.MediaKind) (domain.ExternalRef, bool, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return domain.ExternalRef{}, false, nil
	}
	lower := strings.ToLower(s)
	provider, value, recognized := "", "", false
	if i := strings.IndexByte(lower, ':'); i >= 0 {
		prefix := strings.TrimSpace(lower[:i])
		switch prefix {
		case "tvdb", "tvdbid":
			provider, recognized = "tvdb", true
		case "imdb", "imdbid":
			provider, recognized = "imdb", true
		case "tmdb", "tmdbid":
			provider, recognized = "tmdb", true
		default:
			return domain.ExternalRef{}, false, nil
		}
		value = strings.TrimSpace(s[i+1:])
	} else if strings.HasPrefix(lower, "tt") && allDigits(lower[2:]) && len(lower[2:]) >= 7 && len(lower[2:]) <= 12 {
		provider, value, recognized = "imdb", lower, true
	} else {
		return domain.ExternalRef{}, false, nil
	}

	if kind == domain.KindBook {
		return domain.ExternalRef{}, recognized, fmt.Errorf("%w: %s", ErrWrongMediaKind, kind)
	}
	if provider == "tvdb" && kind != domain.KindSeries {
		return domain.ExternalRef{}, recognized, fmt.Errorf("%w: tvdb requires series", ErrWrongMediaKind)
	}
	if strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return domain.ExternalRef{}, recognized, ErrInvalidExternalID
	}

	if provider == "imdb" {
		v := strings.ToLower(value)
		v = strings.TrimPrefix(v, "tt")
		if !allDigits(v) || len(v) == 0 || len(v) > 12 {
			return domain.ExternalRef{}, recognized, ErrInvalidExternalID
		}
		if len(v) < 7 {
			v = strings.Repeat("0", 7-len(v)) + v
		}
		return domain.ExternalRef{Provider: provider, Value: "tt" + v}, true, nil
	}
	if !allDigits(value) {
		return domain.ExternalRef{}, recognized, ErrInvalidExternalID
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		return domain.ExternalRef{}, recognized, ErrInvalidExternalID
	}
	return domain.ExternalRef{Provider: provider, Value: strconv.FormatInt(n, 10)}, true, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
