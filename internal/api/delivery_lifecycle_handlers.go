package api

import (
	"encoding/json"
	"errors"
	"net/http"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/infra/sqlite"
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
		s.deliveryLifecycleError(w, err, http.StatusNotFound, "delivery receipt not found")
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
		s.deliveryLifecycleError(w, err, http.StatusConflict, "delivery receipt not found")
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
		s.deliveryLifecycleError(w, err, http.StatusConflict, "delivery receipt not found")
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
		s.deliveryLifecycleError(w, err, http.StatusConflict, "delivery receipt not found")
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
		s.deliveryLifecycleError(w, err, http.StatusConflict, "delivery receipt not found")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) AbandonDeliveryDeleteRequest(w http.ResponseWriter, r *http.Request, receiptId int64, batchId string) {
	if s.deps.Acquisition == nil {
		writeError(w, http.StatusServiceUnavailable, "download lifecycle is unavailable")
		return
	}
	if err := s.deps.Acquisition.AbandonDeliveryDeleteRequest(r.Context(), receiptId, batchId); err != nil {
		s.deliveryLifecycleError(w, err, http.StatusNotFound, "delivery receipt or deletion request not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deliveryLifecycleError maps a lifecycle failure to each route's documented
// status. A missing receipt is 404 where the route documents it (the receipt
// read and the exact deletion-request abandon); the retry and import routes
// document only 409, under which a vanished receipt is one more reason the
// work cannot proceed.
func (s *Server) deliveryLifecycleError(w http.ResponseWriter, err error, missingStatus int, missingMessage string) {
	if errors.Is(err, sqlite.ErrNotFound) {
		writeError(w, missingStatus, missingMessage)
		return
	}
	var databaseError interface{ Code() int }
	if errors.As(err, &databaseError) {
		s.deps.Log.Error("delivery lifecycle database error", "err", err)
		writeError(w, http.StatusInternalServerError, "download lifecycle operation could not be saved")
		return
	}
	writeError(w, http.StatusConflict, err.Error())
}
