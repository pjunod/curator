package api

import (
	"net/http"

	"github.com/pjunod/monarr/internal/app/acquisition"
)

func (s *Server) GetStorageObservations(w http.ResponseWriter, r *http.Request) {
	if s.deps.Acquisition == nil {
		writeJSON(w, http.StatusOK, acquisition.StorageObservations{Roots: []acquisition.StorageObservation{}})
		return
	}
	observations, err := s.deps.Acquisition.StorageObservations(r.Context())
	if err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, observations)
}
