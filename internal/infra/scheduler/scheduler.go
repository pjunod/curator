// Package scheduler is Monarr's in-process cron-style task runner
// (blueprint §5): named tasks on jittered intervals, per-task state (last
// run, next run, last error) persisted so the UI can show and trigger them —
// upstream's "Tasks" page is good UX worth keeping.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/monarr/monarr/internal/infra/bus"
)

// TaskFunc does the work. It must respect ctx cancellation.
type TaskFunc func(ctx context.Context) error

// Task is a named periodic job.
type Task struct {
	Name       string
	Interval   time.Duration
	RunOnStart bool
	Fn         TaskFunc
}

// PersistedTask is prior task state loaded from the store at startup, so the
// UI shows history across restarts.
type PersistedTask struct {
	Name         string
	LastRunAt    time.Time // zero if never run
	LastDuration time.Duration
	LastError    string
}

// RunRecord is what the scheduler persists after each run.
type RunRecord struct {
	Name            string
	IntervalSeconds int64
	StartedAt       time.Time
	Duration        time.Duration
	Err             string // empty on success
	NextRunAt       time.Time
}

// Store persists task state. Implemented by infra/sqlite.
type Store interface {
	LoadAll(ctx context.Context) ([]PersistedTask, error)
	RecordRun(ctx context.Context, rec RunRecord) error
}

// TaskState is a point-in-time view for the API/UI.
type TaskState struct {
	Name         string
	Interval     time.Duration
	Running      bool
	LastRunAt    time.Time // zero if never run
	LastDuration time.Duration
	LastError    string // empty if last run succeeded
	NextRunAt    time.Time
}

// TaskCompleted is published on the bus after every run.
type TaskCompleted struct {
	Task       string `json:"task"`
	DurationMS int64  `json:"durationMs"`
	Error      string `json:"error,omitempty"`
}

// EventType implements bus.Event.
func (TaskCompleted) EventType() string { return "task.completed" }

type taskRuntime struct {
	def     Task
	state   TaskState
	trigger chan struct{}
}

// Scheduler runs registered tasks. Create with New, Register tasks, then
// Start once.
type Scheduler struct {
	mu      sync.Mutex
	tasks   map[string]*taskRuntime
	order   []string
	store   Store
	bus     *bus.Bus
	log     *slog.Logger
	started bool
	wg      sync.WaitGroup
	// jitterFrac spreads runs by up to this fraction of the interval so
	// periodic work doesn't thundering-herd external services.
	jitterFrac float64
}

// New returns a Scheduler. store and b may be nil (state is then in-memory
// only and no events are published) — convenient for tests.
func New(store Store, b *bus.Bus, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{
		tasks:      make(map[string]*taskRuntime),
		store:      store,
		bus:        b,
		log:        log,
		jitterFrac: 0.1,
	}
}

// Register adds a task. It must be called before Start.
func (s *Scheduler) Register(t Task) error {
	if t.Name == "" || t.Fn == nil {
		return fmt.Errorf("scheduler: task needs a name and a function")
	}
	if t.Interval <= 0 {
		return fmt.Errorf("scheduler: task %q needs a positive interval", t.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fmt.Errorf("scheduler: cannot register %q after Start", t.Name)
	}
	if _, dup := s.tasks[t.Name]; dup {
		return fmt.Errorf("scheduler: task %q already registered", t.Name)
	}
	s.tasks[t.Name] = &taskRuntime{
		def:     t,
		state:   TaskState{Name: t.Name, Interval: t.Interval},
		trigger: make(chan struct{}, 1),
	}
	s.order = append(s.order, t.Name)
	return nil
}

// Start seeds state from the store and launches one goroutine per task.
// It returns immediately; cancel ctx to stop, then Wait for drain.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return fmt.Errorf("scheduler: already started")
	}
	s.started = true
	s.mu.Unlock()

	if s.store != nil {
		persisted, err := s.store.LoadAll(ctx)
		if err != nil {
			s.log.Warn("scheduler: could not load persisted task state", "err", err)
		} else {
			s.mu.Lock()
			for _, p := range persisted {
				if rt, ok := s.tasks[p.Name]; ok {
					rt.state.LastRunAt = p.LastRunAt
					rt.state.LastDuration = p.LastDuration
					rt.state.LastError = p.LastError
				}
			}
			s.mu.Unlock()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range s.order {
		rt := s.tasks[name]
		rt.state.NextRunAt = time.Now().Add(s.delay(rt.def))
		s.wg.Add(1)
		go s.runLoop(ctx, rt)
	}
	s.log.Info("scheduler: started", "tasks", len(s.order))
	return nil
}

// Wait blocks until all task goroutines have exited (after ctx cancel).
func (s *Scheduler) Wait() { s.wg.Wait() }

// Trigger requests an immediate run of the named task. Requests coalesce: if
// a trigger is already pending, this is a no-op.
func (s *Scheduler) Trigger(name string) error {
	s.mu.Lock()
	rt, ok := s.tasks[name]
	started := s.started
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("scheduler: unknown task %q", name)
	}
	if !started {
		return fmt.Errorf("scheduler: not started")
	}
	select {
	case rt.trigger <- struct{}{}:
	default:
	}
	return nil
}

// Snapshot returns the current state of every task, sorted by name.
func (s *Scheduler) Snapshot() []TaskState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TaskState, 0, len(s.tasks))
	for _, rt := range s.tasks {
		out = append(out, rt.state)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Scheduler) delay(t Task) time.Duration {
	if s.jitterFrac <= 0 {
		return t.Interval
	}
	j := rand.Float64() * s.jitterFrac * float64(t.Interval)
	return t.Interval + time.Duration(j)
}

func (s *Scheduler) runLoop(ctx context.Context, rt *taskRuntime) {
	defer s.wg.Done()
	if rt.def.RunOnStart {
		s.runOnce(ctx, rt)
	}
	for {
		s.mu.Lock()
		next := rt.state.NextRunAt
		s.mu.Unlock()
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.runOnce(ctx, rt)
		case <-rt.trigger:
			timer.Stop()
			s.runOnce(ctx, rt)
		}
	}
}

func (s *Scheduler) runOnce(ctx context.Context, rt *taskRuntime) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	rt.state.Running = true
	s.mu.Unlock()

	start := time.Now()
	err := runRecovered(ctx, rt.def.Fn)
	elapsed := time.Since(start)
	next := time.Now().Add(s.delay(rt.def))

	errStr := ""
	if err != nil {
		errStr = err.Error()
		s.log.Error("scheduler: task failed", "task", rt.def.Name, "err", err, "duration", elapsed)
	} else {
		s.log.Debug("scheduler: task completed", "task", rt.def.Name, "duration", elapsed)
	}

	s.mu.Lock()
	rt.state.Running = false
	rt.state.LastRunAt = start
	rt.state.LastDuration = elapsed
	rt.state.LastError = errStr
	rt.state.NextRunAt = next
	s.mu.Unlock()

	if s.store != nil {
		rec := RunRecord{
			Name:            rt.def.Name,
			IntervalSeconds: int64(rt.def.Interval / time.Second),
			StartedAt:       start,
			Duration:        elapsed,
			Err:             errStr,
			NextRunAt:       next,
		}
		if perr := s.store.RecordRun(context.WithoutCancel(ctx), rec); perr != nil {
			s.log.Warn("scheduler: could not persist task run", "task", rt.def.Name, "err", perr)
		}
	}
	if s.bus != nil {
		s.bus.Publish(TaskCompleted{Task: rt.def.Name, DurationMS: elapsed.Milliseconds(), Error: errStr})
	}
}

func runRecovered(ctx context.Context, fn TaskFunc) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn(ctx)
}
