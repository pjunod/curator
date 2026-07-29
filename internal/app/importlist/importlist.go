// Package importlist syncs external lists into the library (Phase 5):
// each enabled list is fetched, and entries not yet in the library are
// added with the list's root folder, profile, and monitoring policy.
package importlist

import (
	"context"
	"errors"
	"log/slog"

	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Sources are the fetchers a Service can call, injected from cmd/monarr
// (real TMDB/Trakt adapters) or tests (fakes).
type Sources struct {
	// TMDBDiscover fetches "popular" or "top_rated" movies.
	TMDBDiscover func(ctx context.Context, kind string) ([]ports.SearchResult, error)
	// TraktList fetches a public list; clientID comes from the list config.
	TraktList func(ctx context.Context, clientID, user, slug string) ([]ports.SearchResult, error)
}

// Service syncs lists.
type Service struct {
	db  *sqlite.DB
	lib *library.Service
	src Sources
	log *slog.Logger
}

// New returns a Service.
func New(db *sqlite.DB, lib *library.Service, src Sources, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{db: db, lib: lib, src: src, log: log}
}

// Sync fetches every enabled list and adds missing entries. Individual
// list failures are logged, not fatal — one broken list must not stop the
// others.
func (s *Service) Sync(ctx context.Context) error {
	lists, err := s.db.ListImportLists(ctx)
	if err != nil {
		return err
	}
	for _, l := range lists {
		if !l.Enabled {
			continue
		}
		added, err := s.syncOne(ctx, l)
		if err != nil {
			s.log.Warn("importlist: sync failed", "list", l.Name, "err", err)
			continue
		}
		if added > 0 {
			s.log.Info("importlist: synced", "list", l.Name, "added", added)
		}
	}
	return nil
}

func (s *Service) syncOne(ctx context.Context, l sqlite.ImportList) (int, error) {
	var results []ports.SearchResult
	var err error
	switch l.Type {
	case "tmdb-popular":
		results, err = s.src.TMDBDiscover(ctx, "popular")
	case "tmdb-top":
		results, err = s.src.TMDBDiscover(ctx, "top_rated")
	case "trakt-list":
		results, err = s.src.TraktList(ctx, l.Config["clientId"], l.Config["user"], l.Config["slug"])
	default:
		return 0, errors.New("unknown list type " + l.Type)
	}
	if err != nil {
		return 0, err
	}

	added := 0
	for _, res := range results {
		kind := res.Kind
		if kind == "" {
			kind = domain.MediaKind(l.Kind)
		}
		_, err := s.lib.Add(ctx, library.AddRequest{
			Kind: kind, TMDBID: res.TMDBID,
			RootFolderID:     l.RootFolderID,
			QualityProfileID: l.QualityProfileID,
			Monitored:        l.Monitored,
		})
		switch {
		case err == nil:
			added++
		case errors.Is(err, library.ErrAlreadyExists):
			// The steady state: lists mostly contain what we already have.
		default:
			s.log.Warn("importlist: add failed", "list", l.Name, "title", res.Title, "err", err)
		}
	}
	return added, nil
}
