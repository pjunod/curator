package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/downloadpriority"
	"github.com/pjunod/monarr/internal/domain/mediainfo"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/ports"
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
	case errors.Is(err, library.ErrManualEntry):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, library.ErrRootKindMismatch):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, library.ErrInvalidInput):
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

func bookTypeDTO(m domain.MediaItem) *apigen.BookType {
	if m.Kind != domain.KindBook {
		return nil
	}
	bookType := quality.BookTypeForSource(m.QualityTarget.Source)
	if !quality.ValidBookType(bookType) {
		return nil
	}
	v := apigen.BookType(bookType)
	return &v
}

func summaryDTO(m domain.MediaItem) apigen.MediaItemSummary {
	out := apigen.MediaItemSummary{
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
		AddedAt:          m.AddedAt,
	}
	out.Source = optStr(m.Source)
	out.BookType = bookTypeDTO(m)
	if quality.Rank(m.Quality) > 0 {
		out.Quality = optStr(m.Quality.Display())
	}
	if quality.Rank(m.QualityTarget) > 0 {
		out.QualityTarget = optStr(m.QualityTarget.Display())
	}
	if m.Upgrade != domain.UpgradeUnknown {
		u := apigen.MediaItemSummaryUpgrade(m.Upgrade)
		out.Upgrade = &u
	}
	return out
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func detailDTO(m domain.MediaItem) apigen.MediaItemDetail {
	d := apigen.MediaItemDetail{
		Id:                       m.ID,
		Kind:                     apigen.MediaKind(m.Kind),
		Title:                    m.Title,
		Year:                     m.Year,
		PosterPath:               m.PosterPath,
		BackdropPath:             m.BackdropPath,
		Overview:                 m.Overview,
		Genres:                   m.Genres,
		Status:                   m.Status,
		ReleaseDate:              m.ReleaseDate,
		Runtime:                  m.Runtime,
		Rating:                   float32(m.Rating),
		RatingVotes:              m.RatingVotes,
		Ratings:                  ratingsDTO(m.Ratings),
		Monitored:                m.Monitored,
		QualityProfileId:         m.QualityProfileID,
		DownloadPriority:         m.DownloadPriority,
		DownloadPriorityOverride: m.DownloadPriorityOverride,
		Path:                     m.Path,
		RootFolderId:             m.RootFolderID,
		Ended:                    m.Ended,
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
	d.BookType = bookTypeDTO(m)
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
		fi := apigen.MediaFileInfo{
			Id: f.ID, Path: f.Path, Size: f.Size, EpisodeIds: ids,
		}
		// What this file is, and how we know (ADR 0013). The label and the
		// facts line are rendered here rather than in the client so the API,
		// the UI, and anything else reading this all say the same thing.
		fi.Quality = optStr(qualityText(f))
		if f.Provenance != "" {
			prov := apigen.MediaFileInfoProvenance(f.Provenance)
			fi.Provenance = &prov
		}
		fi.ProvenanceLabel = optStr(f.Provenance.Label())
		verified := f.SourceVerified()
		fi.Verified = &verified
		fi.Facts = optStr(f.Info.Summary())
		// Why we do not believe this file, in a sentence the UI can print.
		// Recomputed from the stored measurement rather than persisted, so
		// retuning a threshold re-grades the whole library for free — the
		// provenance column already records the verdict itself.
		if f.Provenance == mediainfo.ProvenanceImplausible {
			if why, bad := mediainfo.Check(f.Info, m.Runtime); bad {
				fi.Implausible = optStr(why.Reason)
			} else {
				fi.Implausible = optStr("this file's measurements did not add up when it was last checked")
			}
		}
		fi.SourceRelease = optStr(f.SourceRelease)
		if f.CopyID != 0 {
			cid := f.CopyID
			fi.CopyId = &cid
		}
		d.Files = append(d.Files, fi)
	}
	d.Source = optStr(m.Source)
	if quality.Rank(m.Quality) > 0 {
		d.Quality = optStr(m.Quality.Display())
	}
	if quality.Rank(m.QualityTarget) > 0 {
		d.QualityTarget = optStr(m.QualityTarget.Display())
	}
	if m.Upgrade != domain.UpgradeUnknown {
		u := apigen.MediaItemDetailUpgrade(m.Upgrade)
		d.Upgrade = &u
	}
	d.Copies = []apigen.MediaCopy{}
	for _, c := range m.Copies {
		d.Copies = append(d.Copies, apigen.MediaCopy{
			Id: c.ID, Name: c.Name, QualityProfileId: c.QualityProfileID,
			RootFolderId: c.RootFolderID, Path: c.Path, Monitored: c.Monitored,
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
	if body.TvdbId != nil {
		req.TVDBID = *body.TvdbId
	}
	if body.Olid != nil {
		req.OLID = *body.Olid
	}
	if body.BookType != nil {
		req.BookType = quality.BookType(*body.BookType)
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
	if body.DownloadPriority != nil {
		priority := *body.DownloadPriority
		if !downloadpriority.Valid(priority) {
			writeError(w, http.StatusBadRequest, "download priority must be one of -100, -50, 0, 50, 100, or 900")
			return
		}
		req.DownloadPriority = &priority
	}
	if body.Monitor != nil {
		req.Monitor = string(*body.Monitor)
	}
	if body.SearchNow != nil && *body.SearchNow && req.RootFolderID == 0 {
		writeError(w, http.StatusBadRequest,
			"choose a library root folder before searching for a download")
		return
	}
	item, err := s.deps.Library.Add(r.Context(), req)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	// Search on add (Sonarr semantics): fire-and-forget; failures land in
	// the log and the item stays wanted for the RSS/backlog loops.
	if body.SearchNow != nil && *body.SearchNow && item.Monitored && s.deps.Acquisition != nil {
		s.searchInBackground(item.ID, "add")
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

// GetLibraryPlacementSuggestion implements GET /library/{id}/placement-suggestion.
func (s *Server) GetLibraryPlacementSuggestion(w http.ResponseWriter, r *http.Request, id int64) {
	suggestion, err := s.deps.Library.SuggestPlacement(r.Context(), id)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.PlacementSuggestion{
		RootFolderId: suggestion.RootFolderID,
		Path:         suggestion.Path,
	})
}

// UpdateLibraryItem implements PATCH /library/{id}: per-item edit of
// monitoring, quality profile, and location.
func (s *Server) UpdateLibraryItem(w http.ResponseWriter, r *http.Request, id int64) {
	var body apigen.UpdateLibraryItemJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	before, beforeErr := s.deps.Library.Get(r.Context(), id)
	if body.DownloadPriority != nil && !downloadpriority.Valid(*body.DownloadPriority) {
		writeError(w, http.StatusBadRequest, "download priority must be one of -100, -50, 0, 50, 100, or 900")
		return
	}
	if body.DownloadPriority != nil && body.InheritDownloadPriority != nil && *body.InheritDownloadPriority {
		writeError(w, http.StatusBadRequest, "choose a download priority or inherit from the profile, not both")
		return
	}
	setDownloadPriority := body.DownloadPriority != nil ||
		(body.InheritDownloadPriority != nil && *body.InheritDownloadPriority)
	item, err := s.deps.Library.UpdateItem(r.Context(), id, library.UpdateRequest{
		Monitored:           body.Monitored,
		QualityProfileID:    body.QualityProfileId,
		RootFolderID:        body.RootFolderId,
		Path:                body.Path,
		SetDownloadPriority: setDownloadPriority,
		DownloadPriority:    body.DownloadPriority,
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
	// Changing the profile is a request, not a note. Before this, the item
	// joined the wanted list and then sat there until the backlog loop came
	// round — up to twelve hours of "nothing seems to happen" after asking
	// for 4K. Search on add already treats an intentional act as a reason to
	// go looking; a profile change is the same kind of act.
	profileChanged := beforeErr == nil && body.QualityProfileId != nil &&
		*body.QualityProfileId != before.QualityProfileID
	if profileChanged && item.Monitored && s.deps.Acquisition != nil {
		s.searchInBackground(item.ID, "profile change")
	}
	// Older search-added rows could exist without a destination. Once this
	// edit supplies one, the thing that blocked their imports is gone, so put
	// them back on the importer without requiring a second trip to Activity.
	if beforeErr == nil && before.Path == "" && item.Path != "" && s.deps.Acquisition != nil {
		if queued, retryErr := s.deps.Acquisition.RetryFolderlessImports(r.Context(), item.ID); retryErr != nil {
			s.deps.Log.Warn("library: could not retry folderless imports",
				"item", item.ID, "queued", queued, "err", retryErr)
		}
	}
	writeJSON(w, http.StatusOK, detailDTO(item))
}

// AddMediaCopy implements POST /library/{id}/copies: an additional quality
// target with its own profile and automation lifecycle.
func (s *Server) AddMediaCopy(w http.ResponseWriter, r *http.Request, id int64) {
	var body apigen.AddMediaCopyJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req := library.CopyRequest{QualityProfileID: body.QualityProfileId, Monitored: true}
	if body.RootFolderId != nil {
		req.RootFolderID = *body.RootFolderId
	}
	if body.Name != nil {
		req.Name = *body.Name
	}
	if body.Monitored != nil {
		req.Monitored = *body.Monitored
	}
	item, err := s.deps.Library.AddCopy(r.Context(), id, req)
	if err != nil {
		if errors.Is(err, library.ErrUnsupportedKind) ||
			strings.Contains(err.Error(), "collide") ||
			strings.Contains(err.Error(), "profile") ||
			strings.Contains(err.Error(), "root folder") ||
			strings.Contains(err.Error(), "already uses") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.libraryErr(w, err)
		return
	}
	if s.deps.Acquisition != nil {
		s.deps.Acquisition.InvalidateWanted()
	}
	writeJSON(w, http.StatusOK, detailDTO(item))
}

// UpdateMediaCopy implements PATCH /library/{id}/copies/{copyId}.
func (s *Server) UpdateMediaCopy(w http.ResponseWriter, r *http.Request, id, copyID int64) {
	var body apigen.UpdateMediaCopyJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	item, err := s.deps.Library.UpdateCopy(r.Context(), id, copyID, body.Name, body.QualityProfileId, body.Monitored)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	if s.deps.Acquisition != nil {
		s.deps.Acquisition.InvalidateWanted()
	}
	writeJSON(w, http.StatusOK, detailDTO(item))
}

// DeleteMediaCopy implements DELETE /library/{id}/copies/{copyId}.
func (s *Server) DeleteMediaCopy(w http.ResponseWriter, r *http.Request, id, copyID int64) {
	item, err := s.deps.Library.DeleteCopy(r.Context(), id, copyID)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
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
func (s *Server) GetScanReport(w http.ResponseWriter, r *http.Request, params apigen.GetScanReportParams) {
	report, ok, err := s.deps.Library.LastScanReport(r.Context())
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no scan has run yet")
		return
	}
	// A library with several hundred unmatched folders should not ship the
	// whole list every time the settings page loads. The count is what that
	// page needs; the list itself is paginated at /library/review.
	limit := 25
	if params.Limit != nil {
		limit = *params.Limit
	}
	if limit < 0 {
		limit = 0
	}
	if limit > 200 {
		limit = 200
	}
	// The stored report already carries the true count; only fall back to
	// the list length for reports written before it was persisted.
	if report.UnmatchedTotal == 0 {
		report.UnmatchedTotal = len(report.UnmatchedDirs)
	}
	if len(report.UnmatchedDirs) > limit {
		report.UnmatchedDirs = report.UnmatchedDirs[:limit]
	}
	// The service report is already JSON-shaped per the spec.
	writeJSON(w, http.StatusOK, report)
}

// GetReviewQueue implements GET /library/review.
func (s *Server) GetReviewQueue(w http.ResponseWriter, r *http.Request, params apigen.GetReviewQueueParams) {
	q := ""
	if params.Q != nil {
		q = *params.Q
	}
	// -1 rather than 0 for "unspecified": 0 is a meaningful value here
	// (it means "all"), so it cannot double as the absent marker.
	limit, offset := -1, 0
	if params.Limit != nil {
		limit = *params.Limit
	}
	if params.Offset != nil {
		offset = *params.Offset
	}
	kind := domain.MediaKind("")
	if params.Kind != nil {
		kind = domain.MediaKind(*params.Kind)
	}
	page := s.deps.Library.ReviewQueue(r.Context(), kind, q, limit, offset)
	writeJSON(w, http.StatusOK, apigen.ReviewPage{
		Items:  proposalsDTO(page.Items),
		Total:  page.Total,
		Offset: page.Offset,
		Limit:  page.Limit,
		Counts: apigen.ReviewCounts{
			Total: page.Counts.Total, Ambiguous: page.Counts.Ambiguous, None: page.Counts.None,
			Movie: page.Counts.Movie, Series: page.Counts.Series,
			Book: page.Counts.Book, Unknown: page.Counts.Unknown,
		},
	})
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
	inLibTvdb := map[int64]bool{}
	inLibOlid := map[string]bool{}
	if items, err := s.deps.Library.List(r.Context(), domain.MediaKind(params.Kind)); err == nil {
		for _, it := range items {
			inLib[it.IDs.TMDB] = true
			if it.IDs.TVDB != 0 {
				inLibTvdb[it.IDs.TVDB] = true
			}
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
		sr.Source = optStr(res.Source)
		if res.TVDBID != 0 {
			id := res.TVDBID
			sr.TvdbId = &id
		}
		// "Already in the library" has to be answered against whichever id
		// this result carries, or a chain result always reads as new and the
		// Add button offers a duplicate.
		switch {
		case res.OLID != "":
			sr.InLibrary = inLibOlid[res.OLID]
		case res.TMDBID != 0:
			sr.InLibrary = inLib[res.TMDBID]
		default:
			sr.InLibrary = inLibTvdb[res.TVDBID]
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
		auto := rf.AutoAdopt
		out = append(out, apigen.RootFolder{
			Id: rf.ID, Path: rf.Path, Kind: apigen.RootKind(rf.Kind),
			FreeBytes: rf.FreeBytes, Accessible: rf.Accessible,
			AutoAdopt: &auto,
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
	var kind domain.RootKind
	if body.Kind != nil {
		kind = domain.RootKind(*body.Kind)
	}
	rf, err := s.deps.Library.AddRootFolder(r.Context(), body.Path, kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, apigen.RootFolder{
		Id: rf.ID, Path: rf.Path, Kind: apigen.RootKind(rf.Kind),
		FreeBytes: 0, Accessible: true,
	})
}

// UpdateRootFolder implements PATCH /rootfolders/{id}: retyping a root.
// Items already in the root keep their own kinds; this only routes what
// happens next (ADR 0009 §1).
func (s *Server) UpdateRootFolder(w http.ResponseWriter, r *http.Request, id int64) {
	var body apigen.UpdateRootFolderJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	rf, err := s.deps.Library.SetRootFolderKind(r.Context(), id, domain.RootKind(body.Kind))
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.RootFolder{
		Id: rf.ID, Path: rf.Path, Kind: apigen.RootKind(rf.Kind),
		FreeBytes: 0, Accessible: true,
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
	traktID := s.readSetting(r.Context(), TraktClientIDSetting)
	traktConfigured, traktHint := traktID != "", keyHint(traktID)
	out.TraktClientIdConfigured = &traktConfigured
	out.TraktClientIdHint = &traktHint
	if patterns := s.readSetting(r.Context(), library.SkipPatternsSetting); patterns != "" {
		out.ScanSkipPatterns = &patterns
	}
	if apiKey := s.readSetting(r.Context(), APIKeySetting); apiKey != "" {
		out.ApiKey = &apiKey
	}
	authOn := s.authRequired(r.Context())
	out.AuthRequired = &authOn
	if s.deps.Acquisition != nil {
		// Always the resolved window, never a blank that reads as "off":
		// this one decides what gets deleted.
		days := s.deps.Acquisition.RetentionDays(r.Context())
		out.ActivityRetentionDays = &days
	}
	// Always the resolved answer, including built-in fallbacks — a settings
	// screen showing nothing for "default profile" is how you end up believing
	// there isn't one.
	if s.deps.Store != nil {
		d := s.deps.Store.DefaultProfiles(r.Context())
		out.DefaultProfiles = &apigen.DefaultProfiles{
			Movie:     d[domain.KindMovie],
			Series:    d[domain.KindSeries],
			Book:      d[domain.KindBook],
			Audiobook: s.deps.Store.DefaultBookProfileID(r.Context(), quality.BookTypeAudiobook),
		}
	}
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
	if body.TraktClientId != nil {
		if err := s.deps.Settings.SetMeta(r.Context(), TraktClientIDSetting, strings.TrimSpace(*body.TraktClientId)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.ActivityRetentionDays != nil && s.deps.Acquisition != nil {
		if err := s.deps.Acquisition.SetRetentionDays(r.Context(), *body.ActivityRetentionDays); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.ScanSkipPatterns != nil {
		if err := s.deps.Settings.SetMeta(r.Context(), library.SkipPatternsSetting, *body.ScanSkipPatterns); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if body.DefaultProfiles != nil {
		if err := s.applyDefaultProfiles(r.Context(), *body.DefaultProfiles); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.applyAuthSettings(r.Context(), body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyDefaultProfiles saves the per-kind defaults, refusing a profile that
// does not exist or is on the wrong format axis. Partial by design: sending
// only {"movie": 3} leaves series and books alone.
func (s *Server) applyDefaultProfiles(ctx context.Context, in apigen.DefaultProfilesUpdate) error {
	if s.deps.Store == nil {
		return errors.New("profile storage unavailable")
	}
	for kind, id := range map[domain.MediaKind]*int64{
		domain.KindMovie:  in.Movie,
		domain.KindSeries: in.Series,
		domain.KindBook:   in.Book,
	} {
		if id == nil {
			continue
		}
		if err := s.deps.Store.SetDefaultProfile(ctx, kind, *id); err != nil {
			return err
		}
	}
	if in.Audiobook != nil {
		if err := s.deps.Store.SetDefaultBookProfile(ctx, quality.BookTypeAudiobook, *in.Audiobook); err != nil {
			return err
		}
	}
	return nil
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

// ---- adoption dismissals (ADR 0009 §4) ----

// ListIgnoredDirs implements GET /library/scan/ignored.
func (s *Server) ListIgnoredDirs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deps.Library.ListIgnoredDirs(r.Context())
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	out := make([]apigen.IgnoredPath, 0, len(rows))
	for _, ip := range rows {
		out = append(out, apigen.IgnoredPath{
			Path: ip.Path, Reason: ip.Reason, IgnoredAt: ip.IgnoredAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// IgnoreDir implements POST /library/scan/ignored.
func (s *Server) IgnoreDir(w http.ResponseWriter, r *http.Request) {
	var body apigen.IgnoreDirJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	reason := ""
	if body.Reason != nil {
		reason = *body.Reason
	}
	if err := s.deps.Library.IgnoreDir(r.Context(), body.Path, reason); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// UnignoreDir implements DELETE /library/scan/ignored?path=...
func (s *Server) UnignoreDir(w http.ResponseWriter, r *http.Request, params apigen.UnignoreDirParams) {
	if err := s.deps.Library.UnignoreDir(r.Context(), params.Path); err != nil {
		s.libraryErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// BrowseFilesystem implements GET /filesystem (ADR 0009 §2a).
//
// This is a directory-enumeration primitive, so it lives behind the same
// authentication as every other settings route and returns directories
// only — never file contents, never sizes, and never a distinction between
// "missing" and "forbidden".
func (s *Server) BrowseFilesystem(w http.ResponseWriter, r *http.Request, params apigen.BrowseFilesystemParams) {
	path := ""
	if params.Path != nil {
		path = *params.Path
	}
	res, err := s.deps.Library.Suggest(r.Context(), path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dirs := make([]apigen.DirEntry, 0, len(res.Dirs))
	for _, d := range res.Dirs {
		dirs = append(dirs, apigen.DirEntry{Name: d.Name, Path: d.Path, Registered: d.Registered})
	}
	writeJSON(w, http.StatusOK, apigen.BrowseResult{
		Path: res.Path, Parent: res.Parent, Dirs: dirs,
	})
}

// AddManualEntry implements POST /library/manual (ADR 0012).
func (s *Server) AddManualEntry(w http.ResponseWriter, r *http.Request) {
	var body apigen.AddManualEntryJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req := library.ManualRequest{
		Kind:  domain.MediaKind(body.Kind),
		Title: body.Title,
		Path:  body.Path,
	}
	if body.Year != nil {
		req.Year = *body.Year
	}
	if body.Author != nil {
		req.Author = *body.Author
	}
	if body.Overview != nil {
		req.Overview = *body.Overview
	}
	item, err := s.deps.Library.AddManual(r.Context(), req)
	if err != nil {
		if errors.Is(err, library.ErrFolderConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.libraryErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, detailDTO(item))
}

// RescanManualEntry implements POST /library/{id}/rescan (ADR 0012).
func (s *Server) RescanManualEntry(w http.ResponseWriter, r *http.Request, id int64) {
	item, err := s.deps.Library.RescanManual(r.Context(), id)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detailDTO(item))
}

// ---- adoption (ADR 0010) ----

func proposalDTO(p library.Proposal) apigen.Proposal {
	out := apigen.Proposal{
		RootFolderId: p.RootFolderID,
		Path:         p.Path,
		Name:         p.Name,
		ParsedTitle:  p.ParsedTitle,
		ParsedYear:   p.ParsedYear,
		Confidence:   apigen.ProposalConfidence(p.Confidence),
		Candidates:   make([]apigen.AdoptionCandidate, 0, len(p.Candidates)),
	}
	if p.Kind != "" {
		k := apigen.MediaKind(p.Kind)
		out.Kind = &k
	}
	out.HeldBy = optStr(p.HeldBy)
	if len(p.SharedWith) > 0 {
		shared := p.SharedWith
		out.SharedWith = &shared
	}
	for _, c := range p.Candidates {
		cand := apigen.AdoptionCandidate{
			Kind: apigen.MediaKind(c.Kind), TmdbId: c.TMDBID,
			Title: c.Title, Year: c.Year,
		}
		if c.TVDBID != 0 {
			id := c.TVDBID
			cand.TvdbId = &id
		}
		cand.Source = optStr(c.Source)
		cand.Overview = optStr(c.Overview)
		cand.PosterPath = optStr(c.PosterPath)
		cand.Olid = optStr(c.OLID)
		cand.Author = optStr(c.Author)
		if len(c.AltTitles) > 0 {
			alts := c.AltTitles
			cand.AltTitles = &alts
		}
		out.Candidates = append(out.Candidates, cand)
	}
	return out
}

func proposalsDTO(ps []library.Proposal) []apigen.Proposal {
	out := make([]apigen.Proposal, 0, len(ps))
	for _, p := range ps {
		out = append(out, proposalDTO(p))
	}
	return out
}

// RunAdoption implements POST /library/adopt.
func (s *Server) RunAdoption(w http.ResponseWriter, r *http.Request) {
	res, err := s.deps.Library.RunAdoption(r.Context())
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	failures := res.Failures
	if failures == nil {
		failures = []string{}
	}
	writeJSON(w, http.StatusOK, apigen.AdoptResult{
		Adopted:  proposalsDTO(res.Adopted),
		Review:   proposalsDTO(res.Review),
		Failures: failures,
	})
}

// ConfirmRootAdopted implements POST /library/adopt/confirm.
func (s *Server) ConfirmRootAdopted(w http.ResponseWriter, r *http.Request) {
	var body apigen.ConfirmRootAdoptedJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	on := true
	if body.AutoAdopt != nil {
		on = *body.AutoAdopt
	}
	if err := s.deps.Library.SetRootAutoAdopt(r.Context(), body.RootFolderId, on); err != nil {
		s.libraryErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdoptOne implements POST /library/adopt/one: accept one proposed match
// without leaving the review window.
func (s *Server) AdoptOne(w http.ResponseWriter, r *http.Request) {
	var body apigen.AdoptOneJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	pick := ports.SearchResult{}
	if body.Kind != nil {
		pick.Kind = domain.MediaKind(*body.Kind)
	}
	if body.TmdbId != nil {
		pick.TMDBID = *body.TmdbId
	}
	if body.TvdbId != nil {
		pick.TVDBID = *body.TvdbId
	}
	if body.Olid != nil {
		pick.OLID = *body.Olid
	}
	if body.Title != nil {
		pick.Title = *body.Title
	}
	if body.Year != nil {
		pick.Year = *body.Year
	}
	force := body.Force != nil && *body.Force
	if err := s.deps.Library.AdoptOne(r.Context(), body.Path, pick, force); err != nil {
		// A folder conflict is resolvable by the user, so it gets its own
		// status: the UI offers "move it here" rather than treating this as
		// a flat rejection.
		if errors.Is(err, library.ErrFolderConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdoptExact implements POST /library/adopt/exact.
func (s *Server) AdoptExact(w http.ResponseWriter, r *http.Request) {
	res, err := s.deps.Library.AdoptExact(r.Context())
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	failures := res.Failures
	if failures == nil {
		failures = []string{}
	}
	writeJSON(w, http.StatusOK, apigen.AdoptResult{
		Adopted:  proposalsDTO(res.Adopted),
		Review:   proposalsDTO(res.Review),
		Failures: failures,
	})
}

// qualityText renders a file's recorded quality, or "" when the file exists
// and nothing could be determined about it. The empty string is meaningful:
// it is the "on disk, unverified" state, not an absent file.
func qualityText(f domain.MediaFile) string {
	if !f.QualityKnown {
		return ""
	}
	return f.Quality.Display()
}

// searchInBackground fires an auto-search without blocking the request that
// asked for it. Fire-and-forget on purpose: a search fans out to every
// indexer, which is far longer than any sane HTTP timeout, and a failure is
// not a reason to fail the edit — the item stays wanted and the RSS/backlog
// loops pick it up regardless.
func (s *Server) searchInBackground(itemID int64, why string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		out, err := s.deps.Acquisition.AutoSearchItem(ctx, itemID)
		if err != nil {
			s.deps.Log.Warn("background search failed", "item", itemID, "trigger", why, "err", err)
			return
		}
		s.deps.Log.Info("background search done", "item", itemID, "trigger", why,
			"targets", len(out.Targets), "grabbed", out.Grabbed)
	}()
}

// RemoveLibraryFile implements DELETE /library/{id}/files/{fileId}: drop one
// file, and optionally condemn the release that produced it.
//
// The three flags arrive separately because they are three separate decisions
// (see the endpoint description). The handler's only job is to not conflate
// them, and to report back what actually happened rather than assuming every
// part worked — a blocklist that could not find a source release is a thing
// the user needs told, not a silent no-op.
func (s *Server) RemoveLibraryFile(w http.ResponseWriter, r *http.Request, id int64, fileID int64,
	params apigen.RemoveLibraryFileParams) {
	req := acquisition.RemoveFileRequest{
		FromDisk:  params.FromDisk != nil && *params.FromDisk,
		Blocklist: params.Blocklist != nil && *params.Blocklist,
		Search:    params.Search != nil && *params.Search,
	}
	if req.Blocklist {
		req.Reason = "marked bad by user"
	}
	res, err := s.deps.Acquisition.RemoveFile(r.Context(), id, fileID, req)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.RemoveFileResult{
		Path:            res.Path,
		DeletedFromDisk: res.DeletedFromDisk,
		Blocklisted:     optStr(res.Blocklisted),
		Searched:        res.Searched,
		Note:            optStr(res.Note),
	})
}

// ReprobeLibraryItem implements POST /library/{id}/probe.
func (s *Server) ReprobeLibraryItem(w http.ResponseWriter, r *http.Request, id int64) {
	if _, err := s.deps.Library.Get(r.Context(), id); err != nil {
		s.libraryErr(w, err)
		return
	}
	n, err := s.deps.Library.ReprobeItem(r.Context(), id)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"files": n})
}
