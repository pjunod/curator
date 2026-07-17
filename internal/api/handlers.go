package api

import (
	"net/http"
	"runtime"
	"time"

	apigen "github.com/monarr-media/monarr/internal/api/gen"
	"github.com/monarr-media/monarr/internal/app/health"
	"github.com/monarr-media/monarr/internal/infra/scheduler"
)

// GetSystemStatus implements GET /system/status.
func (s *Server) GetSystemStatus(w http.ResponseWriter, r *http.Request) {
	var schemaVersion int64
	if s.deps.DB != nil {
		v, err := s.deps.DB.SchemaVersion(r.Context())
		if err != nil {
			s.deps.Log.Warn("status: could not read schema version", "err", err)
		} else {
			schemaVersion = v
		}
	}
	writeJSON(w, http.StatusOK, apigen.SystemStatus{
		AppName:         "Monarr",
		Version:         s.deps.Version,
		Commit:          s.deps.Commit,
		GoVersion:       runtime.Version(),
		Os:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		StartedAt:       s.deps.StartedAt,
		UptimeSeconds:   int64(time.Since(s.deps.StartedAt).Seconds()),
		DataDir:         s.deps.DataDir,
		DbSchemaVersion: schemaVersion,
	})
}

// GetHealth implements GET /health: runs all checks live and reports.
func (s *Server) GetHealth(w http.ResponseWriter, r *http.Request) {
	results := s.deps.Health.Run(r.Context())
	checks := make([]apigen.HealthCheck, 0, len(results))
	for _, res := range results {
		c := apigen.HealthCheck{
			Name:      res.Name,
			Status:    apigen.HealthStatus(res.Status),
			CheckedAt: res.CheckedAt,
		}
		if res.Message != "" {
			msg := res.Message
			c.Message = &msg
		}
		checks = append(checks, c)
	}
	writeJSON(w, http.StatusOK, apigen.HealthReport{
		Overall: apigen.HealthStatus(health.Overall(results)),
		Checks:  checks,
	})
}

// ListTasks implements GET /system/tasks.
func (s *Server) ListTasks(w http.ResponseWriter, r *http.Request) {
	snap := s.deps.Scheduler.Snapshot()
	out := make([]apigen.TaskState, 0, len(snap))
	for _, t := range snap {
		out = append(out, taskStateDTO(t))
	}
	writeJSON(w, http.StatusOK, out)
}

// RunTask implements POST /system/tasks/{name}/run.
func (s *Server) RunTask(w http.ResponseWriter, r *http.Request, name string) {
	if err := s.deps.Scheduler.Trigger(name); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func taskStateDTO(t scheduler.TaskState) apigen.TaskState {
	dto := apigen.TaskState{
		Name:            t.Name,
		IntervalSeconds: int64(t.Interval / time.Second),
		Running:         t.Running,
	}
	if !t.LastRunAt.IsZero() {
		lr := t.LastRunAt
		dto.LastRunAt = &lr
		ms := t.LastDuration.Milliseconds()
		dto.LastDurationMs = &ms
	}
	if t.LastError != "" {
		e := t.LastError
		dto.LastError = &e
	}
	if !t.NextRunAt.IsZero() {
		nr := t.NextRunAt
		dto.NextRunAt = &nr
	}
	return dto
}
