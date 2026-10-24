package api

import (
	"encoding/json"
	"net/http"

	apigen "github.com/pjunod/monarr/internal/api/gen"

	"github.com/pjunod/monarr/internal/app/acquisition"
)

func (s *Server) GetCompletedInventory(w http.ResponseWriter, r *http.Request) {
	if s.deps.Acquisition == nil {
		writeJSON(w, http.StatusOK, acquisition.CompletedInventory{Roots: []acquisition.CompletedRoot{}})
		return
	}
	inventory, err := s.deps.Acquisition.CompletedInventory(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, inventory)
}

func (s *Server) DeleteCompletedEntry(w http.ResponseWriter, r *http.Request) {
	if s.deps.Acquisition == nil {
		writeError(w, http.StatusServiceUnavailable, "download storage unavailable")
		return
	}
	var body apigen.DeleteCompletedEntryJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if body.Files != nil {
		selected := make([]acquisition.CompletedFileSelection, 0, len(*body.Files))
		for _, file := range *body.Files {
			selected = append(selected, acquisition.CompletedFileSelection{Path: file.Path, SnapshotIdentity: file.SnapshotIdentity})
		}
		var err error
		if body.ReviewLegacy != nil && *body.ReviewLegacy {
			err = s.deps.Acquisition.DeleteSelectedLegacyFiles(r.Context(), body.Path, body.Fingerprint, selected)
		} else {
			err = s.deps.Acquisition.DeleteSelectedDeliveryFiles(r.Context(), body.Path, body.Fingerprint, selected)
		}
		if err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := s.deps.Acquisition.DeleteCompletedEntry(r.Context(), body.Path, body.Fingerprint); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
