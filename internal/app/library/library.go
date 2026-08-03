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
	"strings"

	"github.com/pjunod/monarr/internal/domain"
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
	ErrInvalidInput = errors.New("invalid input")
)

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
	queue JobEnqueuer
}

// New returns a Service. bus may be nil (tests).
func New(db *sqlite.DB, meta ports.MetadataProvider, b *bus.Bus, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, meta: meta, bus: b, log: log}
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
// Duplicates are collapsed on normalized title plus year, since a TMDB
// search result carries no TVDB id to compare against. Two records that
// agree on both are the same show often enough, and the cost of being wrong
// is one missing row in a list the user is reading anyway.
func (s *Service) appendSeriesChain(
	ctx context.Context, query string, have []ports.SearchResult,
) []ports.SearchResult {
	if len(s.series) == 0 {
		return have
	}
	seen := make(map[string]bool, len(have))
	for _, r := range have {
		seen[titleYearKey(r)] = true
	}
	out := have
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
			if seen[titleYearKey(r)] {
				continue
			}
			seen[titleYearKey(r)] = true
			out = append(out, r)
		}
	}
	return out
}

func titleYearKey(r ports.SearchResult) string {
	return fmt.Sprintf("%s|%d", matcher.NormalizeTitle(r.Title), r.Year)
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
	Kind   domain.MediaKind
	TMDBID int64
	// TVDBID identifies a series that came from the chain rather than from
	// TMDB (ADR 0011). Exactly one of TMDBID/TVDBID/OLID identifies the item.
	TVDBID       int64
	OLID         string
	RootFolderID int64 // optional; 0 = no folder assigned yet
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
	lookups := []func() (int64, error){}
	if req.TMDBID != 0 {
		lookups = append(lookups, func() (int64, error) {
			return s.db.GetMediaItemByKindTmdb(ctx, req.Kind, req.TMDBID)
		})
	}
	if req.TVDBID != 0 {
		lookups = append(lookups, func() (int64, error) {
			return s.db.GetMediaItemByKindTvdb(ctx, req.Kind, req.TVDBID)
		})
	}
	if len(lookups) == 0 {
		return fmt.Errorf("%w: an id is required to add a %s", ErrNotFound, req.Kind)
	}
	for _, look := range lookups {
		switch _, err := look(); {
		case err == nil:
			return ErrAlreadyExists
		case !errors.Is(err, sqlite.ErrNotFound):
			return err
		}
	}
	return nil
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
		switch {
		case req.Kind == domain.KindMovie:
			item, err = s.meta.GetMovie(ctx, req.TMDBID)
		case req.TMDBID != 0:
			item, err = s.meta.GetSeries(ctx, req.TMDBID)
		default:
			item, err = s.seriesByTVDB(ctx, req.TVDBID)
		}
	case domain.KindBook:
		if s.books == nil {
			return domain.MediaItem{}, ports.ErrProviderNotConfigured
		}
		if _, err := s.db.GetMediaItemByKindOlid(ctx, req.Kind, req.OLID); err == nil {
			return domain.MediaItem{}, ErrAlreadyExists
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

	s.enrichRatings(ctx, &item)
	// Nothing stored yet — an add starts with whatever the provider says.
	s.enrichAiring(ctx, &item, domain.MediaItem{})
	applyMonitorPreset(&item, req.Monitor)
	item.Monitored = req.Monitored
	item.QualityProfileID = req.QualityProfileID
	item.DownloadPriorityOverride = req.DownloadPriority
	if item.QualityProfileID == 0 {
		// Nothing was chosen, so the kind's configured default applies. It
		// resolves to the built-in when unset or dangling, which is what the
		// hardcoded constant here used to do unconditionally.
		item.QualityProfileID = s.db.DefaultProfileID(ctx, item.Kind)
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
		if item.Kind == domain.KindBook {
			item.Path = filepath.Join(rf.Path, naming.BookFolder(item.Author, item.Title))
		} else {
			item.Path = filepath.Join(rf.Path, naming.FolderName(item.Title, item.Year))
		}
	}

	id, err := s.db.CreateMediaItem(ctx, item)
	if err != nil {
		if errors.Is(err, sqlite.ErrDuplicate) {
			return domain.MediaItem{}, ErrAlreadyExists
		}
		return domain.MediaItem{}, err
	}
	s.log.Info("library: added", "kind", item.Kind, "title", item.Title, "id", id)
	s.publish(MediaAdded{ID: id, Kind: string(item.Kind), Title: item.Title})
	return s.db.GetMediaItemFull(ctx, id)
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
		folder := naming.FolderName(item.Title, item.Year)
		if item.Kind == domain.KindBook {
			folder = naming.BookFolder(item.Author, item.Title)
		}
		return PlacementSuggestion{RootFolderID: rf.ID, Path: filepath.Join(rf.Path, folder)}, nil
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
			if item.Kind == domain.KindBook {
				item.Path = filepath.Join(rf.Path, naming.BookFolder(item.Author, item.Title))
			} else {
				item.Path = filepath.Join(rf.Path, naming.FolderName(item.Title, item.Year))
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
	}
	if err := s.db.UpdateMediaItemPlacement(ctx, item); err != nil {
		return domain.MediaItem{}, err
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
	case stored.Kind == domain.KindMovie:
		fresh, err = s.meta.GetMovie(ctx, stored.IDs.TMDB)
	case stored.Kind == domain.KindSeries && stored.IDs.TMDB != 0:
		fresh, err = s.meta.GetSeries(ctx, stored.IDs.TMDB)
	case stored.Kind == domain.KindSeries:
		// Reached through the chain (ADR 0011), so it has a TVDB id and no
		// TMDB one — asking TMDB for series zero is not a refresh.
		fresh, err = s.seriesByTVDB(ctx, stored.IDs.TVDB)
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

	if err := s.db.UpdateMediaItemMetadata(ctx, id, fresh); err != nil {
		return domain.MediaItem{}, err
	}
	s.log.Info("library: metadata refreshed", "kind", stored.Kind, "title", fresh.Title, "id", id)
	return s.db.GetMediaItemFull(ctx, id)
}

// CopyRequest describes a new additional quality target for an item.
type CopyRequest struct {
	QualityProfileID int64
	RootFolderID     int64 // 0 = share the item's folder (filenames carry [Quality])
	Name             string
	Monitored        bool
}

// AddCopy registers an additional quality copy for a movie or series: its
// own profile, and either its own folder under the chosen root or the
// item's folder. The automation treats it as a first-class target.
func (s *Service) AddCopy(ctx context.Context, itemID int64, req CopyRequest) (domain.MediaItem, error) {
	item, err := s.db.GetMediaItemFull(ctx, itemID)
	if err != nil {
		return domain.MediaItem{}, err
	}
	if item.Kind == domain.KindBook {
		return domain.MediaItem{}, fmt.Errorf("%w: books have no quality copies", ErrUnsupportedKind)
	}
	if _, err := s.db.GetProfile(ctx, req.QualityProfileID); err != nil {
		return domain.MediaItem{}, fmt.Errorf("quality profile: %w", err)
	}
	cp := domain.MediaCopy{
		MediaItemID: itemID, Name: strings.TrimSpace(req.Name),
		QualityProfileID: req.QualityProfileID, Monitored: req.Monitored,
	}
	if req.RootFolderID != 0 {
		rf, err := s.db.GetRootFolder(ctx, req.RootFolderID)
		if err != nil {
			return domain.MediaItem{}, fmt.Errorf("root folder: %w", err)
		}
		cp.RootFolderID = rf.ID
		cp.Path = filepath.Join(rf.Path, naming.FolderName(item.Title, item.Year))
		if cp.Path == item.Path {
			return domain.MediaItem{}, fmt.Errorf(
				"copy folder would collide with the item's own folder — pick a different root, or omit the root to share the folder")
		}
		for _, other := range item.Copies {
			if other.Path == cp.Path {
				return domain.MediaItem{}, fmt.Errorf("another copy already uses %s", cp.Path)
			}
		}
	}
	id, err := s.db.AddMediaCopy(ctx, cp)
	if err != nil {
		return domain.MediaItem{}, err
	}
	s.log.Info("library: copy added", "item", item.Title, "copy", id,
		"profile", req.QualityProfileID, "path", cp.Path)
	return s.db.GetMediaItemFull(ctx, itemID)
}

// UpdateCopy edits a copy's name, profile, or monitoring.
func (s *Service) UpdateCopy(ctx context.Context, itemID, copyID int64, name *string, profileID *int64, monitored *bool) (domain.MediaItem, error) {
	cp, err := s.db.GetMediaCopy(ctx, itemID, copyID)
	if err != nil {
		return domain.MediaItem{}, err
	}
	if name != nil {
		cp.Name = strings.TrimSpace(*name)
	}
	if profileID != nil && *profileID != 0 {
		if _, err := s.db.GetProfile(ctx, *profileID); err != nil {
			return domain.MediaItem{}, fmt.Errorf("quality profile: %w", err)
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
// with the titles sharing each one.
//
// Two items on one folder is always wrong and never self-corrects: whichever
// scan runs last decides which of them the files link to, and the other is a
// card that looks real and holds nothing. Adoption refuses to create the
// second one now (see adoptOne), but libraries built before that guard still
// carry the damage, and damage nobody can see does not get repaired.
func (s *Service) SharedFolders(ctx context.Context) (map[string][]string, error) {
	items, err := s.db.ListMediaItems(ctx, "")
	if err != nil {
		return nil, err
	}
	byPath := map[string][]string{}
	for _, it := range items {
		if it.Path == "" {
			continue
		}
		byPath[it.Path] = append(byPath[it.Path], it.Title)
	}
	for path, titles := range byPath {
		if len(titles) < 2 {
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
