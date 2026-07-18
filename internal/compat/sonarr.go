package compat

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/monarr-media/monarr/internal/app/library"
	"github.com/monarr-media/monarr/internal/domain"
)

// mountSonarr adds the series-shaped surface Jellyseerr and Bazarr call.
func (p *Personality) mountSonarr(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v3/series", p.listSeries)
	mux.HandleFunc("GET /api/v3/series/{id}", p.getSeries)
	mux.HandleFunc("POST /api/v3/series", p.addSeries)
	mux.HandleFunc("GET /api/v3/series/lookup", p.lookupSeries)
	mux.HandleFunc("GET /api/v3/episode", p.listEpisodes)
	mux.HandleFunc("GET /api/v3/episodefile", p.listEpisodeFiles)
}

// seriesDTO renders one series in Sonarr v3 shape (the subset consumers read).
func (p *Personality) seriesDTO(item domain.MediaItem, roots []library.RootFolderInfo) map[string]any {
	epTotal, epHave := 0, 0
	seasons := make([]map[string]any, 0, len(item.Seasons))
	for _, s := range item.Seasons {
		have := 0
		for _, e := range s.Episodes {
			if e.HasFile {
				have++
			}
		}
		epTotal += len(s.Episodes)
		epHave += have
		seasons = append(seasons, map[string]any{
			"seasonNumber": s.Number,
			"monitored":    s.Monitored,
			"statistics": map[string]any{
				"episodeCount": len(s.Episodes), "episodeFileCount": have,
				"totalEpisodeCount": len(s.Episodes),
				"percentOfEpisodes": percent(have, len(s.Episodes)),
			},
		})
	}
	status := "continuing"
	if item.Ended {
		status = "ended"
	}
	return map[string]any{
		"id": item.ID, "title": item.Title, "sortTitle": item.SortTitle,
		"status": status, "ended": item.Ended, "overview": item.Overview,
		"seasons": seasons, "year": item.Year, "path": item.Path,
		"qualityProfileId": item.QualityProfileID, "languageProfileId": 1,
		"seasonFolder": true, "monitored": item.Monitored,
		"runtime": item.Runtime, "tvdbId": item.IDs.TVDB,
		"seriesType": "standard", "imdbId": item.IDs.IMDB,
		"titleSlug": slug(item.Title), "rootFolderPath": rootPathOf(item, roots),
		"genres": orEmpty(item.Genres), "tags": []any{},
		"added":      item.AddedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"firstAired": item.ReleaseDate,
		"images":     images(item),
		"statistics": map[string]any{
			"seasonCount": len(seasons), "episodeFileCount": epHave,
			"episodeCount": epTotal, "totalEpisodeCount": epTotal,
			"sizeOnDisk": sizeOnDisk(item), "percentOfEpisodes": percent(epHave, epTotal),
		},
	}
}

func (p *Personality) listSeries(w http.ResponseWriter, r *http.Request) {
	items, err := p.deps.Library.List(r.Context(), domain.KindSeries)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	roots, _ := p.deps.Library.ListRootFolders(r.Context())
	out := make([]map[string]any, 0, len(items))
	for _, sum := range items {
		item, err := p.deps.Library.Get(r.Context(), sum.ID)
		if err != nil {
			continue
		}
		out = append(out, p.seriesDTO(item, roots))
	}
	writeJSON(w, http.StatusOK, out)
}

func (p *Personality) getSeries(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	item, err := p.deps.Library.Get(r.Context(), id)
	if err != nil || item.Kind != domain.KindSeries {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "series not found"})
		return
	}
	roots, _ := p.deps.Library.ListRootFolders(r.Context())
	writeJSON(w, http.StatusOK, p.seriesDTO(item, roots))
}

// lookupSeries handles term= text or "tvdb:12345" (Jellyseerr's add flow).
func (p *Personality) lookupSeries(w http.ResponseWriter, r *http.Request) {
	term := strings.TrimSpace(r.URL.Query().Get("term"))
	roots, _ := p.deps.Library.ListRootFolders(r.Context())

	if tvdbStr, ok := strings.CutPrefix(strings.ToLower(term), "tvdb:"); ok {
		tvdbID, _ := strconv.ParseInt(tvdbStr, 10, 64)
		if p.deps.ResolveTVDB == nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		item, err := p.deps.ResolveTVDB(r.Context(), tvdbID)
		if err != nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		item.IDs.TVDB = tvdbID // /find confirms the mapping; keep the id
		writeJSON(w, http.StatusOK, []map[string]any{p.seriesDTO(item, roots)})
		return
	}

	results, err := p.deps.Library.Search(r.Context(), domain.KindSeries, term)
	if err != nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	out := make([]map[string]any, 0, len(results))
	for _, res := range results {
		out = append(out, map[string]any{
			"title": res.Title, "year": res.Year, "overview": res.Overview,
			"titleSlug": slug(res.Title), "seasons": []any{},
			"remotePoster": res.PosterPath, "tmdbId": res.TMDBID,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// addSeries accepts a Sonarr v3 add payload; the tvdbId is resolved to our
// TMDB-backed series.
func (p *Personality) addSeries(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TVDBID           int64  `json:"tvdbId"`
		Title            string `json:"title"`
		QualityProfileID int64  `json:"qualityProfileId"`
		RootFolderPath   string `json:"rootFolderPath"`
		Monitored        bool   `json:"monitored"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TVDBID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "tvdbId required"})
		return
	}
	if p.deps.ResolveTVDB == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "tvdb resolution unavailable"})
		return
	}
	resolved, err := p.deps.ResolveTVDB(r.Context(), body.TVDBID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}

	req := library.AddRequest{
		Kind: domain.KindSeries, TMDBID: resolved.IDs.TMDB,
		QualityProfileID: body.QualityProfileID, Monitored: body.Monitored,
		RootFolderID: p.rootIDByPath(r, body.RootFolderPath),
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
	writeJSON(w, http.StatusCreated, p.seriesDTO(item, roots))
}

func (p *Personality) rootIDByPath(r *http.Request, path string) int64 {
	if path == "" {
		return 0
	}
	roots, err := p.deps.Library.ListRootFolders(r.Context())
	if err != nil {
		return 0
	}
	clean := strings.TrimRight(path, "/")
	for _, rf := range roots {
		if strings.TrimRight(rf.Path, "/") == clean {
			return rf.ID
		}
	}
	return 0
}

// listEpisodes serves ?seriesId= for Bazarr and Jellyseerr season views.
func (p *Personality) listEpisodes(w http.ResponseWriter, r *http.Request) {
	seriesID, _ := strconv.ParseInt(r.URL.Query().Get("seriesid"), 10, 64)
	if seriesID == 0 {
		seriesID, _ = strconv.ParseInt(r.URL.Query().Get("seriesId"), 10, 64)
	}
	item, err := p.deps.Library.Get(r.Context(), seriesID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "series not found"})
		return
	}
	fileFor := episodeFileIndex(item)
	out := []map[string]any{}
	for _, s := range item.Seasons {
		for _, e := range s.Episodes {
			out = append(out, map[string]any{
				"id": e.ID, "seriesId": item.ID, "seasonNumber": e.SeasonNumber,
				"episodeNumber": e.EpisodeNumber, "title": e.Title,
				"airDate": e.AirDate, "hasFile": e.HasFile, "monitored": e.Monitored,
				"episodeFileId":         fileFor[e.ID],
				"absoluteEpisodeNumber": e.AbsoluteNum,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// listEpisodeFiles serves ?seriesId= for Bazarr's file discovery.
func (p *Personality) listEpisodeFiles(w http.ResponseWriter, r *http.Request) {
	seriesID, _ := strconv.ParseInt(r.URL.Query().Get("seriesid"), 10, 64)
	if seriesID == 0 {
		seriesID, _ = strconv.ParseInt(r.URL.Query().Get("seriesId"), 10, 64)
	}
	item, err := p.deps.Library.Get(r.Context(), seriesID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "series not found"})
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
			"id": f.ID, "seriesId": item.ID, "relativePath": rel, "path": f.Path,
			"size": f.Size, "quality": map[string]any{"quality": map[string]any{"name": qname}},
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// episodeFileIndex maps episode id → covering file id (0 when none).
func episodeFileIndex(item domain.MediaItem) map[int64]int64 {
	out := map[int64]int64{}
	for _, f := range item.Files {
		for _, epID := range f.EpisodeIDs {
			out[epID] = f.ID
		}
	}
	return out
}

func percent(have, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(have) / float64(total) * 100
}

func sizeOnDisk(item domain.MediaItem) int64 {
	var total int64
	for _, f := range item.Files {
		total += f.Size
	}
	return total
}

func orEmpty(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

func images(item domain.MediaItem) []map[string]any {
	out := []map[string]any{}
	if item.PosterPath != "" {
		u := item.PosterPath
		if !strings.HasPrefix(u, "http") {
			u = "https://image.tmdb.org/t/p/w342" + u
		}
		out = append(out, map[string]any{"coverType": "poster", "remoteUrl": u, "url": u})
	}
	return out
}
