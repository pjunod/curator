// Package compat implements the Sonarr/Radarr v3 API personalities
// (ADR 0003): translation-only HTTP surfaces mounted at /sonarr and
// /radarr so the ecosystem (Jellyseerr, Prowlarr, Bazarr) can talk to
// Monarr as if it were the app it already knows. Scope is exactly what
// those consumers call (blueprint §6) — unknown requests are logged to
// guide expansion, never guessed at.
//
// Dependency rule (enforced by internal/arch_test.go): compat imports the
// app layer, domain, and ports — never infra, adapters, or api. Storage
// access goes through the narrow Store interface, satisfied by *sqlite.DB
// and wired in cmd/monarr.
package compat

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/monarr-media/monarr/internal/app/library"
	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/domain/quality"
	"github.com/monarr-media/monarr/internal/ports"
)

// Plausible upstream version strings: new enough that consumers enable
// their v3 code paths, old enough to be believable.
const (
	SonarrVersion = "4.0.10.2544"
	RadarrVersion = "5.14.0.9383"
)

// Store is the storage the personalities need, satisfied by *sqlite.DB.
type Store interface {
	ListProfiles(ctx context.Context) ([]quality.Profile, error)
	FileQualities(ctx context.Context, itemID int64) (map[int64]quality.Quality, error)
	ListIndexers(ctx context.Context) ([]ports.IndexerConfig, error)
	AddIndexer(ctx context.Context, c ports.IndexerConfig) (int64, error)
	GetIndexer(ctx context.Context, id int64) (ports.IndexerConfig, error)
	DeleteIndexer(ctx context.Context, id int64) error
}

// Deps wires a personality.
type Deps struct {
	Log     *slog.Logger
	Library *library.Service
	Store   Store
	// APIKey returns the expected X-Api-Key at call time ("" disables auth —
	// never the case in production wiring).
	APIKey func(ctx context.Context) string
	// ResolveTVDB maps a TVDB id to a hydrated series (Sonarr adds/lookups).
	ResolveTVDB func(ctx context.Context, tvdbID int64) (domain.MediaItem, error)
	// TriggerSearch kicks the backlog search when a consumer posts a
	// *Search command. Optional.
	TriggerSearch func()
}

// Personality is one mounted translation surface.
type Personality struct {
	deps    Deps
	app     string // "Sonarr" | "Radarr"
	version string

	unknown atomic.Int64 // count of unrecognized requests (logged)
}

// NewSonarr returns the /sonarr personality.
func NewSonarr(deps Deps) *Personality {
	return newPersonality(deps, "Sonarr", SonarrVersion)
}

// NewRadarr returns the /radarr personality.
func NewRadarr(deps Deps) *Personality {
	return newPersonality(deps, "Radarr", RadarrVersion)
}

func newPersonality(deps Deps, app, version string) *Personality {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	return &Personality{deps: deps, app: app, version: version}
}

// Handler serves /api/v3/* (mount under /sonarr or /radarr with
// http.StripPrefix). Routes are matched case-insensitively — real v3
// consumers use inconsistent casing.
func (p *Personality) Handler() http.Handler {
	mux := http.NewServeMux()

	// Shared surface.
	mux.HandleFunc("GET /api/v3/system/status", p.systemStatus)
	mux.HandleFunc("GET /api/v3/health", listOf[any]())
	mux.HandleFunc("GET /api/v3/tag", listOf[any]())
	mux.HandleFunc("GET /api/v3/qualityprofile", p.qualityProfiles)
	mux.HandleFunc("GET /api/v3/rootfolder", p.rootFolders)
	mux.HandleFunc("GET /api/v3/queue", p.pagedEmpty)
	mux.HandleFunc("GET /api/v3/history", p.pagedEmpty)
	mux.HandleFunc("POST /api/v3/command", p.command)
	p.mountIndexers(mux)

	if p.app == "Sonarr" {
		mux.HandleFunc("GET /api/v3/languageprofile", p.languageProfiles)
		p.mountSonarr(mux)
	} else {
		p.mountRadarr(mux)
	}

	// Everything else: log it so the shim can grow deliberately (§6).
	mux.HandleFunc("/", p.logUnknown)

	return p.auth(lowercasePath(mux))
}

// ---- middleware ----

// lowercasePath folds the URL path so route matching is case-insensitive.
func lowercasePath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.ToLower(r.URL.Path)
		next.ServeHTTP(w, r2)
	})
}

// auth enforces X-Api-Key (header or ?apikey=), like the real apps.
func (p *Personality) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := ""
		if p.deps.APIKey != nil {
			want = p.deps.APIKey(r.Context())
		}
		if want != "" {
			got := r.Header.Get("X-Api-Key")
			if got == "" {
				got = r.URL.Query().Get("apikey")
			}
			if got != want {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (p *Personality) logUnknown(w http.ResponseWriter, r *http.Request) {
	n := p.unknown.Add(1)
	p.deps.Log.Warn("compat: unknown v3 request — extend the shim if a consumer needs it",
		"personality", p.app, "method", r.Method, "path", r.URL.Path,
		"query", r.URL.RawQuery, "totalUnknown", n)
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "not implemented by monarr compat shim"})
}

// ---- shared handlers ----

func (p *Personality) systemStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"appName":           p.app,
		"instanceName":      "Monarr (" + p.app + " personality)",
		"version":           p.version,
		"buildTime":         "2026-01-01T00:00:00Z",
		"isDebug":           false,
		"isProduction":      true,
		"isAdmin":           true,
		"isUserInteractive": false,
		"urlBase":           "",
		"runtimeVersion":    "8.0.0",
		"authentication":    "apikey",
	})
}

func (p *Personality) qualityProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := p.deps.Store.ListProfiles(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(profiles))
	for _, pr := range profiles {
		// Monarr profiles are targets now (ADR 0014); *arr consumers expect an
		// allowed list plus a cutoff and have no concept of one. Synthesizing
		// the list from the target -- via the same Acceptable predicate the
		// decision engine uses -- keeps the translation honest: the shim
		// cannot describe a profile that does not behave as described.
		// Ids and names pass through untouched (ADR 0003: translation only).
		allowed := pr.AllowedUnder()
		items := make([]map[string]any, 0, len(allowed))
		cutoff := 1
		for i, q := range allowed {
			items = append(items, map[string]any{
				"quality": map[string]any{"id": i + 1, "name": q.Display()},
				"allowed": true,
			})
			if q == pr.Target {
				cutoff = i + 1
			}
		}
		out = append(out, map[string]any{
			"id": pr.ID, "name": pr.Name, "upgradeAllowed": pr.UpgradesAllowed,
			"cutoff": cutoff, "items": items,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (p *Personality) languageProfiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []map[string]any{
		{"id": 1, "name": "English", "upgradeAllowed": false},
	})
}

// kind is the media kind this personality speaks for. Sonarr is series and
// Radarr is movies — the discriminator the *arrs got for free by being
// separate applications, and which Monarr has to state (ADR 0009).
func (p *Personality) kind() domain.MediaKind {
	if p.app == "Sonarr" {
		return domain.KindSeries
	}
	return domain.KindMovie
}

// rootFolders lists only roots this personality can legitimately write to:
// its own kind, plus mixed roots, which restrict nothing. Before ADR 0009
// every root was offered to both personalities, so a Radarr client was shown
// the TV root as a valid movie destination.
func (p *Personality) rootFolders(w http.ResponseWriter, r *http.Request) {
	roots, err := p.deps.Library.ListRootFolders(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(roots))
	for _, rf := range roots {
		if !rf.Kind.Accepts(p.kind()) {
			continue
		}
		out = append(out, map[string]any{
			"id": rf.ID, "path": rf.Path, "accessible": rf.Accessible,
			"freeSpace": rf.FreeBytes, "unmappedFolders": []any{},
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (p *Personality) pagedEmpty(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"page": 1, "pageSize": 10, "sortKey": "date", "sortDirection": "descending",
		"totalRecords": 0, "records": []any{},
	})
}

// command acknowledges v3 commands; search commands kick the backlog.
func (p *Personality) command(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.Contains(strings.ToLower(body.Name), "search") && p.deps.TriggerSearch != nil {
		p.deps.TriggerSearch()
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": 1, "name": body.Name, "commandName": body.Name,
		"status": "completed", "queued": "2026-01-01T00:00:00Z",
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// listOf returns a handler serving a static empty JSON array.
func listOf[T any]() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []T{})
	}
}

var reSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(title string) string {
	s := reSlug.ReplaceAllString(strings.ToLower(title), "-")
	return strings.Trim(s, "-")
}

func rootPathOf(item domain.MediaItem, roots []library.RootFolderInfo) string {
	for _, rf := range roots {
		if rf.ID == item.RootFolderID {
			return rf.Path
		}
	}
	return ""
}
