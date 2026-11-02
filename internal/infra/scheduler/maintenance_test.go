package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/maintenance"
)

func TestDrainCompletesAcceptedTriggerAndSkipsPeriodicRoots(t *testing.T) {
	gate := maintenance.New()
	s := New(nil, nil, nil)
	s.SetAdmission(gate)
	entered := make(chan struct{}, 3)
	finish := make(chan struct{}, 3)
	if err := s.Register(Task{Name: "work", Interval: 20 * time.Millisecond, Fn: func(ctx context.Context) error {
		entered <- struct{}{}
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); s.Wait() }()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Trigger("work"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first trigger did not start")
	}
	if err := s.Trigger("work"); err != nil {
		t.Fatal(err)
	}
	owner := strings.Repeat("11", 32)
	status, err := gate.Start(owner)
	if err != nil {
		t.Fatal(err)
	}
	finish <- struct{}{}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("accepted trigger stranded by drain")
	}
	finish <- struct{}{}
	deadline := time.Now().Add(time.Second)
	for gate.Status().Active != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if gate.Status().Phase != "drained" {
		t.Fatal(gate.Status())
	}
	// Wait several intervals; periodic work cannot begin while paused.
	select {
	case <-entered:
		t.Fatal("periodic work bypassed drain")
	case <-time.After(70 * time.Millisecond):
	}
	if _, err = gate.Abort(owner, status.Generation); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
		finish <- struct{}{}
	case <-time.After(time.Second):
		t.Fatal("periodic producer failed to resume")
	}
}
