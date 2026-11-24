package discover

import (
	"context"
	"fmt"
	"time"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

type LibraryReader interface {
	DiscoverySeeds(context.Context, string, int) ([]ports.LibrarySeed, bool, error)
	DiscoverySeed(context.Context, int64) (ports.LibrarySeed, error)
}

type LibraryService struct {
	reader LibraryReader
	source ports.LibraryDiscoverySource
}

func NewLibrary(reader LibraryReader, source ports.LibraryDiscoverySource) *LibraryService {
	return &LibraryService{reader: reader, source: source}
}
func (s *LibraryService) Seeds(ctx context.Context, q string, offset int) ([]ports.LibrarySeed, bool, error) {
	if s == nil || s.reader == nil {
		return nil, false, fmt.Errorf("library discovery unavailable")
	}
	return s.reader.DiscoverySeeds(ctx, q, offset)
}
func (s *LibraryService) Seed(ctx context.Context, id int64, kind domain.MediaKind, tmdb int64) (ports.LibrarySeed, error) {
	if s == nil || s.reader == nil {
		return ports.LibrarySeed{}, fmt.Errorf("library discovery unavailable")
	}
	seed, err := s.reader.DiscoverySeed(ctx, id)
	if err != nil {
		return seed, err
	}
	if seed.Item.Kind != kind || seed.Item.TMDBID != tmdb || tmdb <= 0 {
		return ports.LibrarySeed{}, fmt.Errorf("library seed changed; select it again")
	}
	return seed, nil
}
func (s *LibraryService) Items(ctx context.Context, seed ports.LibrarySeed, shelf string) (string, []ports.SearchResult, error) {
	if s.source == nil {
		return "", nil, ports.ErrProviderNotConfigured
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	switch shelf {
	case "related":
		items, err := s.source.Related(ctx, seed.Item.Kind, seed.Item.TMDBID)
		return "More like " + seed.Item.Title, items, err
	case "collection":
		if seed.Item.Kind != domain.KindMovie {
			return "", nil, fmt.Errorf("collections require a movie seed")
		}
		collection, err := s.source.MovieCollection(ctx, seed.Item.TMDBID)
		return collection.Name, collection.Items, err
	default:
		return "", nil, fmt.Errorf("unknown library shelf")
	}
}
