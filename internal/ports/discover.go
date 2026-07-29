package ports

import (
	"context"
	"errors"

	"github.com/monarr-media/monarr/internal/domain"
)

// ErrUnknownList is returned by a DiscoverProvider asked for a list id it
// does not publish. Callers turn it into a 400, not a 500: an unknown list
// means the request named something that was never on offer.
var ErrUnknownList = errors.New("unknown discover list")

// DiscoverList is one curated row a provider can serve — "trending this
// week", "in theaters now". It is metadata about the row, not its contents;
// the contents come from DiscoverProvider.Discover, which is where the cost
// is (ADR 0015).
type DiscoverList struct {
	// ID is a stable slug, unique across every provider
	// ("tmdb-trending-movies"). It appears in URLs and in the UI's cache
	// keys, so renaming one is a breaking change to a bookmark.
	ID string
	// Title is the row heading, e.g. "Trending this week".
	Title string
	// Blurb is one line under the heading saying what the row actually
	// measures. It is mandatory in spirit: "trending" means something
	// different at each provider — TMDB counts lookups, Trakt counts
	// playback — and a row that does not say which is a number nobody can
	// interpret.
	Blurb string
	// Kind is the media kind every result in the row will carry. A row is
	// never mixed: the UI filters by kind and an add needs a kind-matching
	// root folder (ADR 0009).
	Kind domain.MediaKind
	// Source names the provider ("tmdb", "trakt") for display and logs.
	Source string
}

// DiscoverProvider serves curated lists of media for browsing — the read-only
// half of discovery, as opposed to import lists, which exist to add things on
// a timer (ADR 0015).
//
// The provider publishes its own catalogue rather than the application
// hardcoding row ids, which is what makes a second provider additive: its
// rows appear because Lists names them, not because the UI learned about it.
type DiscoverProvider interface {
	// Name identifies the provider in logs and in DiscoverList.Source.
	Name() string
	// Lists is the catalogue this provider can serve. Static — it describes
	// capability, not availability, so it makes no request and takes no
	// context. Availability is Configured.
	Lists() []DiscoverList
	// Configured reports whether the provider has what it needs to answer
	// (typically an API key). A provider that is not configured is dropped
	// from the catalogue entirely rather than offering rows that always
	// fail.
	Configured(ctx context.Context) bool
	// Discover returns one page of a list. page is 1-based; a provider with
	// no paging ignores anything above 1 by returning nothing, so a caller
	// paging past the end gets an empty row rather than a repeat of page 1.
	// An unpublished list id returns ErrUnknownList.
	Discover(ctx context.Context, listID string, page int) ([]SearchResult, error)
}
