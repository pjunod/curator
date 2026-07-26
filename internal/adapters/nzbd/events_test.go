package nzbd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/ports"
)

// sseServer serves a scripted event stream, recording the request so the
// resume protocol can be asserted on what we SENT, not only on what we
// accepted.
type sseServer struct {
	*httptest.Server
	connects  chan http.Header
	frames    []string
	holdOpen  bool
	connCount int
}

func newSSE(t *testing.T, frames []string, holdOpen bool) *sseServer {
	t.Helper()
	s := &sseServer{connects: make(chan http.Header, 8), frames: frames, holdOpen: holdOpen}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/events" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.connCount++
		select {
		case s.connects <- r.Header.Clone():
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, f := range s.frames {
			_, _ = w.Write([]byte(f))
			if fl != nil {
				fl.Flush()
			}
		}
		if s.holdOpen {
			<-r.Context().Done()
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func frame(id, event, data string) string {
	var b strings.Builder
	if id != "" {
		fmt.Fprintf(&b, "id: %s\n", id)
	}
	fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", event, data)
	return b.String()
}

func recv(t *testing.T, ch <-chan ports.ClientEvent) ports.ClientEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed early")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
		return ports.ClientEvent{}
	}
}

// `job_pp_finished` is the event this whole channel exists for:
// post-processing is done, the history row is already written, and the
// final directory is attached. Getting it wrong means importing a folder
// that is still being unpacked, or not importing at all.
func TestPpFinishedBecomesACompletionWithItsPath(t *testing.T) {
	srv := newSSE(t, []string{
		frame("17-3", "job_pp_finished", `{"job":7,"name":"Show.S01E01","category":"tv","pp_status":"SUCCESS","final_dir":"/downloads/complete/Show.S01E01","size_bytes":100,"health":1000,"params":[["monarr-transfer","t-42-a3f9c1"]],"history_seq":913,"seq":3}`),
	}, true)
	c := New(ports.ClientConfig{URL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}

	ev := recv(t, ch)
	if ev.Kind != ports.EventCompleted {
		t.Fatalf("kind = %q, want completed", ev.Kind)
	}
	if ev.Handle != "7" {
		t.Errorf("handle = %q", ev.Handle)
	}
	if ev.Status.SavePath != "/downloads/complete/Show.S01E01" {
		t.Errorf("save path = %q — the path is the reason this event exists", ev.Status.SavePath)
	}
	if ev.Status.State != ports.StateCompleted {
		t.Errorf("state = %q", ev.Status.State)
	}
	if ev.Seq != 3 {
		t.Errorf("seq = %d, want the sequence half of the id", ev.Seq)
	}
}

// A failed post-processing run is a failure, and it carries nzbd's own
// reason rather than a generic one.
func TestPpFinishedFailureBecomesAFailure(t *testing.T) {
	srv := newSSE(t, []string{
		frame("17-4", "job_pp_finished",
			`{"job":8,"name":"Bad","pp_status":"UNPACK_FAILURE","final_dir":null}`),
		frame("17-5", "job_pp_finished",
			`{"job":9,"name":"Sick","pp_status":"FAILURE/HEALTH","final_dir":null}`),
	}, true)
	c := New(ports.ClientConfig{URL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := c.Subscribe(ctx)

	for _, want := range []string{"UNPACK_FAILURE", "FAILURE/HEALTH"} {
		ev := recv(t, ch)
		if ev.Kind != ports.EventFailed {
			t.Fatalf("kind = %q for %s, want failed", ev.Kind, want)
		}
		if ev.Status.Message != want {
			t.Errorf("message = %q, want nzbd's own reason %q", ev.Status.Message, want)
		}
	}
}

// Stages are progress with a name — never completion. A stage mistaken for
// completion imports a half-unpacked folder.
func TestStagesAreProgressNotCompletion(t *testing.T) {
	srv := newSSE(t, []string{
		frame("17-1", "job_pp_stage", `{"job":7,"name":"Show","stage":"par_verify"}`),
		frame("17-2", "job_pp_stage", `{"job":7,"name":"Show","stage":"post_unpack_rename"}`),
	}, true)
	c := New(ports.ClientConfig{URL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := c.Subscribe(ctx)

	ev := recv(t, ch)
	if ev.Kind != ports.EventStage || ev.Status.State != ports.StateDownloading {
		t.Fatalf("first stage = %+v, want a stage that is still 'downloading'", ev)
	}
	if !strings.Contains(ev.Status.Message, "par verify") {
		t.Errorf("message = %q, want the stage in readable words", ev.Status.Message)
	}
	ev = recv(t, ch)
	if !strings.Contains(ev.Status.Message, "post unpack rename") {
		t.Errorf("message = %q", ev.Status.Message)
	}
}

// The 1 Hz tick is the progress channel. It must NEVER produce a
// completion: `job_finished` inside it fires when the download ends,
// before post-processing has touched the files.
func TestTickIsProgressOnly(t *testing.T) {
	srv := newSSE(t, []string{
		frame("", "tick", `{"status":{},"jobs":[{"id":1,"name":"A","status":"queued","size_bytes":100,"downloaded_bytes":25},{"id":2,"name":"B","status":"completed","size_bytes":100,"downloaded_bytes":100},{"id":3,"name":"C","status":{"post":{"stage":"unpack"}},"size_bytes":100,"downloaded_bytes":100}]}`),
	}, true)
	c := New(ports.ClientConfig{URL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := c.Subscribe(ctx)

	got := map[ports.Handle]ports.ClientEvent{}
	for i := 0; i < 2; i++ {
		ev := recv(t, ch)
		got[ev.Handle] = ev
	}
	if _, leaked := got["2"]; leaked {
		t.Error("a 'completed' job in a tick was pushed as an event — completion comes from job_pp_finished only")
	}
	if ev := got["1"]; ev.Kind != ports.EventProgress || ev.Status.Progress != 0.25 {
		t.Errorf("job 1 = %+v, want progress 0.25", ev)
	}
	if ev := got["3"]; ev.Status.State != ports.StateDownloading {
		t.Errorf("job 3 (unpacking) = %+v, want still downloading", ev)
	}
}

// A gap the daemon could not bridge, and a lag it reports, both mean the
// same thing to a consumer: you may have missed something, go reconcile.
func TestResetAndLaggedBothAskForAReconcile(t *testing.T) {
	for _, name := range []string{"reset", "lagged"} {
		srv := newSSE(t, []string{frame("", name, `{"reason":"gap","skipped":12}`)}, true)
		c := New(ports.ClientConfig{URL: srv.URL})
		ctx, cancel := context.WithCancel(context.Background())
		ch, _ := c.Subscribe(ctx)
		if ev := recv(t, ch); ev.Kind != ports.EventReset {
			t.Errorf("%s produced %q, want a reset", name, ev.Kind)
		}
		cancel()
	}
}

// The resume protocol: the id is opaque and must be echoed back verbatim,
// and a dropped stream must reconnect on its own — a subscriber that gives
// up turns a daemon restart into "push silently stopped working".
func TestReconnectEchoesTheLastEventIDVerbatim(t *testing.T) {
	srv := newSSE(t, []string{
		frame("1785099779123456789-42", "job_pp_stage", `{"job":7,"name":"S","stage":"unpack"}`),
	}, false) // closes immediately → forces a reconnect
	c := New(ports.ClientConfig{URL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := c.Subscribe(ctx)

	first := <-srv.connects
	if got := first.Get("Last-Event-ID"); got != "" {
		t.Errorf("first connect sent Last-Event-ID %q, want none", got)
	}
	if got := first.Get("X-Nzbd-Client"); !strings.HasPrefix(got, "monarr/") {
		t.Errorf("X-Nzbd-Client = %q", got)
	}
	recv(t, ch) // the stage event
	recv(t, ch) // the reset the closed stream produces

	select {
	case second := <-srv.connects:
		if got := second.Get("Last-Event-ID"); got != "1785099779123456789-42" {
			t.Errorf("resumed with %q, want the id echoed back exactly (it is opaque)", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never reconnected after the stream closed")
	}
}

// A stream that ends is a hole in what we know, whatever ended it. The
// consumer has to be told so it can reconcile.
func TestADroppedStreamReportsAReset(t *testing.T) {
	srv := newSSE(t, nil, false)
	c := New(ports.ClientConfig{URL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := c.Subscribe(ctx)
	if ev := recv(t, ch); ev.Kind != ports.EventReset {
		t.Fatalf("kind = %q, want a reset when the stream drops", ev.Kind)
	}
}

// Cancelling the context must end the goroutine and close the channel, or
// every settings change leaks a subscriber and a connection.
func TestCancelClosesTheChannel(t *testing.T) {
	srv := newSSE(t, nil, true)
	c := New(ports.ClientConfig{URL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := c.Subscribe(ctx)
	cancel()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, open := <-ch:
			if !open {
				return // closed, as required
			}
		case <-deadline:
			t.Fatal("channel still open after the context was cancelled")
		}
	}
}

// Frames the stream owns but that carry no decision must not become
// events — a heartbeat that reconciles the queue would undo the entire
// point of deduplicating an idle stream.
func TestNonDecisionFramesAreIgnored(t *testing.T) {
	if evs := translate("hb", `{"now_unix":1}`, ""); len(evs) != 0 {
		t.Errorf("hb produced %d events", len(evs))
	}
	if evs := translate("log", `{"entries":[]}`, ""); len(evs) != 0 {
		t.Errorf("log produced %d events", len(evs))
	}
	if evs := translate("job_added", `{"job":1,"name":"x"}`, ""); len(evs) != 0 {
		t.Errorf("job_added produced %d events (Monarr already knows; it grabbed it)", len(evs))
	}
	// Malformed JSON must be dropped, not panic the stream.
	if evs := translate("job_pp_finished", `{not json`, ""); len(evs) != 0 {
		t.Errorf("malformed frame produced %d events", len(evs))
	}
}

// A payload split across several data: lines is one payload, per the SSE
// spec. Overwriting instead of concatenating would silently decode only
// the last fragment — garbage, rather than an error anyone would notice.
func TestWrappedDataLinesAreOnePayload(t *testing.T) {
	srv := newSSE(t, []string{
		"id: 9-1\nevent: job_pp_finished\n" +
			`data: {"job":7,"name":"Split",` + "\n" +
			`data: "pp_status":"SUCCESS","final_dir":"/x"}` + "\n\n",
	}, true)
	c := New(ports.ClientConfig{URL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _ := c.Subscribe(ctx)
	ev := recv(t, ch)
	if ev.Kind != ports.EventCompleted || ev.Status.SavePath != "/x" {
		t.Fatalf("wrapped payload decoded as %+v", ev)
	}
}

var _ ports.Subscriber = (*Client)(nil)
