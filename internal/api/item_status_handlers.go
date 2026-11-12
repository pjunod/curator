package api

import (
	"errors"
	"net/http"

	"github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
)

// GetLibraryItemStatus implements GET /library/{id}/status: the item page's
// status panel, from search to cinema.
func (s *Server) GetLibraryItemStatus(w http.ResponseWriter, r *http.Request, id int64, params apigen.GetLibraryItemStatusParams) {
	season := -1
	if params.Season != nil {
		season = *params.Season
	}
	out, err := s.deps.Acquisition.ItemStatus(r.Context(), id, season)
	if err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ConfirmLibraryItemInCinema implements POST /library/{id}/status/confirm:
// a scan request with the item's files as expected files.
func (s *Server) ConfirmLibraryItemInCinema(w http.ResponseWriter, r *http.Request, id int64, params apigen.ConfirmLibraryItemInCinemaParams) {
	var episode int64
	if params.Episode != nil {
		episode = *params.Episode
	}
	n, err := s.deps.Acquisition.ConfirmInCinema(r.Context(), id, episode)
	if errors.Is(err, acquisition.ErrNothingToConfirm) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.acqErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": n})
}
