package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/monarr-media/monarr/internal/adapters/httpx"
	apigen "github.com/monarr-media/monarr/internal/api/gen"
	"github.com/monarr-media/monarr/internal/app/acquisition"
	"github.com/monarr-media/monarr/internal/domain/format"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/infra/sqlite"
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

// profileDTO renders a profile for monarr's own clients: the target model
// itself, plus the one server-rendered sentence that describes it.
//
// The sentence is here rather than in each client on purpose. The old model
// needed a paragraph of UI comment to explain why a profile called "Any"
// stopped at WEB-DL 1080p; a model that has to be explained differently by
// every surface is a model nobody agrees about.
func profileDTO(p quality.Profile, inUse int64) apigen.QualityProfile {
	qp := apigen.QualityProfile{
		Id: p.ID, Name: p.Name, Target: qualityDTO(p.Target),
		UpgradesAllowed: p.UpgradesAllowed, Sentence: p.Sentence(),
	}
	if p.Floor != nil {
		f := qualityDTO(*p.Floor)
		qp.Floor = &f
	}
	if inUse >= 0 {
		n := int(inUse)
		qp.InUse = &n
	}
	return qp
}

func qualityDTO(q quality.Quality) apigen.Quality {
	return apigen.Quality{
		Source: string(q.Source), Resolution: q.Resolution, Display: q.Display(),
	}
}

func profileFromInput(in apigen.ProfileInput) (quality.Profile, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return quality.Profile{}, fmt.Errorf("a profile needs a name")
	}
	target := quality.Quality{Source: quality.Source(strings.TrimSpace(in.Target.Source))}
	if in.Target.Resolution != nil {
		target.Resolution = *in.Target.Resolution
	}
	if target.Source == "" || target.Source == quality.SourceUnknown {
		return quality.Profile{}, fmt.Errorf("a profile needs a target to hunt toward")
	}
	p := quality.Profile{Name: name, Target: target, UpgradesAllowed: true}
	if in.UpgradesAllowed != nil {
		p.UpgradesAllowed = *in.UpgradesAllowed
	}
	if in.Floor != nil {
		floor := quality.Quality{Source: quality.Source(strings.TrimSpace(in.Floor.Source))}
		if in.Floor.Resolution != nil {
			floor.Resolution = *in.Floor.Resolution
		}
		if floor.Source == "" || floor.Source == quality.SourceUnknown {
			return quality.Profile{}, fmt.Errorf("a floor needs a quality; leave it out for no floor")
		}
		if quality.Rank(floor) > quality.Rank(target) {
			return quality.Profile{}, fmt.Errorf(
				"the floor (%s) is above the target (%s), so nothing would ever be grabbed",
				floor.Display(), target.Display())
		}
		p.Floor = &floor
	}
	return p, nil
}

// ListProfiles implements GET /profiles.
func (s *Server) ListProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := s.deps.Store.ListProfiles(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.QualityProfile, 0, len(profiles))
	for _, p := range profiles {
		refs, err := s.deps.Store.ProfileReferences(r.Context(), p.ID)
		if err != nil {
			refs = -1 // unknown rather than a failed list
		}
		out = append(out, profileDTO(p, refs))
	}
	writeJSON(w, http.StatusOK, out)
}

// CreateProfile implements POST /profiles.
func (s *Server) CreateProfile(w http.ResponseWriter, r *http.Request) {
	var in apigen.ProfileInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	p, err := profileFromInput(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := s.deps.Store.AddProfile(r.Context(), p)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	p.ID = id
	writeJSON(w, http.StatusCreated, profileDTO(p, 0))
}

// UpdateProfile implements PUT /profiles/{id}.
func (s *Server) UpdateProfile(w http.ResponseWriter, r *http.Request, id int64) {
	var in apigen.ProfileInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	p, err := profileFromInput(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	p.ID = id
	if err := s.deps.Store.UpdateProfile(r.Context(), p); err != nil {
		s.acqErr(w, err)
		return
	}
	refs, err := s.deps.Store.ProfileReferences(r.Context(), id)
	if err != nil {
		refs = -1
	}
	writeJSON(w, http.StatusOK, profileDTO(p, refs))
}

// DeleteProfile implements DELETE /profiles/{id}.
//
// Refused while anything still points at the profile. Silently orphaning a
// library item onto a profile id that no longer exists is the kind of damage
// nobody sees until an upgrade decision goes strange months later.
func (s *Server) DeleteProfile(w http.ResponseWriter, r *http.Request, id int64) {
	err := s.deps.Store.DeleteProfile(r.Context(), id)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, sqlite.ErrProfileInUse):
		writeError(w, http.StatusConflict, err.Error())
	default:
		s.acqErr(w, err)
	}
}

// ---- indexers ----

func indexerInputToConfig(in apigen.IndexerInput) ports.IndexerConfig {
	cfg := ports.IndexerConfig{
		Name: in.Name, URL: ports.NormalizeURL(in.Url), Protocol: string(in.Protocol), Enabled: true,
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
	if !in.Protocol.Valid() {
		writeError(w, http.StatusBadRequest, "protocol must be torrent or usenet")
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
		writeError(w, http.StatusBadRequest, httpx.Diagnose(err, in.Url).Error())
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
		Type: string(in.Type), Name: in.Name, URL: ports.NormalizeURL(in.Url),
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
	if in.ManualApproval != nil {
		cfg.ManualApproval = *in.ManualApproval
	}
	if in.PathMappings != nil {
		for _, m := range *in.PathMappings {
			// Blank halves are form noise, not a mapping.
			if strings.TrimSpace(m.Remote) == "" || strings.TrimSpace(m.Local) == "" {
				continue
			}
			cfg.PathMappings = append(cfg.PathMappings, ports.PathMapping{
				Remote: strings.TrimSpace(m.Remote), Local: strings.TrimSpace(m.Local),
			})
		}
	}
	return cfg
}

func clientDTO(c ports.ClientConfig) apigen.DownloadClientConfig {
	user, cat, enabled, manual := c.Username, c.Category, c.Enabled, c.ManualApproval
	masked := ""
	if c.Password != "" {
		masked = "••••"
	}
	out := apigen.DownloadClientConfig{
		Id: c.ID, Type: apigen.DownloadClientConfigType(c.Type), Name: c.Name, Url: c.URL,
		Username: &user, Password: &masked, Category: &cat, Enabled: &enabled,
		ManualApproval: &manual,
	}
	if len(c.PathMappings) > 0 {
		maps := make([]apigen.PathMapping, 0, len(c.PathMappings))
		for _, m := range c.PathMappings {
			maps = append(maps, apigen.PathMapping{Remote: m.Remote, Local: m.Local})
		}
		out.PathMappings = &maps
	}
	return out
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
	if !in.Type.Valid() {
		writeError(w, http.StatusBadRequest, "unknown download client type "+string(in.Type))
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
		writeError(w, http.StatusBadRequest, httpx.Diagnose(err, in.Url).Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// TestIndexerById implements POST /indexers/{id}/test: run the reachability
// test against the SAVED config — no re-typing credentials.
func (s *Server) TestIndexerById(w http.ResponseWriter, r *http.Request, id int64) {
	cfg, err := s.deps.Store.GetIndexer(r.Context(), id)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	if err := s.deps.IndexerFactory(cfg).Test(r.Context()); err != nil {
		writeError(w, http.StatusBadRequest, httpx.Diagnose(err, cfg.URL).Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// TestDownloadClientById implements POST /downloadclients/{id}/test.
func (s *Server) TestDownloadClientById(w http.ResponseWriter, r *http.Request, id int64) {
	cfg, err := s.deps.Store.GetDownloadClient(r.Context(), id)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	if err := s.deps.ClientFactory(cfg).Test(r.Context()); err != nil {
		// A Test button exists to say what to change, not merely that
		// something is wrong.
		writeError(w, http.StatusBadRequest, httpx.Diagnose(err, cfg.URL).Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// UpdateDownloadClient implements PUT /downloadclients/{id}. The stored
// password is preserved when the form sends the masked placeholder (or
// nothing), so an edit never blanks credentials the UI can't see.
func (s *Server) UpdateDownloadClient(w http.ResponseWriter, r *http.Request, id int64) {
	var in apigen.DownloadClientInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Name == "" || in.Url == "" {
		writeError(w, http.StatusBadRequest, "type, name, and url are required")
		return
	}
	if !in.Type.Valid() {
		writeError(w, http.StatusBadRequest, "unknown download client type "+string(in.Type))
		return
	}
	existing, err := s.deps.Store.GetDownloadClient(r.Context(), id)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	cfg := clientInputToConfig(in)
	cfg.ID = id
	if cfg.Password == "" || cfg.Password == "••••" {
		cfg.Password = existing.Password
	}
	if err := s.deps.Store.UpdateDownloadClient(r.Context(), cfg); err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, clientDTO(cfg))
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
			Quality: c.QualityStr, Score: c.Score, Accepted: c.Accepted, IsUpgrade: c.IsUpgrade,
			Rejections: []apigen.Rejection{},
		}
		if len(c.Formats) > 0 {
			fs := c.Formats
			rc.Formats = &fs
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
		copyID, clientID := d.CopyID, d.ClientID
		save, imp := d.SavePath, d.ImportPath
		item := apigen.QueueItem{
			Id: d.ID, MediaItemId: d.MediaItemID, CopyId: &copyID, ClientId: &clientID,
			Title: d.ReleaseTitle, State: d.State, Progress: float32(d.Progress),
			Protocol: d.Protocol, Quality: d.Quality.Display(),
			SavePath: &save, ImportPath: &imp, AddedAt: d.AddedAt,
		}
		if d.Error != "" {
			e := d.Error
			item.Error = &e
		}
		if len(d.Handoff) > 0 {
			steps := make([]apigen.HandoffEntry, 0, len(d.Handoff))
			for _, h := range d.Handoff {
				he := apigen.HandoffEntry{Step: h.Step, At: h.At}
				if h.Detail != "" {
					detail := h.Detail
					he.Detail = &detail
				}
				steps = append(steps, he)
			}
			item.Handoff = &steps
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

// ImportQueueItem implements POST /queue/{id}/import: approve a held
// download or retry a failed import. An import error is a 400 with the
// reason — the row stays retryable — while a missing row is a 404.
func (s *Server) ImportQueueItem(w http.ResponseWriter, r *http.Request, id int64) {
	err := s.deps.Acquisition.ImportNow(r.Context(), id)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, acquisition.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// BlocklistQueueItem implements POST /queue/{id}/blocklist: declare the
// release bad, blocklist it, and search a replacement.
func (s *Server) BlocklistQueueItem(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.deps.Acquisition.BlocklistReplace(r.Context(), id); err != nil {
		s.acqErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ScanImportPath implements GET /import/scan: list the media files Monarr
// can see under a path (the manual-import preview).
func (s *Server) ScanImportPath(w http.ResponseWriter, r *http.Request, params apigen.ScanImportPathParams) {
	files, err := s.deps.Acquisition.ScanImportPath(r.Context(), params.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]apigen.ScannedFile, 0, len(files))
	for _, f := range files {
		eps := f.Episodes
		if eps == nil {
			eps = []int{}
		}
		out = append(out, apigen.ScannedFile{
			Path: f.Path, Name: f.Name, Size: f.Size, Kind: f.Kind,
			Quality: f.Quality, Season: f.Season, Episodes: eps,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ManualImport implements POST /import/manual: import files from a path into
// a chosen item/copy.
func (s *Server) ManualImport(w http.ResponseWriter, r *http.Request) {
	var in apigen.ManualImportRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Path == "" || in.MediaItemId == 0 {
		writeError(w, http.StatusBadRequest, "path and mediaItemId are required")
		return
	}
	req := acquisition.ManualImportRequest{Path: in.Path, MediaItemID: in.MediaItemId}
	if in.CopyId != nil {
		req.CopyID = *in.CopyId
	}
	if in.DownloadId != nil {
		req.DownloadID = *in.DownloadId
	}
	files, err := s.deps.Acquisition.ManualImport(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"files": files})
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
			Copy: ws.Copy,
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

// ---- notifiers (Phase 3) ----

func notifierFromInput(in apigen.NotifierInput) ports.NotifierConfig {
	cfg := ports.NotifierConfig{
		Type: string(in.Type), Name: in.Name,
		OnGrab: true, OnImport: true, OnFailed: true, OnHealth: false, Enabled: true,
	}
	if in.Settings != nil {
		cfg.Settings = *in.Settings
	}
	if in.OnGrab != nil {
		cfg.OnGrab = *in.OnGrab
	}
	if in.OnImport != nil {
		cfg.OnImport = *in.OnImport
	}
	if in.OnFailed != nil {
		cfg.OnFailed = *in.OnFailed
	}
	if in.OnHealth != nil {
		cfg.OnHealth = *in.OnHealth
	}
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	return cfg
}

func notifierDTO(cfg ports.NotifierConfig) apigen.Notifier {
	settings := cfg.Settings
	if settings == nil {
		settings = map[string]string{}
	}
	return apigen.Notifier{
		Id: cfg.ID, Type: apigen.NotifierType(cfg.Type), Name: cfg.Name,
		Settings: &settings,
		OnGrab:   &cfg.OnGrab, OnImport: &cfg.OnImport,
		OnFailed: &cfg.OnFailed, OnHealth: &cfg.OnHealth, Enabled: &cfg.Enabled,
	}
}

// ListNotifiers implements GET /notifiers.
func (s *Server) ListNotifiers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deps.Store.ListNotifiers(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.Notifier, 0, len(rows))
	for _, cfg := range rows {
		out = append(out, notifierDTO(cfg))
	}
	writeJSON(w, http.StatusOK, out)
}

// AddNotifier implements POST /notifiers.
func (s *Server) AddNotifier(w http.ResponseWriter, r *http.Request) {
	var body apigen.AddNotifierJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !body.Type.Valid() || body.Name == "" {
		writeError(w, http.StatusBadRequest, "type and name are required")
		return
	}
	cfg := notifierFromInput(body)
	id, err := s.deps.Store.AddNotifier(r.Context(), cfg)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	cfg.ID = id
	writeJSON(w, http.StatusCreated, notifierDTO(cfg))
}

// TestNotifier implements POST /notifiers/test.
func (s *Server) TestNotifier(w http.ResponseWriter, r *http.Request) {
	var body apigen.TestNotifierJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !body.Type.Valid() {
		writeError(w, http.StatusBadRequest, "unknown notifier type")
		return
	}
	if err := s.deps.NotifierFactory(notifierFromInput(body)).Test(r.Context()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// DeleteNotifier implements DELETE /notifiers/{id}.
func (s *Server) DeleteNotifier(w http.ResponseWriter, r *http.Request, id int64) {
	if _, err := s.deps.Store.GetNotifier(r.Context(), id); err != nil {
		s.acqErr(w, err)
		return
	}
	if err := s.deps.Store.DeleteNotifier(r.Context(), id); err != nil {
		s.acqErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListBackups implements GET /system/backups.
func (s *Server) ListBackups(w http.ResponseWriter, r *http.Request) {
	backups, err := s.deps.Store.ListBackups()
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.BackupInfo, 0, len(backups))
	for _, b := range backups {
		out = append(out, apigen.BackupInfo{
			Name: b.Name, SizeBytes: b.SizeBytes, CreatedAt: b.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- custom formats + import lists + bulk edit (Phase 5) ----

// ListCustomFormats implements GET /customformats.
func (s *Server) ListCustomFormats(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deps.Store.ListCustomFormats(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.CustomFormat, 0, len(rows))
	for _, f := range rows {
		score := f.Score
		out = append(out, apigen.CustomFormat{Id: f.ID, Name: f.Name, Pattern: f.Pattern, Score: &score})
	}
	writeJSON(w, http.StatusOK, out)
}

// AddCustomFormat implements POST /customformats.
func (s *Server) AddCustomFormat(w http.ResponseWriter, r *http.Request) {
	var body apigen.AddCustomFormatJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" || body.Pattern == "" {
		writeError(w, http.StatusBadRequest, "name and pattern are required")
		return
	}
	if _, err := regexp.Compile("(?i)" + body.Pattern); err != nil {
		writeError(w, http.StatusBadRequest, "invalid pattern: "+err.Error())
		return
	}
	f := format.CustomFormat{Name: body.Name, Pattern: body.Pattern}
	if body.Score != nil {
		f.Score = *body.Score
	}
	id, err := s.deps.Store.AddCustomFormat(r.Context(), f)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	f.ID = id
	score := f.Score
	writeJSON(w, http.StatusCreated, apigen.CustomFormat{Id: f.ID, Name: f.Name, Pattern: f.Pattern, Score: &score})
}

// DeleteCustomFormat implements DELETE /customformats/{id}.
func (s *Server) DeleteCustomFormat(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.deps.Store.DeleteCustomFormat(r.Context(), id); err != nil {
		s.acqErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ListImportLists implements GET /importlists.
func (s *Server) ListImportLists(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deps.Store.ListImportLists(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.ImportList, 0, len(rows))
	for _, l := range rows {
		out = append(out, importListDTO(l))
	}
	writeJSON(w, http.StatusOK, out)
}

func importListDTO(l sqlite.ImportList) apigen.ImportList {
	cfg := l.Config
	if cfg == nil {
		cfg = map[string]string{}
	}
	kind := apigen.MediaKind(l.Kind)
	return apigen.ImportList{
		Id: l.ID, Name: l.Name, Type: apigen.ImportListType(l.Type),
		Config: &cfg, Kind: &kind,
		RootFolderId: &l.RootFolderID, QualityProfileId: &l.QualityProfileID,
		Monitored: &l.Monitored, Enabled: &l.Enabled,
	}
}

// AddImportList implements POST /importlists.
func (s *Server) AddImportList(w http.ResponseWriter, r *http.Request) {
	var body apigen.AddImportListJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		writeError(w, http.StatusBadRequest, "name and type are required")
		return
	}
	l := sqlite.ImportList{
		Name: body.Name, Type: string(body.Type), Kind: "movie",
		QualityProfileID: 1, Monitored: true, Enabled: true,
	}
	if body.Config != nil {
		l.Config = *body.Config
	}
	if body.Kind != nil {
		l.Kind = string(*body.Kind)
	}
	if body.RootFolderId != nil {
		l.RootFolderID = *body.RootFolderId
	}
	if body.QualityProfileId != nil {
		l.QualityProfileID = *body.QualityProfileId
	}
	if body.Monitored != nil {
		l.Monitored = *body.Monitored
	}
	if body.Enabled != nil {
		l.Enabled = *body.Enabled
	}
	id, err := s.deps.Store.AddImportList(r.Context(), l)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	l.ID = id
	writeJSON(w, http.StatusCreated, importListDTO(l))
}

// DeleteImportList implements DELETE /importlists/{id}.
func (s *Server) DeleteImportList(w http.ResponseWriter, r *http.Request, id int64) {
	if err := s.deps.Store.DeleteImportList(r.Context(), id); err != nil {
		s.acqErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// BulkEditLibrary implements POST /library/bulk (the mass editor).
func (s *Server) BulkEditLibrary(w http.ResponseWriter, r *http.Request) {
	var body apigen.BulkEditLibraryJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Ids) == 0 {
		writeError(w, http.StatusBadRequest, "ids required")
		return
	}
	updated := 0
	for _, id := range body.Ids {
		if err := s.deps.Store.BulkUpdateItem(r.Context(), id, body.Monitored, body.QualityProfileId); err == nil {
			updated++
		}
	}
	if s.deps.Acquisition != nil {
		s.deps.Acquisition.InvalidateWanted()
	}
	writeJSON(w, http.StatusOK, map[string]int{"updated": updated})
}

// AutoSearchLibraryItem implements POST /library/{id}/autosearch: the
// Sonarr-style "search automatically" — background, best-pick, no list.
func (s *Server) AutoSearchLibraryItem(w http.ResponseWriter, r *http.Request, id int64) {
	if _, err := s.deps.Library.Get(r.Context(), id); err != nil {
		s.acqErr(w, err)
		return
	}
	go func(itemID int64) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.deps.Acquisition.AutoSearchItem(ctx, itemID); err != nil {
			s.deps.Log.Warn("auto search failed", "item", itemID, "err", err)
		}
	}(id)
	w.WriteHeader(http.StatusAccepted)
}
