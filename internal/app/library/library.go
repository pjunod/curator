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

	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/naming"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
	"github.com/monarr-media/monarr/internal/ports"
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
	db      *sqlite.DB
	meta    ports.MetadataProvider
	books   ports.BookProvider
	ratings ports.RatingsProvider // optional (OMDb): RT/IMDb/Metacritic
	bus     *bus.Bus
	log     *slog.Logger
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

// WithRatings attaches the extra-ratings provider (OMDb) and returns s.
func (s *Service) WithRatings(rp ports.RatingsProvider) *Service {
	s.ratings = rp
	return s
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

// Search proxies the metadata provider for the given kind.
func (s *Service) Search(ctx context.Context, kind domain.MediaKind, query string) ([]ports.SearchResult, error) {
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

// ---- add / browse ----

// AddRequest is what the API sends to put something in the library.
// TMDBID identifies movies/series; OLID identifies books (ADR 0006).
type AddRequest struct {
	Kind             domain.MediaKind
	TMDBID           int64
	OLID             string
	RootFolderID     int64 // optional; 0 = no folder assigned yet
	QualityProfileID int64 // optional; 0 = kind default (1, or Ebook for books)
	Monitored        bool
	// Monitor picks which seasons start monitored (series only):
	// "all" (default), "latest" (newest season only), or "none".
	Monitor string
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
		if _, err := s.db.GetMediaItemByKindTmdb(ctx, req.Kind, req.TMDBID); err == nil {
			return domain.MediaItem{}, ErrAlreadyExists
		} else if !errors.Is(err, sqlite.ErrNotFound) {
			return domain.MediaItem{}, err
		}
		if req.Kind == domain.KindMovie {
			item, err = s.meta.GetMovie(ctx, req.TMDBID)
		} else {
			item, err = s.meta.GetSeries(ctx, req.TMDBID)
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
	default:
		return domain.MediaItem{}, fmt.Errorf("%w: %q", ErrUnsupportedKind, req.Kind)
	}
	if err != nil {
		return domain.MediaItem{}, err
	}

	s.enrichRatings(ctx, &item)
	applyMonitorPreset(&item, req.Monitor)
	item.Monitored = req.Monitored
	item.QualityProfileID = req.QualityProfileID
	if item.QualityProfileID == 0 && item.Kind == domain.KindBook {
		item.QualityProfileID = quality.EbookProfileID
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
	RootFolderID     *int64  // 0 clears the assignment (and the path)
	Path             *string // explicit folder override; wins over RootFolderID's recompute
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

	var fresh domain.MediaItem
	switch stored.Kind {
	case domain.KindMovie:
		fresh, err = s.meta.GetMovie(ctx, stored.IDs.TMDB)
	case domain.KindSeries:
		fresh, err = s.meta.GetSeries(ctx, stored.IDs.TMDB)
	case domain.KindBook:
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
	return s.db.ListMediaItems(ctx, kind)
}

// Get returns one fully hydrated item.
func (s *Service) Get(ctx context.Context, id int64) (domain.MediaItem, error) {
	return s.db.GetMediaItemFull(ctx, id)
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
}

// AddRootFolder validates and registers a library root. An empty kind means
// mixed, which is how every root behaved before ADR 0009.
func (s *Service) AddRootFolder(ctx context.Context, path string, kind domain.RootKind) (domain.RootFolder, error) {
	if kind == "" {
		kind = domain.KindMixed
	}
	if !domain.ValidRootKind(kind) {
		return domain.RootFolder{}, fmt.Errorf("unknown root folder kind %q", kind)
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
		return domain.RootFolder{}, fmt.Errorf("unknown root folder kind %q", kind)
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
	out := make([]RootFolderInfo, 0, len(roots))
	for _, rf := range roots {
		info := RootFolderInfo{RootFolder: rf}
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
