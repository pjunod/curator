package embedding

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("MONARR_TEST_HELPER_MODE"); mode != "" {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			var request struct {
				Protocol int      `json:"protocol_version"`
				ID       string   `json:"request_id"`
				Model    string   `json:"model_id"`
				Texts    []string `json:"texts"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				os.Exit(2)
			}
			vectors := make([][]float32, len(request.Texts))
			for i := range vectors {
				vectors[i] = make([]float32, 384)
				vectors[i][0] = 1
			}
			if mode == "invalid" {
				vectors[0][0] = 2
			}
			response := map[string]any{"protocol_version": request.Protocol, "request_id": request.ID, "model_id": request.Model, "vectors": vectors}
			if mode == "wrong_id" {
				response["request_id"] = "other"
			}
			if mode == "malformed" {
				_, _ = fmt.Fprintln(os.Stdout, "invalid")
			} else if json.NewEncoder(os.Stdout).Encode(response) != nil {
				os.Exit(3)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type modelTransport func(*http.Request) (*http.Response, error)

func (f modelTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func waitInstaller(t *testing.T, e *Encoder) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.mu.Lock()
		done := e.installCancel == nil
		e.mu.Unlock()
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("installer did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestInstallerVerificationPublicationAndFailures(t *testing.T) {
	originalFiles, transport := files, http.DefaultTransport
	t.Cleanup(func() { files = originalFiles; http.DefaultTransport = transport })
	for i := range files {
		data := []byte(files[i].name)
		files[i].size = int64(len(data))
		files[i].sha = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	for _, mode := range []string{"success", "bad_digest", "oversize", "status", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			http.DefaultTransport = modelTransport(func(r *http.Request) (*http.Response, error) {
				name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				data := []byte(name)
				status := 200
				header := http.Header{}
				switch mode {
				case "bad_digest":
					data = bytes.Repeat([]byte("x"), len(data))
				case "oversize":
					data = append(data, 'x')
				case "status":
					status = 503
				case "redirect":
					status = 302
					header.Set("Location", "http://untrusted.invalid/model")
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(data)), Request: r}, nil
			})
			e := New(t.TempDir(), os.Args[0])
			defer func() { _ = e.Close() }()
			e.Enable(true)
			state, _ := e.State()
			if state != "not_installed" {
				t.Fatal(state)
			}
			if err := e.Install(); err != nil {
				t.Fatal(err)
			}
			if err := e.Install(); err != nil {
				t.Fatal(err)
			}
			waitInstaller(t, e)
			state, _ = e.State()
			if mode == "success" {
				if state != "ready" || verify(e.dir) != nil {
					t.Fatalf("verified install %s", state)
				}
				if err := e.Uninstall(); err == nil {
					t.Fatal("enabled removal allowed")
				}
				e.Enable(false)
				if err := e.Uninstall(); err != nil {
					t.Fatal(err)
				}
			} else if state != "failed" || verify(e.dir) == nil {
				t.Fatalf("bad install accepted %s", state)
			}
			if _, err := os.Stat(e.dir + ".partial"); !os.IsNotExist(err) {
				t.Fatal("partial directory retained")
			}
		})
	}
}

func TestDisableCancelsInstallWithoutStartingAnotherWorker(t *testing.T) {
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	entered, release := make(chan struct{}), make(chan struct{})
	http.DefaultTransport = modelTransport(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return nil, r.Context().Err()
	})
	e := New(t.TempDir(), os.Args[0])
	defer func() { _ = e.Close() }()
	if err := e.Install(); err != nil {
		t.Fatal(err)
	}
	<-entered
	e.Enable(false)
	if err := e.Install(); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitInstaller(t, e)
	state, _ := e.State()
	if state != "disabled" {
		t.Fatal(state)
	}
	if _, err := os.Stat(e.dir + ".partial"); !os.IsNotExist(err) {
		t.Fatal("partial retained")
	}
}

func TestHelperBatchesValidatedProtocolAndCooldown(t *testing.T) {
	for _, mode := range []string{"valid", "invalid", "wrong_id", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("MONARR_TEST_HELPER_MODE", mode)
			e := New(t.TempDir(), os.Args[0])
			e.enabled = true
			e.state = "ready"
			defer func() { _ = e.Close() }()
			texts := make([]string, 17)
			for i := range texts {
				texts[i] = "An adult relationship story"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			vectors, err := e.Encode(ctx, texts)
			if mode == "valid" {
				if err != nil || len(vectors) != 17 || e.sequence != 3 {
					t.Fatalf("batch result %d %v", len(vectors), err)
				}
				e.Enable(false)
			} else {
				if err == nil {
					t.Fatal("invalid protocol admitted")
				}
				state, generation := e.State()
				if state != "failed" {
					t.Fatal(state)
				}
				e.cooldown = time.Now().Add(-time.Second)
				state, after := e.State()
				if state != "ready" || generation != after {
					t.Fatal("runtime cooldown changed configuration")
				}
			}
			if e.ModelID() != ModelID {
				t.Fatal("model identity")
			}
			_, _ = e.Detail()
		})
	}
	e := New(t.TempDir(), "/missing-helper")
	e.Enable(true)
	if err := e.Install(); err == nil {
		t.Fatal("missing helper installed")
	}
	if _, err := e.Encode(context.Background(), []string{"text"}); err == nil {
		t.Fatal("unavailable encoder used")
	}
	e.state = "ready"
	e.binary = os.Args[0]
	e.enabled = true
	e.work.Lock()
	if _, err := e.Encode(context.Background(), []string{"text"}); err == nil {
		t.Fatal("busy encoder accepted")
	}
	e.work.Unlock()
}
