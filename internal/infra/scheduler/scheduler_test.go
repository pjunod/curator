package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/bus"
)

type fakeStore struct {
	mu      sync.Mutex
	records []RunRecord
	seed    []PersistedTask
}

func (f *fakeStore) LoadAll(ctx context.Context) ([]PersistedTask, error) {
	return f.seed, nil
}

func (f *fakeStore) RecordRun(ctx context.Context, rec RunRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
	return nil
}

func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out: " + msg)
}

func TestPeriodicRunAndPersistence(t *testing.T) {
	store := &fakeStore{}
	s := New(store, nil, nil)
	var runs atomic.Int64
	err := s.Register(Task{
		Name:     "tick",
		Interval: 20 * time.Millisecond,
		Fn:       func(ctx context.Context) error { runs.Add(1); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return runs.Load() >= 2 }, "task should run at least twice")
	cancel()
	s.Wait()

	if store.count() < 2 {
		t.Errorf("expected >=2 persisted runs, got %d", store.count())
	}
	snap := s.Snapshot()
	if len(snap) != 1 || snap[0].Name != "tick" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap[0].LastRunAt.IsZero() || snap[0].LastError != "" {
		t.Errorf("unexpected state: %+v", snap[0])
	}
}

func TestRunOnStartAndTrigger(t *testing.T) {
	s := New(nil, nil, nil)
	var runs atomic.Int64
	s.Register(Task{
		Name:       "startup",
		Interval:   time.Hour, // never fires on its own within the test
		RunOnStart: true,
		Fn:         func(ctx context.Context) error { runs.Add(1); return nil },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	waitFor(t, func() bool { return runs.Load() == 1 }, "RunOnStart should fire once")

	if err := s.Trigger("startup"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return runs.Load() == 2 }, "Trigger should fire a run")

	if err := s.Trigger("nope"); err == nil {
		t.Error("triggering an unknown task should error")
	}
}

func TestTaskErrorAndPanicAreCaptured(t *testing.T) {
	b := bus.New(nil)
	defer b.Close()
	events, cancelSub := bus.Subscribe[TaskCompleted](b, 16)
	defer cancelSub()

	s := New(nil, b, nil)
	s.Register(Task{
		Name:       "boom",
		Interval:   time.Hour,
		RunOnStart: true,
		Fn:         func(ctx context.Context) error { panic("kaboom") },
	})
	s.Register(Task{
		Name:       "fail",
		Interval:   time.Hour,
		RunOnStart: true,
		Fn:         func(ctx context.Context) error { return errors.New("nope") },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	got := map[string]string{}
	for len(got) < 2 {
		select {
		case e := <-events:
			got[e.Task] = e.Error
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out; got %v", got)
		}
	}
	if got["boom"] == "" || got["fail"] != "nope" {
		t.Errorf("events = %v", got)
	}

	for _, st := range s.Snapshot() {
		if st.LastError == "" {
			t.Errorf("task %s should have LastError set", st.Name)
		}
		if st.Running {
			t.Errorf("task %s should not be marked running", st.Name)
		}
	}
}

func TestRegistrationValidation(t *testing.T) {
	s := New(nil, nil, nil)
	if err := s.Register(Task{Name: "", Interval: time.Second, Fn: func(context.Context) error { return nil }}); err == nil {
		t.Error("empty name should fail")
	}
	if err := s.Register(Task{Name: "x", Interval: 0, Fn: func(context.Context) error { return nil }}); err == nil {
		t.Error("zero interval should fail")
	}
	if err := s.Register(Task{Name: "x", Interval: time.Second, Fn: nil}); err == nil {
		t.Error("nil fn should fail")
	}
	ok := Task{Name: "x", Interval: time.Second, Fn: func(context.Context) error { return nil }}
	if err := s.Register(ok); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(ok); err == nil {
		t.Error("duplicate name should fail")
	}
}

func TestPersistedStateSeedsSnapshot(t *testing.T) {
	then := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	store := &fakeStore{seed: []PersistedTask{{
		Name:         "seeded",
		LastRunAt:    then,
		LastDuration: 123 * time.Millisecond,
		LastError:    "old failure",
	}}}
	s := New(store, nil, nil)
	s.Register(Task{Name: "seeded", Interval: time.Hour, Fn: func(context.Context) error { return nil }})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if !snap[0].LastRunAt.Equal(then) || snap[0].LastError != "old failure" {
		t.Errorf("persisted state not seeded: %+v", snap[0])
	}
}
