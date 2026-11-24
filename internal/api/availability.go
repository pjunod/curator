package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/availability"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/ports"
)

func availabilityDTO(v domain.Availability) apigen.Availability {
	out := apigen.Availability{Catalog: apigen.AvailabilityCatalog(v.Catalog), Presence: apigen.AvailabilityPresence(v.Presence), CopyId: v.CopyID, Expected: v.Expected, Present: v.Present, Offloaded: v.Offloaded, Acquisition: apigen.AvailabilityAcquisition(v.Acquisition), CheckedAt: v.CheckedAt, Reason: optStr(v.Reason)}
	if v.LibraryItemID > 0 {
		out.LibraryItemId = &v.LibraryItemID
	}
	if len(v.Seasons) > 0 {
		seasons := []apigen.SeasonAvailability{}
		for _, sn := range v.Seasons {
			seasons = append(seasons, apigen.SeasonAvailability{Number: sn.Number, Expected: sn.Expected, Present: sn.Present, Offloaded: sn.Offloaded, UnknownDates: sn.UnknownDates})
		}
		out.Seasons = &seasons
	}
	return out
}
func (s *Server) availabilityFacts(ctx context.Context, targets []domain.AvailabilityTarget) []domain.Availability {
	var reader availability.Reader
	if s.deps.Store != nil {
		reader = s.deps.Store
	}
	service := availability.New(reader)
	out := []domain.Availability{}
	for start := 0; start < len(targets); start += 100 {
		out = append(out, service.Lookup(ctx, targets[start:min(start+100, len(targets))])...)
	}
	return out
}
func (s *Server) LookupAvailability(w http.ResponseWriter, r *http.Request) {
	var body []apigen.AvailabilityTarget
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body == nil || len(body) > 100 {
		writeError(w, 400, "expected up to 100 availability identities")
		return
	}
	if decoder.Decode(new(any)) != io.EOF {
		writeError(w, 400, "expected one JSON array")
		return
	}
	targets := make([]domain.AvailabilityTarget, 0, len(body))
	for _, v := range body {
		if !domain.ValidKind(domain.MediaKind(v.Kind)) {
			writeError(w, 400, "invalid media kind")
			return
		}
		t := domain.AvailabilityTarget{Kind: domain.MediaKind(v.Kind)}
		if v.LibraryItemId != nil {
			t.LibraryItemID = *v.LibraryItemId
		}
		if v.TmdbId != nil {
			t.IDs.TMDB = *v.TmdbId
		}
		if v.TvdbId != nil {
			t.IDs.TVDB = *v.TvdbId
		}
		if v.ImdbId != nil {
			t.IDs.IMDB = *v.ImdbId
		}
		if v.Olid != nil {
			t.IDs.OLID = *v.Olid
		}
		if v.Isbn13 != nil {
			t.IDs.ISBN13 = *v.Isbn13
		}
		if v.Asin != nil {
			t.IDs.ASIN = *v.Asin
		}
		if t.LibraryItemID < 0 || t.IDs.TMDB < 0 || t.IDs.TVDB < 0 {
			writeError(w, 400, "invalid identity")
			return
		}
		targets = append(targets, t)
	}
	out := []apigen.Availability{}
	for _, v := range s.availabilityFacts(r.Context(), targets) {
		out = append(out, availabilityDTO(v))
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, out)
}
func searchResultDTO(v ports.SearchResult) apigen.SearchResult {
	out := apigen.SearchResult{Kind: apigen.MediaKind(v.Kind), TmdbId: v.TMDBID, Title: v.Title, Year: v.Year, Overview: v.Overview, PosterPath: v.PosterPath, Source: optStr(v.Source), ImdbId: optStr(v.IMDBID), Olid: optStr(v.OLID), Author: optStr(v.Author), HydrationSource: optStr(v.HydrationSource)}
	if v.TVDBID > 0 {
		out.TvdbId = &v.TVDBID
	}
	return out
}
func (s *Server) enrichAvailability(ctx context.Context, items []apigen.SearchResult) {
	targets := make([]domain.AvailabilityTarget, len(items))
	for i, v := range items {
		targets[i].Kind = domain.MediaKind(v.Kind)
		targets[i].IDs.TMDB = v.TmdbId
		if v.TvdbId != nil {
			targets[i].IDs.TVDB = *v.TvdbId
		}
		if v.ImdbId != nil {
			targets[i].IDs.IMDB = *v.ImdbId
		}
		if v.Olid != nil {
			targets[i].IDs.OLID = *v.Olid
		}
	}
	for i, v := range s.availabilityFacts(ctx, targets) {
		dto := availabilityDTO(v)
		items[i].Availability = &dto
		items[i].InLibrary = v.Catalog == "present"
	}
}
