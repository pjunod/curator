// Package health is Monarr's health-check registry (blueprint §7):
// named checks surfaced in the UI exactly like upstream's Health page, with
// transitions published on the event bus.
package health

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pjunod/monarr/internal/infra/bus"
)

// Status is a check outcome, ordered by severity.
type Status string

// Severity levels, worst last.
const (
	StatusOK      Status = "ok"
	StatusWarning Status = "warning"
	StatusError   Status = "error"
)

func severity(s Status) int {
	switch s {
	case StatusError:
		return 2
	case StatusWarning:
		return 1
	default:
		return 0
	}
}

// Result is what a check returns.
type Result struct {
	Status  Status
	Message string
}

// OK is the all-clear result.
func OK() Result { return Result{Status: StatusOK} }

// Warn returns a warning result.
func Warn(format string, args ...any) Result {
	return Result{Status: StatusWarning, Message: fmt.Sprintf(format, args...)}
}

// Errorf returns an error result.
func Errorf(format string, args ...any) Result {
	return Result{Status: StatusError, Message: fmt.Sprintf(format, args...)}
}

// CheckFunc performs one health check. It should be fast; the registry
// enforces a per-check timeout.
type CheckFunc func(ctx context.Context) Result

// CheckResult is a completed check with metadata, as served by the API.
type CheckResult struct {
	Name      string    `json:"name"`
	Status    Status    `json:"status"`
	Message   string    `json:"message,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

// Changed is published on the bus when the overall status transitions.
type Changed struct {
	Overall Status        `json:"overall"`
	Results []CheckResult `json:"results"`
}

// EventType implements bus.Event.
func (Changed) EventType() string { return "health.changed" }

type namedCheck struct {
	name string
	fn   CheckFunc
}

// Registry holds checks and their last results.
type Registry struct {
	mu          sync.Mutex
	checks      []namedCheck
	last        []CheckResult
	lastOverall Status // "" until first run
	bus         *bus.Bus
	groups      []GroupFunc
	timeout     time.Duration
	now         func() time.Time
}

// NewRegistry returns an empty registry. b may be nil (no events published).
func NewRegistry(b *bus.Bus) *Registry {
	return &Registry{bus: b, timeout: 5 * time.Second, now: time.Now}
}

// Register adds a named check. Checks run in registration order.
func (r *Registry) Register(name string, fn CheckFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, namedCheck{name: name, fn: fn})
}

// GroupFunc produces several named results in one pass.
type GroupFunc func(ctx context.Context) []CheckResult

// RegisterGroup adds a check whose NAMES are not known until it runs.
//
// The registry is built once at startup, but the things worth checking are
// not: download clients and media servers are configured in the UI, come and
// go, and each deserves its own line rather than being averaged into one
// "connections: 1 of 3 failing". A group closes that gap without making the
// registry mutable at runtime — which would mean handling duplicate
// registrations and removals for something that is really just one query.
func (r *Registry) RegisterGroup(fn GroupFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.groups = append(r.groups, fn)
}

// Run executes every check (with per-check timeout and panic recovery),
// stores the results, and publishes Changed if the overall status
// transitioned. It returns the fresh results.
func (r *Registry) Run(ctx context.Context) []CheckResult {
	r.mu.Lock()
	checks := make([]namedCheck, len(r.checks))
	copy(checks, r.checks)
	groups := make([]GroupFunc, len(r.groups))
	copy(groups, r.groups)
	timeout := r.timeout
	r.mu.Unlock()

	results := make([]CheckResult, 0, len(checks))
	for _, c := range checks {
		results = append(results, CheckResult{
			Name:      c.name,
			CheckedAt: r.now(),
			Status:    StatusOK, // placeholder, set below
		})
		res := runCheck(ctx, c.fn, timeout)
		results[len(results)-1].Status = res.Status
		results[len(results)-1].Message = res.Message
	}
	// Groups run after the fixed checks, so the server's own health leads
	// and the connections it depends on follow.
	for _, g := range groups {
		results = append(results, runGroup(ctx, g, timeout, r.now())...)
	}
	overall := Overall(results)

	r.mu.Lock()
	prev := r.lastOverall
	r.last = results
	r.lastOverall = overall
	b := r.bus
	r.mu.Unlock()

	// Publish on transitions only; a never-run registry counts as ok, so the
	// first run publishes only if something is wrong.
	if prev == "" {
		prev = StatusOK
	}
	if b != nil && overall != prev {
		b.Publish(Changed{Overall: overall, Results: results})
	}
	return results
}

// Snapshot returns the most recent results without running checks.
// ok is false if Run has never completed.
func (r *Registry) Snapshot() (overall Status, results []CheckResult, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastOverall == "" {
		return StatusOK, nil, false
	}
	out := make([]CheckResult, len(r.last))
	copy(out, r.last)
	return r.lastOverall, out, true
}

// Overall reduces results to the worst status; no results means ok.
func Overall(results []CheckResult) Status {
	overall := StatusOK
	for _, res := range results {
		if severity(res.Status) > severity(overall) {
			overall = res.Status
		}
	}
	return overall
}

// runGroup runs one group under the same timeout and panic recovery as a
// single check. A group that dies takes its own line rather than the whole
// run: losing the connections list must not hide the database check.
func runGroup(ctx context.Context, fn GroupFunc, timeout time.Duration, now time.Time) (out []CheckResult) {
	defer func() {
		if rec := recover(); rec != nil {
			out = []CheckResult{{
				Name: "connections", Status: StatusError, CheckedAt: now,
				Message: fmt.Sprintf("check panicked: %v", rec),
			}}
		}
	}()
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out = fn(cctx)
	for i := range out {
		if out[i].CheckedAt.IsZero() {
			out[i].CheckedAt = now
		}
	}
	return out
}

func runCheck(ctx context.Context, fn CheckFunc, timeout time.Duration) (res Result) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if p := recover(); p != nil {
			res = Errorf("check panicked: %v", p)
		}
	}()
	done := make(chan Result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- Errorf("check panicked: %v", p)
			}
		}()
		done <- fn(cctx)
	}()
	select {
	case res = <-done:
		return res
	case <-cctx.Done():
		return Errorf("check timed out after %s", timeout)
	}
}
