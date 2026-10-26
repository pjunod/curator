package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func benchmarkProtocolHelper(t *testing.T, response string, batches int) *embedder {
	t.Helper()
	// A persistent real process exercises pipes, SIGSTOP, timeout cancellation,
	// and reaping without a model download or a root-level helper installation.
	cmd := exec.Command("sh", "-c", `while IFS= read -r line; do printf '%s\n' "$RESPONSE"; done`)
	cmd.Env = append(os.Environ(), "RESPONSE="+response)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make([][]byte, batches)
	for i := range lines {
		lines[i] = []byte(`{"texts":["fixture"]}`)
	}
	e := &embedder{cmd: cmd, in: in, out: bufio.NewReader(out), lines: lines}
	t.Cleanup(e.close)
	return e
}

func TestBenchmarkBudgetCooldownCacheAndHungHelperContract(t *testing.T) {
	m := spec("seed")
	e := benchmarkProtocolHelper(t, `{"vectors":[[1]]}`, m.shallow+m.deep)
	p := newProvider()
	t.Cleanup(p.server.Close)
	t.Cleanup(func() { close(p.schedule.stop) })
	result := runBenchmark(m, 1, e, p)
	if result["failure"] != "" || result["trials_complete"] != 1 {
		t.Fatalf("cold search did not complete: %+v", result)
	}
	if result["provider_calls_complete"] != 34 || result["partial_cooldown"] != true {
		t.Fatalf("call budget/cooldown contract: %+v", result)
	}
	for _, key := range []string{"ordinary_title_latency_ms", "complete_p50_seconds", "complete_p95_seconds", "complete_max_seconds", "cache_p95_ms", "hung_helper_cancel_ms"} {
		value, ok := result[key].(float64)
		if !ok || value <= 0 {
			t.Fatalf("missing or invalid timing %s: %v", key, result[key])
		}
	}
	if result["hung_helper_cancel_ms"].(float64) > 1000 {
		t.Fatalf("hung helper exceeded cancellation bound: %+v", result)
	}
	if result["second_cold_rejection_ms"].(float64) > 1000 || result["cooldown_block_ms"].(float64) > 1000 {
		t.Fatalf("rejected work waited for a provider token: %+v", result)
	}
}

func TestBenchmarkInferenceFailureIsNotCompleteOrCached(t *testing.T) {
	m := spec("seed")
	e := benchmarkProtocolHelper(t, `{"error":"model unavailable"}`, m.shallow+m.deep)
	p := newProvider()
	t.Cleanup(p.server.Close)
	t.Cleanup(func() { close(p.schedule.stop) })
	result := runBenchmark(m, 1, e, p)
	failure, _ := result["failure"].(string)
	if failure != "shallow inference: invalid helper response" || result["trials_complete"] != 0 {
		t.Fatalf("inference failure reported as completion: %+v", result)
	}
	if _, ok := result["cache_p95_ms"]; ok {
		t.Fatalf("failed search produced a cache timing: %+v", result)
	}
	if _, ok := result["complete_p95_seconds"]; ok {
		t.Fatalf("failed search produced a completion percentile: %+v", result)
	}
	if result["partial_cooldown"] != true {
		t.Fatalf("failure hid the cooldown experiment: %+v", result)
	}
}

// One failed provider stage must invalidate the trial rather than publishing
// complete/cache percentiles; the independent cooldown probe must still run.
type benchmarkTransport func(*http.Request) (*http.Response, error)

func (f benchmarkTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBenchmarkProviderFailureCannotProduceCompletedTiming(t *testing.T) {
	for _, stage := range []string{"aliases", "pages", "details"} {
		t.Run(stage, func(t *testing.T) {
			m := modeSpec{name: "outage-fixture", aliases: 1, pages: 1, details: 1, deep: 1}
			e := benchmarkProtocolHelper(t, `{"vectors":[[1]]}`, 1)
			p := newProvider()
			t.Cleanup(p.server.Close)
			t.Cleanup(func() { close(p.schedule.stop) })
			var reject atomic.Bool
			reject.Store(true)
			p.client.Transport = benchmarkTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("stage") == stage && reject.CompareAndSwap(true, false) {
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("provider unavailable")), Request: r}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			result := runBenchmark(m, 1, e, p)
			want := "retrieval stage or 10s provider allocation"
			if stage == "details" {
				want = "detail stage or 10s provider allocation"
			}
			if result["failure"] != want || result["trials_complete"] != 0 || result["partial_cooldown"] != true {
				t.Fatalf("failed provider stage reported success: %+v", result)
			}
			if _, ok := result["complete_p95_seconds"]; ok {
				t.Fatalf("failed provider stage produced a completion percentile: %+v", result)
			}
			if _, ok := result["cache_p95_ms"]; ok {
				t.Fatalf("failed provider stage was cached: %+v", result)
			}
		})
	}
}
