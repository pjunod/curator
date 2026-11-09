package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/pjunod/monarr/internal/app/maintenance"
)

// Explicit read-only operations. Other GETs may persist previews, provider
// budgets, discovery state or candidate tokens and participate in the barrier.
var maintenanceReads = map[string]bool{
	"GET /api/v1/imports":                                  true,
	"GET /api/v1/system/status":                            true,
	"GET /api/v1/system/tasks":                             true,
	"GET /api/v1/health":                                   true,
	"GET /api/v1/library":                                  true,
	"GET /api/v1/library/{id}":                             true,
	"GET /api/v1/library/scan":                             true,
	"GET /api/v1/library/scan/report":                      true,
	"GET /api/v1/library/review":                           true,
	"GET /api/v1/library/scan/ignored":                     true,
	"GET /api/v1/filesystem":                               true,
	"GET /api/v1/rootfolders":                              true,
	"GET /api/v1/settings":                                 true,
	"GET /api/v1/profiles":                                 true,
	"GET /api/v1/languages":                                true,
	"GET /api/v1/indexers":                                 true,
	"GET /api/v1/downloadclients":                          true,
	"GET /api/v1/queue/{id}/import-review":                 true,
	"GET /api/v1/queue":                                    true,
	"GET /api/v1/queue/summary":                            true,
	"GET /api/v1/history":                                  true,
	"GET /api/v1/import/scan":                              true,
	"GET /api/v1/import/default-path":                      true,
	"GET /api/v1/import/recovery/settings":                 true,
	"GET /api/v1/import/recovery/previews/{id}":            true,
	"GET /api/v1/import/recovery/jobs":                     true,
	"GET /api/v1/import/recovery/{id}":                     true,
	"GET /api/v1/customformats":                            true,
	"GET /api/v1/importlists":                              true,
	"GET /api/v1/replacement-observations/{observationId}": true,
	"GET /api/v1/replacement-operations/{operationId}":     true,
	"GET /api/v1/notifiers":                                true,
	"GET /api/v1/notifiers/{id}/deliveries":                true,
	"GET /api/v1/system/transfers":                         true,
	"GET /api/v1/system/pipeline":                          true,
	"GET /api/v1/system/connections":                       true,
	"GET /api/v1/system/backups":                           true,
	"GET /api/v1/calendar":                                 true,
	"GET /api/v1/wanted":                                   true,
	"GET /api/v1/wanted/searches/{runId}":                  true,
	"GET /api/v1/wanted/searches/{runId}/results":          true,
	"GET /api/v1/blocklist":                                true,
	"GET /api/v1/events":                                   true,
	"GET /api/v1/system/deployment-drain":                  true,
	"POST /api/v1/system/deployment-drain":                 true,
	"DELETE /api/v1/system/deployment-drain":               true,
	"POST /api/v1/auth/login":                              true,
	"POST /api/v1/auth/logout":                             true,
}

func (s *Server) admitMutation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if maintenanceReads[r.Pattern] {
			next.ServeHTTP(w, r)
			return
		}
		ctx, release, err := s.deps.Maintenance.TryEnter(r.Context(), "HTTP "+r.Pattern)
		if err != nil {
			writeCodedError(w, http.StatusServiceUnavailable, "deployment_draining", err.Error())
			return
		}
		defer release()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) GetDeploymentDrain(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.deps.Maintenance.Status())
}

func (s *Server) BeginDeploymentDrain(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OwnerToken string `json:"ownerToken"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid maintenance owner request")
		return
	}
	status, err := s.deps.Maintenance.Start(body.OwnerToken)
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, maintenance.ErrToken) {
			code = http.StatusBadRequest
		}
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) AbortDeploymentDrain(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OwnerToken string `json:"ownerToken"`
		Generation string `json:"generation"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Generation == "" {
		writeError(w, http.StatusBadRequest, "invalid maintenance abort request")
		return
	}
	status, err := s.deps.Maintenance.Abort(body.OwnerToken, body.Generation)
	if err != nil {
		code := http.StatusConflict
		if errors.Is(err, maintenance.ErrToken) {
			code = http.StatusBadRequest
		}
		writeError(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, status)
}
