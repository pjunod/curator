package api

import (
	"encoding/json"
	"errors"
	"net/http"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// Offload (ADR 0028): POST /library/{id}/offload/preview, POST and GET
// /library/{id}/offload, GET /offload/{operationId}, POST .../cancel and
// .../dismiss, and POST /library/{id}/reacquire.

func offloadRequest(id int64, in apigen.OffloadRequest) acquisition.OffloadRequest {
	req := acquisition.OffloadRequest{ItemID: id, Scope: string(in.Scope)}
	if in.Season != nil {
		req.Season = *in.Season
	}
	if in.EpisodeId != nil {
		req.EpisodeID = *in.EpisodeId
	}
	if in.RequestId != nil {
		req.RequestID = *in.RequestId
	}
	if in.Fingerprint != nil {
		req.PlanFingerprint = *in.Fingerprint
	}
	return req
}

func (s *Server) offloadErr(w http.ResponseWriter, err error) {
	var blocked *acquisition.OffloadBlockedError
	switch {
	case errors.As(err, &blocked):
		code := "offload_blocked"
		writeJSON(w, http.StatusConflict, apigen.OffloadConflict{Code: &code, Message: blocked.Error(), Blocked: &apigen.OffloadBlocked{
			Reason: apigen.OffloadBlockedReason(blocked.Blocked.Reason), Ref: optStr(blocked.Blocked.Ref),
			Message: blocked.Blocked.Message, Retry: blocked.Blocked.Retry}})
	case errors.Is(err, acquisition.ErrOffloadStalePreview):
		writeCodedError(w, http.StatusConflict, "stale", err.Error())
	case errors.Is(err, sqlite.ErrOffloadRequestConflict):
		writeCodedError(w, http.StatusConflict, "request_conflict", err.Error())
	case errors.Is(err, ports.ErrMaintenance):
		writeCodedError(w, http.StatusServiceUnavailable, "maintenance", err.Error())
	case errors.Is(err, acquisition.ErrImportTargetBusy), errors.Is(err, acquisition.ErrOffloadBusy):
		writeCodedError(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, acquisition.ErrInvalidOffload):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, library.ErrNotFound), errors.Is(err, acquisition.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	default:
		s.acqErr(w, err)
	}
}

func decodeOffload(w http.ResponseWriter, r *http.Request) (apigen.OffloadRequest, bool) {
	var in apigen.OffloadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return in, false
	}
	return in, true
}

// PreviewOffload implements POST /library/{id}/offload/preview.
func (s *Server) PreviewOffload(w http.ResponseWriter, r *http.Request, id int64) {
	in, ok := decodeOffload(w, r)
	if !ok {
		return
	}
	preview, err := s.deps.Acquisition.PreviewOffload(r.Context(), offloadRequest(id, in))
	if err != nil {
		s.offloadErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// StartOffload implements POST /library/{id}/offload.
func (s *Server) StartOffload(w http.ResponseWriter, r *http.Request, id int64) {
	in, ok := decodeOffload(w, r)
	if !ok {
		return
	}
	op, err := s.deps.Acquisition.StartOffload(r.Context(), offloadRequest(id, in))
	if err != nil {
		s.offloadErr(w, err)
		return
	}
	status, err := s.deps.Acquisition.GetOffload(r.Context(), op.ID)
	if err != nil {
		s.offloadErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, status)
}

// GetLibraryOffload implements GET /library/{id}/offload (204 when none).
func (s *Server) GetLibraryOffload(w http.ResponseWriter, r *http.Request, id int64) {
	status, ok, err := s.deps.Acquisition.OffloadForItem(r.Context(), id)
	if err != nil {
		s.offloadErr(w, err)
		return
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// GetOffload implements GET /offload/{operationId}.
func (s *Server) GetOffload(w http.ResponseWriter, r *http.Request, operationID string) {
	status, err := s.deps.Acquisition.GetOffload(r.Context(), operationID)
	if err != nil {
		s.offloadErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// CancelOffload implements POST /offload/{operationId}/cancel.
func (s *Server) CancelOffload(w http.ResponseWriter, r *http.Request, operationID string) {
	if err := s.deps.Acquisition.StopOffload(r.Context(), operationID); err != nil {
		if errors.Is(err, acquisition.ErrInvalidOffload) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.offloadErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// DismissOffload implements POST /offload/{operationId}/dismiss.
func (s *Server) DismissOffload(w http.ResponseWriter, r *http.Request, operationID string) {
	if err := s.deps.Acquisition.DismissOffload(r.Context(), operationID); err != nil {
		if errors.Is(err, acquisition.ErrInvalidOffload) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		s.offloadErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ReacquireLibraryItem implements POST /library/{id}/reacquire.
func (s *Server) ReacquireLibraryItem(w http.ResponseWriter, r *http.Request, id int64) {
	in, ok := decodeOffload(w, r)
	if !ok {
		return
	}
	req := offloadRequest(id, in)
	res, err := s.deps.Acquisition.Reacquire(r.Context(), acquisition.ReacquireRequest{
		ItemID: id, Scope: req.Scope, Season: req.Season, EpisodeID: req.EpisodeID})
	if err != nil {
		s.offloadErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
