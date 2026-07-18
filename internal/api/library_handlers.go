package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	apigen "github.com/monarr-media/monarr/internal/api/gen"
	"github.com/monarr-media/monarr/internal/app/library"
	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/ports"
)

// TMDBKeySetting is the app_meta key holding the metadata provider key.
const TMDBKeySetting = "tmdb_api_key"

// OMDBKeySetting is the app_meta key for the optional OMDb key, which
// unlocks Rotten Tomatoes / IMDb / Metacritic ratings.
const OMDBKeySetting = "omdb_api_key"

// APIKeySetting is the app_meta key holding Monarr's own API key —
// required by the compat personalities (X-Api-Key) and, from Phase 5,
// the native API.
const APIKeySetting = "api_key"

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

func ratingsDTO(rs []domain.Rating) []apigen.Rating {
	out := make([]apigen.Rating, 0, len(rs))
	for _, r := range rs {
		dto := apigen.Rating{Source: r.Source, Value: float32(r.Value), Scale: r.Scale}
		if r.Votes > 0 {
			v := r.Votes
			dto.Votes = &v
		}
		out = append(out, dto)
	}
	return out
}

func summaryDTO(m domain.MediaItem) apigen.MediaItemSummary {
	return apigen.MediaItemSummary{
		Id:               m.ID,
		Kind:             apigen.MediaKind(m.Kind),
		Title:            m.Title,
		Year:             m.Year,
		Author:           m.Author,
		PosterPath:       m.PosterPath,
		Monitored:        m.Monitored,
		Path:             m.Path,
		Rating:           float32(m.Rating),
		RatingVotes:      m.RatingVotes,
		Ratings:          ratingsDTO(m.Ratings),
		EpisodeCount:     m.EpisodeCount,
		EpisodeFileCount: m.EpisodeFileCount,
		FileCount:        m.FileCount,
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
		Id:               m.ID,
		Kind:             apigen.MediaKind(m.Kind),
		Title:            m.Title,
		Year:             m.Year,
		PosterPath:       m.PosterPath,
		BackdropPath:     m.BackdropPath,
		Overview:         m.Overview,
		Genres:           m.Genres,
		Status:           m.Status,
		ReleaseDate:      m.ReleaseDate,
		Runtime:          m.Runtime,
		Rating:           float32(m.Rating),
		RatingVotes:      m.RatingVotes,
		Ratings:          ratingsDTO(m.Ratings),
		Monitored:        m.Monitored,
		QualityProfileId: m.QualityProfileID,
		Path:             m.Path,
		RootFolderId:     m.RootFolderID,
		Ended:            m.Ended,
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
	// Search on add (Sonarr semantics): fire-and-forget; failures land in
	// the log and the item stays wanted for the RSS/backlog loops.
	if body.SearchNow != nil && *body.SearchNow && item.Monitored && s.deps.Acquisition != nil {
		go func(itemID int64) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := s.deps.Acquisition.AutoSearchItem(ctx, itemID); err != nil {
				s.deps.Log.Warn("search on add failed", "item", itemID, "err", err)
			}
		}(item.ID)
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

// UpdateLibraryItem implements PATCH /library/{id}: per-item edit of
// monitoring, quality profile, and location.
func (s *Server) UpdateLibraryItem(w http.ResponseWriter, r *http.Request, id int64) {
	var body apigen.UpdateLibraryItemJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	item, err := s.deps.Library.UpdateItem(r.Context(), id, library.UpdateRequest{
		Monitored:        body.Monitored,
		QualityProfileID: body.QualityProfileId,
		RootFolderID:     body.RootFolderId,
		Path:             body.Path,
	})
	if err != nil {
		if strings.Contains(err.Error(), "path must be absolute") || strings.Contains(err.Error(), "root folder") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.libraryErr(w, err)
		return
	}
	// Monitoring and profile edits change what is wanted.
	if s.deps.Acquisition != nil {
		s.deps.Acquisition.InvalidateWanted()
	}
	writeJSON(w, http.StatusOK, detailDTO(item))
}

// SetSeasonMonitored implements PATCH /library/{id}/seasons/{season}:
// flip a season's monitored flag, cascading to its episodes.
func (s *Server) SetSeasonMonitored(w http.ResponseWriter, r *http.Request, id int64, season int) {
	var body apigen.SetSeasonMonitoredJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	item, err := s.deps.Library.SetSeasonMonitored(r.Context(), id, season, body.Monitored)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	if s.deps.Acquisition != nil {
		s.deps.Acquisition.InvalidateWanted()
	}
	writeJSON(w, http.StatusOK, detailDTO(item))
}

// SetEpisodeMonitored implements PATCH /library/{id}/episodes/{episodeId}.
func (s *Server) SetEpisodeMonitored(w http.ResponseWriter, r *http.Request, id int64, episodeID int64) {
	var body apigen.SetEpisodeMonitoredJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	item, err := s.deps.Library.SetEpisodeMonitored(r.Context(), id, episodeID, body.Monitored)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	if s.deps.Acquisition != nil {
		s.deps.Acquisition.InvalidateWanted()
	}
	writeJSON(w, http.StatusOK, detailDTO(item))
}

// RefreshLibraryItem implements POST /library/{id}/refresh: re-hydrate the
// item's metadata from its provider and return the updated detail.
func (s *Server) RefreshLibraryItem(w http.ResponseWriter, r *http.Request, id int64) {
	item, err := s.deps.Library.RefreshItem(r.Context(), id)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	// New episodes may have appeared — the wanted index must notice.
	if s.deps.Acquisition != nil {
		s.deps.Acquisition.InvalidateWanted()
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

func keyHint(key string) string {
	if n := len(key); n > 4 {
		return "…" + key[n-4:]
	} else if n > 0 {
		return strings.Repeat("•", n)
	}
	return ""
}

// GetSettings implements GET /settings (secrets masked).
func (s *Server) GetSettings(w http.ResponseWriter, r *http.Request) {
	key := s.readSetting(r.Context(), TMDBKeySetting)
	omdbKey := s.readSetting(r.Context(), OMDBKeySetting)
	out := apigen.Settings{
		TmdbApiKeyConfigured: key != "",
		TmdbApiKeyHint:       keyHint(key),
	}
	omdbConfigured, omdbHint := omdbKey != "", keyHint(omdbKey)
	out.OmdbApiKeyConfigured = &omdbConfigured
	out.OmdbApiKeyHint = &omdbHint
	if apiKey := s.readSetting(r.Context(), APIKeySetting); apiKey != "" {
		out.ApiKey = &apiKey
	}
	authOn := s.authRequired(r.Context())
	out.AuthRequired = &authOn
	writeJSON(w, http.StatusOK, out)
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
	if body.OmdbApiKey != nil {
		if err := s.deps.Settings.SetMeta(r.Context(), OMDBKeySetting, strings.TrimSpace(*body.OmdbApiKey)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := s.applyAuthSettings(r.Context(), body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
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
