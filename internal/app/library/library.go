// Package library is the Phase 1 application service: add and browse media,
// manage root folders, and reconcile the database against what is actually
// on disk (blueprint §5.1 "library reconcile" — the system must tolerate
// humans touching the filesystem).
package library

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pjunod/monarr/internal/domain"
	identityinput "github.com/pjunod/monarr/internal/domain/identity"
	"github.com/pjunod/monarr/internal/domain/matcher"
	"github.com/pjunod/monarr/internal/domain/naming"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Errors surfaced to the API layer.
var (
	ErrAlreadyExists   = errors.New("item already in library")
	ErrUnsupportedKind = errors.New("kind not supported yet")
	ErrNotFound        = sqlite.ErrNotFound
	// ErrRootKindMismatch is returned when an item is placed in a root
	// declared to hold a different kind (ADR 0009). A mixed root never
	// produces this.
	ErrRootKindMismatch = errors.New("root folder holds a different media kind")
	// ErrNestedRoot is returned when a root would contain, or sit inside,
	// one that is already registered (ADR 0009 §3).
	ErrNestedRoot = errors.New("root folders may not nest")
	// ErrFolderConflict is returned when two folders claim one title and
	// both exist on disk. Distinct from ErrAlreadyExists because it is
	// resolvable by the user rather than simply wrong: they know which
	// folder holds the files.
	ErrFolderConflict = errors.New("another folder already holds this title")
	// ErrManualEntry is returned by provider-backed operations asked to act
	// on a record no provider backs (ADR 0012). Not a failure: the caller
	// skips rather than reports.
	ErrManualEntry = errors.New("manual entry has no metadata provider")
	// ErrInvalidInput is returned when the caller asked for something
	// malformed — a relative path, a kind that does not exist, a blank title.
	//
	// It exists because libraryErr's default branch is a 500, so any refusal
	// without a sentinel reported a server fault for a typo. Three did:
	// a relative manual-entry path, an unknown root-folder kind, and a blank
	// manual title (which was wrapped in ErrNotFound and answered 404).
	// Wrapping them all in one sentinel is deliberate — "the request is
	// wrong" is a single fact about the caller, and one match in the handler
	// cannot drift out of step with three.
	ErrInvalidInput         = errors.New("invalid input")
	ErrInvalidExternalID    = errors.New("invalid external id")
	ErrProviderUnavailable  = errors.New("metadata provider unavailable")
	ErrUnsupportedHydration = errors.New("external id resolved without a supported hydration route")
)

// IdentityConflictError carries the affected local rows when known.
type IdentityConflictError struct {
	ItemIDs []int64
	Cause   error
}

func (e *IdentityConflictError) Error() string {
	return fmt.Sprintf("identity conflict for library items %v", e.ItemIDs)
}
func (e *IdentityConflictError) Unwrap() error { return e.Cause }

// MediaAdded is published on the bus after a successful add.
type MediaAdded struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

// EventType implements bus.Event.
func (MediaAdded) EventType() string { return "media.added" }

// Service wires storage, the metadata providers, and the bus.
type Service struct {
	db   *sqlite.DB
	meta ports.MetadataProvider
	// series is the rest of the chain from ADR 0011, in order, consulted
	// only when the link before it has nothing usable. TMDB (meta) is the
	// always-present last link and is not in here.
	series  []ports.SeriesProvider
	books   ports.BookProvider
	ratings ports.RatingsProvider // optional (OMDb): RT/IMDb/Metacritic
	airing  ports.AiringProvider  // optional (TVmaze): broadcast slot, ADR 0016
	bus     *bus.Bus
	log     *slog.Logger
	// queue is optional: without it, work that would be enqueued runs
	// inline, so the queue stays an addition rather than a dependency.
	queue       JobEnqueuer
	identitySem chan struct{}
}

// New returns a Service. bus may be nil (tests).
func New(db *sqlite.DB, meta ports.MetadataProvider, b *bus.Bus, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, meta: meta, bus: b, log: log, identitySem: make(chan struct{}, 2)}
}

// WithBooks attaches the book metadata provider (ADR 0006) and returns s.
func (s *Service) WithBooks(books ports.BookProvider) *Service {
	s.books = books
	return s
}

// WithSeriesProviders attaches the extra links of the series chain in
// priority order (ADR 0011) and returns s. Omitting them leaves behaviour
// exactly as it was: TMDB alone.
func (s *Service) WithSeriesProviders(p ...ports.SeriesProvider) *Service {
	s.series = append(s.series, p...)
	return s
}

// WithRatings attaches the extra-ratings provider (OMDb) and returns s.
func (s *Service) WithRatings(rp ports.RatingsProvider) *Service {
	s.ratings = rp
	return s
}

// WithAiring attaches the broadcast-slot provider (TVmaze) and returns s.
// Omitting it leaves every item date-only, which is what the calendar showed
// before ADR 0016.
func (s *Service) WithAiring(ap ports.AiringProvider) *Service {
	s.airing = ap
	return s
}

// enrichAiring fills the item's broadcast slot — time, timezone, network —
// from the airing provider (ADR 0016). Series only: a movie has a release
// date and no weekly slot, and a book has neither.
//
// Best-effort by contract, exactly like enrichRatings: no provider, no ids,
// or an upstream failure all mean "no schedule known", and the refresh
// carries on. What it must NOT do is blank a slot it already knows because
// this one call failed — a show whose schedule was fetched last week keeps
// it until a successful fetch says otherwise.
func (s *Service) enrichAiring(ctx context.Context, item *domain.MediaItem, stored domain.MediaItem) {
	if item.Kind != domain.KindSeries {
		return
	}
	// Carry the stored slot forward first: the fresh record came from a
	// metadata provider that knows nothing about schedules, so its fields
	// are zero and would otherwise erase what is already there.
	item.AirsTime, item.AirsTimezone, item.Network = stored.AirsTime, stored.AirsTimezone, stored.Network
	if s.airing == nil || (item.IDs.TVDB == 0 && item.IDs.IMDB == "") {
		return
	}
	got, err := s.airing.Airing(ctx, item.IDs.TVDB, item.IDs.IMDB)
	if err != nil {
		if !errors.Is(err, ports.ErrProviderNotConfigured) {
			s.log.Debug("library: airing enrichment failed",
				"title", item.Title, "tvdb", item.IDs.TVDB, "err", err)
		}
		return
	}
	// An empty answer is authoritative: a show that moved to streaming has
	// no broadcast instant any more, and keeping the old one would keep
	// putting it on the calendar at 9 PM forever.
	item.AirsTime, item.AirsTimezone, item.Network = got.Time, got.Timezone, got.Network
}

// enrichRatings merges provider-external ratings (RT/IMDb/Metacritic via
// OMDb) into the item, keyed by IMDb id. Best-effort by contract: no key,
// no id, or an upstream hiccup all mean "no extra ratings".
func (s *Service) enrichRatings(ctx context.Context, item *domain.MediaItem) {
	if s.ratings == nil || item.IDs.IMDB == "" {
		return
	}
	extra, err := s.ratings.Ratings(ctx, item.IDs.IMDB)
	if err != nil {
		if !errors.Is(err, ports.ErrProviderNotConfigured) {
			s.log.Warn("library: ratings enrichment failed", "imdb", item.IDs.IMDB, "err", err)
		}
		return
	}
	have := map[string]bool{}
	for _, r := range item.Ratings {
		have[r.Source] = true
	}
	for _, r := range extra {
		if !have[r.Source] {
			item.Ratings = append(item.Ratings, r)
		}
	}
}

func (s *Service) publish(e bus.Event) {
	if s.bus != nil {
		s.bus.Publish(e)
	}
}

// ---- metadata search ----

// Search is the interactive lookup behind the Add page and the review row's
// search box. For series it returns the union of the chain (ADR 0011),
// nearest link first.
//
// Adoption does NOT come through here — it uses searchPrimary and reaches
// the rest of the chain only when nothing clears the bar. The difference is
// deliberate: a person typing a query wants everything anyone has, while a
// scan over six hundred folders should not spend a request per folder on
// providers the first one already answered for.
func (s *Service) Search(ctx context.Context, kind domain.MediaKind, query string) ([]ports.SearchResult, error) {
	ref, recognized, parseErr := identityinput.ParseInput(query, kind)
	if recognized {
		if parseErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidExternalID, parseErr)
		}
		return s.ResolveExternal(ctx, kind, ref)
	}
	if kind != domain.KindSeries {
		return s.searchPrimary(ctx, kind, query)
	}
	res, err := s.searchPrimary(ctx, kind, query)
	if err != nil {
		// TMDB being unreachable or unconfigured is not a reason to withhold
		// results a keyless provider can still supply.
		s.log.Debug("library: primary series search failed, trying the chain", "err", err)
		res = nil
	}
	merged := s.appendSeriesChain(ctx, query, res)
	if len(merged) == 0 && err != nil {
		return nil, err
	}
	return merged, nil
}

// ResolveExternal resolves one exact ID without a text search. Local hits
// remain available offline and ambiguity is returned rather than hidden.
func (s *Service) ResolveExternal(ctx context.Context, kind domain.MediaKind, ref domain.ExternalRef) ([]ports.SearchResult, error) {
	local, err := s.db.FindMediaItemsByExternalID(ctx, kind, ref)
	if err != nil {
		return nil, err
	}
	if len(local) > 1 {
		ids := make([]int64, len(local))
		for i, item := range local {
			ids[i] = item.ID
		}
		return nil, &IdentityConflictError{ItemIDs: ids, Cause: sqlite.ErrIdentityConflict}
	}
	if len(local) == 1 {
		return []ports.SearchResult{resultFromItem(local[0], local[0].Source)}, nil
	}

	var providers []ports.ExternalLookupProvider
	if kind == domain.KindSeries && (ref.Provider == "tvdb" || ref.Provider == "imdb") {
		for _, candidate := range s.series {
			if provider, ok := candidate.(ports.ExternalLookupProvider); ok {
				providers = append(providers, provider)
			}
		}
	}
	if provider, ok := s.meta.(ports.ExternalLookupProvider); ok {
		providers = append(providers, provider)
	}
	if len(providers) == 0 {
		return nil, ports.ErrProviderNotConfigured
	}
	var unsupported bool
	for _, provider := range providers {
		results, lookupErr := provider.LookupExternal(ctx, kind, ref)
		if lookupErr == nil {
			if len(results) > 1 {
				return nil, &IdentityConflictError{Cause: sqlite.ErrIdentityConflict}
			}
			if len(results) == 0 {
				continue
			}
			if !addable(results[0]) {
				unsupported = true
				continue
			}
			return results, nil
		}
		if errors.Is(lookupErr, ports.ErrProviderNotConfigured) {
			continue
		}
		var remote *ports.RemoteError
		if errors.As(lookupErr, &remote) {
			switch remote.Category {
			case ports.RemoteNotFound:
				continue
			case ports.RemoteUnsupportedHydration, ports.RemoteUnsupportedQuery:
				unsupported = true
				continue
			case ports.RemoteIdentityConflict:
				return nil, &IdentityConflictError{Cause: lookupErr}
			default:
				return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, lookupErr)
			}
		}
		return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, lookupErr)
	}
	if unsupported {
		return nil, ErrUnsupportedHydration
	}
	return nil, nil
}

func addable(result ports.SearchResult) bool {
	return result.TMDBID != 0 || (result.Kind == domain.KindSeries && result.TVDBID != 0)
}
func resultFromItem(item domain.MediaItem, source string) ports.SearchResult {
	return ports.SearchResult{Kind: item.Kind, TMDBID: item.IDs.TMDB, TVDBID: item.IDs.TVDB, IMDBID: item.IDs.IMDB, Source: source, HydrationSource: item.Source, Title: item.Title, Year: item.Year, Overview: item.Overview, PosterPath: item.PosterPath}
}

// searchPrimary asks only the first link: TMDB for video, Open Library for
// books.
func (s *Service) searchPrimary(ctx context.Context, kind domain.MediaKind, query string) ([]ports.SearchResult, error) {
	switch kind {
	case domain.KindMovie:
		return s.meta.SearchMovies(ctx, query)
	case domain.KindSeries:
		return s.meta.SearchSeries(ctx, query)
	case domain.KindBook:
		if s.books == nil {
			return nil, ports.ErrProviderNotConfigured
		}
		return s.books.SearchBooks(ctx, query)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedKind, kind)
	}
}

// appendSeriesChain adds results from the later links that the earlier ones
// did not already produce.
//
// Interactive search merges rather than falling back, because the user is
// looking for something and a provider that has it should not be silent
// merely because an earlier one returned *a* result. (Adoption uses a
// stricter rule — see propose: there, "usable" means "clears the bar", so
// the chain engages exactly where matching fails.)
//
// Duplicates collapse only on a verified shared ID with no conflicting known
// namespace. Equal title/year alone is not identity: regional remakes often
// have both.
func (s *Service) appendSeriesChain(
	ctx context.Context, query string, have []ports.SearchResult,
) []ports.SearchResult {
	if len(s.series) == 0 {
		return have
	}
	// We enrich duplicate rows with identities learned from later providers;
	// copy first so a provider-owned/cached result slice is never mutated.
	out := append([]ports.SearchResult(nil), have...)
	for _, p := range s.series {
		if ctx.Err() != nil {
			return out
		}
		res, err := p.SearchSeries(ctx, query)
		if err != nil {
			s.log.Debug("library: series provider search failed",
				"provider", p.Name(), "query", query, "err", err)
			continue
		}
		for _, r := range res {
			duplicate := -1
			for i := range out {
				if verifiedSameResult(out[i], r) {
					duplicate = i
					break
				}
			}
			if duplicate >= 0 {
				i := duplicate
				if out[i].TMDBID == 0 {
					out[i].TMDBID = r.TMDBID
				}
				if out[i].TVDBID == 0 {
					out[i].TVDBID = r.TVDBID
				}
				if out[i].IMDBID == "" {
					out[i].IMDBID = r.IMDBID
				}
				continue
			}
			out = append(out, r)
		}
	}
	return out
}

func verifiedSameResult(a, b ports.SearchResult) bool {
	shared := a.TMDBID != 0 && a.TMDBID == b.TMDBID || a.TVDBID != 0 && a.TVDBID == b.TVDBID || a.IMDBID != "" && strings.EqualFold(a.IMDBID, b.IMDBID)
	conflict := a.TMDBID != 0 && b.TMDBID != 0 && a.TMDBID != b.TMDBID || a.TVDBID != 0 && b.TVDBID != 0 && a.TVDBID != b.TVDBID || a.IMDBID != "" && b.IMDBID != "" && !strings.EqualFold(a.IMDBID, b.IMDBID)
	// Search endpoints commonly return only their native namespace. Preserve
	// the established cross-provider merge when the two rows have complementary
	// IDs and the exact same kind/title/year; Add rehydrates and verifies the
	// combined IDs before anything is persisted.
	complementary := a.Kind == b.Kind && a.Year != 0 && a.Year == b.Year && matcher.NormalizeTitle(a.Title) == matcher.NormalizeTitle(b.Title)
	return (shared || complementary) && !conflict
}

// seriesByTVDB hydrates a series from whichever link of the chain knows the
// id. Used by Add for anything the chain — rather than TMDB — identified.
func (s *Service) seriesByTVDB(ctx context.Context, tvdbID int64) (domain.MediaItem, error) {
	for _, p := range s.series {
		item, err := p.GetSeriesByTVDB(ctx, tvdbID)
		if err == nil {
			// Which link answered is a fact worth keeping: it is what tells
			// refresh where to go back to, and the UI where this came from.
			item.Source = p.Name()
			return item, nil
		}
		s.log.Debug("library: series provider could not hydrate",
			"provider", p.Name(), "tvdb", tvdbID, "err", err)
	}
	return domain.MediaItem{}, fmt.Errorf("%w: no provider has tvdb series %d", ErrNotFound, tvdbID)
}

// ---- add / browse ----

// AddRequest is what the API sends to put something in the library.
// TMDBID identifies movies/series; OLID identifies books (ADR 0006).
type AddRequest struct {
	Kind domain.MediaKind
	// BookType chooses the edition being added when Kind is book. It is
	// persistent identity, not a quality inferred from the profile: the same
	// Open Library work may own one ebook and one audiobook (ADR 0018).
	BookType quality.BookType
	TMDBID   int64
	// TVDBID identifies a series that came from the chain rather than from
	// TMDB (ADR 0011). Exactly one of TMDBID/TVDBID/OLID identifies the item.
	TVDBID          int64
	IMDBID          string
	HydrationSource string
	OLID            string
	RootFolderID    int64 // optional; 0 = no folder assigned yet
	// QualityProfileID is optional; 0 means "use the configured default for
	// this kind" (Settings → Profiles), which falls back to the built-in when
	// nothing has been chosen.
	QualityProfileID int64
	// DownloadPriority overrides the selected profile when non-nil.
	DownloadPriority *int
	Monitored        bool
	// Monitor picks which seasons start monitored (series only):
	// "all" (default), "latest" (newest season only), or "none".
	Monitor string
}

// existingByAnyID returns ErrAlreadyExists when the library already holds
// this title under any id the request carries.
func (s *Service) existingByAnyID(ctx context.Context, req AddRequest) error {
	var refs []domain.ExternalRef
	if req.TMDBID != 0 {
		refs = append(refs, domain.ExternalRef{Provider: "tmdb", Value: fmt.Sprintf("%d", req.TMDBID)})
	}
	if req.TVDBID != 0 {
		refs = append(refs, domain.ExternalRef{Provider: "tvdb", Value: fmt.Sprintf("%d", req.TVDBID)})
	}
	if req.IMDBID != "" {
		refs = append(refs, domain.ExternalRef{Provider: "imdb", Value: req.IMDBID})
	}
	if len(refs) == 0 {
		return fmt.Errorf("%w: an id is required to add a %s", ErrNotFound, req.Kind)
	}
	claimed := map[int64]domain.MediaItem{}
	for _, ref := range refs {
		items, err := s.db.FindMediaItemsByExternalID(ctx, req.Kind, ref)
		if err != nil {
			return err
		}
		for _, item := range items {
			claimed[item.ID] = item
		}
	}
	if len(claimed) > 1 {
		ids := make([]int64, 0, len(claimed))
		for id := range claimed {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		return &IdentityConflictError{ItemIDs: ids, Cause: sqlite.ErrIdentityConflict}
	}
	for _, item := range claimed {
		// A request that joins an existing identity on one namespace while
		// contradicting another is corruption evidence, not a duplicate add.
		if (req.TMDBID != 0 && item.IDs.TMDB != 0 && req.TMDBID != item.IDs.TMDB) ||
			(req.TVDBID != 0 && item.IDs.TVDB != 0 && req.TVDBID != item.IDs.TVDB) ||
			(req.IMDBID != "" && item.IDs.IMDB != "" && !strings.EqualFold(req.IMDBID, item.IDs.IMDB)) {
			return &IdentityConflictError{ItemIDs: []int64{item.ID}, Cause: sqlite.ErrIdentityConflict}
		}
		return ErrAlreadyExists
	}
	return nil
}

func (s *Service) hydrateAdd(ctx context.Context, req AddRequest) (domain.MediaItem, error) {
	switch req.HydrationSource {
	case "":
		// Legacy requests preserve baseline routing.
		if req.Kind == domain.KindMovie {
			return s.meta.GetMovie(ctx, req.TMDBID)
		}
		if req.TMDBID != 0 {
			return s.meta.GetSeries(ctx, req.TMDBID)
		}
		return s.seriesByTVDB(ctx, req.TVDBID)
	case "tmdb":
		if req.Kind == domain.KindMovie {
			return s.meta.GetMovie(ctx, req.TMDBID)
		}
		return s.meta.GetSeries(ctx, req.TMDBID)
	default:
		if req.Kind != domain.KindSeries {
			return domain.MediaItem{}, fmt.Errorf("%w: hydration source %s", ErrInvalidInput, req.HydrationSource)
		}
		for _, provider := range s.series {
			if provider.Name() == req.HydrationSource {
				item, err := provider.GetSeriesByTVDB(ctx, req.TVDBID)
				if err == nil {
					item.Source = provider.Name()
				}
				return item, err
			}
		}
		return domain.MediaItem{}, fmt.Errorf("%w: hydration source %s is not configured", ErrProviderUnavailable, req.HydrationSource)
	}
}

// applyMonitorPreset flips season/episode flags per the add-time choice.
func applyMonitorPreset(item *domain.MediaItem, preset string) {
	if item.Kind != domain.KindSeries || preset == "" || preset == "all" {
		return
	}
	latest := 0
	for _, s := range item.Seasons {
		if s.Number > latest {
			latest = s.Number
		}
	}
	for i := range item.Seasons {
		mon := preset == "latest" && item.Seasons[i].Number == latest && latest != 0
		item.Seasons[i].Monitored = mon
		for j := range item.Seasons[i].Episodes {
			item.Seasons[i].Episodes[j].Monitored = mon
		}
	}
}

// Add hydrates the item from the provider and stores it. The on-disk folder
// is derived as <root>/<Title (Year)> — or the Calibre-friendly
// <root>/<Author>/<Title> for books — when a root folder is given; nothing
// is created on disk until import.
func (s *Service) Add(ctx context.Context, req AddRequest) (domain.MediaItem, error) {
	var item domain.MediaItem
	var err error
	switch req.Kind {
	case domain.KindMovie, domain.KindSeries:
		// Look for an existing row under *every* id the request carries, not
		// just TMDB's: a series added through the chain is keyed on TVDB, and
		// checking one id space would add it a second time (ADR 0011 §4).
		if err := s.existingByAnyID(ctx, req); err != nil {
			return domain.MediaItem{}, err
		}
		item, err = s.hydrateAdd(ctx, req)
	case domain.KindBook:
		if s.books == nil {
			return domain.MediaItem{}, ports.ErrProviderNotConfigured
		}
		if existingID, err := s.db.GetMediaItemByKindOlid(ctx, req.Kind, req.OLID); err == nil {
			return s.addExistingBookEdition(ctx, existingID, req)
		} else if !errors.Is(err, sqlite.ErrNotFound) {
			return domain.MediaItem{}, err
		}
		item, err = s.books.GetBook(ctx, req.OLID)
		item.Source = "openlibrary"
	default:
		return domain.MediaItem{}, fmt.Errorf("%w: %q", ErrUnsupportedKind, req.Kind)
	}
	if err != nil {
		return domain.MediaItem{}, err
	}
	if (req.TMDBID != 0 && item.IDs.TMDB != 0 && req.TMDBID != item.IDs.TMDB) ||
		(req.TVDBID != 0 && item.IDs.TVDB != 0 && req.TVDBID != item.IDs.TVDB) ||
		(req.IMDBID != "" && item.IDs.IMDB != "" && !strings.EqualFold(req.IMDBID, item.IDs.IMDB)) {
		return domain.MediaItem{}, &IdentityConflictError{Cause: sqlite.ErrIdentityConflict}
	}
	if item.IDs.TMDB == 0 {
		item.IDs.TMDB = req.TMDBID
	}
	if item.IDs.TVDB == 0 {
		item.IDs.TVDB = req.TVDBID
	}
	if item.IDs.IMDB == "" {
		item.IDs.IMDB = req.IMDBID
	}
	if item.Kind == domain.KindMovie || item.Kind == domain.KindSeries {
		if req.HydrationSource != "" {
			item.Source = req.HydrationSource
		} else if item.Source == "" {
			if item.IDs.TMDB != 0 {
				item.Source = "tmdb"
			} else {
				item.Source = "tvmaze"
			}
		}
	}
	if item.Kind == domain.KindMovie || item.Kind == domain.KindSeries {
		// TMDB search results do not include external ids; GetSeries does.
		// Check again after hydration so an item first added through a
		// TVDB-keyed provider cannot be added a second time through TMDB.
		hydrated := req
		hydrated.TMDBID = item.IDs.TMDB
		hydrated.TVDBID = item.IDs.TVDB
		hydrated.IMDBID = item.IDs.IMDB
		if err := s.existingByAnyID(ctx, hydrated); err != nil {
			return domain.MediaItem{}, err
		}
	}

	s.enrichRatings(ctx, &item)
	// Nothing stored yet — an add starts with whatever the provider says.
	s.enrichAiring(ctx, &item, domain.MediaItem{})
	applyMonitorPreset(&item, req.Monitor)
	item.Monitored = req.Monitored
	if item.Kind == domain.KindBook {
		item.BookType = req.BookType
		if item.BookType == "" {
			item.BookType = quality.BookTypeEbook
		}
		if !quality.ValidBookType(item.BookType) {
			return domain.MediaItem{}, fmt.Errorf("%w: unknown book type %q", ErrInvalidInput, item.BookType)
		}
	}
	item.QualityProfileID = req.QualityProfileID
	item.DownloadPriorityOverride = req.DownloadPriority
	if item.QualityProfileID == 0 {
		// Nothing was chosen, so the kind's configured default applies. It
		// resolves to the built-in when unset or dangling, which is what the
		// hardcoded constant here used to do unconditionally.
		if item.Kind == domain.KindBook {
			item.QualityProfileID = s.db.DefaultBookProfileID(ctx, item.BookType)
		} else {
			item.QualityProfileID = s.db.DefaultProfileID(ctx, item.Kind)
		}
	}
	if err := s.validateItemProfile(ctx, item.Kind, item.QualityProfileID, item.BookType); err != nil {
		return domain.MediaItem{}, err
	}
	if req.RootFolderID != 0 {
		rf, err := s.db.GetRootFolder(ctx, req.RootFolderID)
		if err != nil {
			return domain.MediaItem{}, fmt.Errorf("root folder: %w", err)
		}
		if !rf.Kind.Accepts(item.Kind) {
			return domain.MediaItem{}, fmt.Errorf(
				"%w: root folder %s holds %s", ErrRootKindMismatch, rf.Path, rf.Kind)
		}
		item.RootFolderID = rf.ID
		if item.Path, err = s.freeFolder(ctx, rf.Path, item); err != nil {
			return domain.MediaItem{}, err
		}
	}

	id, err := s.db.CreateMediaItem(ctx, item)
	if err != nil {
		if errors.Is(err, sqlite.ErrFolderTaken) {
			return domain.MediaItem{}, folderTakenErr(err, item.Path)
		}
		if errors.Is(err, sqlite.ErrDuplicate) {
			return domain.MediaItem{}, ErrAlreadyExists
		}
		return domain.MediaItem{}, err
	}
	s.log.Info("library: added", "kind", item.Kind, "title", item.Title, "id", id)
	s.publish(MediaAdded{ID: id, Kind: string(item.Kind), Title: item.Title})
	if item.Kind != domain.KindBook {
		if err := s.EnqueueIdentityRefresh(ctx, id, false); err != nil {
			s.log.Warn("library: identity enrichment deferred", "id", id, "err", err)
		}
	}
	// Return the same graded representation List/Get expose.
	return s.Get(ctx, id)
}

// addExistingBookEdition turns a duplicate work add into the missing edition
// add. The Open Library work remains the aggregate root; the new target gets
// an independent profile, files, downloads, wanted state, and search scope.
func (s *Service) addExistingBookEdition(ctx context.Context, itemID int64, req AddRequest) (domain.MediaItem, error) {
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return domain.MediaItem{}, err
	}
	bookType := req.BookType
	if bookType == "" {
		bookType = quality.BookTypeEbook
	}
	if !quality.ValidBookType(bookType) {
		return domain.MediaItem{}, fmt.Errorf("%w: unknown book type %q", ErrInvalidInput, bookType)
	}
	if item.BookType == bookType {
		return domain.MediaItem{}, fmt.Errorf("%w: %s edition already present", ErrAlreadyExists, bookType)
	}
	for _, edition := range item.Copies {
		if edition.BookType == bookType {
			return domain.MediaItem{}, fmt.Errorf("%w: %s edition already present", ErrAlreadyExists, bookType)
		}
	}
	profileID := req.QualityProfileID
	if profileID == 0 {
		profileID = s.db.DefaultBookProfileID(ctx, bookType)
	}
	if err := s.validateItemProfile(ctx, domain.KindBook, profileID, bookType); err != nil {
		return domain.MediaItem{}, err
	}

	// A book added before roots were configured needs one shared destination
	// before either edition can import. If it already has a destination, the
	// second edition shares it unless the caller deliberately selected a
	// different root.
	if item.Path == "" && req.RootFolderID != 0 {
		rootID := req.RootFolderID
		monitored := item.Monitored || req.Monitored
		if _, err := s.UpdateItem(ctx, itemID, UpdateRequest{
			Monitored: &monitored, RootFolderID: &rootID,
		}); err != nil {
			return domain.MediaItem{}, err
		}
		item, err = s.db.GetMediaItemFull(ctx, itemID)
		if err != nil {
			return domain.MediaItem{}, err
		}
	} else if req.Monitored && !item.Monitored {
		monitored := true
		if _, err := s.UpdateItem(ctx, itemID, UpdateRequest{Monitored: &monitored}); err != nil {
			return domain.MediaItem{}, err
		}
	}
	copyRoot := req.RootFolderID
	if copyRoot == item.RootFolderID {
		copyRoot = 0
	}
	return s.AddCopy(ctx, itemID, CopyRequest{
		BookType: bookType, QualityProfileID: profileID,
		RootFolderID: copyRoot, Monitored: req.Monitored,
	})
}

func (s *Service) validateItemProfile(ctx context.Context, kind domain.MediaKind, profileID int64, bookType quality.BookType) error {
	p, err := s.db.GetProfile(ctx, profileID)
	if err != nil {
		return fmt.Errorf("%w: quality profile: %v", ErrInvalidInput, err)
	}
	profileBookType := quality.BookTypeForSource(p.Target.Source)
	if kind == domain.KindBook {
		if profileBookType == "" {
			return fmt.Errorf("%w: profile %q is for video, not books", ErrInvalidInput, p.Name)
		}
		if bookType != "" && (!quality.ValidBookType(bookType) || profileBookType != bookType) {
			return fmt.Errorf("%w: profile %q is for %s, not %s", ErrInvalidInput, p.Name, profileBookType, bookType)
		}
		return nil
	}
	if profileBookType != "" {
		return fmt.Errorf("%w: profile %q is for books, not %s", ErrInvalidInput, p.Name, kind)
	}
	if bookType != "" {
		return fmt.Errorf("%w: book type only applies to books", ErrInvalidInput)
	}
	return nil
}

// UpdateRequest is a per-item edit; nil fields are left as they are.
type UpdateRequest struct {
	Monitored        *bool
	QualityProfileID *int64
	// SetDownloadPriority distinguishes "leave unchanged" from clearing the
	// nullable override. DownloadPriority nil plus true means inherit.
	SetDownloadPriority bool
	DownloadPriority    *int
	RootFolderID        *int64  // 0 clears the assignment (and the path)
	Path                *string // explicit folder override; wins over RootFolderID's recompute
}

// PlacementSuggestion is the same root-and-name decision Add media would
// have made for an item. It is used when repairing an older item that reached
// the library without a destination.
type PlacementSuggestion struct {
	RootFolderID int64
	Path         string
}

// SuggestPlacement chooses the first compatible root (the Add page default)
// and applies Monarr's naming rules. Keeping this on the server prevents the
// repair form from growing a second, subtly different folder-name algorithm.
func (s *Service) SuggestPlacement(ctx context.Context, id int64) (PlacementSuggestion, error) {
	item, err := s.db.GetMediaItemFull(ctx, id)
	if err != nil {
		return PlacementSuggestion{}, err
	}
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return PlacementSuggestion{}, err
	}
	for _, rf := range roots {
		if !rf.Kind.Accepts(item.Kind) {
			continue
		}
		path, err := s.freeFolder(ctx, rf.Path, item)
		if err != nil {
			return PlacementSuggestion{}, err
		}
		return PlacementSuggestion{RootFolderID: rf.ID, Path: path}, nil
	}
	return PlacementSuggestion{}, fmt.Errorf("%w: no library root accepts %s", ErrInvalidInput, item.Kind)
}

// UpdateItem applies a per-item edit: monitoring, quality profile, and
// location. Changing the root folder recomputes the item folder from the
// naming rules; files already on disk are never moved.
func (s *Service) UpdateItem(ctx context.Context, id int64, req UpdateRequest) (domain.MediaItem, error) {
	item, err := s.db.GetMediaItemFull(ctx, id)
	if err != nil {
		return domain.MediaItem{}, err
	}
	if req.Monitored != nil {
		item.Monitored = *req.Monitored
	}
	if req.QualityProfileID != nil && *req.QualityProfileID != 0 {
		if err := s.validateItemProfile(ctx, item.Kind, *req.QualityProfileID, item.BookType); err != nil {
			return domain.MediaItem{}, err
		}
		item.QualityProfileID = *req.QualityProfileID
	}
	if req.SetDownloadPriority {
		item.DownloadPriorityOverride = req.DownloadPriority
	}
	if req.RootFolderID != nil {
		if *req.RootFolderID == 0 {
			item.RootFolderID = 0
			item.Path = ""
		} else {
			rf, err := s.db.GetRootFolder(ctx, *req.RootFolderID)
			if err != nil {
				return domain.MediaItem{}, fmt.Errorf("root folder: %w", err)
			}
			item.RootFolderID = rf.ID
			if item.Path, err = s.freeFolder(ctx, rf.Path, item); err != nil {
				return domain.MediaItem{}, err
			}
		}
	}
	if req.Path != nil {
		p := strings.TrimSpace(*req.Path)
		if p != "" && !filepath.IsAbs(p) {
			return domain.MediaItem{}, fmt.Errorf("path must be absolute")
		}
		item.Path = filepath.Clean(p)
		if p == "" {
			item.Path = ""
		}
		if err := s.ensureFolderFree(ctx, item.Path, item.ID); err != nil {
			return domain.MediaItem{}, err
		}
	}
	if err := s.db.UpdateMediaItemPlacement(ctx, item); err != nil {
		return domain.MediaItem{}, folderTakenErr(err, item.Path)
	}
	s.log.Info("library: item updated", "id", id, "title", item.Title,
		"monitored", item.Monitored, "profile", item.QualityProfileID, "path", item.Path)
	return s.db.GetMediaItemFull(ctx, id)
}

// RefreshItem re-hydrates one item from its metadata provider: cached
// fields (overview, poster, status, rating, …) update in place, series gain
// newly announced seasons/episodes, and library placement (monitored flags,
// profile, folder) is untouched. Nothing is ever deleted.
func (s *Service) RefreshItem(ctx context.Context, id int64) (domain.MediaItem, error) {
	stored, err := s.db.GetMediaItemFull(ctx, id)
	if err != nil {
		return domain.MediaItem{}, err
	}

	// A manual entry has nothing to refresh against (ADR 0012 §3). Treating
	// "no provider id" as "go look it up" would either error every cycle or
	// overwrite what the user typed with whatever id zero returns.
	if stored.IsManual() {
		return domain.MediaItem{}, fmt.Errorf("%w: %q", ErrManualEntry, stored.Title)
	}

	var fresh domain.MediaItem
	switch {
	case stored.Kind == domain.KindMovie && stored.Source == "tmdb":
		fresh, err = s.meta.GetMovie(ctx, stored.IDs.TMDB)
	case stored.Kind == domain.KindSeries && stored.Source == "tmdb":
		fresh, err = s.meta.GetSeries(ctx, stored.IDs.TMDB)
	case stored.Kind == domain.KindSeries:
		for _, provider := range s.series {
			if provider.Name() == stored.Source {
				fresh, err = provider.GetSeriesByTVDB(ctx, stored.IDs.TVDB)
				break
			}
		}
		if fresh.Title == "" && err == nil {
			err = fmt.Errorf("%w: hydration source %s is not configured", ErrProviderUnavailable, stored.Source)
		}
	case stored.Kind == domain.KindBook:
		if s.books == nil {
			return domain.MediaItem{}, ports.ErrProviderNotConfigured
		}
		fresh, err = s.books.GetBook(ctx, stored.IDs.OLID)
	default:
		return domain.MediaItem{}, fmt.Errorf("%w: %q", ErrUnsupportedKind, stored.Kind)
	}
	if err != nil {
		return domain.MediaItem{}, err
	}
	if fresh.Title == "" {
		return domain.MediaItem{}, fmt.Errorf("provider returned no title for %q — refresh aborted", stored.Title)
	}

	// Ids only ever enrich: a provider hiccup must not blank what we know.
	if fresh.IDs.IMDB == "" {
		fresh.IDs.IMDB = stored.IDs.IMDB
	}
	if fresh.IDs.TVDB == 0 {
		fresh.IDs.TVDB = stored.IDs.TVDB
	}
	if fresh.IDs.ISBN13 == "" {
		fresh.IDs.ISBN13 = stored.IDs.ISBN13
	}
	if fresh.IDs.ASIN == "" {
		fresh.IDs.ASIN = stored.IDs.ASIN
	}
	if fresh.Author == "" {
		fresh.Author = stored.Author
	}
	if fresh.PosterPath == "" {
		fresh.PosterPath = stored.PosterPath
	}
	fresh.Source = stored.Source
	s.enrichRatings(ctx, &fresh)
	s.enrichAiring(ctx, &fresh, stored)

	// Season flags are the user's: known seasons keep their stored flag,
	// and NEW episodes appearing in them inherit it (existing episodes keep
	// their own flag via the upsert). Brand-new seasons follow the series.
	storedSeason := map[int]bool{}
	for _, s := range stored.Seasons {
		storedSeason[s.Number] = s.Monitored
	}
	for i := range fresh.Seasons {
		mon, known := storedSeason[fresh.Seasons[i].Number]
		if !known {
			mon = fresh.Seasons[i].Monitored && stored.Monitored
		}
		fresh.Seasons[i].Monitored = mon
		for j := range fresh.Seasons[i].Episodes {
			fresh.Seasons[i].Episodes[j].Monitored = mon
		}
	}

	if err := s.db.RefreshMediaItemMetadata(ctx, id, fresh); err != nil {
		return domain.MediaItem{}, err
	}
	s.log.Info("library: metadata refreshed", "kind", stored.Kind, "title", fresh.Title, "id", id)
	if fresh.Kind != domain.KindBook {
		if err := s.EnqueueIdentityRefresh(ctx, id, true); err != nil {
			s.log.Warn("library: identity enrichment deferred after refresh", "id", id, "err", err)
		}
	}
	return s.db.GetMediaItemFull(ctx, id)
}

// CopyRequest describes a new additional acquisition target for an item.
type CopyRequest struct {
	BookType         quality.BookType // required for a book edition; empty for video copies
	QualityProfileID int64
	RootFolderID     int64 // 0 = share the item's folder (filenames carry [Quality])
	Name             string
	Monitored        bool
}

// AddCopy registers an additional quality copy for video or the other
// first-class edition for a book. The automation treats both as independent
// targets; the UI deliberately calls the latter an edition, not a quality.
func (s *Service) AddCopy(ctx context.Context, itemID int64, req CopyRequest) (domain.MediaItem, error) {
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return domain.MediaItem{}, err
	}
	if _, err := s.db.GetProfile(ctx, req.QualityProfileID); err != nil {
		return domain.MediaItem{}, fmt.Errorf("quality profile: %w", err)
	}
	if err := s.validateItemProfile(ctx, item.Kind, req.QualityProfileID, req.BookType); err != nil {
		return domain.MediaItem{}, err
	}
	if item.Kind == domain.KindBook {
		if !quality.ValidBookType(req.BookType) {
			return domain.MediaItem{}, fmt.Errorf("%w: book edition type is required", ErrInvalidInput)
		}
		if item.BookType == req.BookType {
			return domain.MediaItem{}, fmt.Errorf("%w: %s edition already present", ErrAlreadyExists, req.BookType)
		}
		for _, other := range item.Copies {
			if other.BookType == req.BookType {
				return domain.MediaItem{}, fmt.Errorf("%w: %s edition already present", ErrAlreadyExists, req.BookType)
			}
		}
	}
	cp := domain.MediaCopy{
		MediaItemID: itemID, Name: strings.TrimSpace(req.Name),
		BookType:         req.BookType,
		QualityProfileID: req.QualityProfileID, Monitored: req.Monitored,
	}
	if req.RootFolderID != 0 {
		rf, err := s.db.GetRootFolder(ctx, req.RootFolderID)
		if err != nil {
			return domain.MediaItem{}, fmt.Errorf("root folder: %w", err)
		}
		cp.RootFolderID = rf.ID
		folder := naming.FolderName(item.Title, item.Year)
		if item.Kind == domain.KindBook {
			folder = naming.BookFolder(item.Author, item.Title)
		}
		cp.Path = filepath.Join(rf.Path, folder)
		if cp.Path == item.Path {
			return domain.MediaItem{}, fmt.Errorf(
				"copy folder would collide with the item's own folder — pick a different root, or omit the root to share the folder")
		}
		for _, other := range item.Copies {
			if other.Path == cp.Path {
				return domain.MediaItem{}, fmt.Errorf("another copy already uses %s", cp.Path)
			}
		}
		if err := s.ensureFolderFree(ctx, cp.Path, item.ID); err != nil {
			return domain.MediaItem{}, err
		}
	}
	id, err := s.db.AddMediaCopy(ctx, cp)
	if err != nil {
		return domain.MediaItem{}, err
	}
	// The folder may be present in the last scan/review snapshot from before
	// the user attached it as a copy. It is claimed now; leaving stale work
	// behind lets the adoption job turn this copy into another media item.
	if cp.Path != "" {
		s.dropFromOutstanding(ctx, cp.Path)
	}
	s.log.Info("library: copy added", "item", item.Title, "copy", id,
		"profile", req.QualityProfileID, "path", cp.Path)
	return s.db.GetMediaItemFull(ctx, itemID)
}

// UpdateCopy edits a copy's name, profile, or monitoring.
func (s *Service) UpdateCopy(ctx context.Context, itemID, copyID int64, name *string, profileID *int64, monitored *bool) (domain.MediaItem, error) {
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return domain.MediaItem{}, err
	}
	cp, err := s.db.GetMediaCopy(ctx, itemID, copyID)
	if err != nil {
		return domain.MediaItem{}, err
	}
	if name != nil {
		cp.Name = strings.TrimSpace(*name)
	}
	if profileID != nil && *profileID != 0 {
		if err := s.validateItemProfile(ctx, item.Kind, *profileID, cp.BookType); err != nil {
			return domain.MediaItem{}, err
		}
		cp.QualityProfileID = *profileID
	}
	if monitored != nil {
		cp.Monitored = *monitored
	}
	if err := s.db.UpdateMediaCopy(ctx, cp); err != nil {
		return domain.MediaItem{}, err
	}
	return s.db.GetMediaItemFull(ctx, itemID)
}

// DeleteCopy removes a copy and its file records; disk is never touched.
func (s *Service) DeleteCopy(ctx context.Context, itemID, copyID int64) (domain.MediaItem, error) {
	if err := s.db.DeleteMediaCopy(ctx, itemID, copyID); err != nil {
		return domain.MediaItem{}, err
	}
	s.log.Info("library: copy removed", "item", itemID, "copy", copyID)
	return s.db.GetMediaItemFull(ctx, itemID)
}

// SetSeasonMonitored flips a season's monitored flag (cascading to its
// episodes) and returns the refreshed item.
func (s *Service) SetSeasonMonitored(ctx context.Context, itemID int64, season int, monitored bool) (domain.MediaItem, error) {
	if err := s.db.SetSeasonMonitored(ctx, itemID, season, monitored); err != nil {
		return domain.MediaItem{}, err
	}
	s.log.Info("library: season monitoring", "item", itemID, "season", season, "monitored", monitored)
	return s.db.GetMediaItemFull(ctx, itemID)
}

// SetEpisodeMonitored flips one episode's monitored flag and returns the
// refreshed item.
func (s *Service) SetEpisodeMonitored(ctx context.Context, itemID, episodeID int64, monitored bool) (domain.MediaItem, error) {
	if err := s.db.SetEpisodeMonitored(ctx, itemID, episodeID, monitored); err != nil {
		return domain.MediaItem{}, err
	}
	return s.db.GetMediaItemFull(ctx, itemID)
}

// RefreshAll refreshes every library item (the metadata.refresh task).
// Individual failures are logged and skipped so one delisted item cannot
// wedge the loop; the first error is reported at the end for task health.
func (s *Service) RefreshAll(ctx context.Context) error {
	items, err := s.db.ListMediaItems(ctx, "")
	if err != nil {
		return err
	}
	var firstErr error
	for _, item := range items {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Nothing to refresh it against, and asking is what would clobber it.
		if item.IsManual() {
			continue
		}
		if _, err := s.RefreshItem(ctx, item.ID); err != nil {
			// No provider key yet is a setup state, not a task failure.
			if errors.Is(err, ports.ErrProviderNotConfigured) {
				continue
			}
			s.log.Warn("library: refresh failed", "id", item.ID, "title", item.Title, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("refreshing %q: %w", item.Title, err)
			}
		}
	}
	return firstErr
}

// List returns item summaries, optionally filtered by kind ("" = all).
func (s *Service) List(ctx context.Context, kind domain.MediaKind) ([]domain.MediaItem, error) {
	items, err := s.db.ListMediaItems(ctx, kind)
	if err != nil {
		return nil, err
	}
	s.gradeUpgrades(ctx, items)
	return items, nil
}

// KnownTMDBIDs returns the TMDB ids already in the library for a kind.
//
// List answers the same question and is what the search handler uses, but it
// hydrates every item and grades every upgrade to do it. That is the right
// trade for one search box and the wrong one for a Discover page, which asks
// about fourteen rows of thirty titles on every load (ADR 0015).
func (s *Service) KnownTMDBIDs(ctx context.Context, kind domain.MediaKind) (map[int64]struct{}, error) {
	return s.db.TmdbIDsByKind(ctx, kind)
}

// gradeUpgrades fills in each item's quality target and upgrade state.
//
// The storage layer knows what is on disk; only here is the profile known,
// and the profile is what turns "1080p" into "1080p, and still looking" or
// "1080p, done". A list view that shows the first without the second makes
// the reader open every item to find out which — which is the state the
// poster grid was in.
//
// Best-effort: profiles unreadable means the items come back ungraded
// (UpgradeUnknown) rather than the list failing.
func (s *Service) gradeUpgrades(ctx context.Context, items []domain.MediaItem) {
	profiles, err := s.db.ListProfiles(ctx)
	if err != nil {
		s.log.Debug("library: profiles unreadable, list is ungraded", "err", err)
		return
	}
	byID := make(map[int64]quality.Profile, len(profiles))
	for _, p := range profiles {
		byID[p.ID] = p
	}
	for i := range items {
		p, ok := byID[items[i].QualityProfileID]
		if !ok {
			continue
		}
		items[i].QualityTarget = p.Target
		items[i].Upgrade = upgradeState(items[i], p)
	}
}

// upgradeState answers the three questions a card should not need a click
// for: is anything here, is it good enough, and is monarr still looking.
func upgradeState(m domain.MediaItem, p quality.Profile) domain.UpgradeState {
	// "Nothing on disk" is a fact about files, not about quality: a file
	// whose quality was never recorded still counts as present, and calling
	// that missing would have the grid contradict the completeness pill
	// beside it.
	if !hasFiles(m) {
		return domain.UpgradeMissing
	}
	if quality.Rank(m.Quality) == 0 {
		// Present but unrecognised — say nothing rather than guess.
		return domain.UpgradeUnknown
	}
	switch {
	case p.Met(m.Quality, m.QualityVerified):
		return domain.UpgradeMet
	case p.UpgradesAllowed:
		return domain.UpgradeSeeking
	default:
		return domain.UpgradeCapped
	}
}

// hasFiles reports whether anything is on disk, counting episodes for series
// and raw files for everything else — the same split the completeness pill
// uses, so the two never disagree.
func hasFiles(m domain.MediaItem) bool {
	if m.Kind == domain.KindSeries {
		return m.EpisodeFileCount > 0
	}
	return m.FileCount > 0
}

// Get returns one fully hydrated item, graded against its quality profile.
func (s *Service) Get(ctx context.Context, id int64) (domain.MediaItem, error) {
	item, err := s.db.GetMediaItemFull(ctx, id)
	if err != nil {
		return domain.MediaItem{}, err
	}
	s.gradeOne(ctx, &item)
	return item, nil
}

// gradeOne fills in quality, target and upgrade state for a single hydrated
// item — the same three facts the list computes in bulk, derived here from
// the files and episodes already loaded.
//
// Best-effort, like gradeUpgrades: a provider of none of this is an
// ungraded item, never a failed request.
func (s *Service) gradeOne(ctx context.Context, item *domain.MediaItem) {
	records, err := s.db.FileQualityRecords(ctx, item.ID)
	if err != nil {
		s.log.Debug("library: file qualities unreadable", "item", item.ID, "err", err)
	}
	byID := make(map[int64]sqlite.FileQuality, len(records))
	for _, r := range records {
		byID[r.FileID] = r
	}
	var worst quality.Quality
	var verified, have bool
	for _, f := range item.Files {
		if f.CopyID != 0 {
			continue // a deliberate 720p second copy is not the main copy's problem
		}
		rec, ok := byID[f.ID]
		if !ok || !rec.Known {
			continue
		}
		if !have || quality.Rank(rec.Quality) < quality.Rank(worst) {
			worst, verified, have = rec.Quality, rec.SourceVerified(), true
		}
	}
	item.Quality, item.QualityVerified = worst, verified

	// The list view counts monitored aired episodes; here the loaded
	// episodes answer the same question directly.
	if item.Kind == domain.KindSeries {
		for _, season := range item.Seasons {
			for _, e := range season.Episodes {
				if e.HasFile {
					item.EpisodeFileCount++
				}
			}
		}
	}
	item.FileCount = len(item.Files)

	p, err := s.db.GetProfile(ctx, item.QualityProfileID)
	if err != nil {
		return
	}
	item.QualityTarget = p.Target
	item.Upgrade = upgradeState(*item, p)
}

// SharedFolders returns folders more than one library item points at,
// with a label ("Title (item N)") for each item sharing one.
//
// Two items on one folder is always wrong and never self-corrects: whichever
// scan runs last decides which of them the files link to, and the other is a
// card that looks real and holds nothing. Folder choice now avoids it (see
// freeFolder) and migration 0032 put a unique index on item folders, so what
// this can still find is a copy's folder that another item also claims —
// written before the copy path checked other items.
func (s *Service) SharedFolders(ctx context.Context) (map[string][]string, error) {
	claims, err := s.db.ListFolderClaims(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]map[int64]bool{}
	byPath := map[string][]string{}
	for _, c := range claims {
		if seen[c.Path] == nil {
			seen[c.Path] = map[int64]bool{}
		}
		if seen[c.Path][c.ItemID] {
			continue // an item and its own copy sharing a folder is by design
		}
		seen[c.Path][c.ItemID] = true
		byPath[c.Path] = append(byPath[c.Path], fmt.Sprintf("%s (item %d)", c.Title, c.ItemID))
	}
	for path, labels := range byPath {
		if len(labels) < 2 {
			delete(byPath, path)
		}
	}
	return byPath, nil
}

// Delete removes an item from the library. Files on disk are never touched.
func (s *Service) Delete(ctx context.Context, id int64) error {
	if _, err := s.db.GetMediaItemFull(ctx, id); err != nil {
		return err
	}
	return s.db.DeleteMediaItem(ctx, id)
}

// ---- root folders ----

// RootFolderInfo is a root folder plus live disk facts for the UI.
type RootFolderInfo struct {
	domain.RootFolder
	FreeBytes  int64
	Accessible bool
	// AutoAdopt reports whether adoption applies matches into this root
	// without asking first (ADR 0010 §5).
	AutoAdopt bool
}

// AddRootFolder validates and registers a library root. An empty kind means
// mixed, which is how every root behaved before ADR 0009.
func (s *Service) AddRootFolder(ctx context.Context, path string, kind domain.RootKind) (domain.RootFolder, error) {
	if kind == "" {
		kind = domain.KindMixed
	}
	if !domain.ValidRootKind(kind) {
		return domain.RootFolder{}, fmt.Errorf("%w: unknown root folder kind %q", ErrInvalidInput, kind)
	}
	if !filepath.IsAbs(path) {
		return domain.RootFolder{}, fmt.Errorf("root folder path must be absolute")
	}
	info, err := os.Stat(path)
	if err != nil {
		return domain.RootFolder{}, fmt.Errorf("root folder not accessible: %w", err)
	}
	if !info.IsDir() {
		return domain.RootFolder{}, fmt.Errorf("root folder is not a directory")
	}
	clean := filepath.Clean(path)
	if err := s.checkNotNested(ctx, clean); err != nil {
		return domain.RootFolder{}, err
	}
	rf, err := s.db.AddRootFolder(ctx, clean, kind)
	if errors.Is(err, sqlite.ErrDuplicate) {
		return domain.RootFolder{}, fmt.Errorf("root folder already registered")
	}
	return rf, err
}

// checkNotNested rejects a root that contains, or is contained by, one that
// is already registered (ADR 0009 §3). Nested roots offer every folder under
// the overlap twice, and registering a base like /media when the libraries
// are /media/Movies and /media/TV is the specific mistake that makes a scan
// look like it is trawling the whole tree. The error names the conflict,
// because that message is where the model gets learned.
func (s *Service) checkNotNested(ctx context.Context, path string) error {
	existing, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return err
	}
	for _, rf := range existing {
		switch {
		case isSubPath(rf.Path, path):
			return fmt.Errorf(
				"%w: %s is inside the registered root %s", ErrNestedRoot, path, rf.Path)
		case isSubPath(path, rf.Path):
			return fmt.Errorf(
				"%w: %s contains the registered root %s — register the media folders under it instead",
				ErrNestedRoot, path, rf.Path)
		}
	}
	return nil
}

// isSubPath reports whether child is strictly beneath parent. Both are
// expected to be cleaned absolute paths. Comparison is component-wise via
// the separator suffix, so /media/tv2 is not treated as being inside
// /media/tv.
func isSubPath(parent, child string) bool {
	if parent == child {
		return false
	}
	if !strings.HasSuffix(parent, string(filepath.Separator)) {
		parent += string(filepath.Separator)
	}
	return strings.HasPrefix(child, parent)
}

// SetRootFolderKind retypes an existing root. Items already in it keep their
// own kinds — this only changes how future adoption and placement route.
func (s *Service) SetRootFolderKind(ctx context.Context, id int64, kind domain.RootKind) (domain.RootFolder, error) {
	if !domain.ValidRootKind(kind) {
		return domain.RootFolder{}, fmt.Errorf("%w: unknown root folder kind %q", ErrInvalidInput, kind)
	}
	if _, err := s.db.GetRootFolder(ctx, id); err != nil {
		return domain.RootFolder{}, err
	}
	if err := s.db.SetRootFolderKind(ctx, id, kind); err != nil {
		return domain.RootFolder{}, err
	}
	return s.db.GetRootFolder(ctx, id)
}

// ListRootFolders returns roots with free-space and accessibility info.
func (s *Service) ListRootFolders(ctx context.Context) ([]RootFolderInfo, error) {
	roots, err := s.db.ListRootFolders(ctx)
	if err != nil {
		return nil, err
	}
	auto := s.AutoAdoptRoots(ctx)
	out := make([]RootFolderInfo, 0, len(roots))
	for _, rf := range roots {
		info := RootFolderInfo{RootFolder: rf, AutoAdopt: auto[rf.ID]}
		if st, err := os.Stat(rf.Path); err == nil && st.IsDir() {
			info.Accessible = true
			info.FreeBytes = freeBytes(rf.Path)
		}
		out = append(out, info)
	}
	return out, nil
}

// DeleteRootFolder removes the registration (never touches disk).
func (s *Service) DeleteRootFolder(ctx context.Context, id int64) error {
	if _, err := s.db.GetRootFolder(ctx, id); err != nil {
		return err
	}
	return s.db.DeleteRootFolder(ctx, id)
}
