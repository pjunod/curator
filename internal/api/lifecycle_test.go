package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pjunod/monarr/internal/infra/bus"
)

func awaitLifecycle(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestShutdownStopsSSEWithoutCancelingOrdinaryHandler(t *testing.T) {
	s := New(Deps{Bus: bus.New(edgeQuietLog()), Log: edgeQuietLog()})
	t.Cleanup(func() { s.BeginShutdown(); s.CancelRequests(); s.Wait(); s.deps.Bus.Close() })
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseHandler := func() { once.Do(func() { close(release) }) }
	ordinaryDone := make(chan struct{})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", s.Handler())
	mux.Handle("/ordinary", s.trackRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
			t.Error("ordinary request canceled before its HTTP grace elapsed")
		case <-release:
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	ts := httptest.NewServer(mux)
	t.Cleanup(func() { releaseHandler(); ts.Close() })
	client := ts.Client()
	client.Timeout = 3 * time.Second
	go func() {
		resp, err := client.Get(ts.URL + "/ordinary")
		if err == nil {
			_ = resp.Body.Close()
		}
		close(ordinaryDone)
	}()
	awaitLifecycle(t, started, "handler/background startup")
	resp, err := client.Get(ts.URL + "/api/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	streamDone := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, reader); streamDone <- err }()
	s.BeginShutdown()
	select {
	case err := <-streamDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("responsive SSE stream did not stop")
	}
	select {
	case <-ordinaryDone:
		t.Fatal("ordinary handler finished before release")
	default:
	}
	releaseHandler()
	select {
	case <-ordinaryDone:
	case <-time.After(time.Second):
		t.Fatal("ordinary handler did not finish")
	}
	s.Wait()
}

type streamObservedWriter struct {
	http.ResponseWriter
	writeResult chan error
}

func (w *streamObservedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *streamObservedWriter) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w *streamObservedWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if len(p) > 1<<20 {
		select {
		case w.writeResult <- err:
		default:
		}
	}
	return n, err
}

func TestSSEDeadlineStopsClientThatDoesNotRead(t *testing.T) {
	b := bus.New(edgeQuietLog())
	s := New(Deps{Bus: b, Log: edgeQuietLog()})
	s.streamWriteTimeout = 100 * time.Millisecond
	writeResult := make(chan error, 1)
	handlerDone := make(chan struct{})
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		s.Handler().ServeHTTP(&streamObservedWriter{ResponseWriter: w, writeResult: writeResult}, r)
	}))
	ts.Config.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateNew {
			if tcp, ok := c.(*net.TCPConn); ok {
				_ = tcp.SetWriteBuffer(4096)
			}
		}
	}
	ts.Start()
	t.Cleanup(func() { s.BeginShutdown(); s.CancelRequests(); ts.Close(); s.Wait(); b.Close() })
	conn, err := net.Dial("tcp", ts.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(1024)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "GET /api/v1/events HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, "retry: 3000") {
			break
		}
	}
	// Stop reading. One frame exceeds both socket buffers, so success cannot
	// merely mean the handler was idle when cancellation arrived.
	b.Publish(edgeEvent{Msg: strings.Repeat("x", 8<<20)})
	select {
	case err := <-writeResult:
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("large frame write = %v, want timeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("backpressured write did not time out")
	}
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("stream remained active after write failure")
	}
}

func TestShutdownJoinsCanceledHandlerAndRejectsNewAdmission(t *testing.T) {
	s := New(Deps{})
	t.Cleanup(func() { s.BeginShutdown(); s.CancelRequests(); s.Wait() })
	started := make(chan struct{})
	finished := make(chan struct{})
	h := s.trackRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(finished)
	}))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	awaitLifecycle(t, started, "handler/background startup")
	s.BeginShutdown()
	rejected := httptest.NewRecorder()
	h.ServeHTTP(rejected, httptest.NewRequest(http.MethodGet, "/", nil))
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("new request = %d", rejected.Code)
	}
	s.CancelRequests()
	s.Wait()
	select {
	case <-finished:
	default:
		t.Fatal("wait returned before handler completion")
	}
}

func TestEventStreamRejectsUnsupportedDeadlineThroughLogging(t *testing.T) {
	s := New(Deps{Bus: bus.New(edgeQuietLog())})
	defer s.deps.Bus.Close()
	w := httptest.NewRecorder() // Flushing works, network deadlines do not.
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "write deadline unavailable") {
		t.Fatalf("unsupported writer = %d %s", w.Code, w.Body.String())
	}
}

func TestShutdownJoinsBackgroundCleanupAndClosesAdmission(t *testing.T) {
	s := New(Deps{})
	started := make(chan struct{})
	cleaning := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseCleanup := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() { s.BeginShutdown(); s.CancelRequests(); releaseCleanup(); s.Wait() })
	if !s.startBackground(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cleaning)
		<-release // Cancellation is not cleanup completion.
	}) {
		t.Fatal("work was rejected before shutdown")
	}
	awaitLifecycle(t, started, "handler/background startup")
	s.BeginShutdown()
	awaitLifecycle(t, cleaning, "background cancellation cleanup")
	if s.startBackground(func(context.Context) { t.Error("late background work ran") }) {
		t.Error("background admission remained open")
	}
	waited := make(chan struct{})
	go func() { s.Wait(); close(waited) }()
	select {
	case <-waited:
		t.Error("wait returned before background cleanup")
	default:
	}
	releaseCleanup()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("background cleanup was not joined")
	}
}

func TestHTTP2EventStreamSurvivesIdleWriteDeadline(t *testing.T) {
	b := bus.New(edgeQuietLog())
	s := New(Deps{Bus: b, Log: edgeQuietLog()})
	s.streamWriteTimeout = 40 * time.Millisecond
	ts := httptest.NewUnstartedServer(s.Handler())
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(func() { s.BeginShutdown(); s.CancelRequests(); ts.Close(); s.Wait(); b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 {
		t.Fatalf("protocol = %s, want HTTP/2", resp.Proto)
	}
	r := bufio.NewReader(resp.Body)
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	// Wait beyond the frame's deadline without writing another frame. An
	// armed HTTP/2 deadline resets this stream even while it is idle.
	time.Sleep(3 * s.streamWriteTimeout)
	b.Publish(edgeEvent{Msg: "after idle"})
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("idle event stream ended: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			if !strings.Contains(line, "after idle") {
				t.Fatalf("frame = %q", line)
			}
			break
		}
	}
}
