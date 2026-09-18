package identity

import (
	"errors"
	"testing"

	"github.com/pjunod/monarr/internal/domain"
)

func TestParseInput(t *testing.T) {
	tests := []struct {
		in       string
		kind     domain.MediaKind
		provider string
		value    string
		known    bool
		err      error
	}{
		{" tvdb:414217 ", domain.KindSeries, "tvdb", "414217", true, nil},
		{"TVDBID:414217", domain.KindSeries, "tvdb", "414217", true, nil},
		{"imdb:16867040", domain.KindSeries, "imdb", "tt16867040", true, nil},
		{"IMDbID:tt0137523", domain.KindMovie, "imdb", "tt0137523", true, nil},
		{"tt0000001", domain.KindMovie, "imdb", "tt0000001", true, nil},
		{"imdb:42", domain.KindMovie, "imdb", "tt0000042", true, nil},
		{"tmdb:550", domain.KindMovie, "tmdb", "550", true, nil},
		{"1917", domain.KindMovie, "", "", false, nil},
		{"ttwrong", domain.KindMovie, "", "", false, nil},
		{"tvdb:abc", domain.KindSeries, "", "", true, ErrInvalidExternalID},
		{"imdb:", domain.KindSeries, "", "", true, ErrInvalidExternalID},
		{"tvdb:-1", domain.KindSeries, "", "", true, ErrInvalidExternalID},
		{"imdb:tt1234567890123", domain.KindMovie, "", "", true, ErrInvalidExternalID},
		{"tvdb:12", domain.KindMovie, "", "", true, ErrWrongMediaKind},
		{"imdb:tt0137523", domain.KindBook, "", "", true, ErrWrongMediaKind},
	}
	for _, tt := range tests {
		got, known, err := ParseInput(tt.in, tt.kind)
		if known != tt.known || !errors.Is(err, tt.err) || got.Provider != tt.provider || got.Value != tt.value {
			t.Errorf("ParseInput(%q, %s) = %+v, %v, %v", tt.in, tt.kind, got, known, err)
		}
	}
}
