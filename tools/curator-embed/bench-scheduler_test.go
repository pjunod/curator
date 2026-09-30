package main

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPrototypeAdmissionAndPriority(t *testing.T) {
	checkGate()
	checkPriority()
}

func TestProviderDispatchBatchAndCooldown(t *testing.T) {
	p := newProvider()
	defer p.server.Close()
	defer close(p.schedule.stop)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if n, err := p.batch(ctx, "details", 12); err != nil || n != 12 {
		t.Fatalf("batch %d %v", n, err)
	}
	p.injected.Store(true)
	if err := p.request(ctx, "pages", true); !errors.Is(err, errCooldown) {
		t.Fatal(err)
	}
	before := p.calls.Load()
	if err := p.request(ctx, "normal", false); !errors.Is(err, errCooldown) || p.calls.Load() != before {
		t.Fatalf("cooldown read: %v", err)
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if err := p.schedule.acquire(cancelled, true); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestBenchmarkHelperProtocolAndReaping(t *testing.T) {
	for _, line := range []string{`{"vectors":[[1]]}`, `{"vectors":[]}`, `{"error":"bad","vectors":[[1]]}`, `garbage`} {
		cmd := exec.Command("cat")
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		e := &embedder{cmd: cmd, in: in, out: bufio.NewReader(out), lines: [][]byte{[]byte(line)}}
		err = e.encodeBounded(context.Background(), 0, 1)
		if (err == nil) != (line == `{"vectors":[[1]]}`) {
			t.Fatalf("protocol %s: %v", line, err)
		}
		if err = e.encodeBounded(context.Background(), 0, 0); err != nil {
			t.Fatal(err)
		}
		e.close()
		if cmd.ProcessState == nil {
			t.Fatal("helper not reaped")
		}
	}
	if _, err := newEmbedder(filepath.Join(t.TempDir(), "missing"), 1); err == nil {
		t.Fatal("missing input accepted")
	}
	p := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(p, []byte("{}\n{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newEmbedder(p, 1); err == nil {
		t.Fatal("wrong batches accepted")
	}
}

func TestBenchmarkModesAndPercentile(t *testing.T) {
	for _, mode := range []string{"theme", "seed", "combined", "hybrid"} {
		s := spec(mode)
		if s.name != mode || s.details+s.pages+s.aliases > 41 {
			t.Fatal(s)
		}
	}
	values := []float64{3, 1, 2, 4}
	if percentile(values, .95) != 4 || values[0] != 3 {
		t.Fatal("percentile mutated samples")
	}
	if boolInt(true) != 1 || boolInt(false) != 0 {
		t.Fatal("bool conversion")
	}
	if vmhwm(-1) != "unavailable" {
		t.Fatal("nonexistent process")
	}
}
