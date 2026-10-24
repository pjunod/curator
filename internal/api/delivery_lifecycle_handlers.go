package api

import (
	"encoding/json"
	"net/http"

	apigen "github.com/pjunod/monarr/internal/api/gen"
)

func (s *Server) ListDeliveryLifecycle(w http.ResponseWriter, r *http.Request, params apigen.ListDeliveryLifecycleParams) {
	if s.deps.Acquisition == nil {
		writeError(w, http.StatusServiceUnavailable, "download lifecycle is unavailable")
		return
	}
	limit := 50
	if params.Limit != nil {
		limit = *params.Limit
	}
	before := int64(0)
	if params.BeforeId != nil {
		before = *params.BeforeId
	}
	downloadID := int64(0)
	if params.DownloadId != nil {
		downloadID = *params.DownloadId
	}
	page, err := s.deps.Acquisition.DeliveryLifecycle(r.Context(), before, downloadID, limit)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) GetDeliveryLifecycleReceipt(w http.ResponseWriter, r *http.Request, receiptId int64) {
	if s.deps.Acquisition == nil {
		writeError(w, http.StatusServiceUnavailable, "download lifecycle is unavailable")
		return
	}
	item, err := s.deps.Acquisition.DeliveryLifecycleByID(r.Context(), receiptId)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) RetryDeliveryHandoff(w http.ResponseWriter, r *http.Request, receiptId int64) {
	if s.deps.Acquisition == nil {
		writeError(w, http.StatusServiceUnavailable, "download lifecycle is unavailable")
		return
	}
	if err := s.deps.Acquisition.RetryDeliveryHandoff(r.Context(), receiptId); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) RetryRunnerDeliveryCleanup(w http.ResponseWriter, r *http.Request, receiptId int64) {
	if s.deps.Acquisition == nil {
		writeError(w, http.StatusServiceUnavailable, "download lifecycle is unavailable")
		return
	}
	if err := s.deps.Acquisition.RetryRunnerSourceCleanup(r.Context(), receiptId); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) RetryCuratorDeliveryCleanup(w http.ResponseWriter, r *http.Request, receiptId int64) {
	if s.deps.Acquisition == nil {
		writeError(w, http.StatusServiceUnavailable, "download lifecycle is unavailable")
		return
	}
	if err := s.deps.Acquisition.RetryCuratorDeliveryCleanup(r.Context(), receiptId); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) ResolveDeliveryImport(w http.ResponseWriter, r *http.Request, receiptId int64) {
	if s.deps.Acquisition == nil {
		writeError(w, http.StatusServiceUnavailable, "download lifecycle is unavailable")
		return
	}
	var body apigen.ResolveDeliveryImportJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Paths) == 0 {
		writeError(w, http.StatusBadRequest, "select one or more accepted manifest paths")
		return
	}
	if err := s.deps.Acquisition.QueueDeliveryImport(r.Context(), receiptId, body.Paths); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
