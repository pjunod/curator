// Package api serves Monarr's native /api/v1 (OpenAPI-first, see
// openapi.yaml), the SSE event stream, and the embedded web UI.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/discover"
	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/scheduler"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"github.com/pjunod/monarr/internal/ports"
)

// SchemaVersioner reports the database schema version; satisfied by
// *sqlite.DB without coupling the API layer to the storage package.
type SchemaVersioner interface {
	SchemaVersion(ctx context.Context) (int64, error)
}

// SettingsStore reads and writes settings key-values; satisfied by
// *sqlite.DB (app_meta).
type SettingsStore interface {
	GetMeta(ctx context.Context, key string) (string, error)
	SetMeta(ctx context.Context, key, value string) error
}

// ScanTaskName is the scheduler task POST /library/scan triggers.
const ScanTaskName = "library.reconcile"

// Deps is everything the server needs, wired in cmd/monarr.
type Deps struct {
	Log    *slog.Logger
	Bus    *bus.Bus
	Health *health.Registry
	// Connections is the monitor behind the Connections panel; nil renders
	// the panel empty rather than failing.
	Connections *health.ConnectionMonitor
	// Callers remembers which other applications have called in — the
	// inbound half of "are these apps talking".
	Callers     *CallerRegistry
	Scheduler   *scheduler.Scheduler
	DB          SchemaVersioner
	Library     *library.Service
	Acquisition *acquisition.Service
	// Discover serves the browse rows (ADR 0015); nil returns an empty
	// catalogue rather than failing, so a test server needs no provider.
	Discover *discover.Service
	// Store is the config storage for profiles/indexers/clients.
	Store *sqlite.DB
	// Factories used by the /test endpoints to probe unsaved configs.
	IndexerFactory  acquisition.IndexerFactory
	ClientFactory   acquisition.ClientFactory
	NotifierFactory func(ports.NotifierConfig) ports.Notifier
	Settings        SettingsStore
	// Compat personalities (ADR 0003), mounted at /sonarr and /radarr.
	// Built in cmd/monarr from internal/compat; nil disables them.
	CompatSonarr http.Handler
	CompatRadarr http.Handler
	Version      string
	Commit       string
	DataDir      string
	StartedAt    time.Time
}

// Server implements apigen.ServerInterface.
type Server struct {
	deps Deps
}

var _ apigen.ServerInterface = (*Server)(nil)

// New returns a Server. Deps.Log must be non-nil.
func New(deps Deps) *Server {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.StartedAt.IsZero() {
		deps.StartedAt = time.Now()
	}
	return &Server{deps: deps}
}

// Handler returns the root handler: /api/v1/* plus the embedded SPA on /.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	apiHandler := apigen.HandlerWithOptions(s, apigen.StdHTTPServerOptions{
		BaseURL: "/api/v1",
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			writeError(w, http.StatusBadRequest, err.Error())
		},
	})
	mux.Handle("/api/v1/", s.noteCallers(s.authGuard(apiHandler)))
	mux.Handle("/metrics", s.metricsHandler())
	// The compat personalities are machine callers by definition — Prowlarr,
	// Jellyseerr, Bazarr — so they belong on the same list.
	if s.deps.CompatSonarr != nil {
		mux.Handle("/sonarr/", s.noteCallers(http.StripPrefix("/sonarr", s.deps.CompatSonarr)))
	}
	if s.deps.CompatRadarr != nil {
		mux.Handle("/radarr/", s.noteCallers(http.StripPrefix("/radarr", s.deps.CompatRadarr)))
	}
	mux.Handle("/", s.spaHandler())
	return s.recoverer(s.requestLogger(mux))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apigen.Error{Message: msg})
}

func writeCodedError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apigen.Error{Code: &code, Message: msg})
}

// statusRecorder captures the response code for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Flush passes through so SSE works behind the recorder.
func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.deps.Log.Debug("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Microsecond),
		)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				s.deps.Log.Error("http handler panicked", "path", r.URL.Path, "panic", p)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
