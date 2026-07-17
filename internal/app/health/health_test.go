package health

import (
	"context"
	"testing"
	"time"

	"github.com/monarr/monarr/internal/infra/bus"
)

func TestOverallIsWorst(t *testing.T) {
	r := NewRegistry(nil)
	r.Register("a", func(ctx context.Context) Result { return OK() })
	r.Register("b", func(ctx context.Context) Result { return Warn("meh") })
	r.Register("c", func(ctx context.Context) Result { return OK() })

	results := r.Run(context.Background())
	if len(results) != 3 {
		t.Fatalf("got %d results", len(results))
	}
	if got := Overall(results); got != StatusWarning {
		t.Errorf("overall = %s, want warning", got)
	}

	overall, snap, ok := r.Snapshot()
	if !ok || overall != StatusWarning || len(snap) != 3 {
		t.Errorf("snapshot = %s, %d results, ok=%v", overall, len(snap), ok)
	}
}

func TestSnapshotBeforeRun(t *testing.T) {
	r := NewRegistry(nil)
	r.Register("a", func(ctx context.Context) Result { return OK() })
	if _, _, ok := r.Snapshot(); ok {
		t.Error("snapshot before Run should report ok=false")
	}
}

func TestChangedPublishedOnTransitionOnly(t *testing.T) {
	b := bus.New(nil)
	defer b.Close()
	events, cancel := bus.Subscribe[Changed](b, 16)
	defer cancel()

	healthy := true
	r := NewRegistry(b)
	r.Register("flappy", func(ctx context.Context) Result {
		if healthy {
			return OK()
		}
		return Errorf("down")
	})

	ctx := context.Background()
	r.Run(ctx) // ok → ok: no event
	select {
	case e := <-events:
		t.Fatalf("unexpected event on first healthy run: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}

	healthy = false
	r.Run(ctx) // ok → error: event
	select {
	case e := <-events:
		if e.Overall != StatusError {
			t.Errorf("event overall = %s", e.Overall)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected event on degradation")
	}

	r.Run(ctx) // error → error: no event
	select {
	case e := <-events:
		t.Fatalf("unexpected event without transition: %+v", e)
	case <-time.After(50 * time.Millisecond):
	}

	healthy = true
	r.Run(ctx) // error → ok: recovery event
	select {
	case e := <-events:
		if e.Overall != StatusOK {
			t.Errorf("recovery event overall = %s", e.Overall)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected recovery event")
	}
}

func TestPanickingCheckBecomesError(t *testing.T) {
	r := NewRegistry(nil)
	r.Register("bad", func(ctx context.Context) Result { panic("oops") })
	results := r.Run(context.Background())
	if results[0].Status != StatusError {
		t.Errorf("status = %s, want error", results[0].Status)
	}
}

func TestSlowCheckTimesOut(t *testing.T) {
	r := NewRegistry(nil)
	r.timeout = 30 * time.Millisecond
	r.Register("slow", func(ctx context.Context) Result {
		select {
		case <-time.After(5 * time.Second):
			return OK()
		case <-ctx.Done():
			// A well-behaved check returns promptly on cancel; the registry
			// still reports a timeout error either way.
			time.Sleep(5 * time.Second)
			return OK()
		}
	})
	start := time.Now()
	results := r.Run(context.Background())
	if time.Since(start) > 2*time.Second {
		t.Fatal("Run did not enforce the per-check timeout")
	}
	if results[0].Status != StatusError {
		t.Errorf("status = %s, want error (timeout)", results[0].Status)
	}
}
