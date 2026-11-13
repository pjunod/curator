// Package api serves Monarr's native /api/v1 (OpenAPI-first, see
// openapi.yaml), the SSE event stream, and the embedded web UI.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/app/acquisition"
	"github.com/pjunod/monarr/internal/app/discover"
	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/app/household"
	"github.com/pjunod/monarr/internal/app/library"
	"github.com/pjunod/monarr/internal/app/maintenance"
	"github.com/pjunod/monarr/internal/app/pipeline"
	"github.com/pjunod/monarr/internal/app/recommendation"
	"github.com/pjunod/monarr/internal/domain"
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

// ScanTaskName is the periodic scheduler task that enqueues library scans.
const ScanTaskName = "library.reconcile"

// Deps is everything the server needs, wired in cmd/monarr.
type Deps struct {
	// TrustedProxies may assert HTTPS only through an overwritten X-Forwarded-Proto.
	TrustedProxies []netip.Prefix
	// Context owns background work; request cancellation remains separate so
	// ordinary handlers retain their HTTP shutdown grace.
	Context     context.Context
	Maintenance *maintenance.Coordinator
	Log         *slog.Logger
	Bus         *bus.Bus
	Health      *health.Registry
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
	Household   *household.Service
	// Pipeline serves the seam timings and the overdue count; nil serves an
	// empty summary rather than failing.
	Pipeline *pipeline.Timings
	// Discover serves the browse rows (ADR 0015); nil returns an empty
	// catalogue rather than failing, so a test server needs no provider.
	LibraryDiscovery    *discover.LibraryService
	Discover            *discover.Service
	Recommendations     *recommendation.Service
	RecommendationModel interface {
		Enable(bool)
		Detail() (string, string)
		Install() error
		Uninstall() error
	}
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
	authMu        sync.Mutex
	loginFailures householdLoginFailures
	deps          Deps

	streamsCtx         context.Context
	cancelStreams      context.CancelFunc
	backgroundCtx      context.Context
	cancelBackground   context.CancelFunc
	requestsCtx        context.Context
	cancelRequests     context.CancelFunc
	lifecycleMu        sync.Mutex
	closing            bool
	handlers           sync.WaitGroup
	background         sync.WaitGroup
	streamWriteTimeout time.Duration
}

var _ apigen.ServerInterface = (*Server)(nil)

// New returns a Server. Deps.Log must be non-nil.
func New(deps Deps) *Server {
	if deps.Maintenance == nil {
		deps.Maintenance = maintenance.New()
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.StartedAt.IsZero() {
		deps.StartedAt = time.Now()
	}
	parent := deps.Context
	if parent == nil {
		parent = context.Background()
	}
	streamsCtx, cancelStreams := context.WithCancel(context.Background())
	backgroundCtx, cancelBackground := context.WithCancel(parent)
	requestsCtx, cancelRequests := context.WithCancel(context.Background())
	return &Server{loginFailures: householdLoginFailures{entries: map[string][]time.Time{}}, deps: deps, streamsCtx: streamsCtx, cancelStreams: cancelStreams,
		backgroundCtx: backgroundCtx, cancelBackground: cancelBackground,
		requestsCtx: requestsCtx, cancelRequests: cancelRequests,
		streamWriteTimeout: 5 * time.Second}
}

// Handler returns the root handler: /api/v1/* plus the embedded SPA on /.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	apiHandler := apigen.HandlerWithOptions(s, apigen.StdHTTPServerOptions{
		BaseURL:     "/api/v1",
		Middlewares: []apigen.MiddlewareFunc{s.admitMutation},
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.URL.Path == "/api/v1/metadata/preview" {
				writeCodedError(w, http.StatusBadRequest, "invalid_external_id", err.Error())
				return
			}
			writeError(w, http.StatusBadRequest, err.Error())
		},
	})
	mux.Handle("/api/v1/", s.noteCallers(s.authGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiHandler.ServeHTTP(w, r.WithContext(domain.WithInteractiveJob(r.Context())))
	}))))
	mux.Handle("/metrics", s.authGuard(s.metricsHandler()))
	// The compat personalities are machine callers by definition — Prowlarr,
	// Jellyseerr, Bazarr — so they belong on the same list.
	if s.deps.CompatSonarr != nil {
		mux.Handle("/sonarr/", s.noteCallers(s.authGuard(http.StripPrefix("/sonarr", s.deps.CompatSonarr))))
	}
	if s.deps.CompatRadarr != nil {
		mux.Handle("/radarr/", s.noteCallers(s.authGuard(http.StripPrefix("/radarr", s.deps.CompatRadarr))))
	}
	mux.Handle("/", s.spaHandler())
	return s.trackRequests(s.recoverer(s.requestLogger(mux)))
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

// Unwrap preserves ResponseController deadline support through logging.
func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }

// FlushError preserves flush failures, which SSE must observe to end a stream.
func (sr *statusRecorder) FlushError() error {
	return http.NewResponseController(sr.ResponseWriter).Flush()
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
