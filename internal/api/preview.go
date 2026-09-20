package api

import (
	"net/http"
	"strconv"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/domain"
)

func (s *Server) PreviewMetadata(w http.ResponseWriter, r *http.Request, params apigen.PreviewMetadataParams) {
	var ref domain.ExternalRef
	count := 0
	if params.TmdbId != nil {
		count++
		ref = domain.ExternalRef{Provider: "tmdb", Value: strconv.FormatInt(*params.TmdbId, 10)}
	}
	if params.TvdbId != nil {
		count++
		ref = domain.ExternalRef{Provider: "tvdb", Value: strconv.FormatInt(*params.TvdbId, 10)}
	}
	if params.Olid != nil {
		count++
		ref = domain.ExternalRef{Provider: "olid", Value: *params.Olid}
	}
	if count != 1 || len(r.URL.Query()["tmdbId"]) > 1 || len(r.URL.Query()["tvdbId"]) > 1 || len(r.URL.Query()["olid"]) > 1 || len(r.URL.Query()["kind"]) != 1 {
		s.libraryErr(w, library.ErrInvalidExternalID)
		return
	}
	preview, err := s.deps.Library.PreviewMetadata(r.Context(), domain.MediaKind(params.Kind), ref)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	item := preview.Item
	out := apigen.MetadataPreview{
		Kind: apigen.MediaKind(item.Kind), Title: item.Title, Overview: item.Overview,
		Ownership:      apigen.MetadataPreviewOwnership(preview.Ownership),
		Addability:     apigen.MetadataPreviewAddability(preview.Addability),
		AddBlockReason: optStr(preview.AddBlockReason), Author: optStr(item.Author),
		PosterPath: optStr(item.PosterPath), PreviewSource: optStr(item.Source), Status: optStr(item.Status),
	}
	if item.Year > 0 {
		out.Year = &item.Year
	}
	if item.Runtime > 0 {
		out.RuntimeMinutes = &item.Runtime
	}
	if len(item.Genres) > 0 {
		out.Genres = &item.Genres
	}
	if item.IDs.TMDB > 0 {
		out.Ids.Tmdb = &item.IDs.TMDB
	}
	if item.IDs.TVDB > 0 {
		out.Ids.Tvdb = &item.IDs.TVDB
	}
	out.Ids.Imdb, out.Ids.Olid = optStr(item.IDs.IMDB), optStr(item.IDs.OLID)
	if preview.LibraryItemID > 0 {
		out.LibraryItemId = &preview.LibraryItemID
	}
	if item.Kind == domain.KindBook {
		editions := make([]apigen.BookType, 0, len(preview.BookTypes))
		for _, edition := range preview.BookTypes {
			editions = append(editions, apigen.BookType(edition))
		}
		out.BookTypes = &editions
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}
