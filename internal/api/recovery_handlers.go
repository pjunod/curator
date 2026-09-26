package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"net/http"
)

func (s *Server) GetRecoverySettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.deps.Acquisition.RecoverySettings(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	cfg.ConsumerToken = ""
	writeJSON(w, http.StatusOK, map[string]any{"settings": cfg, "advisory": s.deps.Acquisition.RecoveryAdvisory(r.Context())})
}
func (s *Server) PutRecoverySettings(w http.ResponseWriter, r *http.Request) {
	var in apigen.RecoverySettings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cfg := acquisition.RecoverySettings{Enabled: in.Enabled, LocalRoot: in.LocalRoot, RemoteRoot: in.RemoteRoot}
	if in.ConsumerToken != nil {
		cfg.ConsumerToken = *in.ConsumerToken
	}
	if err := s.deps.Acquisition.SetRecoverySettings(r.Context(), cfg); err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
func (s *Server) ListRecoveries(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deps.Acquisition.RunnerRecoveries(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	if rows == nil {
		rows = []acquisition.RunnerRecovery{}
	}
	writeJSON(w, http.StatusOK, rows)
}
func recoveryRequest(r *http.Request) (acquisition.RecoveryRequest, error) {
	var in apigen.RecoveryImportRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return acquisition.RecoveryRequest{}, err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return acquisition.RecoveryRequest{}, err
	}
	var out acquisition.RecoveryRequest
	err = json.Unmarshal(raw, &out)
	return out, err
}

func (s *Server) PreviewRecoveryImport(w http.ResponseWriter, r *http.Request) {
	in, err := recoveryRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	preview, err := s.deps.Acquisition.StartRecoveryPreview(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, preview)
}
func (s *Server) QueueRecoveryImport(w http.ResponseWriter, r *http.Request) {
	in, err := recoveryRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.deps.Acquisition.QueueRecovery(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}
func (s *Server) GetRecoveryImport(w http.ResponseWriter, r *http.Request, id string) {
	job, err := s.deps.Acquisition.RecoveryImport(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "recovery import not found")
		return
	}
	if err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) ListRecoveryImports(w http.ResponseWriter, r *http.Request) {
	rows, err := s.deps.Acquisition.RecoveryImports(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}
func (s *Server) CancelRecoveryImport(w http.ResponseWriter, r *http.Request, id string) {
	if err := s.deps.Acquisition.CancelRecoveryImport(r.Context(), id); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"state": "cancel_pending"})
}

func (s *Server) GetRecoveryPreview(w http.ResponseWriter, r *http.Request, id string) {
	task, err := s.deps.Acquisition.RecoveryPreviewStatus(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "recovery preview not found")
		return
	}
	if err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}
