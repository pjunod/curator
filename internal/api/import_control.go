package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

type importIdentity struct {
	GenerationKey string `json:"generationKey"`
	Epoch         string `json:"epoch"`
}

func (i importIdentity) key(id int64) (sqlite.ImportAttemptKey, error) {
	epoch, err := strconv.ParseInt(i.Epoch, 10, 64)
	if err != nil || epoch < 0 || i.GenerationKey == "" {
		return sqlite.ImportAttemptKey{}, fmt.Errorf("current generationKey and decimal epoch required")
	}
	return sqlite.ImportAttemptKey{DownloadID: id, GenerationKey: i.GenerationKey, Epoch: epoch}, nil
}
func importStatus(a sqlite.ImportAttempt) apigen.ImportAttemptStatus {
	return apigen.ImportAttemptStatus{GenerationKey: a.GenerationKey, Epoch: strconv.FormatInt(a.Epoch, 10), Outcome: apigen.ImportAttemptStatusOutcome(a.Outcome), PendingPublications: a.PendingPublications, RetainedPublications: a.RetainedPublications, HeldStages: a.HeldStages}
}
func (s *Server) importControlErr(w http.ResponseWriter, err error) {
	code := http.StatusConflict
	if errors.Is(err, sqlite.ErrNotFound) {
		code = 404
	}
	writeError(w, code, err.Error())
}
func (s *Server) GetImportReview(w http.ResponseWriter, r *http.Request, id int64) {
	dl, err := s.deps.Store.GetDownload(r.Context(), id)
	if err != nil {
		s.importControlErr(w, err)
		return
	}
	// Read-only: pre-control rows have the implicit initial epoch zero.
	a, err := s.deps.Store.GetImportAttempt(r.Context(), id, dl.GenerationKey)
	if errors.Is(err, sql.ErrNoRows) {
		a = sqlite.ImportAttempt{ImportAttemptKey: sqlite.ImportAttemptKey{DownloadID: id, GenerationKey: dl.GenerationKey}}
		err = nil
	}
	if err != nil {
		s.importControlErr(w, err)
		return
	}
	pubs, err := s.deps.Store.ImportPublications(r.Context(), id, dl.GenerationKey)
	if err != nil {
		s.importControlErr(w, err)
		return
	}
	custody, err := s.deps.Store.ImportStageCustody(r.Context(), id, dl.GenerationKey)
	if err != nil {
		s.importControlErr(w, err)
		return
	}
	// Avoid JSON numeric epochs even for evidence entries.
	evidence := make([]map[string]any, 0, len(pubs))
	for _, p := range pubs {
		evidence = append(evidence, map[string]any{"id": p.ID, "epoch": strconv.FormatInt(p.Epoch, 10), "source": p.Source, "target": p.Target, "outcome": p.Outcome, "resolution": p.Resolution})
	}
	writeJSON(w, 200, map[string]any{"attempt": importStatus(a), "writerActive": s.deps.Acquisition.ImportRunning(id), "publications": evidence, "stages": custody})
}
func (s *Server) ResolveImportReview(w http.ResponseWriter, r *http.Request, id int64) {
	var body struct {
		importIdentity
		Action       string   `json:"action"`
		IDs          []string `json:"ids"`
		Acknowledged bool     `json:"acknowledged"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&body); err != nil {
		writeError(w, 400, "invalid review request")
		return
	}
	key, err := body.key(id)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	a, err := s.deps.Acquisition.ReviewImportCancellation(r.Context(), key, body.Action, body.IDs, body.Acknowledged)
	if err != nil {
		s.importControlErr(w, err)
		return
	}
	writeJSON(w, 200, importStatus(a))
}
