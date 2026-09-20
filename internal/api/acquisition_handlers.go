package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/adapters/httpx"
	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/domain/downloadpriority"
	"github.com/pjunod/monarr/internal/domain/format"
	"github.com/pjunod/monarr/internal/domain/quality"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

func (s *Server) acqErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, acquisition.ErrNoIndexers):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, acquisition.ErrNoClient):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, acquisition.ErrNoLibraryFolder):
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
		UpgradesAllowed: p.UpgradesAllowed, DownloadPriority: p.DownloadPriority,
		Sentence: p.Sentence(),
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
	if in.DownloadPriority != nil {
		p.DownloadPriority = *in.DownloadPriority
	}
	if !downloadpriority.Valid(p.DownloadPriority) {
		return quality.Profile{}, fmt.Errorf(
			"download priority must be one of -100, -50, 0, 50, 100, or 900",
		)
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
	// Being some kind's default is the same class of refusal as being in use:
	// a live reference the caller has to clear first, not a server fault.
	// Only ErrProfileInUse was listed, so this answered 500 — printing a
	// perfectly actionable message ("quality profile is a default for new
	// items: Movies") behind a status code that said the server broke.
	case errors.Is(err, sqlite.ErrProfileIsDefault):
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
		Type: string(in.Type), Name: in.Name,
		URL:      ports.NormalizeServiceURL(in.Url, ports.DefaultPortFor(string(in.Type))),
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
	if in.RemoveCompleted != nil {
		cfg.RemoveCompleted = *in.RemoveCompleted
	} else {
		// Unset means the caller has no opinion, so take the one the client
		// type implies — the same split migration 0025 applied to existing
		// rows. Usenet is finished with the payload; a torrent is still
		// seeding and its data is not monarr's to throw away on a guess.
		cfg.RemoveCompleted = ports.ProtocolOfClient(cfg.Type) == "usenet"
	}
	if in.Mode != nil {
		cfg.Mode = string(*in.Mode)
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
	removeDone := c.RemoveCompleted
	mode := apigen.DownloadClientConfigMode(c.Mode)
	if c.Mode == "" {
		mode = "poll"
	}
	masked := ""
	if c.Password != "" {
		masked = "••••"
	}
	out := apigen.DownloadClientConfig{
		Id: c.ID, Type: apigen.DownloadClientConfigType(c.Type), Name: c.Name, Url: c.URL,
		Username: &user, Password: &masked, Category: &cat, Enabled: &enabled,
		ManualApproval: &manual, Mode: &mode, RemoveCompleted: &removeDone,
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
	copyID := int64(0)
	if params.Season != nil {
		season = *params.Season
	}
	if params.Episode != nil {
		episode = *params.Episode
	}
	if params.CopyId != nil {
		copyID = *params.CopyId
	}
	cands, status, err := s.deps.Acquisition.SearchCopyDetailed(r.Context(), id, copyID, season, episode)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	if status.Partial {
		w.Header().Set("X-Monarr-Search-Partial", "true")
		reason := strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.Join(status.Reasons, "; "))
		w.Header().Set("X-Monarr-Search-Reason", reason)
	}
	out := make([]apigen.ReleaseCandidate, 0, len(cands))
	for _, c := range cands {
		match := apiMatchEvidence(c.Match)
		rc := apigen.ReleaseCandidate{
			Title: c.Release.Title, DownloadUrl: c.Release.DownloadURL,
			Indexer: c.Release.Indexer, Protocol: c.Release.Protocol,
			Size: c.Release.Size, Seeders: c.Release.Seeders, Age: c.Age,
			Quality: c.QualityStr, Score: c.Score, Accepted: c.Accepted, IsUpgrade: c.IsUpgrade,
			Rejections: []apigen.Rejection{}, Match: match, CandidateToken: c.Token,
		}
		if len(c.Formats) > 0 {
			fs := c.Formats
			rc.Formats = &fs
		}
		if c.Release.InfoURL != "" {
			u := c.Release.InfoURL
			rc.InfoUrl = &u
		}
		if c.Warning != "" {
			warning := c.Warning
			rc.Warning = &warning
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
	if in.CopyId != nil {
		req.CopyID = *in.CopyId
	}
	if in.Indexer != nil {
		req.Indexer = *in.Indexer
	}
	if in.Size != nil {
		req.Size = *in.Size
	}
	if in.CandidateToken != nil {
		req.CandidateToken = *in.CandidateToken
	}
	id, err := s.deps.Acquisition.Grab(r.Context(), req)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// ListQueue implements GET /queue.
//
// Unfiltered it is what it always was: the last hundred rows, every state.
// With a filter it is one page of one group — which is what the Activity
// page asks for now, because "Finished" was a section that only ever grew.
func (s *Server) ListQueue(w http.ResponseWriter, r *http.Request, params apigen.ListQueueParams) {
	rows, err := s.queueRows(r.Context(), params)
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
		if d.MatchEvidence.Version > 0 {
			match := apiMatchEvidence(d.MatchEvidence)
			item.Match = &match
		}
		if d.Error != "" {
			e := d.Error
			item.Error = &e
		}
		// The live stage rides on the row, not on a panel somewhere else.
		//
		// Copying into the library is not a different kind of thing from
		// fetching the release — it is the next part of the same job, and
		// putting it on its own surface made you look in two places to answer
		// one question. So the row carries whatever is happening to it right
		// now, down to the bytes and the rate, and the stage word changes as
		// it moves. `state` is untouched and still the persisted lifecycle;
		// `stage` is finer-grained, lives only while something is moving, and
		// is the only thing that knows the difference between repairing and
		// extracting.
		if live, ok := s.deps.Acquisition.LiveStage(d.ID); ok {
			stage := live.Stage
			item.Stage = &stage
			if live.Bytes > 0 {
				b := live.Bytes
				item.Bytes = &b
			}
			if live.Total > 0 {
				t := live.Total
				item.Total = &t
			}
			if live.BytesPerSecond > 0 {
				r := float32(live.BytesPerSecond)
				item.BytesPerSecond = &r
			}
			if live.Detail != "" {
				dt := live.Detail
				item.StageDetail = &dt
			}
			if live.Peer != "" {
				pr := live.Peer
				item.StagePeer = &pr
			}
			since := live.StartedAt
			item.StageSince = &since
		}
		if state := s.deps.Acquisition.ImportStatus(d.ID); state != "" {
			importState := apigen.QueueItemImportState(state)
			item.ImportState = &importState
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

// CancelQueueImport implements POST /queue/{id}/cancel-import. Cancellation
// keeps the completed payload and returns the row to a restartable state.
func (s *Server) CancelQueueImport(w http.ResponseWriter, r *http.Request, id int64) {
	err := s.deps.Acquisition.CancelImport(r.Context(), id)
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

// GetManualImportDefaultPath implements GET /import/default-path.
func (s *Server) GetManualImportDefaultPath(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, apigen.ImportPathDefault{
		Path: s.deps.Acquisition.ManualImportDefaultPath(r.Context()),
	})
}

// ManualImport implements POST /import/manual: validate the selection and
// queue an Activity job. Copying belongs to an importer worker, never to the
// request goroutine.
func (s *Server) ManualImport(w http.ResponseWriter, r *http.Request) {
	var in apigen.ManualImportRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Path == "" || in.MediaItemId == 0 {
		writeError(w, http.StatusBadRequest, "path and mediaItemId are required")
		return
	}
	req := acquisition.ManualImportRequest{Path: in.Path, MediaItemID: in.MediaItemId}
	if in.Paths != nil {
		req.Paths = *in.Paths
	}
	if in.CopyId != nil {
		req.CopyID = *in.CopyId
	}
	if in.DownloadId != nil {
		req.DownloadID = *in.DownloadId
	}
	id, err := s.deps.Acquisition.QueueManualImport(r.Context(), req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, apigen.ManualImportAccepted{DownloadId: id})
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
			Copy: ws.Copy, CopyId: ws.CopyID, Reason: apigen.WantedItemReason(ws.Reason),
			Kind: apigen.WantedItemKind(ws.Kind), Season: ws.Season, Episode: ws.Episode,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func wantedSearchDTO(run acquisition.WantedSearchRun) apigen.WantedSearchRun {
	scope := apigen.WantedSearchScope{Scope: apigen.WantedSearchScopeScope(run.Scope)}
	if run.Reason != "" {
		reason := apigen.WantedSearchScopeReason(run.Reason)
		scope.Reason = &reason
	}
	if run.MediaItemID != 0 {
		scope.MediaItemId = &run.MediaItemID
	}
	if run.WantableID != "" {
		scope.WantableId = &run.WantableID
	}
	out := apigen.WantedSearchRun{
		RunId: run.RunID, Scope: scope, ScopeLabel: run.ScopeLabel,
		Status: apigen.WantedSearchRunStatus(run.Status), CreatedAt: run.CreatedAt,
		TargetDelayMs: run.TargetDelayMs, Selected: run.Selected, Processed: run.Processed,
		Searched: run.Searched, Skipped: run.Skipped, Failed: run.Failed, Grabbed: run.Grabbed,
	}
	if !run.StartedAt.IsZero() {
		out.StartedAt = &run.StartedAt
	}
	if !run.FinishedAt.IsZero() {
		out.FinishedAt = &run.FinishedAt
	}
	if !run.CancelRequestedAt.IsZero() {
		out.CancelRequestedAt = &run.CancelRequestedAt
	}
	if run.Error != "" {
		out.Error = &run.Error
	}
	return out
}

// CreateWantedSearch validates the discriminated request before the service
// snapshots any server-owned membership.
func (s *Server) CreateWantedSearch(w http.ResponseWriter, r *http.Request) {
	var body apigen.WantedSearchRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req := acquisition.WantedSearchRequest{Scope: string(body.Scope), TargetDelay: body.TargetDelayMs}
	if body.Reason != nil {
		req.Reason = acquisition.WantedReason(*body.Reason)
	}
	if body.MediaItemId != nil {
		req.MediaItemID = *body.MediaItemId
	}
	if body.WantableId != nil {
		req.WantableID = *body.WantableId
	}
	run, err := s.deps.Acquisition.StartWantedSearch(r.Context(), req)
	if err != nil {
		var conflict *acquisition.WantedSearchConflict
		switch {
		case errors.As(err, &conflict):
			writeJSON(w, http.StatusConflict, apigen.WantedSearchConflict{
				ActiveRunId: conflict.ActiveRunID, CanCancel: true, Message: conflict.Message})
		case errors.Is(err, acquisition.ErrInvalidWantedSearch):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, acquisition.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			s.acqErr(w, err)
		}
		return
	}
	writeJSON(w, http.StatusAccepted, wantedSearchDTO(run))
}

func (s *Server) GetWantedSearch(w http.ResponseWriter, r *http.Request, runID apigen.WantedSearchRunId) {
	run, err := s.deps.Acquisition.WantedSearch(r.Context(), runID)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, wantedSearchDTO(run))
}

func (s *Server) CancelWantedSearch(w http.ResponseWriter, r *http.Request, runID apigen.WantedSearchRunId) {
	before, err := s.deps.Acquisition.WantedSearch(r.Context(), runID)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	run, err := s.deps.Acquisition.CancelWantedSearch(r.Context(), runID)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	status := http.StatusOK
	if before.Status == "queued" || before.Status == "running" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, wantedSearchDTO(run))
}

func (s *Server) ListWantedSearchResults(w http.ResponseWriter, r *http.Request, runID apigen.WantedSearchRunId, params apigen.ListWantedSearchResultsParams) {
	limit, offset := 100, 0
	if params.Limit != nil {
		limit = *params.Limit
	}
	if params.Offset != nil {
		offset = *params.Offset
	}
	rows, err := s.deps.Acquisition.WantedSearchResults(r.Context(), runID, limit, offset)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	items := make([]apigen.WantedSearchResult, 0, len(rows))
	for _, row := range rows {
		dto := apigen.WantedSearchResult{Ordinal: row.Ordinal, WantableId: row.WantableID,
			Label: row.Label, State: apigen.WantedSearchResultState(row.State), Seen: row.Seen,
			Matched: row.Matched, Accepted: row.Accepted, FinishedAt: row.FinishedAt,
			Grabbed: optStr(row.Grabbed), Error: optStr(row.Error)}
		if row.Skipped != "" {
			skipped := apigen.WantedSearchResultSkipped(row.Skipped)
			dto.Skipped = &skipped
		}
		items = append(items, dto)
	}
	writeJSON(w, http.StatusOK, apigen.WantedSearchResultsPage{Items: items, Limit: limit, Offset: offset})
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
		entry := apigen.CalendarEntry{
			Date: e.Date, Kind: e.Kind, MediaItemId: e.MediaItemID,
			Title: e.Title, Detail: e.Detail, HasFile: e.HasFile,
		}
		// Ids only when we have them: an absent field says "unknown", where a
		// zero would say "TMDB id 0" and match whatever the consumer stores
		// for its own unknowns.
		if e.TmdbID != 0 {
			id := e.TmdbID
			entry.TmdbId = &id
		}
		if e.ImdbID != "" {
			imdb := e.ImdbID
			entry.ImdbId = &imdb
		}
		// The presentation extras, added with the calendar rebuild. Same
		// rule as the ids throughout: absent means unknown, because a zero
		// runtime and a 0-minute runtime are not the same claim.
		if e.PosterPath != "" {
			p := e.PosterPath
			entry.PosterPath = &p
		}
		if e.Runtime > 0 {
			rt := e.Runtime
			entry.Runtime = &rt
		}
		mon := e.Monitored
		entry.Monitored = &mon
		if e.Kind == "episode" {
			season, ep := e.SeasonNumber, e.EpisodeNumber
			entry.SeasonNumber, entry.EpisodeNumber = &season, &ep
			if e.EpisodeTitle != "" {
				t := e.EpisodeTitle
				entry.EpisodeTitle = &t
			}
			if e.Network != "" {
				n := e.Network
				entry.Network = &n
			}
			if at, ok := composeAirTime(e.Date, e.AirsTime, e.AirsTimezone); ok {
				entry.AirDateUtc = &at
			}
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, out)
}

// locationCache memoizes time.LoadLocation, which reads and parses the
// zoneinfo database on every call — the calendar asks for the same handful
// of zones once per row.
var locationCache sync.Map // string -> *time.Location

func loadLocation(name string) (*time.Location, bool) {
	if v, ok := locationCache.Load(name); ok {
		loc, ok := v.(*time.Location)
		// Failed lookups are cached as a typed nil. A type assertion still
		// succeeds for that sentinel, so check the pointer too; otherwise the
		// second request for an unknown zone reaches ParseInLocation with a nil
		// location and panics instead of cleanly omitting the air time.
		return loc, ok && loc != nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		// Cache the failure too: an unknown zone name stored on an item is
		// wrong permanently, not intermittently, and retrying the parse on
		// every row of every request buys nothing.
		locationCache.Store(name, (*time.Location)(nil))
		return nil, false
	}
	locationCache.Store(name, loc)
	return loc, true
}

// composeAirTime turns an air DATE plus the show's broadcast slot into the
// exact UTC instant that episode airs (ADR 0016).
//
// Composing here rather than storing an instant is what makes DST correct
// for free: a 21:00 America/New_York show is 02:00Z in January and 01:00Z in
// July, and each date carries its own offset. Any piece missing or
// unparsable returns false and the field is omitted — never guessed.
func composeAirTime(date, airsTime, tz string) (time.Time, bool) {
	if date == "" || airsTime == "" || tz == "" {
		return time.Time{}, false
	}
	loc, ok := loadLocation(tz)
	if !ok {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02 15:04", date+" "+airsTime, loc)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
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
	// Complete the URL on the way in, so the settings row shows back exactly
	// what Monarr will dial. What a person types is a host — `plurxd`,
	// `192.168.1.10` — because that is the part they had to look up; the
	// scheme and the port are constants the software already knows.
	if u, ok := cfg.Settings["url"]; ok && u != "" {
		cfg.Settings["url"] = ports.NormalizeServiceURL(u, ports.DefaultPortFor(cfg.Type))
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

// TestNotifierByID implements POST /notifiers/{id}/test — the same probe,
// against the SAVED config rather than whatever is typed into the add form.
//
// Download clients and indexers have had this since the beginning; notifiers
// did not, so the only way to check a saved one was to retype its settings
// into the add form and trust you retyped them identically. That tests the
// typing. This tests the notifier that actually runs after an import.
func (s *Server) TestNotifierByID(w http.ResponseWriter, r *http.Request, id int64) {
	cfg, err := s.deps.Store.GetNotifier(r.Context(), id)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	if err := s.deps.NotifierFactory(cfg).Test(r.Context()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
}

// UpdateNotifier implements PUT /notifiers/{id}.
func (s *Server) UpdateNotifier(w http.ResponseWriter, r *http.Request, id int64) {
	if _, err := s.deps.Store.GetNotifier(r.Context(), id); err != nil {
		s.acqErr(w, err)
		return
	}
	var body apigen.UpdateNotifierJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !body.Type.Valid() || body.Name == "" {
		writeError(w, http.StatusBadRequest, "type and name are required")
		return
	}
	cfg := notifierFromInput(apigen.AddNotifierJSONRequestBody(body))
	cfg.ID = id
	if err := s.deps.Store.UpdateNotifier(r.Context(), cfg); err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, notifierDTO(cfg))
}

// ListDeliveries implements GET /notifiers/{id}/deliveries.
//
// The answer to "did it actually arrive?" — which, for a notifier that acts
// on a media server rather than talking to a person, is the same question as
// "is the file in my library?".
func (s *Server) ListDeliveries(w http.ResponseWriter, r *http.Request, id int64) {
	if _, err := s.deps.Store.GetNotifier(r.Context(), id); err != nil {
		s.acqErr(w, err)
		return
	}
	rows, err := s.deps.Store.ListDeliveries(r.Context(), id, 100)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.Delivery, 0, len(rows))
	for _, d := range rows {
		out = append(out, apigen.Delivery{
			Id: d.ID, NotifierId: d.NotifierID, DownloadId: ptrIfSet(d.DownloadID),
			Event: d.Event, Attempts: d.Attempts,
			LastError: ptrIfText(d.LastError), Result: ptrIfText(d.Result),
			Status:    apigen.DeliveryStatus(d.Status),
			NextAt:    ptrIfSet(d.NextAt),
			CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func ptrIfSet(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func ptrIfText(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func apiMatchEvidence(e domain.MatchEvidence) apigen.MatchEvidence {
	match := apigen.MatchEvidence{
		Version: e.Version, Matched: e.Matched, Reason: e.Reason,
		Method: ptrIfText(e.Method), Code: ptrIfText(e.Code), Country: ptrIfText(e.Country),
		OriginalTitle: ptrIfText(e.OriginalTitle), ParsedTitle: ptrIfText(e.ParsedTitle),
		TargetTitle: ptrIfText(e.TargetTitle), MatchedTitle: ptrIfText(e.MatchedTitle),
	}
	if e.MatchedID != nil {
		match.MatchedId = &struct {
			Provider string `json:"provider"`
			Value    string `json:"value"`
		}{Provider: e.MatchedID.Provider, Value: e.MatchedID.Value}
	}
	if len(e.Warnings) > 0 {
		warnings := e.Warnings
		match.Warnings = &warnings
	}
	return match
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

// connectionsResponse is the body of GET /system/connections.
type connectionsResponse struct {
	CheckedAt   *int64              `json:"checkedAt,omitempty"`
	Connections []apigen.Connection `json:"connections"`
}

// GetConnections implements GET /system/connections.
//
// The single screen that answers "are the three apps actually talking right
// now" (plan §5.7). It reports the LAST probe rather than probing on demand:
// a page that opened four connections every few seconds would itself become
// load on the servers it is reporting about, and `checkedAt` says how fresh
// the answer is so nobody has to guess.
func (s *Server) GetConnections(w http.ResponseWriter, r *http.Request) {
	out := connectionsResponse{Connections: []apigen.Connection{}}
	// The two halves are independent: inbound callers are recorded whether
	// or not the outbound probe has ever run, and a missing monitor must not
	// hide them.
	if s.deps.Connections != nil {
		states, at := s.deps.Connections.Snapshot()
		if !at.IsZero() {
			ms := at.UnixMilli()
			out.CheckedAt = &ms
		}
		links := map[int64]acquisition.ClientLink{}
		if s.deps.Acquisition != nil {
			for _, l := range s.deps.Acquisition.Subscriptions() {
				links[l.ClientID] = l
			}
		}
		for _, st := range states {
			out.Connections = append(out.Connections, connectionDTO(st, links[st.ID]))
		}
	}
	// The other direction. Everything above is something Monarr reaches out
	// to; these are applications that reach IN — plurx pushing watch state,
	// a compat consumer polling. Leaving them off made a plurx that was
	// configured perfectly and calling every few minutes appear nowhere at
	// all, which reads as "it is not working".
	if s.deps.Callers != nil {
		for _, c := range s.deps.Callers.List() {
			out.Connections = append(out.Connections, inboundDTO(c))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// connectionDTO folds the probe's findings and the live push state into the
// one word somebody actually reads off the page.
func connectionDTO(st health.ConnectionState, link acquisition.ClientLink) apigen.Connection {
	c := apigen.Connection{
		Name: st.Name, Kind: apigen.ConnectionKind(st.Kind), Type: st.Type,
		Url: ptrIfText(st.URL), Version: ptrIfText(st.Version),
	}
	if !st.LastSeen.IsZero() {
		ms := st.LastSeen.UnixMilli()
		c.LastContact = &ms
	}
	if link.LastSeq > 0 {
		seq := int64(link.LastSeq)
		c.LastEventSeq = &seq
	}

	state, detail := connectionState(st, link)
	c.State = apigen.ConnectionState(state)
	c.Detail = ptrIfText(detail)
	return c
}

// inboundQuietAfter is how long since a caller was last heard from before
// the row stops claiming it is talking to us. Generous: plurx's coming-soon
// rail refreshes every 15 minutes and its watch pushes are as rare as
// somebody finishing something, so a stricter window would call a perfectly
// healthy pairing quiet all afternoon.
const inboundQuietAfter = time.Hour

func inboundDTO(c Caller) apigen.Connection {
	seen := c.LastSeen.UnixMilli()
	state := apigen.Calling
	detail := fmt.Sprintf("last called %s (%d since startup)", c.LastPath, c.Calls)
	if time.Since(c.LastSeen) > inboundQuietAfter {
		state = apigen.Quiet
		detail = fmt.Sprintf("nothing since %s — last called %s",
			c.LastSeen.Format(time.RFC3339), c.LastPath)
	}
	return apigen.Connection{
		Name: c.Name, Kind: apigen.Inbound, Type: c.Agent,
		State: state, LastContact: &seen, Detail: &detail,
	}
}

func connectionState(st health.ConnectionState, link acquisition.ClientLink) (string, string) {
	switch {
	case !st.Probed:
		// Configured, and deliberately never probed — its only test is the
		// action itself. Said out loud, because a blank row reads as fine.
		return "unprobed", "not probed: its only test is a full library rescan"
	case st.Reach != nil:
		return "unreachable", st.Reach.Error()
	}
	// Answering. Now: is it actually working?
	if st.Capacity != nil {
		if problems := st.Capacity.Problems(); len(problems) > 0 {
			return "degraded", strings.Join(problems, "; ")
		}
	}
	if st.StaleFor > 5*time.Minute {
		// Never a degraded row with an empty Detail column. This branch used
		// to return st.LastError verbatim, and LastError is empty whenever
		// the last exchange did not fail — which is exactly the case here,
		// because "stale" means Monarr stopped asking, not that asking
		// failed. The result was an amber badge with nothing beside it and
		// no way to find out why. A state the panel cannot explain is worse
		// than no state at all.
		return "degraded", st.StaleMessage()
	}
	// A push client that is answering but whose stream is down is not
	// "live" — it is running on the poll, which is the fallback working as
	// designed rather than a failure.
	if link.ClientID != 0 && link.Mode == "push" {
		if link.Connected {
			return "live", ""
		}
		return "polling", "push is configured but the stream is not connected" + suffix(link.LastError)
	}
	return "polling", ""
}

func suffix(msg string) string {
	if msg == "" {
		return ""
	}
	return ": " + msg
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
	// Valid() comes from the spec's enum, so this cannot drift from
	// openapi.yaml the way a second hardcoded list would. Every sibling
	// create already called it; this one did not, so a list of a type no
	// syncer knows about stored happily, appeared in the UI, and silently
	// never synced — `importlist.syncOne` answers "unknown list type" into a
	// log nobody is reading.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" || !body.Type.Valid() {
		writeError(w, http.StatusBadRequest, "name and a known type are required")
		return
	}
	l := sqlite.ImportList{
		Name: body.Name, Type: string(body.Type), Kind: "movie",
		Monitored: true, Enabled: true,
	}
	if body.Config != nil {
		l.Config = *body.Config
	}
	if body.Kind != nil {
		l.Kind = string(*body.Kind)
	}
	// Everything a list adds behaves like a manual add of that kind, so an
	// unspecified profile means what it means there: the kind's default.
	l.QualityProfileID = s.deps.Store.DefaultProfileID(r.Context(), domain.MediaKind(l.Kind))
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
		if _, err := s.deps.Library.UpdateItem(r.Context(), id, library.UpdateRequest{
			Monitored: body.Monitored, QualityProfileID: body.QualityProfileId,
		}); err == nil {
			updated++
		}
	}
	if s.deps.Acquisition != nil {
		s.deps.Acquisition.InvalidateWanted()
	}
	writeJSON(w, http.StatusOK, map[string]int{"updated": updated})
}

// AutoSearchLibraryItem implements POST /library/{id}/autosearch: the
// Sonarr-style "search automatically" — best-pick, no candidate list.
//
// Synchronous, and it answers with what it did. It used to hand back a bare
// 202 and run the search in a goroutine, which meant the UI said the same
// hopeful sentence whether the search grabbed a remux, turned down everything
// it found, or never ran at all. Interactive search on the same item already
// blocks for the same fan-out against the same indexers, so there was never a
// latency argument for the 202 — only the assumption that there was nothing
// worth reporting.
//
// The one caller that still wants fire-and-forget is search-on-add
// (searchInBackground): nobody is waiting on that one, and its failure must
// not fail the edit that triggered it.
func (s *Server) AutoSearchLibraryItem(w http.ResponseWriter, r *http.Request, id int64) {
	if _, err := s.deps.Library.Get(r.Context(), id); err != nil {
		s.acqErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	out, err := s.deps.Acquisition.AutoSearchItem(ctx, id)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	targets := make([]apigen.AutoSearchTarget, 0, len(out.Targets))
	for _, t := range out.Targets {
		dto := apigen.AutoSearchTarget{
			WantableId: t.WantableID, Label: t.Label,
			Seen: t.Seen, Matched: t.Matched, Accepted: t.Accepted,
			Grabbed: optStr(t.Grabbed), Error: optStr(t.Error),
		}
		if t.Skipped != "" {
			skipped := apigen.AutoSearchTargetSkipped(t.Skipped)
			dto.Skipped = &skipped
		}
		targets = append(targets, dto)
	}
	writeJSON(w, http.StatusOK, apigen.AutoSearchResult{
		Grabbed: out.Grabbed, Targets: targets,
	})
}

// ListTransfers implements GET /system/transfers — the data plane.
//
// Separate from /system/connections on purpose. That endpoint answers "can
// Monarr still talk to these applications"; this one answers "is anything
// actually moving". They were the same question once, and the result was a
// 20 GB import reported as a degraded download client while being visible
// nowhere else at all.
func (s *Server) ListTransfers(w http.ResponseWriter, r *http.Request) {
	if s.deps.Acquisition == nil {
		writeJSON(w, http.StatusOK, []apigen.Transfer{})
		return
	}
	live := s.deps.Acquisition.Transfers()
	out := make([]apigen.Transfer, 0, len(live))
	for _, t := range live {
		item := apigen.Transfer{
			DownloadId: t.DownloadID,
			Title:      t.Title,
			Stage:      apigen.TransferStage(t.Stage),
			StartedAt:  t.StartedAt,
			Outbound:   t.Outbound,
		}
		if t.Transfer != "" {
			v := t.Transfer
			item.Transfer = &v
		}
		if t.Peer != "" {
			v := t.Peer
			item.Peer = &v
		}
		if t.Detail != "" {
			v := t.Detail
			item.Detail = &v
		}
		// Absent, not zero: "we cannot measure this stage" and "nothing has
		// moved yet" look identical as a 0 and mean opposite things.
		if t.Total > 0 {
			total := t.Total
			item.Total = &total
			b := t.Bytes
			item.Bytes = &b
		}
		if t.BytesPerSecond > 0 {
			rate := t.BytesPerSecond
			item.BytesPerSecond = &rate
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// queueRows reads the page the parameters describe. No parameters at all
// keeps the old behaviour exactly, so every existing consumer is untouched.
func (s *Server) queueRows(ctx context.Context, params apigen.ListQueueParams) ([]sqlite.Download, error) {
	search := ""
	if params.Q != nil {
		search = strings.TrimSpace(*params.Q)
	}
	if params.Filter == nil && search == "" && params.Limit == nil && params.Offset == nil {
		return s.deps.Acquisition.Queue(ctx)
	}
	limit, offset := 100, 0
	if params.Limit != nil && *params.Limit > 0 {
		limit = min(*params.Limit, 500)
	}
	if params.Offset != nil && *params.Offset > 0 {
		offset = *params.Offset
	}
	var filter sqlite.QueueFilter
	if params.Filter != nil {
		filter = sqlite.QueueFilter(*params.Filter)
	}
	return s.deps.Acquisition.QueuePage(ctx, filter, search, limit, offset)
}

// QueueSummary implements GET /queue/summary — the section counts, without
// the sections.
func (s *Server) QueueSummary(w http.ResponseWriter, r *http.Request) {
	counts, err := s.deps.Acquisition.QueueCounts(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := apigen.QueueSummary{Counts: map[string]int{}}
	for state, n := range counts {
		out.Counts[state] = int(n)
		out.Total += int(n)
		if state != "imported" && state != "failed" {
			out.Active += int(n)
		}
	}
	days := s.deps.Acquisition.RetentionDays(r.Context())
	out.RetentionDays = &days
	writeJSON(w, http.StatusOK, out)
}

// ClearFinishedQueue implements DELETE /queue/finished.
func (s *Server) ClearFinishedQueue(w http.ResponseWriter, r *http.Request) {
	n, err := s.deps.Acquisition.ClearFinished(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.ClearedCount{Cleared: int(n)})
}

// ClearFailedQueue implements DELETE /queue/failed.
func (s *Server) ClearFailedQueue(w http.ResponseWriter, r *http.Request) {
	n, err := s.deps.Acquisition.ClearFailed(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apigen.ClearedCount{Cleared: int(n)})
}

// ListHistory implements GET /history — the per-release timeline.
//
// Monarr has written these events since the beginning and shown them
// nowhere: `ListHistory` had no caller outside its own package. That is
// how five consecutive failed grabs of one movie in ten hours, and two
// grabs of the same NZB six minutes apart, looked like a quiet evening.
// The rows were always there; this is the part that hands them over.
func (s *Server) ListHistory(w http.ResponseWriter, r *http.Request, params apigen.ListHistoryParams) {
	limit, offset := 100, 0
	if params.Limit != nil && *params.Limit > 0 {
		limit = min(*params.Limit, 500)
	}
	if params.Offset != nil && *params.Offset > 0 {
		offset = *params.Offset
	}
	var item int64
	if params.MediaItemId != nil {
		item = *params.MediaItemId
	}
	rows, err := s.deps.Acquisition.HistoryPage(r.Context(), item, limit, offset)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	out := make([]apigen.HistoryEvent, 0, len(rows))
	for _, e := range rows {
		ev := apigen.HistoryEvent{
			Ts: e.At, Type: e.Type,
			MediaItemId: e.MediaItemID, ReleaseTitle: e.ReleaseTitle,
		}
		// Recorded as JSON and handed over as JSON. A detail monarr could
		// not parse is still a detail worth reading.
		var data map[string]any
		if len(e.Data) > 0 && json.Unmarshal([]byte(e.Data), &data) == nil {
			ev.Data = &data
		}
		out = append(out, ev)
	}
	writeJSON(w, http.StatusOK, out)
}
