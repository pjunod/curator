package api

import (
	"errors"
	"net/http"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/ports"
)

// TraktClientIDSetting is the app_meta key holding the Trakt app client id.
// Optional: without it Discover still serves the TMDB rows (ADR 0015).
const TraktClientIDSetting = "trakt_client_id"

// ListDiscoverLists implements GET /discover/lists.
//
// An install with no provider configured gets an empty array and a 200, not
// a 503. The catalogue being empty is a true and complete answer to "what can
// you show me"; the UI turns it into "add a TMDB key", which is a better
// screen than an error banner.
func (s *Server) ListDiscoverLists(w http.ResponseWriter, r *http.Request) {
	if s.deps.Discover == nil {
		writeJSON(w, http.StatusOK, []apigen.DiscoverList{})
		return
	}
	lists := s.deps.Discover.Lists(r.Context())
	out := make([]apigen.DiscoverList, 0, len(lists))
	for _, l := range lists {
		out = append(out, apigen.DiscoverList{
			Id: l.ID, Title: l.Title, Blurb: l.Blurb,
			Kind: apigen.MediaKind(l.Kind), Source: l.Source,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// DiscoverItems implements GET /discover/items.
func (s *Server) DiscoverItems(w http.ResponseWriter, r *http.Request, params apigen.DiscoverItemsParams) {
	if s.deps.Discover == nil {
		writeError(w, http.StatusServiceUnavailable, "discovery is not available on this install")
		return
	}
	page := 1
	if params.Page != nil && *params.Page > 0 {
		page = *params.Page
	}
	results, err := s.deps.Discover.Items(r.Context(), params.List, page)
	switch {
	case errors.Is(err, ports.ErrUnknownList):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, ports.ErrProviderNotConfigured):
		writeError(w, http.StatusServiceUnavailable,
			"the provider for this list is not configured — set its key under Settings")
		return
	case err != nil:
		s.deps.Log.Error("discover api error", "list", params.List, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]apigen.SearchResult, 0, len(results))
	for _, res := range results {
		sr := apigen.SearchResult{
			Kind:       apigen.MediaKind(res.Kind),
			TmdbId:     res.TMDBID,
			Title:      res.Title,
			Year:       res.Year,
			Overview:   res.Overview,
			PosterPath: res.PosterPath,
			InLibrary:  false,
		}
		sr.Source = optStr(res.Source)
		if res.TVDBID != 0 {
			id := res.TVDBID
			sr.TvdbId = &id
		}
		out = append(out, sr)
	}
	s.enrichAvailability(r.Context(), out)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
