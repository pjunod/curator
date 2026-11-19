package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
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
		targets = append(targets, sqlite.ActivityIdentity{ID: row.ID, GenerationKey: row.GenerationKey, Epoch: row.Epoch})
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
	type event struct {
		At      string `json:"at"`
		Step    string `json:"step"`
		Message string `json:"message"`
	}
	events := []event{}
	if id < 0 {
		pub, e := s.deps.Store.GetPublication(r.Context(), -id)
		if e != nil {
			s.acqErr(w, e)
			return
		}
		events = append(events, event{pub.DiscoveredAt.UTC().Format(time.RFC3339), "publication recorded", "Historical publication recorded; original transfer identity was not recorded."})
		events = append(events, event{pub.UpdatedAt.UTC().Format(time.RFC3339), "publication outcome", pub.State})
		writeJSON(w, 200, map[string]any{"title": filepath.Base(pub.Path), "summary": "Historical import; transfer identity incomplete.", "events": events})
		return
	}
	dl, e := s.deps.Store.GetDownload(r.Context(), id)
	if e != nil {
		s.acqErr(w, e)
		return
	}
	for _, h := range dl.Handoff {
		events = append(events, event{time.UnixMilli(h.At).UTC().Format(time.RFC3339Nano), strings.ReplaceAll(h.Step, "_", " "), h.Detail})
	}
	rows, e := s.deps.Store.R.QueryContext(r.Context(), `SELECT at,step,message FROM activity_action_events WHERE download_id=? AND generation_key=? ORDER BY at,id`, id, dl.GenerationKey)
	if e != nil {
		s.acqErr(w, e)
		return
	}
	for rows.Next() {
		var at int64
		var step, message string
		if e = rows.Scan(&at, &step, &message); e != nil {
			_ = rows.Close()
			s.acqErr(w, e)
			return
		}
		events = append(events, event{time.UnixMilli(at).UTC().Format(time.RFC3339Nano), step, message})
	}
	e = rows.Err()
	_ = rows.Close()
	if e != nil {
		s.acqErr(w, e)
		return
	}
	// Publication and notification observations retain their own source times.
	// They are explicitly labelled as recorded state, not invented event spans.
	rows, e = s.deps.Store.R.QueryContext(r.Context(), `SELECT updated_at,'publication',state||CASE WHEN error!='' THEN ': '||error ELSE '' END FROM publications WHERE download_id=? UNION ALL SELECT n.updated_at,'Cinema update',n.status||CASE WHEN n.last_error!='' THEN ': '||n.last_error ELSE '' END FROM notifier_deliveries n WHERE n.download_id=? ORDER BY 1`, id, id)
	if e != nil {
		s.acqErr(w, e)
		return
	}
	for rows.Next() {
		var at int64
		var step, message string
		if e = rows.Scan(&at, &step, &message); e != nil {
			_ = rows.Close()
			s.acqErr(w, e)
			return
		}
		events = append(events, event{time.UnixMilli(at).UTC().Format(time.RFC3339Nano), step + " observation", message})
	}
	e = rows.Err()
	_ = rows.Close()
	if e != nil {
		s.acqErr(w, e)
		return
	}
	sort.SliceStable(events, func(i, j int) bool {
		a, _ := time.Parse(time.RFC3339Nano, events[i].At)
		b, _ := time.Parse(time.RFC3339Nano, events[j].At)
		return a.Before(b)
	})
	info, e := s.deps.Acquisition.ActivityActions(r.Context(), dl)
	if e != nil {
		s.acqErr(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"title": dl.ReleaseTitle, "summary": info.Reason, "events": events})
}
