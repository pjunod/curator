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
	db    *sqlite.DB
	meta  ports.MetadataProvider
	books ports.BookProvider
	bus   *bus.Bus
	log   *slog.Logger
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

// AddRootFolder validates and registers a library root.
func (s *Service) AddRootFolder(ctx context.Context, path string) (domain.RootFolder, error) {
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
	rf, err := s.db.AddRootFolder(ctx, filepath.Clean(path))
	if errors.Is(err, sqlite.ErrDuplicate) {
		return domain.RootFolder{}, fmt.Errorf("root folder already registered")
	}
	return rf, err
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
