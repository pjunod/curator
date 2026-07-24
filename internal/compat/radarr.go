package compat

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/monarr-media/monarr/internal/app/library"
	"github.com/monarr-media/monarr/internal/domain"
)

// mountRadarr adds the movie-shaped surface Jellyseerr and Bazarr call.
func (p *Personality) mountRadarr(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v3/movie", p.listMovies)
	mux.HandleFunc("GET /api/v3/movie/{id}", p.getMovie)
	mux.HandleFunc("POST /api/v3/movie", p.addMovie)
	mux.HandleFunc("GET /api/v3/movie/lookup", p.lookupMovies)
	mux.HandleFunc("GET /api/v3/moviefile", p.listMovieFiles)
}

// movieDTO renders one movie in Radarr v3 shape (the subset consumers read).
func (p *Personality) movieDTO(item domain.MediaItem, roots []library.RootFolderInfo) map[string]any {
	hasFile := len(item.Files) > 0
	status := "released"
	if !strings.EqualFold(item.Status, "Released") && item.Status != "" {
		status = "announced"
	}
	return map[string]any{
		"id": item.ID, "title": item.Title, "sortTitle": item.SortTitle,
		"sizeOnDisk": sizeOnDisk(item), "status": status, "overview": item.Overview,
		"year": item.Year, "hasFile": hasFile, "path": item.Path,
		"qualityProfileId": item.QualityProfileID, "monitored": item.Monitored,
		"minimumAvailability": "released", "isAvailable": true,
		"runtime": item.Runtime, "cleanTitle": slug(item.Title),
		"imdbId": item.IDs.IMDB, "tmdbId": item.IDs.TMDB,
		"titleSlug":      strconv.FormatInt(item.IDs.TMDB, 10),
		"rootFolderPath": rootPathOf(item, roots),
		"genres":         orEmpty(item.Genres), "tags": []any{},
		"added":  item.AddedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"images": images(item),
	}
}

func (p *Personality) listMovies(w http.ResponseWriter, r *http.Request) {
	items, err := p.deps.Library.List(r.Context(), domain.KindMovie)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	roots, _ := p.deps.Library.ListRootFolders(r.Context())
	// tmdbId query filter (Jellyseerr existence checks).
	tmdbFilter, _ := strconv.ParseInt(r.URL.Query().Get("tmdbid"), 10, 64)
	if tmdbFilter == 0 {
		tmdbFilter, _ = strconv.ParseInt(r.URL.Query().Get("tmdbId"), 10, 64)
	}
	out := make([]map[string]any, 0, len(items))
	for _, sum := range items {
		if tmdbFilter != 0 && sum.IDs.TMDB != tmdbFilter {
			continue
		}
		item, err := p.deps.Library.Get(r.Context(), sum.ID)
		if err != nil {
			continue
		}
		out = append(out, p.movieDTO(item, roots))
	}
	writeJSON(w, http.StatusOK, out)
}

func (p *Personality) getMovie(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	item, err := p.deps.Library.Get(r.Context(), id)
	if err != nil || item.Kind != domain.KindMovie {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "movie not found"})
		return
	}
	roots, _ := p.deps.Library.ListRootFolders(r.Context())
	writeJSON(w, http.StatusOK, p.movieDTO(item, roots))
}

// lookupMovies handles term= text or "tmdb:603" (Jellyseerr's add flow).
func (p *Personality) lookupMovies(w http.ResponseWriter, r *http.Request) {
	term := strings.TrimSpace(r.URL.Query().Get("term"))
	results := []map[string]any{}
	if tmdbStr, ok := strings.CutPrefix(strings.ToLower(term), "tmdb:"); ok {
		tmdbID, _ := strconv.ParseInt(tmdbStr, 10, 64)
		// Hydrate directly through the library's metadata path: search by id
		// is not a thing, so surface a minimal lookup result.
		writeJSON(w, http.StatusOK, []map[string]any{{
			"tmdbId": tmdbID, "title": "", "titleSlug": tmdbStr, "year": 0,
		}})
		return
	}
	found, err := p.deps.Library.Search(r.Context(), domain.KindMovie, term)
	if err != nil {
		writeJSON(w, http.StatusOK, results)
		return
	}
	for _, res := range found {
		results = append(results, map[string]any{
			"tmdbId": res.TMDBID, "title": res.Title, "year": res.Year,
			"overview": res.Overview, "titleSlug": strconv.FormatInt(res.TMDBID, 10),
			"remotePoster": res.PosterPath,
		})
	}
	writeJSON(w, http.StatusOK, results)
}

// addMovie accepts a Radarr v3 add payload (tmdbId keyed — our native id).
func (p *Personality) addMovie(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TMDBID           int64  `json:"tmdbId"`
		Title            string `json:"title"`
		QualityProfileID int64  `json:"qualityProfileId"`
		RootFolderPath   string `json:"rootFolderPath"`
		Monitored        bool   `json:"monitored"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TMDBID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "tmdbId required"})
		return
	}
	rootID, err := p.rootIDByPath(r, body.RootFolderPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, []map[string]string{{"errorMessage": err.Error()}})
		return
	}
	req := library.AddRequest{
		Kind: domain.KindMovie, TMDBID: body.TMDBID,
		QualityProfileID: body.QualityProfileID, Monitored: body.Monitored,
		RootFolderID: rootID,
	}
	item, err := p.deps.Library.Add(r.Context(), req)
	if err != nil {
		status := http.StatusBadRequest
		if err == library.ErrAlreadyExists {
			status = http.StatusConflict
		}
		writeJSON(w, status, []map[string]string{{"errorMessage": err.Error()}})
		return
	}
	roots, _ := p.deps.Library.ListRootFolders(r.Context())
	writeJSON(w, http.StatusCreated, p.movieDTO(item, roots))
}

// listMovieFiles serves ?movieId= for Bazarr.
func (p *Personality) listMovieFiles(w http.ResponseWriter, r *http.Request) {
	movieID, _ := strconv.ParseInt(r.URL.Query().Get("movieid"), 10, 64)
	if movieID == 0 {
		movieID, _ = strconv.ParseInt(r.URL.Query().Get("movieId"), 10, 64)
	}
	item, err := p.deps.Library.Get(r.Context(), movieID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "movie not found"})
		return
	}
	quals, _ := p.deps.Store.FileQualities(r.Context(), item.ID)
	out := []map[string]any{}
	for _, f := range item.Files {
		qname := "Unknown"
		if q, ok := quals[f.ID]; ok {
			qname = q.Display()
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(f.Path, item.Path), "/")
		out = append(out, map[string]any{
			"id": f.ID, "movieId": item.ID, "relativePath": rel, "path": f.Path,
			"size": f.Size, "quality": map[string]any{"quality": map[string]any{"name": qname}},
		})
	}
	writeJSON(w, http.StatusOK, out)
}
