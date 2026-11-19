package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pjunod/monarr/internal/infra/sqlite"
)

func activityID() string { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }

func (s *Server) CreateActivitySelection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		View  string `json:"view"`
		Scope string `json:"scope"`
		Q     string `json:"q"`
	}
	if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body); e != nil || body.View != "attention" || (body.Scope != "all" && body.Scope != "imports") {
		writeError(w, 400, "select an attention view and scope")
		return
	}
	page, e := s.deps.Acquisition.Activity(r.Context(), "attention", body.Q, body.Scope == "imports", 1<<30, 0)
	if e != nil {
		s.acqErr(w, e)
		return
	}
	targets := []sqlite.ActivityIdentity{}
	retryable := 0
	for _, row := range page.Rows {
		if row.ID < 1 {
			continue
		}
		dl, e := s.deps.Store.GetDownload(r.Context(), row.ID)
		if e != nil {
			s.acqErr(w, e)
			return
		}
		info, e := s.deps.Acquisition.ActivityActions(r.Context(), dl)
		if e != nil {
			s.acqErr(w, e)
			return
		}
		targets = append(targets, info.Identity)
		for _, a := range info.Actions {
			if a.Kind == "retry" && a.Enabled {
				retryable++
			}
		}
	}
	id := activityID()
	if e = s.deps.Store.SaveActivitySelection(r.Context(), id, targets); e != nil {
		s.acqErr(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "total": len(targets), "retryable": retryable, "targets": targets})
}
func (s *Server) CreateActivityAction(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestID string                    `json:"requestId"`
		Action    string                    `json:"action"`
		Targets   []sqlite.ActivityIdentity `json:"targets"`
		Selection *struct {
			ID         string  `json:"id"`
			ExcludeIDs []int64 `json:"excludeIds"`
		} `json:"selection"`
	}
	if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&body); e != nil {
		writeError(w, 400, "invalid action request")
		return
	}
	if len(body.RequestID) < 8 || len(body.RequestID) > 128 || strings.ContainsAny(body.RequestID, "/\\\x00") || (body.Action != "retry" && body.Action != "delete" && body.Action != "approve") || ((body.Selection == nil) == (len(body.Targets) == 0)) {
		writeError(w, 400, "provide a requestId, action, and either frozen selection or exact targets")
		return
	}
	if prior, found, err := s.deps.Store.ReplayActivityOperation(r.Context(), body.RequestID, body); err != nil {
		writeError(w, 409, err.Error())
		return
	} else if found {
		writeActivityOperation(w, http.StatusAccepted, prior)
		return
	}
	identities := body.Targets
	if body.Selection != nil {
		var e error
		identities, e = s.deps.Store.ActivitySelection(r.Context(), body.Selection.ID)
		if e != nil {
			writeError(w, 409, "selection expired; select the jobs again")
			return
		}
		exclude := map[int64]bool{}
		for _, id := range body.Selection.ExcludeIDs {
			exclude[id] = true
		}
		filtered := []sqlite.ActivityIdentity{}
		for _, i := range identities {
			if !exclude[i.ID] {
				filtered = append(filtered, i)
			}
		}
		identities = filtered
	}
	targets := make([]sqlite.ActivityTarget, 0, len(identities))
	seen := map[int64]bool{}
	for _, i := range identities {
		if i.ID < 1 || i.GenerationKey == "" || i.Epoch == "" || seen[i.ID] {
			writeError(w, 400, "each target needs a unique positive ID, generation and epoch")
			return
		}
		seen[i.ID] = true
		targets = append(targets, sqlite.ActivityTarget{ActivityIdentity: i})
	}
	if len(targets) == 0 {
		writeError(w, 400, "select at least one job")
		return
	}
	o, e := s.deps.Store.CreateActivityOperation(r.Context(), body.RequestID, body.Action, body, targets)
	if e != nil {
		writeError(w, 409, e.Error())
		return
	}
	writeActivityOperation(w, http.StatusAccepted, o)
}
func writeActivityOperation(w http.ResponseWriter, status int, o sqlite.ActivityOperation) {
	for i := range o.Targets {
		o.Targets[i].OwnerPayload = ""
	}
	writeJSON(w, status, o)
}
func (s *Server) GetActivityOperation(w http.ResponseWriter, r *http.Request, id string) {
	o, e := s.deps.Store.ActivityOperation(r.Context(), id)
	if e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			writeError(w, 404, "operation not found")
		} else {
			s.acqErr(w, e)
		}
		return
	}
	writeActivityOperation(w, 200, o)
}
func (s *Server) GetActivityJobLog(w http.ResponseWriter, r *http.Request, id int64) {
	dl, e := s.deps.Store.GetDownload(r.Context(), id)
	if e != nil {
		s.acqErr(w, e)
		return
	}
	type event struct {
		At      string `json:"at"`
		Step    string `json:"step"`
		Message string `json:"message"`
	}
	events := []event{}
	for _, h := range dl.Handoff {
		events = append(events, event{time.UnixMilli(h.At).UTC().Format(time.RFC3339), strings.ReplaceAll(h.Step, "_", " "), h.Detail})
	}
	rows, e := s.deps.Store.R.QueryContext(r.Context(), `SELECT created_at,action,targets FROM activity_operations WHERE EXISTS(SELECT 1 FROM json_each(targets) t WHERE json_extract(t.value,'$.id')=? AND json_extract(t.value,'$.generationKey')=?) ORDER BY created_at`, id, dl.GenerationKey)
	if e != nil {
		s.acqErr(w, e)
		return
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var at int64
		var action, raw string
		if e = rows.Scan(&at, &action, &raw); e != nil {
			s.acqErr(w, e)
			return
		}
		var targets []sqlite.ActivityTarget
		if e = json.Unmarshal([]byte(raw), &targets); e != nil {
			s.acqErr(w, e)
			return
		}
		for _, t := range targets {
			if t.ID == id && t.GenerationKey == dl.GenerationKey {
				events = append(events, event{time.UnixMilli(at).UTC().Format(time.RFC3339), action, t.Message})
			}
		}
	}
	if e = rows.Err(); e != nil {
		s.acqErr(w, e)
		return
	}
	info, e := s.deps.Acquisition.ActivityActions(r.Context(), dl)
	if e != nil {
		s.acqErr(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"title": dl.ReleaseTitle, "summary": info.Reason, "events": events})
}
