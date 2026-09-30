// Package embedding supervises the standalone, optional local encoder.
package embedding

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ModelID = "all-MiniLM-L6-v2@1110a243fdf4706b3f48f1d95db1a4f5529b4d41:tokenizers-0.22-onig-truncate256:unmasked-token-mean-l2:curator-metadata-v1"
const revision = "1110a243fdf4706b3f48f1d95db1a4f5529b4d41"

var files = []struct {
	name string
	size int64
	sha  string
}{
	{"config.json", 612, "953f9c0d463486b10a6871cc2fd59f223b2c70184f49815e7efbcab5d8908b41"},
	{"tokenizer.json", 466247, "be50c3628f2bf5bb5e3a7f17b1f74611b2561a3a27eeab05e5aa30f411572037"},
	{"model.safetensors", 90868376, "53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db"},
}

type process struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Scanner
	done   chan struct{}
}
type Encoder struct {
	mu, work                    sync.Mutex
	binary, dir, state, message string
	enabled                     bool
	generation, sequence        uint64
	child                       *process
	cooldown                    time.Time
	installCancel               context.CancelFunc
}

func New(dataDir, binary string) *Encoder {
	if binary == "" {
		binary = "/curator-embed"
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(ModelID)))
	e := &Encoder{binary: binary, dir: filepath.Join(dataDir, "models", hash), state: "disabled", generation: 1}
	// Only our incomplete install directory is disposable on restart.
	_ = os.RemoveAll(e.dir + ".partial")
	return e
}
func (*Encoder) ModelID() string { return ModelID }
func (e *Encoder) State() (string, uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enabled && e.state == "failed" && !e.cooldown.IsZero() && time.Now().After(e.cooldown) {
		e.cooldown = time.Time{}
		e.state, e.message = "ready", ""
	}
	return e.state, e.generation
}
func (e *Encoder) Detail() (string, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state, e.message
}
func (e *Encoder) setState(state, message string) {
	if e.state != state {
		e.generation++
	}
	e.state, e.message = state, message
}
func (e *Encoder) Enable(enabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.enabled = enabled
	e.generation++
	if !enabled {
		if e.installCancel != nil {
			e.installCancel()
		}
		e.stopLocked()
		e.setState("disabled", "")
		return
	}
	if e.installCancel != nil {
		return
	}
	if _, err := os.Stat(e.binary); err != nil {
		e.setState("failed", "The standalone encoder is not installed on this platform.")
		return
	}
	if err := verify(e.dir); err != nil {
		e.setState("not_installed", "Download or install the verified model files.")
		return
	}
	e.setState("ready", "")
}
func verify(dir string) error {
	for _, f := range files {
		file, err := os.Open(filepath.Join(dir, f.name))
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(file, f.size+1))
		_ = file.Close()
		if err != nil || n != f.size || fmt.Sprintf("%x", h.Sum(nil)) != f.sha {
			return errors.New("model verification failed")
		}
	}
	return nil
}
func (e *Encoder) Install() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.installCancel != nil {
		return nil
	}
	if _, err := os.Stat(e.binary); err != nil {
		return errors.New("standalone encoder is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	e.installCancel = cancel
	e.setState("downloading", "")
	go e.download(ctx, cancel)
	return nil
}
func (e *Encoder) download(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	partial := e.dir + ".partial"
	err := os.MkdirAll(partial, 0700)
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		host := r.URL.Hostname()
		if r.URL.Scheme != "https" || len(via) > 5 || host != "huggingface.co" && !strings.HasSuffix(host, ".huggingface.co") && !strings.HasSuffix(host, ".hf.co") {
			return errors.New("model redirect rejected")
		}
		return nil
	}}
	for _, f := range files {
		if err != nil {
			break
		}
		var request *http.Request
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, "https://huggingface.co/sentence-transformers/all-MiniLM-L6-v2/resolve/"+revision+"/"+f.name, nil)
		if err != nil {
			break
		}
		var response *http.Response
		response, err = client.Do(request)
		if err != nil {
			break
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			err = errors.New("model download failed")
			break
		}
		var file *os.File
		file, err = os.OpenFile(filepath.Join(partial, f.name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err == nil {
			var n int64
			n, err = io.Copy(file, io.LimitReader(response.Body, f.size+1))
			_ = file.Close()
			if n != f.size {
				err = errors.New("model size mismatch")
			}
		}
		_ = response.Body.Close()
	}
	if err == nil {
		err = verify(partial)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// Cleanup completes before releasing installation ownership. A retry cannot
	// reuse this directory while an earlier worker is still removing it.
	defer func() { _ = os.RemoveAll(partial) }()
	e.installCancel = nil
	if ctx.Err() != nil {
		if e.enabled {
			e.setState("failed", "Model installation was interrupted; retry.")
		} else {
			e.setState("disabled", "")
		}
		return
	}
	if err == nil {
		err = os.MkdirAll(filepath.Dir(e.dir), 0700)
		if err == nil {
			_ = os.RemoveAll(e.dir)
			err = os.Rename(partial, e.dir)
		}
	}
	if err != nil {
		e.setState("failed", "Model installation failed verification or download; retry.")
		return
	}
	if e.enabled {
		e.setState("ready", "")
	} else {
		e.setState("disabled", "")
	}
}
func (e *Encoder) Uninstall() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enabled || e.installCancel != nil {
		return errors.New("disable semantic ranking before removing the model")
	}
	e.stopLocked()
	e.generation++
	return os.RemoveAll(e.dir)
}
func (e *Encoder) startLocked() error {
	if e.child != nil {
		return nil
	}
	if time.Now().Before(e.cooldown) {
		return errors.New("encoder cooling down")
	}
	cmd := exec.Command(e.binary, "--model-dir", e.dir)
	cmd.Env = append(os.Environ(), "RAYON_NUM_THREADS=2", "OMP_NUM_THREADS=2")
	cmd.Stderr = io.Discard
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return err
	}
	if err = cmd.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		return err
	}
	p := &process{cmd: cmd, input: input, output: bufio.NewScanner(output), done: make(chan struct{})}
	p.output.Buffer(make([]byte, 4096), 256<<10)
	go func() { _ = cmd.Wait(); close(p.done) }()
	e.child = p
	return nil
}
func (e *Encoder) stopLocked() {
	if e.child == nil {
		return
	}
	p := e.child
	e.child = nil
	_ = p.input.Close()
	_ = p.cmd.Process.Kill()
	<-p.done
}
func (e *Encoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.installCancel != nil {
		e.installCancel()
	}
	e.stopLocked()
	return nil
}
func (e *Encoder) Encode(ctx context.Context, texts []string) ([][]float32, error) {
	if !e.work.TryLock() {
		return nil, errors.New("encoder busy")
	}
	defer e.work.Unlock()
	e.mu.Lock()
	if !e.enabled || e.state != "ready" {
		e.mu.Unlock()
		return nil, errors.New("encoder unavailable")
	}
	if err := e.startLocked(); err != nil {
		e.cooldown = time.Now().Add(30 * time.Second)
		e.state, e.message = "failed", "The encoder could not start."
		e.mu.Unlock()
		return nil, err
	}
	p := e.child
	generation := e.generation
	e.mu.Unlock()
	all := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += 8 {
		batch := texts[start:min(start+8, len(texts))]
		for _, text := range batch {
			if text == "" || len(text) > 8192 {
				return nil, errors.New("invalid embedding text")
			}
		}
		e.sequence++
		id := strconv.FormatUint(e.sequence, 10)
		body, _ := json.Marshal(struct {
			Protocol int      `json:"protocol_version"`
			ID       string   `json:"request_id"`
			Model    string   `json:"model_id"`
			Texts    []string `json:"texts"`
		}{1, id, ModelID, batch})
		type result struct {
			vectors [][]float32
			err     error
		}
		done := make(chan result, 1)
		go func() {
			if _, err := p.input.Write(append(body, '\n')); err != nil {
				done <- result{err: errors.New("encoder pipe failed")}
				return
			}
			if !p.output.Scan() {
				done <- result{err: errors.New("encoder stopped")}
				return
			}
			var response struct {
				Protocol int         `json:"protocol_version"`
				ID       string      `json:"request_id"`
				Model    string      `json:"model_id"`
				Vectors  [][]float32 `json:"vectors"`
				Error    *string     `json:"error"`
			}
			err := json.Unmarshal(p.output.Bytes(), &response)
			if err == nil && (response.Protocol != 1 || response.ID != id || response.Model != ModelID || response.Error != nil || len(response.Vectors) != len(batch)) {
				err = errors.New("invalid encoder response")
			}
			if err == nil {
				for _, v := range response.Vectors {
					if !valid(v) {
						err = errors.New("invalid embedding vector")
						break
					}
				}
			}
			done <- result{response.Vectors, err}
		}()
		var value result
		select {
		case value = <-done:
		case <-ctx.Done():
			e.mu.Lock()
			if e.child == p {
				e.stopLocked()
			}
			if e.enabled && generation == e.generation {
				e.cooldown = time.Now().Add(30 * time.Second)
				e.state, e.message = "failed", "Encoding exceeded its deadline; retry after 30 seconds."
			}
			e.mu.Unlock()
			<-done
			return nil, ctx.Err()
		}
		if value.err != nil {
			e.mu.Lock()
			if e.child == p {
				e.stopLocked()
			}
			if e.enabled && generation == e.generation {
				e.cooldown = time.Now().Add(30 * time.Second)
				e.state, e.message = "failed", "The encoder failed; retry after 30 seconds."
			}
			e.mu.Unlock()
			return nil, value.err
		}
		e.mu.Lock()
		changed := generation != e.generation || !e.enabled
		e.mu.Unlock()
		if changed {
			return nil, errors.New("encoder state changed")
		}
		all = append(all, value.vectors...)
	}
	return all, nil
}
func valid(v []float32) bool {
	if len(v) != 384 {
		return false
	}
	norm := 0.
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
		norm += float64(x) * float64(x)
	}
	return math.Abs(norm-1) < .01
}
