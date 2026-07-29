package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/scheduler"
)

type fakeDB struct{ v int64 }

func (f fakeDB) SchemaVersion(ctx context.Context) (int64, error) { return f.v, nil }

type testEvent struct {
	Msg string `json:"msg"`
}

func (testEvent) EventType() string { return "test.event" }

func newTestServer(t *testing.T) (*Server, *bus.Bus, *scheduler.Scheduler, func()) {
	t.Helper()
	b := bus.New(nil)

	reg := health.NewRegistry(b)
	reg.Register("always-ok", func(ctx context.Context) health.Result { return health.OK() })
	reg.Register("warns", func(ctx context.Context) health.Result { return health.Warn("phase 0") })

	sched := scheduler.New(nil, b, nil)
	if err := sched.Register(scheduler.Task{
		Name:     "noop",
		Interval: time.Hour,
		Fn:       func(ctx context.Context) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := sched.Start(ctx); err != nil {
		t.Fatal(err)
	}

	srv := New(Deps{
		Bus:       b,
		Health:    reg,
		Scheduler: sched,
		DB:        fakeDB{v: 1},
		Version:   "test",
		Commit:    "cafebabe",
		DataDir:   t.TempDir(),
		StartedAt: time.Now().Add(-time.Minute),
	})
	cleanup := func() {
		cancel()
		sched.Wait()
		b.Close()
	}
	return srv, b, sched, cleanup
}

func TestGetSystemStatus(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/system/status", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["appName"] != "Monarr" || got["version"] != "test" {
		t.Errorf("body = %v", got)
	}
	if got["dbSchemaVersion"].(float64) != 1 {
		t.Errorf("dbSchemaVersion = %v", got["dbSchemaVersion"])
	}
	if got["uptimeSeconds"].(float64) < 59 {
		t.Errorf("uptimeSeconds = %v", got["uptimeSeconds"])
	}
}

func TestGetHealth(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/health", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var got struct {
		Overall string `json:"overall"`
		Checks  []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Overall != "warning" || len(got.Checks) != 2 {
		t.Errorf("got %+v", got)
	}
}

func TestTasksEndpoints(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()
	h := srv.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/system/tasks", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"noop"`) {
		t.Fatalf("list: status = %d, body = %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/system/tasks/noop/run", nil))
	if rr.Code != http.StatusAccepted {
		t.Errorf("run known: status = %d", rr.Code)
	}

	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/system/tasks/ghost/run", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("run unknown: status = %d", rr.Code)
	}
}

func TestSPAServesHTMLAtRoot(t *testing.T) {
	srv, _, _, cleanup := newTestServer(t)
	defer cleanup()

	for _, path := range []string{"/", "/system", "/some/client/route"} {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status = %d", path, rr.Code)
		}
		ct := rr.Header().Get("Content-Type")
		body := strings.ToLower(rr.Body.String())
		if !strings.Contains(ct, "text/html") || !strings.Contains(body, "monarr") {
			t.Errorf("%s: content-type = %q, body does not look like the shell", path, ct)
		}
	}
}

func TestSSEStreamsPublishedEvents(t *testing.T) {
	srv, b, _, cleanup := newTestServer(t)
	defer cleanup()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	// Give the subscription a moment to be registered, then publish.
	go func() {
		time.Sleep(100 * time.Millisecond)
		b.Publish(testEvent{Msg: "hello"})
	}()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			var env struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env); err != nil {
				t.Fatalf("bad envelope %q: %v", line, err)
			}
			if env.Type != "test.event" {
				t.Errorf("type = %q", env.Type)
			}
			return // success
		}
	}
	t.Fatalf("stream ended without a data line: %v", scanner.Err())
}
