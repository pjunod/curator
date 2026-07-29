package deluge

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

// rpcServer is the harness the existing tests use, generalised: it decodes
// the JSON-RPC envelope, records the methods it was asked for, and answers
// from a per-method table. Anything unlisted logs in and returns null, which
// is what the real daemon does for a method with no interesting result.
type rpcServer struct {
	mu      sync.Mutex
	calls   []string
	params  map[string][]any
	answers map[string]string
	status  int
}

func newRPCServer(t *testing.T, answers map[string]string) (*httptest.Server, *rpcServer) {
	t.Helper()
	rs := &rpcServer{answers: answers, params: map[string][]any{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		rs.mu.Lock()
		rs.calls = append(rs.calls, req.Method)
		rs.params[req.Method] = req.Params
		status, answer := rs.status, rs.answers[req.Method]
		rs.mu.Unlock()

		if status != 0 && req.Method != "auth.login" {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"result":null,"error":null}`))
			return
		}
		if req.Method == "auth.login" {
			_, _ = w.Write([]byte(`{"result":true,"error":null}`))
			return
		}
		if answer == "" {
			answer = `{"result":null,"error":null}`
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

// Deluge answers a rejected password with result:false and no error object,
// so a client that only checked the error field would call a wrong password
// a successful login and then fail confusingly on the next call.
func TestTailRejectedPasswordIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":false,"error":null}`))
	}))
	defer srv.Close()

	c := New(ports.ClientConfig{URL: srv.URL, Password: "wrong"})
	err := c.Test(context.Background())
	if err == nil {
		t.Fatal("a rejected password reported success")
	}
	if !strings.Contains(err.Error(), "password rejected") {
		t.Errorf("err = %v, want it to name the password", err)
	}
}

// Test forces a fresh login on purpose: the whole point of pressing "test"
// is to find out whether the credentials work NOW, and a cached session
// would answer for the ones that worked an hour ago.
func TestTailTestForcesAFreshLogin(t *testing.T) {
	srv, rs := newRPCServer(t, nil)
	c := New(ports.ClientConfig{URL: srv.URL, Password: "hunter2"})

	if err := c.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := rs.called("auth.login"); n != 2 {
		t.Errorf("logins = %d, want 2 — Test reused a cached session", n)
	}
}

// A non-200 is reported with its code. The daemon behind a misconfigured
// reverse proxy is the common cause, and "unexpected status 502" is the only
// thing that tells the user where to look.
func TestTailHTTPStatusIsSurfaced(t *testing.T) {
	srv, rs := newRPCServer(t, nil)
	rs.mu.Lock()
	rs.status = http.StatusBadGateway
	rs.mu.Unlock()

	c := New(ports.ClientConfig{URL: srv.URL, Password: "hunter2"})
	_, err := c.Add(context.Background(), "http://x/t.torrent", "")
	if err == nil {
		t.Fatal("a 502 was treated as success")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v, want the status code in it", err)
	}
}

// A body that is not JSON at all — an HTML login page from a proxy is the
// usual one — must not be mistaken for an empty result.
func TestTailMalformedBodyIsAnError(t *testing.T) {
	srv, _ := newRPCServer(t, map[string]string{
		"core.get_torrents_status": `<html>sign in</html>`,
	})
	c := New(ports.ClientConfig{URL: srv.URL, Password: "hunter2"})
	_, err := c.Statuses(context.Background())
	if err == nil {
		t.Fatal("an HTML body was decoded as a status map")
	}
	if !strings.Contains(err.Error(), "bad response") {
		t.Errorf("err = %v, want it to say the response was bad", err)
	}
}

// The daemon's own error object is the message the user needs; wrapping it
// away would leave "something went wrong".
func TestTailDaemonErrorObjectIsPassedThrough(t *testing.T) {
	srv, _ := newRPCServer(t, map[string]string{
		"core.add_torrent_url": `{"result":null,"error":{"message":"Torrent already in session"}}`,
	})
	c := New(ports.ClientConfig{URL: srv.URL, Password: "hunter2"})
	_, err := c.Add(context.Background(), "http://x/t.torrent", "")
	if err == nil {
		t.Fatal("an RPC error object was treated as success")
	}
	if !strings.Contains(err.Error(), "Torrent already in session") {
		t.Errorf("err = %v, want the daemon's own message", err)
	}
}

// The Label plugin is optional, so labelling is attempted and its failure
// ignored: a category nobody can set is not a reason to lose a torrent that
// was added successfully.
func TestTailACategoryIsAppliedButItsFailureIsNotFatal(t *testing.T) {
	srv, rs := newRPCServer(t, map[string]string{
		"core.add_torrent_url": `{"result":"hash9","error":null}`,
		"label.set_torrent":    `{"result":null,"error":{"message":"Unknown method"}}`,
	})
	c := New(ports.ClientConfig{URL: srv.URL, Password: "hunter2"})

	h, err := c.Add(context.Background(), "http://x/t.torrent", "monarr")
	if err != nil {
		t.Fatalf("a missing Label plugin lost the torrent: %v", err)
	}
	if h != "hash9" {
		t.Errorf("handle = %q, want hash9", h)
	}
	if rs.called("label.set_torrent") != 1 {
		t.Error("the category was never applied")
	}
	if p := rs.paramsFor("label.set_torrent"); len(p) != 2 || p[0] != "hash9" || p[1] != "monarr" {
		t.Errorf("label params = %v, want [hash9 monarr]", p)
	}

	// No category means no call at all, rather than labelling with "".
	if _, err := c.Add(context.Background(), "http://x/u.torrent", ""); err != nil {
		t.Fatal(err)
	}
	if rs.called("label.set_torrent") != 1 {
		t.Error("an empty category still made a label call")
	}
}

// Deluge's state vocabulary is bigger than the three states monarr acts on,
// and the mapping is where a paused-but-finished torrent either gets
// imported or sits in the queue forever.
func TestTailDelugeStatesMapOntoMonarrStates(t *testing.T) {
	srv, _ := newRPCServer(t, map[string]string{
		"core.get_torrents_status": `{"result":{
			"h1":{"name":"Broken","progress":12.5,"state":"Error","save_path":"/dl","message":"missing files"},
			"h2":{"name":"Seeding","progress":100,"state":"Seeding","save_path":"/dl"},
			"h3":{"name":"Half","progress":50,"state":"Downloading","save_path":"/dl"},
			"h4":{"name":"Finishing","progress":100,"state":"Downloading","save_path":"/dl"},
			"h5":{"name":"Waiting","progress":0,"state":"Queued","save_path":"/dl"},
			"h6":{"name":"PausedDone","progress":100,"state":"Paused","save_path":"/dl"}
		},"error":null}`,
	})
	c := New(ports.ClientConfig{URL: srv.URL, Password: "hunter2"})

	sts, err := c.Statuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byHandle := map[ports.Handle]ports.DownloadStatus{}
	for _, s := range sts {
		byHandle[s.Handle] = s
	}
	if len(byHandle) != 6 {
		t.Fatalf("statuses = %d, want 6", len(byHandle))
	}
	want := map[ports.Handle]ports.DownloadState{
		"h1": ports.StateFailed,
		"h2": ports.StateCompleted,
		"h3": ports.StateDownloading,
		// 100% while still "Downloading" is the moment before the daemon
		// flips to Seeding; treating it as downloading would delay the
		// import by a poll for no reason.
		"h4": ports.StateCompleted,
		"h5": ports.StateQueued,
		// A finished torrent the user paused is still finished. Reporting it
		// as queued would leave it unimported forever.
		"h6": ports.StateCompleted,
	}
	for h, wantState := range want {
		if got := byHandle[h].State; got != wantState {
			t.Errorf("%s: state = %q, want %q", h, got, wantState)
		}
	}
	// Progress is reported 0..100 by Deluge and 0..1 by the port.
	if got := byHandle["h1"].Progress; got != 0.125 {
		t.Errorf("progress = %v, want 0.125 (Deluge's percentage scaled)", got)
	}
	// The save path a caller needs is the torrent's own directory, not the
	// download root every torrent shares.
	if got := byHandle["h2"].SavePath; got != "/dl/Seeding" {
		t.Errorf("save path = %q, want /dl/Seeding", got)
	}
	if got := byHandle["h1"].Message; got != "missing files" {
		t.Errorf("message = %q — the daemon's reason for failing was dropped", got)
	}
}

// Remove has to log in first (it is the first call on a fresh client) and
// pass the delete-data flag through, because getting that wrong either
// leaves orphaned data or deletes a file monarr just imported.
func TestTailRemovePassesTheDeleteDataFlag(t *testing.T) {
	srv, rs := newRPCServer(t, map[string]string{
		"core.remove_torrent": `{"result":true,"error":null}`,
	})
	c := New(ports.ClientConfig{URL: srv.URL, Password: "hunter2"})

	if err := c.Remove(context.Background(), "hash9", true); err != nil {
		t.Fatal(err)
	}
	if rs.called("auth.login") != 1 {
		t.Error("Remove did not log in first")
	}
	p := rs.paramsFor("core.remove_torrent")
	if len(p) != 2 || p[0] != "hash9" || p[1] != true {
		t.Errorf("remove params = %v, want [hash9 true]", p)
	}

	if err := c.Remove(context.Background(), "hash9", false); err != nil {
		t.Fatal(err)
	}
	if p := rs.paramsFor("core.remove_torrent"); p[1] != false {
		t.Errorf("remove params = %v, want the false flag preserved", p)
	}
}

// A daemon that is not there at all is a transport error, and every entry
// point has to fail rather than return an empty-but-successful answer — an
// empty status list reads as "nothing is downloading", which would retire
// every queue row monarr has.
func TestTailAnUnreachableDaemonFailsEveryCall(t *testing.T) {
	srv, _ := newRPCServer(t, nil)
	url := srv.URL
	srv.Close()

	c := New(ports.ClientConfig{URL: url, Password: "hunter2"})
	if _, err := c.Add(context.Background(), "http://x/t.torrent", ""); err == nil {
		t.Error("Add succeeded against a dead daemon")
	}
	if _, err := c.Statuses(context.Background()); err == nil {
		t.Error("Statuses succeeded against a dead daemon")
	}
	if err := c.Remove(context.Background(), "h", false); err == nil {
		t.Error("Remove succeeded against a dead daemon")
	}
	if err := c.Test(context.Background()); err == nil {
		t.Error("Test succeeded against a dead daemon")
	}
}
