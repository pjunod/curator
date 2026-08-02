// Package ports defines the driven-port interfaces between the application
// core and the outside world (blueprint §5). Adapters live under
// internal/adapters and are the only code implementing these against real
// services. Dependency rule (enforced by internal/arch_test.go): this
// package imports domain and stdlib only.
package ports

import (
	"context"
	"errors"

	"github.com/pjunod/monarr/internal/domain"
)

// ErrProviderNotConfigured is returned when a provider is called before its
// credentials are set (e.g. no TMDB API key in settings yet).
var ErrProviderNotConfigured = errors.New("metadata provider not configured")

// SearchResult is one candidate from a metadata search. TMDBID identifies
// movies/series; OLID identifies books (ADR 0006).
type SearchResult struct {
	Kind   domain.MediaKind
	TMDBID int64
	// TVDBID identifies a series reached through the provider chain (ADR
	// 0011). It is the key for anything TMDB did not supply, because it is
	// the one id every series source agrees on — TVmaze publishes TheTVDB's
	// ids, so a library built with no TVDB key is still keyed for one.
	TVDBID int64
	// Source names the provider that produced this result ("tmdb", "tvmaze",
	// …), for display and for logs. Identity is the ids, never this.
	Source string
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

// SeriesProvider is an additional source of series identity and episodes,
// consulted when the one before it in the chain has nothing usable (ADR
// 0011). TMDB is not one of these — it is the always-present last link,
// reached through MetadataProvider.
//
// Everything here is keyed on TheTVDB's series id rather than the provider's
// own. That is the point of the interface: release names carry TVDB episode
// numbering, monarr's schema already has a tvdb_id column, and a provider
// that cannot say which TVDB series it is describing cannot be checked
// against another one. A provider with no TVDB id for a show returns nothing
// for it and the chain moves on.
type SeriesProvider interface {
	// Name identifies the provider in logs and in the UI ("tvmaze").
	Name() string
	// SearchSeries returns candidates, each carrying a non-zero TVDBID.
	SearchSeries(ctx context.Context, query string) ([]SearchResult, error)
	// GetSeriesByTVDB hydrates one series — seasons and episodes populated,
	// library-placement fields left zero, exactly as MetadataProvider does.
	GetSeriesByTVDB(ctx context.Context, tvdbID int64) (domain.MediaItem, error)
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

// Airing is a series' broadcast slot as its source publishes it: a
// network-local wall-clock time, the timezone that clock is in, and the
// channel's display name. Any field may be empty, and empty means unknown —
// never a default (ADR 0016).
type Airing struct {
	Time     string // "21:00", 24h network-local; "" = unknown
	Timezone string // IANA name, e.g. "America/New_York"; "" = unknown
	Network  string // display name, e.g. "HBO", "Netflix"; "" = unknown
}

// AiringProvider supplies the show-level schedule the calendar composes air
// times from (ADR 0016). TVmaze implements it keylessly; a keyed TheTVDB
// adapter could implement it later, which is why nothing here names a
// provider.
//
// Optional in exactly the sense RatingsProvider is: callers treat
// ErrProviderNotConfigured, an unknown show, and an upstream failure alike as
// "no schedule known", never as an error. Both ids are passed because lookup
// keys differ per provider; a provider uses whichever it can and returns the
// zero value when it can use neither.
type AiringProvider interface {
	Airing(ctx context.Context, tvdbID int64, imdbID string) (Airing, error)
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
