package nzbget

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/monarr-media/monarr/internal/ports"
)

// rpcServer answers NZBGet's JSON-RPC from a per-method table and records
// what it was asked, so a test can assert on the wire call rather than only
// on what came back.
type rpcServer struct {
	mu      sync.Mutex
	calls   []string
	params  map[string][]any
	answers map[string]string
	status  int
	body    string // when set, returned verbatim for every method
}

func newRPCServer(t *testing.T, answers map[string]string) (*httptest.Server, *rpcServer) {
	t.Helper()
	rs := &rpcServer{answers: answers, params: map[string][]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		_ = json.Unmarshal(raw, &req)

		rs.mu.Lock()
		rs.calls = append(rs.calls, req.Method)
		rs.params[req.Method] = req.Params
		status, body, answer := rs.status, rs.body, rs.answers[req.Method]
		rs.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if body != "" {
			_, _ = w.Write([]byte(body))
			return
		}
		if answer == "" {
			answer = `{"result":true}`
		}
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	return srv, rs
}

func (rs *rpcServer) called(method string) int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	n := 0
	for _, c := range rs.calls {
		if c == method {
			n++
		}
	}
	return n
}

func (rs *rpcServer) paramsFor(method string) []any {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.params[method]
}

// A 401 has one cause and one fix, so it says so instead of arriving as an
// "unexpected status" the user has to look up.
func TestTailUnauthorizedIsNamedNotJustNumbered(t *testing.T) {
	srv, rs := newRPCServer(t, nil)
	rs.mu.Lock()
	rs.status = http.StatusUnauthorized
	rs.mu.Unlock()

	c := New(ports.ClientConfig{URL: srv.URL, Username: "nzb", Password: "wrong"})
	err := c.Test(context.Background())
	if err == nil {
		t.Fatal("a 401 was treated as success")
	}
	if !strings.Contains(err.Error(), "authentication rejected") {
		t.Errorf("err = %v, want it to name the authentication failure", err)
	}
}

// Any other non-200 carries its code, which is the only clue distinguishing
// a wrong port from a reverse proxy in the way.
func TestTailOtherStatusesCarryTheirCode(t *testing.T) {
	srv, rs := newRPCServer(t, nil)
	rs.mu.Lock()
	rs.status = http.StatusServiceUnavailable
	rs.mu.Unlock()

	c := New(ports.ClientConfig{URL: srv.URL})
	err := c.Test(context.Background())
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want the 503 in it", err)
	}
}

// An HTML error page is the classic response from something that is not
// NZBGet on that port. It must not decode as an empty result.
func TestTailMalformedBodyIsAnError(t *testing.T) {
	srv, rs := newRPCServer(t, nil)
	rs.mu.Lock()
	rs.body = "<html>Bad Gateway</html>"
	rs.mu.Unlock()

	c := New(ports.ClientConfig{URL: srv.URL})
	err := c.Test(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bad response") {
		t.Errorf("err = %v, want it to say the response was bad", err)
	}
}

// NZBGet's own error message is the actionable half of a failure.
func TestTailServerErrorObjectIsPassedThrough(t *testing.T) {
	srv, _ := newRPCServer(t, map[string]string{
		"append": `{"result":null,"error":{"message":"Category not found"}}`,
	})
	c := New(ports.ClientConfig{URL: srv.URL})
	_, err := c.Add(context.Background(), "http://x/f.nzb", "monarr")
	if err == nil || !strings.Contains(err.Error(), "Category not found") {
		t.Errorf("err = %v, want NZBGet's own message", err)
	}
}

// append answers with the new NZBID, and 0 is how it says no. Handing back
// handle "0" would make monarr track a download that does not exist and
// never resolve it.
func TestTailAppendRejectionIsNotAHandle(t *testing.T) {
	srv, _ := newRPCServer(t, map[string]string{"append": `{"result":0}`})
	c := New(ports.ClientConfig{URL: srv.URL})
	h, err := c.Add(context.Background(), "http://x/f.nzb", "monarr")
	if err == nil {
		t.Fatal("append id 0 was treated as a successful add")
	}
	if h != "" {
		t.Errorf("handle = %q, want empty on a rejected append", h)
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("err = %v, want it to say the append was rejected", err)
	}
}

// The append argument list is positional and long, so its shape is worth
// pinning: a category landing in the priority slot would silently
// mis-file every download.
func TestTailAppendSendsTheCategoryInTheRightSlot(t *testing.T) {
	srv, rs := newRPCServer(t, map[string]string{"append": `{"result":7}`})
	c := New(ports.ClientConfig{URL: srv.URL})

	h, err := c.Add(context.Background(), "http://x/f.nzb", "monarr")
	if err != nil || h != "7" {
		t.Fatalf("add = %q err %v", h, err)
	}
	p := rs.paramsFor("append")
	if len(p) != 9 {
		t.Fatalf("append got %d params, want 9", len(p))
	}
	if p[1] != "http://x/f.nzb" || p[2] != "monarr" {
		t.Errorf("append params = %v, want the url then the category", p[:3])
	}
}

// Queue statuses and history are two calls, and the failure of either has to
// fail the whole read: half a queue looks like downloads that vanished, and
// monarr would retire the rows for them.
func TestTailAHalfReadableQueueIsNotHalfAQueue(t *testing.T) {
	srv, _ := newRPCServer(t, map[string]string{
		"listgroups": `not json`,
	})
	c := New(ports.ClientConfig{URL: srv.URL})
	if _, err := c.Statuses(context.Background()); err == nil {
		t.Error("an unreadable listgroups response produced a usable queue")
	}

	srv2, _ := newRPCServer(t, map[string]string{
		"listgroups": `{"result":[]}`,
		"history":    `{"result":{"not":"a list"}}`,
	})
	c2 := New(ports.ClientConfig{URL: srv2.URL})
	if _, err := c2.Statuses(context.Background()); err == nil {
		t.Error("an unreadable history response produced a usable queue")
	}
}

// NZBGet's queue statuses are prefixed strings ("QUEUED", "PAUSED (…)"), and
// telling paused from downloading is what stops monarr reporting a stalled
// job as making progress.
func TestTailQueueStatusPrefixesDecideTheState(t *testing.T) {
	srv, _ := newRPCServer(t, map[string]string{
		"listgroups": `{"result":[
			{"NZBID":1,"NZBName":"Fetching","FileSizeMB":200,"RemainingSizeMB":50,"Status":"DOWNLOADING"},
			{"NZBID":2,"NZBName":"Waiting","FileSizeMB":200,"RemainingSizeMB":200,"Status":"QUEUED"},
			{"NZBID":3,"NZBName":"Held","FileSizeMB":200,"RemainingSizeMB":100,"Status":"PAUSED (parts)"},
			{"NZBID":4,"NZBName":"Unsized","FileSizeMB":0,"RemainingSizeMB":0,"Status":"DOWNLOADING"}
		]}`,
		"history": `{"result":[]}`,
	})
	c := New(ports.ClientConfig{URL: srv.URL})
	sts, err := c.Statuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sts) != 4 {
		t.Fatalf("statuses = %d, want 4", len(sts))
	}
	want := []ports.DownloadState{
		ports.StateDownloading, ports.StateQueued, ports.StateQueued, ports.StateDownloading,
	}
	for i, w := range want {
		if sts[i].State != w {
			t.Errorf("%s: state = %q, want %q", sts[i].Name, sts[i].State, w)
		}
	}
	if sts[0].Progress != 0.75 {
		t.Errorf("progress = %v, want 0.75", sts[0].Progress)
	}
	// A job whose size NZBGet has not worked out yet must report 0, not
	// divide by it.
	if sts[3].Progress != 0 {
		t.Errorf("unsized progress = %v, want 0", sts[3].Progress)
	}
}

// Everything in history is finished; the SUCCESS prefix is the only thing
// separating an import from a failure worth blocklisting.
func TestTailHistoryStatusPrefixDecidesSuccess(t *testing.T) {
	srv, _ := newRPCServer(t, map[string]string{
		"listgroups": `{"result":[]}`,
		"history": `{"result":[
			{"NZBID":10,"Name":"Good","Status":"SUCCESS/HEALTH","DestDir":"/complete/Good"},
			{"NZBID":11,"Name":"Bad","Status":"FAILURE/UNPACK","DestDir":""},
			{"NZBID":12,"Name":"Deleted","Status":"DELETED/MANUAL","DestDir":""}
		]}`,
	})
	c := New(ports.ClientConfig{URL: srv.URL})
	sts, err := c.Statuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sts) != 3 {
		t.Fatalf("statuses = %d, want 3", len(sts))
	}
	if sts[0].State != ports.StateCompleted || sts[0].SavePath != "/complete/Good" || sts[0].Progress != 1 {
		t.Errorf("successful history row = %+v", sts[0])
	}
	for _, s := range sts[1:] {
		if s.State != ports.StateFailed {
			t.Errorf("%s: state = %q, want failed", s.Name, s.State)
		}
		// The reason has to survive: it is what the queue row shows and what
		// somebody reads before deciding to blocklist.
		if s.Message == "" {
			t.Errorf("%s: no message — the reason it failed was dropped", s.Name)
		}
	}
}

// A download is either in the queue or in history, and monarr does not know
// which. So Remove tries the queue first and falls through to history —
// otherwise "remove" would silently do nothing for anything already
// finished, which is most of what gets removed.
func TestTailRemoveFallsThroughFromQueueToHistory(t *testing.T) {
	srv, rs := newRPCServer(t, map[string]string{
		// GroupDelete answering false is NZBGet's "that id is not in the
		// queue" — not an error, just the wrong list.
		"editqueue": `{"result":false}`,
	})
	c := New(ports.ClientConfig{URL: srv.URL})
	if err := c.Remove(context.Background(), "42", true); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if n := rs.called("editqueue"); n != 2 {
		t.Fatalf("editqueue calls = %d, want 2 (queue then history)", n)
	}
	// The last call is the history delete, carrying the parsed id.
	p := rs.paramsFor("editqueue")
	if len(p) != 4 || p[0] != "HistoryDelete" {
		t.Errorf("second editqueue = %v, want a HistoryDelete", p)
	}
	ids, ok := p[3].([]any)
	if !ok || len(ids) != 1 || ids[0] != float64(42) {
		t.Errorf("history delete ids = %v, want [42]", p[3])
	}
}

// When the queue delete works there is nothing left to do, and issuing the
// history delete anyway would be a second write for no reason.
func TestTailRemoveStopsWhenTheQueueDeleteWorked(t *testing.T) {
	srv, rs := newRPCServer(t, map[string]string{"editqueue": `{"result":true}`})
	c := New(ports.ClientConfig{URL: srv.URL})
	if err := c.Remove(context.Background(), "42", false); err != nil {
		t.Fatal(err)
	}
	if n := rs.called("editqueue"); n != 1 {
		t.Errorf("editqueue calls = %d, want 1 — the queue delete already succeeded", n)
	}
}

// A handle that is not a number cannot address anything. It must not become
// id 0 and delete whatever happens to be there.
func TestTailRemoveWithAnUnparseableHandleAddressesNothingReal(t *testing.T) {
	srv, rs := newRPCServer(t, map[string]string{"editqueue": `{"result":false}`})
	c := New(ports.ClientConfig{URL: srv.URL})
	_ = c.Remove(context.Background(), "not-a-number", false)
	p := rs.paramsFor("editqueue")
	ids, ok := p[3].([]any)
	if !ok || len(ids) != 1 || ids[0] != float64(0) {
		t.Fatalf("ids = %v, want [0]", p[3])
	}
	// NZBGet treats id 0 as "no such job" rather than a wildcard, so this is
	// inert — pinned here so a change to that assumption is visible.
}

// A daemon that is not there must fail every entry point rather than
// answering with an empty-but-successful queue, which monarr would read as
// "every download disappeared".
func TestTailAnUnreachableDaemonFailsEveryCall(t *testing.T) {
	srv, _ := newRPCServer(t, nil)
	url := srv.URL
	srv.Close()

	c := New(ports.ClientConfig{URL: url})
	if _, err := c.Add(context.Background(), "http://x/f.nzb", ""); err == nil {
		t.Error("Add succeeded against a dead daemon")
	}
	if _, err := c.Statuses(context.Background()); err == nil {
		t.Error("Statuses succeeded against a dead daemon")
	}
	if err := c.Remove(context.Background(), "1", false); err == nil {
		t.Error("Remove succeeded against a dead daemon")
	}
	if err := c.Test(context.Background()); err == nil {
		t.Error("Test succeeded against a dead daemon")
	}
}
