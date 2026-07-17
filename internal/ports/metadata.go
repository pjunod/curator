// Package ports defines the driven-port interfaces between the application
// core and the outside world (blueprint §5). Adapters live under
// internal/adapters and are the only code implementing these against real
// services. Dependency rule (enforced by internal/arch_test.go): this
// package imports domain and stdlib only.
package ports

import (
	"context"
	"errors"

	"github.com/monarr-media/monarr/internal/domain"
)

// ErrProviderNotConfigured is returned when a provider is called before its
// credentials are set (e.g. no TMDB API key in settings yet).
var ErrProviderNotConfigured = errors.New("metadata provider not configured")

// SearchResult is one candidate from a metadata search.
type SearchResult struct {
	Kind       domain.MediaKind
	TMDBID     int64
	Title      string
	Year       int
	Overview   string
	PosterPath string
}

// MetadataProvider hydrates library entries from an external source. The
// MVP adapter is TMDB, which serves both movies and TV (blueprint §4.2);
// books get their own adapter behind this same port in Phase 2.5.
//
// GetMovie and GetSeries return a fully hydrated domain.MediaItem (for
// series: seasons and episodes populated) with ID and library-placement
// fields (RootFolderID, Path, Monitored) left zero — those belong to the
// caller.
type MetadataProvider interface {
	SearchMovies(ctx context.Context, query string) ([]SearchResult, error)
	SearchSeries(ctx context.Context, query string) ([]SearchResult, error)
	GetMovie(ctx context.Context, tmdbID int64) (domain.MediaItem, error)
	GetSeries(ctx context.Context, tmdbID int64) (domain.MediaItem, error)
}
