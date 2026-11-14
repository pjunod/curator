package api

import (
	"errors"
	"net/http"
	"strings"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/discover"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func (s *Server) libraryDiscovery() *discover.LibraryService {
	if s.deps.LibraryDiscovery != nil {
		return s.deps.LibraryDiscovery
	}
	if s.deps.Store == nil {
		return discover.NewLibrary(nil, nil)
	}
	return discover.NewLibrary(s.deps.Store, nil)
}
func (s *Server) ListLibraryDiscoverySeeds(w http.ResponseWriter, r *http.Request, p apigen.ListLibraryDiscoverySeedsParams) {
	q, offset := "", 0
	if p.Q != nil {
		q = strings.TrimSpace(*p.Q)
	}
	if p.Offset != nil {
		offset = *p.Offset
	}
	if len(q) > 200 || offset < 0 || offset > 100000 {
		writeError(w, 400, "invalid seed search or page")
		return
	}
	seeds, more, err := s.libraryDiscovery().Seeds(r.Context(), q, offset)
	if err != nil {
		writeError(w, 503, "library seeds are unavailable")
		return
	}
	items := make([]apigen.SearchResult, len(seeds))
	targets := make([]domain.AvailabilityTarget, len(seeds))
	for i, seed := range seeds {
		items[i] = searchResultDTO(seed.Item)
		targets[i] = domain.AvailabilityTarget{LibraryItemID: seed.LibraryItemID, Kind: seed.Item.Kind, IDs: domain.ExternalIDs{TMDB: seed.Item.TMDBID, TVDB: seed.Item.TVDBID, IMDB: seed.Item.IMDBID}}
	}
	for i, fact := range s.availabilityFacts(r.Context(), targets) {
		items[i].Availability = ptrAvailability(availabilityDTO(fact))
		items[i].InLibrary = fact.Catalog == "present"
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, struct {
		Items   []apigen.SearchResult `json:"items"`
		HasMore bool                  `json:"hasMore"`
	}{items, more})
}
func (s *Server) GetLibraryDiscoveryItems(w http.ResponseWriter, r *http.Request, id int64, p apigen.GetLibraryDiscoveryItemsParams) {
	if id <= 0 || p.TmdbId <= 0 || (p.Kind != "movie" && p.Kind != "series") || (p.Shelf != "related" && p.Shelf != "collection") {
		writeError(w, 400, "invalid seed identity or shelf")
		return
	}
	seed, err := s.libraryDiscovery().Seed(r.Context(), id, domain.MediaKind(p.Kind), p.TmdbId)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			writeError(w, 409, "library seed is no longer available; select it again")
		} else {
			writeError(w, 409, "library seed changed; select it again")
		}
		return
	}
	// Conflicting provider identities must not silently choose one seed.

	if !s.validLibrarySeed(r, seed) {
		writeError(w, 409, "library seed identity is unavailable or conflicting")
		return
	}
	title, values, err := s.libraryDiscovery().Items(r.Context(), seed, string(p.Shelf))
	if err != nil {
		if errors.Is(err, ports.ErrProviderNotConfigured) {
			writeError(w, 503, "configure the metadata provider to load this shelf")
		} else {
			writeError(w, 503, "this shelf could not be refreshed; try again")
		}
		return
	}
	// Provider IO may overlap a rematch or deletion. Recheck the selected identity
	// before returning a shelf attributed to this title.
	currentSeed, seedErr := s.libraryDiscovery().Seed(r.Context(), id, domain.MediaKind(p.Kind), p.TmdbId)
	if seedErr != nil || domain.ExternalIDsConflict(domain.ExternalIDs{TMDB: seed.Item.TMDBID, TVDB: seed.Item.TVDBID, IMDB: seed.Item.IMDBID}, domain.ExternalIDs{TMDB: currentSeed.Item.TMDBID, TVDB: currentSeed.Item.TVDBID, IMDB: currentSeed.Item.IMDBID}) {
		writeError(w, 409, "library seed changed; select it again")
		return
	}
	seed = currentSeed
	facts := s.availabilityFacts(r.Context(), []domain.AvailabilityTarget{{LibraryItemID: id, Kind: seed.Item.Kind, IDs: domain.ExternalIDs{TMDB: seed.Item.TMDBID, TVDB: seed.Item.TVDBID, IMDB: seed.Item.IMDBID}}})
	if !s.validLibrarySeed(r, seed) {
		writeError(w, 409, "library seed identity changed; select it again")
		return
	}
	items := []apigen.SearchResult{}
	for _, v := range values {
		items = append(items, searchResultDTO(v))
	}
	s.enrichAvailability(r.Context(), items)
	seedDTO := searchResultDTO(seed.Item)
	seedDTO.Availability = ptrAvailability(availabilityDTO(facts[0]))
	seedDTO.InLibrary = true
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, struct {
		Title  string                `json:"title"`
		Source string                `json:"source"`
		Seed   apigen.SearchResult   `json:"seed"`
		Items  []apigen.SearchResult `json:"items"`
	}{title, "tmdb", seedDTO, items})
}
func ptrAvailability(v apigen.Availability) *apigen.Availability { return &v }

func (s *Server) validLibrarySeed(r *http.Request, seed ports.LibrarySeed) bool {
	facts := s.availabilityFacts(r.Context(), []domain.AvailabilityTarget{{LibraryItemID: seed.LibraryItemID, Kind: seed.Item.Kind, IDs: domain.ExternalIDs{TMDB: seed.Item.TMDBID, TVDB: seed.Item.TVDBID, IMDB: seed.Item.IMDBID}}})
	return len(facts) == 1 && facts[0].Catalog == "present" && facts[0].LibraryItemID == seed.LibraryItemID
}
