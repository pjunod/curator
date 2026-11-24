package api

import (
	"encoding/json"
	"io"
	"net/http"
)

func (s *Server) PreviewLibraryVolume(w http.ResponseWriter, r *http.Request, id int64) {
	value, err := s.deps.Library.PreviewLibraryVolume(r.Context(), id)
	if err != nil {
		s.libraryErr(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, value)
}
func (s *Server) ConfirmLibraryVolume(w http.ResponseWriter, r *http.Request, id int64) {
	var body struct {
		Fingerprint  string `json:"fingerprint"`
		Acknowledged bool   `json:"acknowledged"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || decoder.Decode(new(any)) != io.EOF || len(body.Fingerprint) != 64 {
		writeError(w, 400, "invalid volume confirmation")
		return
	}
	if err := s.deps.Library.ConfirmLibraryVolume(r.Context(), id, body.Fingerprint, body.Acknowledged); err != nil {
		writeError(w, 409, err.Error())
		return
	}
	w.WriteHeader(204)
}
