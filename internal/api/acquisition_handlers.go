package api

import (
	"encoding/json"
	"errors"
	"net/http"

	apigen "github.com/monarr-media/monarr/internal/api/gen"
	"github.com/monarr-media/monarr/internal/app/acquisition"
	"github.com/monarr-media/monarr/internal/ports"
)

func (s *Server) acqErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, acquisition.ErrNoIndexers):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, acquisition.ErrNoClient):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, acquisition.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		s.deps.Log.Error("acquisition api error", "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// ---- profiles ----

// ListProfiles implements GET /profiles.
func (s *Server) ListProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.deps.Store.ListProfiles(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.QualityProfile, 0, len(profiles))
	for _, p := range profiles {
		qp := apigen.QualityProfile{
			Id: p.ID, Name: p.Name, Cutoff: p.Cutoff.Display(),
			UpgradesAllowed: p.UpgradesAllowed, Qualities: []string{},
		}
		for _, q := range p.Allowed {
			qp.Qualities = append(qp.Qualities, q.Display())
		}
		out = append(out, qp)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- indexers ----

func indexerInputToConfig(in apigen.IndexerInput) ports.IndexerConfig {
	cfg := ports.IndexerConfig{
		Name: in.Name, URL: in.Url, Protocol: string(in.Protocol), Enabled: true,
	}
	if in.ApiKey != nil {
		cfg.APIKey = *in.ApiKey
	}
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if in.Categories != nil {
		cfg.Categories = *in.Categories
	}
	return cfg
}

func indexerDTO(c ports.IndexerConfig) apigen.Indexer {
	key := c.APIKey
	cats := c.Categories
	if cats == nil {
		cats = []int{}
	}
	enabled := c.Enabled
	return apigen.Indexer{
		Id: c.ID, Name: c.Name, Url: c.URL, ApiKey: &key,
		Protocol: apigen.IndexerProtocol(c.Protocol), Categories: &cats, Enabled: &enabled,
	}
}

// ListIndexers implements GET /indexers.
func (s *Server) ListIndexers(w http.ResponseWriter, r *http.Request) {
	list, err := s.deps.Store.ListIndexers(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.Indexer, 0, len(list))
	for _, c := range list {
		out = append(out, indexerDTO(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// AddIndexer implements POST /indexers.
func (s *Server) AddIndexer(w http.ResponseWriter, r *http.Request) {
	var in apigen.IndexerInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.Url == "" {
		writeError(w, http.StatusBadRequest, "name, url, and protocol are required")
		return
	}
	cfg := indexerInputToConfig(in)
	id, err := s.deps.Store.AddIndexer(r.Context(), cfg)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	cfg.ID = id
	writeJSON(w, http.StatusCreated, indexerDTO(cfg))
}

// TestIndexer implements POST /indexers/test.
func (s *Server) TestIndexer(w http.ResponseWriter, r *http.Request) {
	var in apigen.IndexerInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := s.deps.IndexerFactory(indexerInputToConfig(in)).Test(r.Context()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// DeleteIndexer implements DELETE /indexers/{id}.
func (s *Server) DeleteIndexer(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.deps.Store.DeleteIndexer(r.Context(), id); err != nil {
		s.acqErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- download clients ----

func clientInputToConfig(in apigen.DownloadClientInput) ports.ClientConfig {
	cfg := ports.ClientConfig{
		Type: string(in.Type), Name: in.Name, URL: in.Url,
		Category: "monarr", Enabled: true,
	}
	if in.Username != nil {
		cfg.Username = *in.Username
	}
	if in.Password != nil {
		cfg.Password = *in.Password
	}
	if in.Category != nil && *in.Category != "" {
		cfg.Category = *in.Category
	}
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	return cfg
}

func clientDTO(c ports.ClientConfig) apigen.DownloadClientConfig {
	user, cat, enabled := c.Username, c.Category, c.Enabled
	masked := ""
	if c.Password != "" {
		masked = "••••"
	}
	return apigen.DownloadClientConfig{
		Id: c.ID, Type: apigen.DownloadClientConfigType(c.Type), Name: c.Name, Url: c.URL,
		Username: &user, Password: &masked, Category: &cat, Enabled: &enabled,
	}
}

// ListDownloadClients implements GET /downloadclients.
func (s *Server) ListDownloadClients(w http.ResponseWriter, r *http.Request) {
	list, err := s.deps.Store.ListDownloadClients(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.DownloadClientConfig, 0, len(list))
	for _, c := range list {
		out = append(out, clientDTO(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// AddDownloadClient implements POST /downloadclients.
func (s *Server) AddDownloadClient(w http.ResponseWriter, r *http.Request) {
	var in apigen.DownloadClientInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.Url == "" {
		writeError(w, http.StatusBadRequest, "type, name, and url are required")
		return
	}
	cfg := clientInputToConfig(in)
	id, err := s.deps.Store.AddDownloadClient(r.Context(), cfg)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	cfg.ID = id
	writeJSON(w, http.StatusCreated, clientDTO(cfg))
}

// TestDownloadClient implements POST /downloadclients/test.
func (s *Server) TestDownloadClient(w http.ResponseWriter, r *http.Request) {
	var in apigen.DownloadClientInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := s.deps.ClientFactory(clientInputToConfig(in)).Test(r.Context()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// DeleteDownloadClient implements DELETE /downloadclients/{id}.
func (s *Server) DeleteDownloadClient(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.deps.Store.DeleteDownloadClient(r.Context(), id); err != nil {
		s.acqErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- search / grab / queue ----

// SearchReleases implements GET /library/{id}/releases.
func (s *Server) SearchReleases(w http.ResponseWriter, r *http.Request, id int64, params apigen.SearchReleasesParams) {
	season, episode := 0, 0
	if params.Season != nil {
		season = *params.Season
	}
	if params.Episode != nil {
		episode = *params.Episode
	}
	cands, err := s.deps.Acquisition.Search(r.Context(), id, season, episode)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.ReleaseCandidate, 0, len(cands))
	for _, c := range cands {
		rc := apigen.ReleaseCandidate{
			Title: c.Release.Title, DownloadUrl: c.Release.DownloadURL,
			Indexer: c.Release.Indexer, Protocol: c.Release.Protocol,
			Size: c.Release.Size, Seeders: c.Release.Seeders, Age: c.Age,
			Quality: c.QualityStr, Accepted: c.Accepted, IsUpgrade: c.IsUpgrade,
			Rejections: []apigen.Rejection{},
		}
		if c.Release.InfoURL != "" {
			u := c.Release.InfoURL
			rc.InfoUrl = &u
		}
		for _, rej := range c.Rejections {
			rc.Rejections = append(rc.Rejections, apigen.Rejection{Code: rej.Code, Reason: rej.Reason})
		}
		out = append(out, rc)
	}
	writeJSON(w, http.StatusOK, out)
}

// GrabRelease implements POST /grab.
func (s *Server) GrabRelease(w http.ResponseWriter, r *http.Request) {
	var in apigen.GrabRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Title == "" || in.DownloadUrl == "" {
		writeError(w, http.StatusBadRequest, "mediaItemId, title, downloadUrl, protocol required")
		return
	}
	req := acquisition.GrabRequest{
		MediaItemID: in.MediaItemId, Season: -1,
		Title: in.Title, DownloadURL: in.DownloadUrl, Protocol: in.Protocol,
	}
	if in.Season != nil {
		req.Season = *in.Season
	}
	if in.Episode != nil {
		req.Episode = *in.Episode
	}
	if in.Indexer != nil {
		req.Indexer = *in.Indexer
	}
	if in.Size != nil {
		req.Size = *in.Size
	}
	id, err := s.deps.Acquisition.Grab(r.Context(), req)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// ListQueue implements GET /queue.
func (s *Server) ListQueue(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deps.Acquisition.Queue(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.QueueItem, 0, len(rows))
	for _, d := range rows {
		item := apigen.QueueItem{
			Id: d.ID, MediaItemId: d.MediaItemID, Title: d.ReleaseTitle,
			State: d.State, Progress: float32(d.Progress), Protocol: d.Protocol,
			Quality: d.Quality.Display(), AddedAt: d.AddedAt,
		}
		if d.Error != "" {
			e := d.Error
			item.Error = &e
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// RemoveQueueItem implements DELETE /queue/{id}.
func (s *Server) RemoveQueueItem(w http.ResponseWriter, r *http.Request, id int64, params apigen.RemoveQueueItemParams) {
	fromClient := params.FromClient != nil && *params.FromClient
	if err := s.deps.Acquisition.RemoveDownload(r.Context(), id, fromClient); err != nil {
		s.acqErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListWanted implements GET /wanted.
func (s *Server) ListWanted(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deps.Acquisition.WantedList(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.WantedItem, 0, len(rows))
	for _, ws := range rows {
		out = append(out, apigen.WantedItem{
			WantableId: ws.WantableID, MediaItemId: ws.MediaItemID,
			Title: ws.Title, Detail: ws.Detail, Missing: ws.Missing, Current: ws.Current,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ListBlocklist implements GET /blocklist.
func (s *Server) ListBlocklist(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deps.Store.ListBlocklist(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.BlocklistEntry, 0, len(rows))
	for _, b := range rows {
		out = append(out, apigen.BlocklistEntry{
			Id: b.ID, MediaItemId: b.MediaItemID, ReleaseTitle: b.ReleaseTitle,
			Indexer: b.Indexer, Reason: b.Reason, CreatedAt: b.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// RemoveBlocklistEntry implements DELETE /blocklist/{id}.
func (s *Server) RemoveBlocklistEntry(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.deps.Store.DeleteBlocklist(r.Context(), id); err != nil {
		s.acqErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetCalendar implements GET /calendar.
func (s *Server) GetCalendar(w http.ResponseWriter, r *http.Request, params apigen.GetCalendarParams) {
	entries, err := s.deps.Store.Calendar(r.Context(), params.Start, params.End)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.CalendarEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, apigen.CalendarEntry{
			Date: e.Date, Kind: e.Kind, MediaItemId: e.MediaItemID,
			Title: e.Title, Detail: e.Detail, HasFile: e.HasFile,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
