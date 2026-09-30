package domain

import "strings"

// ExternalIDsConflict compares only namespaces verified on both records.
func ExternalIDsConflict(a, b ExternalIDs) bool {
	return a.TMDB > 0 && b.TMDB > 0 && a.TMDB != b.TMDB || a.TVDB > 0 && b.TVDB > 0 && a.TVDB != b.TVDB || a.IMDB != "" && b.IMDB != "" && !strings.EqualFold(a.IMDB, b.IMDB)
}
