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

// SearchResult is one candidate from a metadata search. TMDBID identifies
// movies/series; OLID identifies books (ADR 0006).
type SearchResult struct {
	Kind   domain.MediaKind
	TMDBID int64
	OLID   string
	Author string
	Title  string
	// AltTitles are other names the same work is released under: the
	// original-language title, a regional retitle, a festival title. One
	// work having several names is ordinary — "Cunk on Life" is released as
	// "Cunk's Quest for Meaning" — and a library folder may well be named
	// after any of them, so matching that compares only Title fails on
	// perfectly correct data.
	//
	// Providers fill this in as far as it is free; AltTitleProvider fetches
	// the rest on demand.
	AltTitles  []string
	Year       int
	Overview   string
	PosterPath string
}

// AltTitleProvider is an optional capability of a MetadataProvider: the
// alternate names one work is released under, fetched for a single title.
//
// Separate from SearchResult because it costs a request per candidate.
// Callers should ask only when a cheap match has already failed — the point
// of the interface being optional is that adoption works without it, just
// with a folder or two more in review.
type AltTitleProvider interface {
	AlternativeTitles(ctx context.Context, kind domain.MediaKind, tmdbID int64) ([]string, error)
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

// RatingsProvider enriches an item with additional labeled ratings keyed
// by IMDb id — the OMDb adapter supplies Rotten Tomatoes, IMDb, and
// Metacritic. Optional: callers treat ErrProviderNotConfigured (no key)
// and failures as "no extra ratings", never as an error.
type RatingsProvider interface {
	Ratings(ctx context.Context, imdbID string) ([]domain.Rating, error)
}

// BookProvider hydrates book entries (ADR 0006) — a separate port because
// book metadata comes from a different upstream (Open Library) with its own
// identity scheme (work OLID), auth (none), and rate policy.
//
// GetBook returns a hydrated domain.MediaItem (Kind=book, Author set,
// ISBN/OLID populated where known) with library-placement fields left zero.
type BookProvider interface {
	SearchBooks(ctx context.Context, query string) ([]SearchResult, error)
	GetBook(ctx context.Context, olid string) (domain.MediaItem, error)
}
