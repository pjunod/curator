package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	apigen "github.com/monarr-media/monarr/internal/api/gen"
	"github.com/monarr-media/monarr/internal/app/library"
	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// TMDBKeySetting is the app_meta key holding the metadata provider key.
const TMDBKeySetting = "tmdb_api_key"

// libraryErr maps service errors to HTTP responses.
func (s *Server) libraryErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, library.ErrAlreadyExists):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, library.ErrUnsupportedKind):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ports.ErrProviderNotConfigured):
		writeError(w, http.StatusServiceUnavailable,
			"TMDB API key not configured — set it under Settings")
	case errors.Is(err, library.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		s.deps.Log.Error("library api error", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// ---- DTO mapping ----

func summaryDTO(m domain.MediaItem) apigen.MediaItemSummary {
	return apigen.MediaItemSummary{
		Id:         m.ID,
		Kind:       apigen.MediaKind(m.Kind),
		Title:      m.Title,
		Year:       m.Year,
		Author:     m.Author,
		PosterPath: m.PosterPath,
		Monitored:  m.Monitored,
		Path:       m.Path,
	}
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func detailDTO(m domain.MediaItem) apigen.MediaItemDetail {
	d := apigen.MediaItemDetail{
		Id:           m.ID,
		Kind:         apigen.MediaKind(m.Kind),
		Title:        m.Title,
		Year:         m.Year,
		PosterPath:   m.PosterPath,
		BackdropPath: m.BackdropPath,
		Overview:     m.Overview,
		Genres:       m.Genres,
		Status:       m.Status,
		ReleaseDate:  m.ReleaseDate,
		Runtime:      m.Runtime,
		Monitored:    m.Monitored,
		Path:         m.Path,
		RootFolderId: m.RootFolderID,
		Ended:        m.Ended,
		Ids: apigen.ExternalIds{
			Tmdb: m.IDs.TMDB,
		},
		Seasons: []apigen.SeasonInfo{},
		Files:   []apigen.MediaFileInfo{},
		AddedAt: m.AddedAt,
	}
	if m.Genres == nil {
		d.Genres = []string{}
	}
	d.Author = m.Author
	d.Ids.Imdb = optStr(m.IDs.IMDB)
	d.Ids.Isbn13 = optStr(m.IDs.ISBN13)
	d.Ids.Olid = optStr(m.IDs.OLID)
	d.Ids.Asin = optStr(m.IDs.ASIN)
	if m.IDs.TVDB != 0 {
		tvdb := m.IDs.TVDB
		d.Ids.Tvdb = &tvdb
	}
	for _, season := range m.Seasons {
		si := apigen.SeasonInfo{
			Number:    season.Number,
			Monitored: season.Monitored,
			Episodes:  []apigen.EpisodeInfo{},
		}
		for _, e := range season.Episodes {
			si.Episodes = append(si.Episodes, apigen.EpisodeInfo{
				Id:            e.ID,
				SeasonNumber:  e.SeasonNumber,
				EpisodeNumber: e.EpisodeNumber,
				Title:         e.Title,
				AirDate:       e.AirDate,
				Monitored:     e.Monitored,
				HasFile:       e.HasFile,
			})
		}
		d.Seasons = append(d.Seasons, si)
	}
	for _, f := range m.Files {
		ids := f.EpisodeIDs
		if ids == nil {
			ids = []int64{}
		}
		d.Files = append(d.Files, apigen.MediaFileInfo{
			Id: f.ID, Path: f.Path, Size: f.Size, EpisodeIds: ids,
		})
	}
	return d
}

// ---- library ----

// ListLibrary implements GET /library.
func (s *Server) ListLibrary(w http.ResponseWriter, r *http.Request, params apigen.ListLibraryParams) {
	kind := domain.MediaKind("")
	if params.Kind != nil {
		kind = domain.MediaKind(*params.Kind)
	}
	items, err := s.deps.Library.List(r.Context(), kind)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	out := make([]apigen.MediaItemSummary, 0, len(items))
	for _, m := range items {
		out = append(out, summaryDTO(m))
	}
	writeJSON(w, http.StatusOK, out)
}

// AddLibraryItem implements POST /library.
func (s *Server) AddLibraryItem(w http.ResponseWriter, r *http.Request) {
	var body apigen.AddLibraryItemJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req := library.AddRequest{
		Kind:      domain.MediaKind(body.Kind),
		Monitored: true,
	}
	if body.TmdbId != nil {
		req.TMDBID = *body.TmdbId
	}
	if body.Olid != nil {
		req.OLID = *body.Olid
	}
	if body.Monitored != nil {
		req.Monitored = *body.Monitored
	}
	if body.RootFolderId != nil {
		req.RootFolderID = *body.RootFolderId
	}
	if body.QualityProfileId != nil {
		req.QualityProfileID = *body.QualityProfileId
	}
	item, err := s.deps.Library.Add(r.Context(), req)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, detailDTO(item))
}

// GetLibraryItem implements GET /library/{id}.
func (s *Server) GetLibraryItem(w http.ResponseWriter, r *http.Request, id int64) {
	item, err := s.deps.Library.Get(r.Context(), id)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detailDTO(item))
}

// DeleteLibraryItem implements DELETE /library/{id}.
func (s *Server) DeleteLibraryItem(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.deps.Library.Delete(r.Context(), id); err != nil {
		s.libraryErr(w, err)
		return
	}
	if s.deps.Acquisition != nil {
		s.deps.Acquisition.InvalidateWanted()
	}
	w.WriteHeader(http.StatusNoContent)
}

// ScanLibrary implements POST /library/scan: queues the reconcile task.
func (s *Server) ScanLibrary(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.Scheduler.Trigger(ScanTaskName); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// GetScanReport implements GET /library/scan/report.
func (s *Server) GetScanReport(w http.ResponseWriter, r *http.Request) {
	report, ok, err := s.deps.Library.LastScanReport(r.Context())
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no scan has run yet")
		return
	}
	// The service report is already JSON-shaped per the spec.
	writeJSON(w, http.StatusOK, report)
}

// SearchMetadata implements GET /metadata/search.
func (s *Server) SearchMetadata(w http.ResponseWriter, r *http.Request, params apigen.SearchMetadataParams) {
	results, err := s.deps.Library.Search(r.Context(), domain.MediaKind(params.Kind), params.Query)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	// Mark results already in the library (movies/series by TMDB id,
	// books by Open Library work id).
	inLib := map[int64]bool{}
	inLibOlid := map[string]bool{}
	if items, err := s.deps.Library.List(r.Context(), domain.MediaKind(params.Kind)); err == nil {
		for _, it := range items {
			inLib[it.IDs.TMDB] = true
			if it.IDs.OLID != "" {
				inLibOlid[it.IDs.OLID] = true
			}
		}
	}
	out := make([]apigen.SearchResult, 0, len(results))
	for _, res := range results {
		sr := apigen.SearchResult{
			Kind:       apigen.MediaKind(res.Kind),
			TmdbId:     res.TMDBID,
			Olid:       optStr(res.OLID),
			Author:     optStr(res.Author),
			Title:      res.Title,
			Year:       res.Year,
			Overview:   res.Overview,
			PosterPath: res.PosterPath,
		}
		if res.OLID != "" {
			sr.InLibrary = inLibOlid[res.OLID]
		} else {
			sr.InLibrary = inLib[res.TMDBID]
		}
		out = append(out, sr)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- root folders ----

// ListRootFolders implements GET /rootfolders.
func (s *Server) ListRootFolders(w http.ResponseWriter, r *http.Request) {
	roots, err := s.deps.Library.ListRootFolders(r.Context())
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	out := make([]apigen.RootFolder, 0, len(roots))
	for _, rf := range roots {
		out = append(out, apigen.RootFolder{
			Id: rf.ID, Path: rf.Path, FreeBytes: rf.FreeBytes, Accessible: rf.Accessible,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// AddRootFolder implements POST /rootfolders.
func (s *Server) AddRootFolder(w http.ResponseWriter, r *http.Request) {
	var body apigen.AddRootFolderJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	rf, err := s.deps.Library.AddRootFolder(r.Context(), body.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, apigen.RootFolder{
		Id: rf.ID, Path: rf.Path, FreeBytes: 0, Accessible: true,
	})
}

// DeleteRootFolder implements DELETE /rootfolders/{id}.
func (s *Server) DeleteRootFolder(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.deps.Library.DeleteRootFolder(r.Context(), id); err != nil {
		s.libraryErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- settings ----

// GetSettings implements GET /settings (secrets masked).
func (s *Server) GetSettings(w http.ResponseWriter, r *http.Request) {
	key := s.readSetting(r.Context(), TMDBKeySetting)
	hint := ""
	if n := len(key); n > 4 {
		hint = "…" + key[n-4:]
	} else if n > 0 {
		hint = strings.Repeat("•", n)
	}
	writeJSON(w, http.StatusOK, apigen.Settings{
		TmdbApiKeyConfigured: key != "",
		TmdbApiKeyHint:       hint,
	})
}

// UpdateSettings implements PUT /settings.
func (s *Server) UpdateSettings(w http.ResponseWriter, r *http.Request) {
	var body apigen.UpdateSettingsJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.TmdbApiKey != nil {
		if err := s.deps.Settings.SetMeta(r.Context(), TMDBKeySetting, strings.TrimSpace(*body.TmdbApiKey)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) readSetting(ctx context.Context, key string) string {
	if s.deps.Settings == nil {
		return ""
	}
	v, err := s.deps.Settings.GetMeta(ctx, key)
	if err != nil {
		return ""
	}
	return v
}
