// bench-scheduler is a temporary M0c resource prototype, not production code.
// It measures exact theme, seed, and combined call/text budgets with a
// synthetic provider and deadline-bounded helper in one cgroup. Hybrid mode
// preserves the older stress envelope. Ownership/cache reads are synthetic.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type grant struct {
	ready chan struct{}
	ctx   context.Context
}

type scheduler struct {
	normal chan grant
	rec    chan grant
	stop   chan struct{}
}

func dispatchOne(normals, recs *[]grant, masterTokens, recTokens *int) bool {
	for len(*normals) > 0 && (*normals)[0].ctx.Err() != nil {
		*normals = (*normals)[1:]
	}
	for len(*recs) > 0 && (*recs)[0].ctx.Err() != nil {
		*recs = (*recs)[1:]
	}
	if *masterTokens > 0 && len(*normals) > 0 {
		close((*normals)[0].ready)
		*normals = (*normals)[1:]
		*masterTokens--
		return true
	}
	if *masterTokens > 0 && *recTokens > 0 && len(*recs) > 0 {
		close((*recs)[0].ready)
		*recs = (*recs)[1:]
		*masterTokens--
		*recTokens--
		return true
	}
	return false
}

func checkPriority() {
	normal := grant{ready: make(chan struct{}), ctx: context.Background()}
	recommendation := grant{ready: make(chan struct{}), ctx: context.Background()}
	normals, recs := []grant{normal}, []grant{recommendation}
	masterTokens, recTokens := 1, 1
	if !dispatchOne(&normals, &recs, &masterTokens, &recTokens) {
		panic("queued dispatch failed")
	}
	select {
	case <-normal.ready:
	default:
		panic("ordinary work lost dispatch priority")
	}
	select {
	case <-recommendation.ready:
		panic("recommendation dispatched before queued ordinary work")
	default:
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := grant{ready: make(chan struct{}), ctx: canceledCtx}
	recs = []grant{canceled, recommendation}
	normals = nil
	masterTokens, recTokens = 1, 1
	if !dispatchOne(&normals, &recs, &masterTokens, &recTokens) || masterTokens != 0 {
		panic("canceled grant blocked live work")
	}
	select {
	case <-canceled.ready:
		panic("canceled grant consumed a token")
	default:
	}
	select {
	case <-recommendation.ready:
	default:
		panic("live recommendation was not dispatched")
	}
}

func newScheduler() *scheduler {
	s := &scheduler{normal: make(chan grant, 64), rec: make(chan grant, 64), stop: make(chan struct{})}
	go s.run()
	return s
}

func (s *scheduler) run() {
	master := time.NewTicker(100 * time.Millisecond)
	sub := time.NewTicker(125 * time.Millisecond)
	defer master.Stop()
	defer sub.Stop()
	masterTokens, recTokens := 10, 4
	var normals, recs []grant
	for {
		for {
			select {
			case item := <-s.normal:
				normals = append(normals, item)
			case item := <-s.rec:
				recs = append(recs, item)
			default:
				goto drained
			}
		}
	drained:
		if dispatchOne(&normals, &recs, &masterTokens, &recTokens) {
			continue
		}
		select {
		case <-s.stop:
			return
		case item := <-s.normal:
			normals = append(normals, item)
		case item := <-s.rec:
			recs = append(recs, item)
		case <-master.C:
			if masterTokens < 10 {
				masterTokens++
			}
		case <-sub.C:
			if recTokens < 4 {
				recTokens++
			}
		}
	}
}

func (s *scheduler) acquire(ctx context.Context, recommendation bool) error {
	item := grant{ready: make(chan struct{}), ctx: ctx}
	queue := s.normal
	if recommendation {
		queue = s.rec
	}
	select {
	case queue <- item:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-item.ready:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

type provider struct {
	server   *httptest.Server
	client   *http.Client
	schedule *scheduler
	calls    atomic.Int64
	injected atomic.Bool
	cooldown atomic.Int64
}

func newProvider() *provider {
	p := &provider{client: &http.Client{Timeout: 2 * time.Second}, schedule: newScheduler()}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		time.Sleep(35 * time.Millisecond)
		if p.injected.Load() && r.URL.Query().Get("stage") == "pages" {
			if p.injected.CompareAndSwap(true, false) {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	return p
}

var errCooldown = errors.New("provider cooldown")

func (p *provider) request(ctx context.Context, stage string, rec bool) error {
	if time.Now().UnixNano() < p.cooldown.Load() {
		return errCooldown
	}
	if err := p.schedule.acquire(ctx, rec); err != nil {
		return err
	}
	if time.Now().UnixNano() < p.cooldown.Load() {
		return errCooldown
	}
	req, err := http.NewRequestWithContext(ctx, "GET", p.server.URL+"/?stage="+stage, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		p.cooldown.Store(time.Now().Add(2 * time.Second).UnixNano())
		return errCooldown
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("provider status %d", resp.StatusCode)
	}
	return nil
}

func (p *provider) batch(ctx context.Context, stage string, count int) (int, error) {
	jobs := make(chan struct{}, count)
	for i := 0; i < count; i++ {
		jobs <- struct{}{}
	}
	close(jobs)
	var wg sync.WaitGroup
	var success atomic.Int64
	var firstErr error
	var errMu sync.Mutex
	for i := 0; i < 4 && i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				if err := p.request(ctx, stage, true); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					continue
				}
				success.Add(1)
			}
		}()
	}
	wg.Wait()
	return int(success.Load()), firstErr
}

type embedder struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	out   *bufio.Reader
	lines [][]byte
}

func newEmbedder(path string, expected int) (*embedder, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != expected {
		return nil, fmt.Errorf("expected %d helper batches, got %d", expected, len(lines))
	}
	cmd := exec.Command("/curator-embed", "--model-dir", "/model")
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &embedder{cmd: cmd, in: in, out: bufio.NewReaderSize(out, 256*1024), lines: lines}, nil
}

func (e *embedder) encode(from, to int) error {
	for _, line := range e.lines[from:to] {
		if _, err := e.in.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			return err
		}
		response, err := e.out.ReadBytes('\n')
		if err != nil {
			return err
		}
		var parsed struct {
			Error   any         `json:"error"`
			Vectors [][]float64 `json:"vectors"`
		}
		if err := json.Unmarshal(response, &parsed); err != nil {
			return err
		}
		if parsed.Error != nil || len(parsed.Vectors) == 0 {
			return errors.New("invalid helper response")
		}
	}
	return nil
}

func (e *embedder) encodeBounded(ctx context.Context, from, to int) error {
	if from == to {
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- e.encode(from, to) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = e.cmd.Process.Kill()
		_ = e.in.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return ctx.Err()
	}
}

func (e *embedder) close() {
	_ = e.in.Close()
	done := make(chan struct{})
	go func() { _ = e.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = e.cmd.Process.Kill()
		<-done
	}
}

type modeSpec struct {
	name           string
	aliases, pages int
	details        int
	shallow, deep  int
	input          string
}

func spec(name string) modeSpec {
	switch name {
	case "theme":
		return modeSpec{name, 6, 3, 30, 0, 4, "/input/theme.jsonl"}
	case "seed":
		return modeSpec{name, 0, 4, 30, 8, 4, "/input/seed.jsonl"}
	case "combined":
		return modeSpec{name, 6, 7, 28, 0, 4, "/input/combined.jsonl"}
	case "hybrid":
		return modeSpec{name, 6, 7, 28, 8, 4, "/input/seed.jsonl"}
	default:
		panic("mode must be theme, seed, combined, or hybrid")
	}
}

type gate struct {
	mu           sync.Mutex
	running      bool
	key          string
	attached     int
	cached       map[string]bool
	cacheReaders int
}

func (g *gate) admit(key string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running && g.key == key {
		if g.attached < 4 {
			g.attached++
			return "joined"
		}
		return "429"
	}
	if g.cached[key] {
		if g.cacheReaders >= 2 {
			return "429"
		}
		g.cacheReaders++
		return "cache"
	}
	if g.running {
		return "429"
	}
	g.running, g.key, g.attached = true, key, 1
	return "cold"
}

func (g *gate) releaseCache() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cacheReaders == 0 {
		panic("cache reader underflow")
	}
	g.cacheReaders--
}

func (g *gate) releaseJoin() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.running || g.attached < 2 {
		panic("join underflow")
	}
	g.attached--
}

func (g *gate) finish(key string, success bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.running || g.key != key {
		panic("finishing wrong cold query")
	}
	g.running, g.attached = false, 0
	if success {
		g.cached[key] = true
	}
}

func checkGate() {
	g := &gate{cached: map[string]bool{"B": true, "C": true, "D": true}}
	if g.admit("A") != "cold" {
		panic("cold admission failed")
	}
	if g.admit("B") != "cache" || g.admit("C") != "cache" || g.admit("D") != "429" {
		panic("cached reader allowance during cold search failed")
	}
	if g.admit("E") != "429" {
		panic("second cold key was not rejected")
	}
	g.releaseCache()
	g.releaseCache()
	for i := 0; i < 3; i++ {
		if g.admit("A") != "joined" {
			panic("join below cap failed")
		}
	}
	if g.admit("A") != "429" || g.admit("E") != "429" {
		panic("cold/join cap failed")
	}
	g.releaseJoin() // canceled attached caller frees one place
	if g.admit("A") != "joined" {
		panic("released join not reusable")
	}
	g.finish("A", true)
	for i := 0; i < 2; i++ {
		if g.admit("A") != "cache" {
			panic("cached reader below cap failed")
		}
	}
	if g.admit("A") != "429" {
		panic("two cached reader cap failed")
	}
	g.releaseCache()
	if g.admit("A") != "cache" {
		panic("cached reader release failed")
	}
	g.releaseCache()
	g.releaseCache()
	if g.admit("E") != "cold" {
		panic("distinct-key cache miss failed")
	}
	g.finish("E", false)
}

func percentile(values []float64, quantile float64) float64 {
	copyOf := append([]float64(nil), values...)
	sort.Float64s(copyOf)
	index := int(float64(len(copyOf)-1)*quantile + 0.999999)
	return copyOf[index]
}

func vmhwm(pid int) string {
	data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			return strings.TrimSpace(line)
		}
	}
	return "unavailable"
}

func main() {
	mode := flag.String("mode", "combined", "theme, seed, combined, or hybrid")
	trials := flag.Int("trials", 10, "complete cold-search repetitions")
	flag.Parse()
	m := spec(*mode)
	checkGate()
	checkPriority()
	e, err := newEmbedder(m.input, m.shallow+m.deep)
	if err != nil {
		panic(err)
	}
	defer e.close()
	warmCtx, warmCancel := context.WithTimeout(context.Background(), 4*time.Second)
	if err := e.encodeBounded(warmCtx, 0, 1); err != nil {
		panic(err)
	}
	warmCancel()
	p := newProvider()
	defer p.server.Close()
	defer close(p.schedule.stop)
	g := &gate{cached: map[string]bool{}}
	var complete []float64
	var failed string
	var ordinaryLatency, rejectLatency time.Duration
	for trial := 0; trial < *trials; trial++ {
		delete(g.cached, m.name)
		if g.admit(m.name) != "cold" {
			panic("cold admission failed")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		started := time.Now()
		if trial == 0 {
			rejectStart := time.Now()
			if g.admit("Different") != "429" || g.admit(m.name) != "joined" {
				panic("second-cold or join behavior invalid")
			}
			rejectLatency = time.Since(rejectStart)
			g.releaseJoin()
		}
		ordinaryDone := make(chan time.Duration, 1)
		if trial == 0 {
			go func() {
				time.Sleep(150 * time.Millisecond)
				began := time.Now()
				if err := p.request(ctx, "ordinary-title", false); err != nil {
					ordinaryDone <- -1
					return
				}
				ordinaryDone <- time.Since(began)
			}()
		}
		calls0 := p.calls.Load()
		providerCtx, providerCancel := context.WithDeadline(ctx, started.Add(10*time.Second))
		rankUsed := time.Duration(0)
		rank := func(from, to int) error {
			remaining := 4*time.Second - rankUsed
			if remaining <= 0 {
				return errors.New("ranking allocation exceeded")
			}
			rankCtx, rankCancel := context.WithTimeout(ctx, remaining)
			began := time.Now()
			err := e.encodeBounded(rankCtx, from, to)
			rankUsed += time.Since(began)
			rankCancel()
			return err
		}
		alias, aliasErr := p.batch(providerCtx, "aliases", m.aliases)
		pages, pageErr := p.batch(providerCtx, "pages", m.pages)
		if aliasErr != nil || pageErr != nil || alias != m.aliases || pages != m.pages {
			failed = "retrieval stage or 10s provider allocation"
		}
		if failed == "" && m.shallow > 0 {
			if err := rank(0, m.shallow); err != nil {
				failed = "shallow inference: " + err.Error()
			}
		}
		if failed == "" {
			details, detailErr := p.batch(providerCtx, "details", m.details)
			if detailErr != nil || details != m.details {
				failed = "detail stage or 10s provider allocation"
			}
		}
		if failed == "" {
			if err := rank(m.shallow, m.shallow+m.deep); err != nil {
				failed = "detail inference: " + err.Error()
			}
		}
		elapsed := time.Since(started)
		providerCancel()
		cancel()
		calls := p.calls.Load() - calls0
		if trial == 0 {
			ordinaryLatency = <-ordinaryDone
		}
		if failed == "" && calls != int64(m.aliases+m.pages+m.details+boolInt(trial == 0)) {
			failed = fmt.Sprintf("unexpected call count %d", calls)
		}
		if failed == "" && elapsed > 15*time.Second {
			failed = "15s total deadline exceeded"
		}
		g.finish(m.name, failed == "")
		if failed != "" {
			break
		}
		complete = append(complete, elapsed.Seconds())
	}
	cacheTimes := []float64{}
	for i := 0; i < 20 && failed == ""; i++ {
		start := time.Now()
		if g.admit(m.name) != "cache" {
			panic("cache admission failed")
		}
		time.Sleep(5 * time.Millisecond) // synthetic fresh ownership read
		cacheTimes = append(cacheTimes, time.Since(start).Seconds())
		g.releaseCache()
	}
	// A separate injected 429 must trigger a shared cooldown without retry.
	p.injected.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	partialStart := time.Now()
	_, _ = p.batch(ctx, "aliases", m.aliases)
	partialPages, partialErr := p.batch(ctx, "pages", m.pages)
	cooldownStart := time.Now()
	_, blockedErr := p.batch(ctx, "details", 1)
	blockedLatency := time.Since(cooldownStart)
	partialElapsed := time.Since(partialStart)
	cancel()
	if partialErr == nil || !errors.Is(blockedErr, errCooldown) {
		panic("injected cooldown not observed")
	}
	// Stop the helper and verify a hung inference is bounded and reaped.
	helperHWM := vmhwm(e.cmd.Process.Pid)
	if err := syscall.Kill(e.cmd.Process.Pid, syscall.SIGSTOP); err != nil {
		panic(err)
	}
	hungCtx, hungCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	hungStart := time.Now()
	hungErr := e.encodeBounded(hungCtx, 0, 1)
	hungElapsed := time.Since(hungStart)
	hungCancel()
	if !errors.Is(hungErr, context.DeadlineExceeded) || hungElapsed > time.Second {
		panic("hung helper cancellation failed")
	}
	peakText, _ := os.ReadFile("/sys/fs/cgroup/memory.peak")
	result := map[string]any{
		"scope": "single-cgroup Go scheduler + persistent helper; loopback stub provider with 35ms injected latency",
		"mode":  m.name, "trials_requested": *trials, "trials_complete": len(complete),
		"failure": failed, "provider_calls_complete": m.aliases + m.pages + m.details,
		"ordinary_title_latency_ms": float64(ordinaryLatency.Microseconds()) / 1000,
		"second_cold_rejection_ms":  float64(rejectLatency.Microseconds()) / 1000,
		"partial_cooldown":          errors.Is(blockedErr, errCooldown),
		"partial_pages_success":     partialPages,
		"partial_stage_seconds":     partialElapsed.Seconds(),
		"cooldown_block_ms":         float64(blockedLatency.Microseconds()) / 1000,
		"hung_helper_cancel_ms":     float64(hungElapsed.Microseconds()) / 1000,
		"cgroup_peak_bytes":         strings.TrimSpace(string(peakText)),
		"go_vmhwm":                  vmhwm(os.Getpid()), "helper_vmhwm": helperHWM,
	}
	if len(complete) > 0 {
		result["complete_p50_seconds"] = percentile(complete, 0.5)
		result["complete_p95_seconds"] = percentile(complete, 0.95)
		result["complete_max_seconds"] = percentile(complete, 1)
	}
	if len(cacheTimes) > 0 {
		result["cache_p95_ms"] = percentile(cacheTimes, 0.95) * 1000
	}
	output, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(output))
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
