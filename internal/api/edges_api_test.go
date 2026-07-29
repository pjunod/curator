package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/monarr-media/monarr/internal/app/discover"
	"github.com/monarr-media/monarr/internal/app/health"
	"github.com/monarr-media/monarr/internal/domain"
	"github.com/monarr-media/monarr/internal/infra/bus"
	"github.com/monarr-media/monarr/internal/ports"
)

// The edges of the API: the guard in front of it, the stream out of it, the
// shell under it, and the small handlers nobody writes a test for until one
// of them 500s in front of a user.
//
// These are the files the wired harness made reachable but nothing yet
// walked: auth.go, sse.go, spa.go, metrics.go, callers.go,
// discover_handlers.go and handlers.go. Each test below names the failure it
// prevents rather than the line it covers — coverage is the by-product.

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// edgeDo issues a request with arbitrary headers, which the env helpers
// deliberately do not do — the guard, the caller registry and the session
// cookie are all header-shaped, so they cannot be exercised without one.
func edgeDo(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// edgeLockDown turns auth on the way the settings page does: credentials
// first, then the flag. Optionally seeds the API key main writes at startup.
func edgeLockDown(t *testing.T, e *apiEnv, apiKey string) {
	t.Helper()
	if apiKey != "" {
		e.setting(t, APIKeySetting, apiKey)
	}
	e.put(t, "/api/v1/settings",
		`{"authUsername":"edge","authPassword":"s3cret","authRequired":true}`).
		expect(t, http.StatusNoContent)
}

func edgeQuietLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// ---------------------------------------------------------------------------
// auth.go — the guard
// ---------------------------------------------------------------------------

// Turning auth on with nothing to authenticate with locks everybody out of a
// self-hosted app with no recovery path but editing the database by hand.
// The refusal has to leave the setting alone as well as return 400: a 400
// that still wrote "true" would be the same lockout with a nicer message.
func TestEdgeAuthRefusesToLockTheDoorWithNoKeyUnderIt(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.put(t, "/api/v1/settings", `{"authRequired":true}`).expect(t, http.StatusBadRequest)
	if !strings.Contains(strings.ToLower(rr.Body.String()), "password") {
		t.Errorf("the refusal must say what is missing: %s", rr.Body.String())
	}

	// An unset key reads back as an error from the store, which is itself
	// the wanted outcome: nothing was written.
	if got, err := e.db.GetMeta(context.Background(), AuthRequiredSetting); err == nil && got == "true" {
		t.Fatal("auth was enabled anyway — the install is now unreachable")
	}
	// And the door is demonstrably still open.
	e.get(t, "/api/v1/system/status").expect(t, http.StatusOK)
}

// What the guard must NOT cover. The login route is how you get a session, so
// gating it is a deadlock; the SPA shell is how the login page loads at all;
// and /metrics is scraped by a monitoring agent that has no session and,
// being outside /api/v1, was never meant to need one.
func TestEdgeAuthGuardLetsTheShellLoginAndMetricsThrough(t *testing.T) {
	e := newAPIEnv(t)
	edgeLockDown(t, e, "edge-key")

	// Control: the API itself is shut.
	e.get(t, "/api/v1/system/status").expect(t, http.StatusUnauthorized)

	// The login route reaches its handler — a malformed body gets the
	// handler's 400, not the guard's 401.
	e.post(t, "/api/v1/auth/login", `{not json`).expect(t, http.StatusBadRequest)

	// The shell still loads, or there is no login page to log in from.
	rr := e.get(t, "/").expect(t, http.StatusOK)
	if !strings.Contains(rr.Header().Get("Content-Type"), "text/html") {
		t.Errorf("the shell is not HTML: %q", rr.Header().Get("Content-Type"))
	}

	t.Setenv("MONARR_METRICS", "true")
	e.get(t, "/metrics").expect(t, http.StatusOK)
}

// A wrong key is not "no key": the comparison is constant-time and easy to
// invert by accident, and an install whose gate accepts any non-empty header
// looks exactly like a working one from the outside.
func TestEdgeAPIKeyMustActuallyMatch(t *testing.T) {
	e := newAPIEnv(t)
	edgeLockDown(t, e, "edge-key")

	for name, headers := range map[string]map[string]string{
		"wrong key":  {"X-Api-Key": "not-the-key"},
		"empty key":  {"X-Api-Key": ""},
		"key prefix": {"X-Api-Key": "edge-ke"},
	} {
		if rr := edgeDo(t, e.h, http.MethodGet, "/api/v1/system/status", "", headers); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rr.Code)
		}
	}
	if rr := e.get(t, "/api/v1/system/status?apikey=not-the-key"); rr.Code != http.StatusUnauthorized {
		t.Errorf("wrong query key: status = %d, want 401", rr.Code)
	}
	// Control: the real key opens it, so the assertions above are about the
	// value and not about the plumbing.
	if rr := edgeDo(t, e.h, http.MethodGet, "/api/v1/system/status", "",
		map[string]string{"X-Api-Key": "edge-key"}); rr.Code != http.StatusOK {
		t.Errorf("the real key was refused: %d %s", rr.Code, rr.Body.String())
	}
}

// With no API key stored, the header is not a credential at all. The guard
// reads the stored key first and must not treat "" == "" as a match, which
// is precisely what a naive string compare would do on a fresh install.
func TestEdgeAPIKeyHeaderIsWorthlessWhenNoKeyIsConfigured(t *testing.T) {
	e := newAPIEnv(t)
	edgeLockDown(t, e, "") // credentials, but no API key

	for _, key := range []string{"", "anything", "null"} {
		rr := edgeDo(t, e.h, http.MethodGet, "/api/v1/system/status", "",
			map[string]string{"X-Api-Key": key})
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("X-Api-Key %q was accepted with no key configured: %d", key, rr.Code)
		}
	}
}

// Login's error paths. "No credentials configured" and "bad credentials" are
// both 401 on purpose, but a broken body is the client's fault and must be a
// 400 — a 500 here would be reported as "the login page is down".
func TestEdgeLoginRefusesBadBodiesAndMissingCredentials(t *testing.T) {
	e := newAPIEnv(t)

	e.post(t, "/api/v1/auth/login", `{`).expect(t, http.StatusBadRequest)

	rr := e.post(t, "/api/v1/auth/login", `{"username":"edge","password":"s3cret"}`).
		expect(t, http.StatusUnauthorized)
	if !strings.Contains(rr.Body.String(), "no credentials") {
		t.Errorf("an install with no login configured should say so: %s", rr.Body.String())
	}

	// Configure credentials without enabling auth: login still works, which
	// is what lets somebody test their password before locking the door.
	e.put(t, "/api/v1/settings", `{"authUsername":"edge","authPassword":"s3cret"}`).
		expect(t, http.StatusNoContent)

	for name, body := range map[string]string{
		"wrong password": `{"username":"edge","password":"wrong"}`,
		"wrong username": `{"username":"nobody","password":"s3cret"}`,
		"empty both":     `{"username":"","password":""}`,
	} {
		if rr := e.post(t, "/api/v1/auth/login", body); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rr.Code)
		}
	}

	rr = e.post(t, "/api/v1/auth/login", `{"username":"edge","password":"s3cret"}`).
		expect(t, http.StatusNoContent)
	cookie := rr.Header().Get("Set-Cookie")
	// The session cookie is the whole browser-side credential: JavaScript
	// must not be able to read it, and it must not ride a cross-site POST.
	if !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Lax") {
		t.Errorf("session cookie = %q, want HttpOnly and SameSite=Lax", cookie)
	}
}

// A cookie value the server never issued must not open anything: sessions
// live in memory, so every restart turns yesterday's cookie into this.
func TestEdgeAnUnknownSessionCookieIsNotACredential(t *testing.T) {
	e := newAPIEnv(t)
	edgeLockDown(t, e, "edge-key")

	rr := edgeDo(t, e.h, http.MethodGet, "/api/v1/system/status", "",
		map[string]string{"Cookie": sessionCookie + "=deadbeefdeadbeef"})
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("a forged session was accepted: %d", rr.Code)
	}
}

// Logging out without a session must still clear the browser's cookie and
// answer 204. The branch matters because a logged-out tab that clicks Logout
// again is the ordinary case, not an exotic one.
func TestEdgeLogoutWithoutASessionStillClearsTheCookie(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.post(t, "/api/v1/auth/logout", "").expect(t, http.StatusNoContent)
	cookie := rr.Header().Get("Set-Cookie")
	if !strings.Contains(cookie, sessionCookie+"=;") && !strings.Contains(cookie, sessionCookie+"=\"\"") {
		t.Errorf("logout did not clear the cookie: %q", cookie)
	}
	if !strings.Contains(cookie, "Max-Age=0") {
		t.Errorf("the cleared cookie must expire immediately: %q", cookie)
	}
}

// The session store on its own: tokens are unique, expire on time, and a
// dropped token stays dropped. Expiry is only observable here — the TTL is
// thirty days, so no handler test can wait for it.
func TestEdgeSessionsExpireAndStayDropped(t *testing.T) {
	var s sessions

	a, b := s.create(), s.create()
	if a == b {
		t.Fatal("two sessions got the same token — one login would log everybody in")
	}
	if !s.valid(a) || !s.valid(b) {
		t.Fatal("a freshly created session is not valid")
	}
	if s.valid("never-issued") {
		t.Error("an unknown token validated")
	}

	// Age one past its expiry: it must stop being accepted without anything
	// having to sweep it first.
	s.mu.Lock()
	s.m[a] = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	if s.valid(a) {
		t.Error("an expired session still opened the door")
	}

	s.drop(b)
	if s.valid(b) {
		t.Error("a dropped session survived logout")
	}
}

// ---------------------------------------------------------------------------
// metrics.go
// ---------------------------------------------------------------------------

// The acquisition gauges only exist when an acquisition service is wired, so
// nothing reached them until the harness wired one. They are the data-plane
// half of the exposition — the pipeline cannot be joined in Prometheus from
// Monarr's end without them.
func TestEdgeMetricsExposeTheAcquisitionGauges(t *testing.T) {
	e := newAPIEnv(t)
	e.addMovie(t)

	// "1" is as valid a truthy value as "true", and a scrape that 404s
	// because the operator wrote 1 is a support ticket.
	t.Setenv("MONARR_METRICS", "1")

	rr := e.get(t, "/metrics").expect(t, http.StatusOK)
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain; version=0.0.4") {
		t.Errorf("content-type = %q — Prometheus needs the exposition version", ct)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`monarr_media_items{kind="movie"} 1`,
		`monarr_media_items{kind="series"} 0`,
		"monarr_queue_active 0",
		"monarr_wanted_total ",
		`monarr_transfers_in_flight{stage="downloading"} 0`,
		`monarr_transfers_in_flight{stage="importing"} 0`,
		`monarr_transfers_in_flight{stage="notifying"} 0`,
		"monarr_transfer_bytes_moved 0",
		"monarr_transfer_bytes_total 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
	// Every series needs its HELP/TYPE preamble or Prometheus files the
	// metric as untyped and the graphs are wrong rather than absent.
	for _, want := range []string{"# TYPE monarr_queue_active gauge", "# TYPE monarr_transfers_in_flight gauge"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

// Anything other than 1/true keeps the endpoint dark. Opt-in means opt-in:
// an install that never asked for metrics must not publish its library size
// on an unauthenticated path.
func TestEdgeMetricsStayDarkUnlessAskedFor(t *testing.T) {
	e := newAPIEnv(t)
	for _, v := range []string{"", "no", "0", "yes", "TRUE"} {
		t.Setenv("MONARR_METRICS", v)
		rr := e.get(t, "/metrics")
		want := http.StatusNotFound
		if strings.EqualFold(v, "true") {
			want = http.StatusOK
		}
		if rr.Code != want {
			t.Errorf("MONARR_METRICS=%q: status = %d, want %d", v, rr.Code, want)
		}
	}
}

// ---------------------------------------------------------------------------
// callers.go — the inbound half of the Connections panel
// ---------------------------------------------------------------------------

// The panel shows one row per application, so the name has to be the product
// token and browsers have to be dropped: one row per Chrome version buries
// the row that matters.
func TestEdgeProductTokenNamesTheApplication(t *testing.T) {
	long := strings.Repeat("x", 60)
	for agent, want := range map[string]string{
		"plurx/0.4.1 (linux; amd64)":  "plurx",
		"plurx/0.4.1":                 "plurx",
		"Prowlarr":                    "Prowlarr",
		"  Bazarr/1.4\tfoo":           "Bazarr",
		"Mozilla/5.0 (X11) Chrome/14": "",
		"chrome/140":                  "",
		"":                            "",
		"   ":                         "",
		long + "/1.0":                 long[:40],
		"/leading-slash":              "/leading-slash",
	} {
		if got := productToken(agent); got != want {
			t.Errorf("productToken(%q) = %q, want %q", agent, got, want)
		}
	}
}

// One caller is one row however often it calls, and the newest caller sorts
// first — the panel is read top-down when somebody is asking "is it talking
// to me right now".
func TestEdgeCallerRegistryCollapsesRepeatCallsIntoOneRow(t *testing.T) {
	reg := NewCallerRegistry()
	reg.Note("plurx/0.4.1", "/api/v1/calendar")
	reg.Note("plurx/0.4.1", "/api/v1/webhooks/plurx")
	time.Sleep(2 * time.Millisecond) // so LastSeen genuinely differs
	reg.Note("Prowlarr/1.2", "/api/v1/indexers")

	got := reg.List()
	if len(got) != 2 {
		t.Fatalf("callers = %+v, want one row each for plurx and Prowlarr", got)
	}
	if got[0].Name != "Prowlarr" {
		t.Errorf("most recent caller = %q, want Prowlarr first", got[0].Name)
	}
	for _, c := range got {
		if c.Name != "plurx" {
			continue
		}
		if c.Calls != 2 {
			t.Errorf("plurx calls = %d, want 2", c.Calls)
		}
		if c.LastPath != "/api/v1/webhooks/plurx" {
			t.Errorf("lastPath = %q, want the most recent call", c.LastPath)
		}
		if c.Agent != "plurx/0.4.1" {
			t.Errorf("agent = %q", c.Agent)
		}
	}
}

// Keyed off the API key, not the path: a session cookie is a person with a
// browser open, and a person is not a connection. The failure this prevents
// is the panel filling with the operator's own tabs.
func TestEdgeOnlyKeyedRequestsAreRecordedAsCallers(t *testing.T) {
	e := newAPIEnv(t)
	reg := e.srv.deps.Callers

	// A browser-shaped request: no key, so no row.
	edgeDo(t, e.h, http.MethodGet, "/api/v1/system/status", "",
		map[string]string{"User-Agent": "plurx/0.4.1"})
	if got := reg.List(); len(got) != 0 {
		t.Fatalf("an unkeyed request was recorded: %+v", got)
	}

	// Both key forms count, and the key does not have to be the right one —
	// noting who called is not the same question as letting them in.
	edgeDo(t, e.h, http.MethodGet, "/api/v1/system/status", "",
		map[string]string{"User-Agent": "plurx/0.4.1", "X-Api-Key": "whatever"})
	edgeDo(t, e.h, http.MethodGet, "/api/v1/library?apikey=whatever", "",
		map[string]string{"User-Agent": "plurx/0.4.1"})

	got := reg.List()
	if len(got) != 1 || got[0].Name != "plurx" || got[0].Calls != 2 {
		t.Fatalf("callers = %+v, want plurx with 2 calls", got)
	}
	if got[0].LastPath != "/api/v1/library" {
		t.Errorf("lastPath = %q, want the path it last called", got[0].LastPath)
	}

	// The SPA is not the API: a browser fetching the shell with a key in the
	// query string is still not a connection.
	edgeDo(t, e.h, http.MethodGet, "/library?apikey=whatever", "",
		map[string]string{"User-Agent": "someapp/1.0"})
	for _, c := range reg.List() {
		if c.Name == "someapp" {
			t.Error("a request for the web UI was recorded as an application calling in")
		}
	}
}

// ---------------------------------------------------------------------------
// spa.go
// ---------------------------------------------------------------------------

// Deep links and refreshes have to reach the client router, so any unknown
// path returns the shell — and it must not be cached, or a deploy leaves
// browsers holding an index.html that references assets that no longer exist.
func TestEdgeSPADeepLinksFallBackToTheShell(t *testing.T) {
	e := newAPIEnv(t)

	for _, path := range []string{"/library/42", "/settings/indexers", "/assets/does-not-exist.js"} {
		rr := e.get(t, path).expect(t, http.StatusOK)
		if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("%s: content-type = %q", path, ct)
		}
		if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: cache-control = %q, want no-store", path, cc)
		}
		body := rr.Body.String()
		if !strings.Contains(strings.ToLower(body), "monarr") {
			t.Errorf("%s: body is not the shell: %q", path, body)
		}
		// A binary built without `make web` must still explain itself rather
		// than serving a blank page — that is what the fallback page is for.
		if !UIBuilt() && !strings.Contains(body, "built without the web UI") {
			t.Errorf("%s: no UI is embedded, so the fallback page should say so: %q", path, body)
		}
	}
}

// A file that really is in the embedded dist is served as itself, not
// swallowed by the SPA fallback. dist always contains at least .gitkeep (see
// web/embed.go), so this branch is reachable whether or not the UI was built.
func TestEdgeSPAServesEmbeddedFilesRatherThanTheShell(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.get(t, "/.gitkeep").expect(t, http.StatusOK)
	if cc := rr.Header().Get("Cache-Control"); cc == "no-store" {
		t.Error("the request fell through to the SPA fallback instead of serving the file")
	}
	if body := rr.Body.String(); strings.Contains(strings.ToLower(body), "<!doctype html") {
		t.Errorf("a real file was answered with the shell: %q", body)
	}
}

// Browsers only offer "install" when the manifest arrives as
// application/manifest+json; served as text/plain the PWA silently is not
// one, with nothing in any log to say why.
func TestEdgeWebmanifestHasItsOwnMIMEType(t *testing.T) {
	if got := mime.TypeByExtension(".webmanifest"); !strings.Contains(got, "application/manifest+json") {
		t.Errorf("mime for .webmanifest = %q, want application/manifest+json", got)
	}
}

// ---------------------------------------------------------------------------
// sse.go
// ---------------------------------------------------------------------------

// edgeEvent is this file's own bus event, so what arrives on the stream can
// only have come from the publish below.
type edgeEvent struct {
	Msg string `json:"msg"`
}

func (edgeEvent) EventType() string { return "edge.stream.event" }

// edgeStreamLines reads the response body line by line into a channel and
// stops on context cancellation. Every read in these tests is bounded: a
// hung SSE test takes the whole package's timeout with it.
func edgeStreamLines(ctx context.Context, body io.Reader) <-chan string {
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(body)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	return lines
}

// edgeNextLine takes the next non-blank line or fails the test. Blank lines
// are the SSE frame separators and carry nothing.
func edgeNextLine(t *testing.T, lines <-chan string, why string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line, open := <-lines:
			if !open {
				t.Fatalf("the stream ended while waiting for %s", why)
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			return line
		case <-deadline:
			t.Fatalf("timed out waiting for %s", why)
			return ""
		}
	}
}

// The stream is the UI's only live channel: the queue, the scan and every
// toast come down it. This checks the whole contract — proxy-defeating
// headers, the reconnect hint, and one real bus event arriving as a framed
// data line — because a stream that opens and then says nothing looks
// identical to a working one until somebody grabs a release.
func TestEdgeEventStreamDeliversABusEventToAConnectedClient(t *testing.T) {
	e := newAPIEnv(t)
	ts := httptest.NewServer(e.h)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

	for header, want := range map[string]string{
		"Content-Type":  "text/event-stream",
		"Cache-Control": "no-cache",
		// nginx buffers text/event-stream by default, which turns a live
		// stream into a batch delivered at disconnect.
		"X-Accel-Buffering": "no",
	} {
		if got := resp.Header.Get(header); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	lines := edgeStreamLines(ctx, resp.Body)

	// The retry hint is written and flushed before anything else, so
	// receiving it also proves the subscription is registered — publishing
	// only after it removes the sleep this test would otherwise need.
	if got := edgeNextLine(t, lines, "the retry preamble"); got != "retry: 3000" {
		t.Fatalf("first line = %q, want the reconnect hint", got)
	}

	e.bus.Publish(edgeEvent{Msg: "hello"})

	line := edgeNextLine(t, lines, "the published event")
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("line = %q, want an SSE data frame", line)
	}
	var env struct {
		Type    string    `json:"type"`
		TS      time.Time `json:"ts"`
		Payload struct {
			Msg string `json:"msg"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env); err != nil {
		t.Fatalf("bad envelope %q: %v", line, err)
	}
	if env.Type != "edge.stream.event" {
		t.Errorf("type = %q, want the event's own type — the client dispatches on it", env.Type)
	}
	if env.Payload.Msg != "hello" {
		t.Errorf("payload = %+v, want the published event verbatim", env.Payload)
	}
	if env.TS.IsZero() {
		t.Error("the envelope carries no timestamp")
	}
}

// When the bus closes the handler must return rather than sit on the
// connection: a shutdown that leaks one goroutine per connected browser
// never finishes draining.
func TestEdgeEventStreamEndsWhenTheBusCloses(t *testing.T) {
	b := bus.New(edgeQuietLog())
	defer b.Close()
	srv := New(Deps{Log: edgeQuietLog(), Bus: b, DB: fakeDB{v: 1}, Version: "test"})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

	lines := edgeStreamLines(ctx, resp.Body)
	edgeNextLine(t, lines, "the retry preamble") // the stream is up

	b.Close()

	// The frame separator left over from the preamble may still be in the
	// pipe; anything with content in it is the handler still writing.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line, open := <-lines:
			if !open {
				return // the handler returned and the response ended
			}
			if strings.TrimSpace(line) != "" {
				t.Fatalf("the stream produced %q after the bus closed", line)
			}
		case <-deadline:
			t.Fatal("the stream did not end when the bus closed — the handler is stuck on a dead subscription")
		}
	}
}

// edgeUnflushableWriter is a ResponseWriter that cannot flush, which is what
// a middleware that wraps the writer without forwarding Flush produces.
type edgeUnflushableWriter struct {
	hdr    http.Header
	status int
	body   bytes.Buffer
}

func (w *edgeUnflushableWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}

func (w *edgeUnflushableWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func (w *edgeUnflushableWriter) WriteHeader(code int) { w.status = code }

// Without a flusher every frame would sit in a buffer, so the handler says so
// instead of opening a stream that appears to work and delivers nothing.
func TestEdgeEventStreamRefusesAWriterItCannotFlush(t *testing.T) {
	srv := New(Deps{Log: edgeQuietLog(), Bus: bus.New(edgeQuietLog())})
	w := &edgeUnflushableWriter{}

	srv.StreamEvents(w, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if w.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.status)
	}
	if !strings.Contains(w.body.String(), "streaming unsupported") {
		t.Errorf("body = %q, want it to name the problem", w.body.String())
	}
}

// ---------------------------------------------------------------------------
// discover_handlers.go
// ---------------------------------------------------------------------------

// edgeDiscoverProvider is a DiscoverProvider under the test's control. The
// real ones need a TMDB or Trakt key and a network, neither of which belongs
// in a handler test — the seam is the port, so the fake goes here.
type edgeDiscoverProvider struct {
	configured bool
	results    []ports.SearchResult
	err        error
	lastPage   int
}

func (p *edgeDiscoverProvider) Name() string { return "edgefake" }

func (p *edgeDiscoverProvider) Lists() []ports.DiscoverList {
	return []ports.DiscoverList{{
		ID: "edge-row", Title: "Edge row", Blurb: "what this row measures",
		Kind: domain.KindMovie, Source: "edgefake",
	}}
}

func (p *edgeDiscoverProvider) Configured(context.Context) bool { return p.configured }

func (p *edgeDiscoverProvider) Discover(_ context.Context, listID string, page int) ([]ports.SearchResult, error) {
	p.lastPage = page
	if p.err != nil {
		return nil, p.err
	}
	if listID != "edge-row" {
		return nil, ports.ErrUnknownList
	}
	return p.results, nil
}

// edgeServerWith builds a second server around the harness's real library and
// database, with a Discover service the test controls. The harness wires a
// provider-less Discover on purpose; this is how a test gets one that answers
// without editing the harness.
func edgeServerWith(e *apiEnv, providers ...ports.DiscoverProvider) http.Handler {
	return New(Deps{
		Log:      edgeQuietLog(),
		Bus:      e.bus,
		Health:   health.NewRegistry(nil),
		DB:       fakeDB{v: 2},
		Library:  e.lib,
		Discover: discover.New(edgeQuietLog(), nil, providers...),
		Store:    e.db,
		Settings: e.db,
		Version:  "test",
	}).Handler()
}

// An install with no Discover service at all still answers the catalogue,
// because "there is nothing to browse" is a true and complete answer that the
// UI renders as an empty state. Asking for the contents of a row that cannot
// exist is a different question and gets a 503.
func TestEdgeDiscoverWithoutAServiceStillAnswersTheCatalogue(t *testing.T) {
	h := New(Deps{
		Log: edgeQuietLog(), Health: health.NewRegistry(nil),
		DB: fakeDB{v: 1}, Version: "test",
	}).Handler()

	rr := edgeDo(t, h, http.MethodGet, "/api/v1/discover/lists", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("lists: status = %d", rr.Code)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
		t.Errorf("lists = %q, want an empty array — the UI maps over this", got)
	}

	rr = edgeDo(t, h, http.MethodGet, "/api/v1/discover/items?list=anything", "", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("items: status = %d, want 503", rr.Code)
	}
}

// A list id that was never on offer is the caller naming something wrong, so
// it is a 400. A 500 here would page somebody for a stale bookmark.
func TestEdgeDiscoverUnknownListIs400(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.get(t, "/api/v1/discover/items?list=no-such-row").expect(t, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "no-such-row") {
		t.Errorf("the error should name the list asked for: %s", rr.Body.String())
	}
	// Omitting the parameter entirely is also the caller's fault.
	e.get(t, "/api/v1/discover/items").expect(t, http.StatusBadRequest)
}

// A provider that exists but has no key is a configuration problem, not a
// bug: the row is dropped from the catalogue, and asking for its contents
// directly says what to go and do about it.
func TestEdgeDiscoverUnconfiguredProviderIs503(t *testing.T) {
	e := newAPIEnv(t)
	h := edgeServerWith(e, &edgeDiscoverProvider{
		configured: false,
		err:        ports.ErrProviderNotConfigured,
	})

	rr := edgeDo(t, h, http.MethodGet, "/api/v1/discover/lists", "", nil)
	if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
		t.Errorf("an unconfigured provider must contribute no rows: %q", got)
	}

	rr = edgeDo(t, h, http.MethodGet, "/api/v1/discover/items?list=edge-row", "", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Settings") {
		t.Errorf("the 503 should say where the key goes: %s", rr.Body.String())
	}
}

// Any other provider failure is genuinely ours to answer for: upstream being
// down is a 500, distinct from the 400 and 503 above so the UI can tell
// "you asked wrong" from "go and configure it" from "try again later".
func TestEdgeDiscoverProviderFailureIs500(t *testing.T) {
	e := newAPIEnv(t)
	h := edgeServerWith(e, &edgeDiscoverProvider{
		configured: true,
		err:        errors.New("tmdb: 502 bad gateway"),
	})

	rr := edgeDo(t, h, http.MethodGet, "/api/v1/discover/items?list=edge-row", "", nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", rr.Code, rr.Body.String())
	}
}

// The Add button is the point of the page, so every row says what is already
// in the library. Getting this wrong offers Add for something added — the add
// itself still refuses, so the only symptom is a button that lies.
func TestEdgeDiscoverMarksWhatIsAlreadyInTheLibrary(t *testing.T) {
	e := newAPIEnv(t)
	e.addMovie(t) // TMDB 550, the same id the first result carries

	p := &edgeDiscoverProvider{configured: true, results: []ports.SearchResult{
		{Kind: domain.KindMovie, TMDBID: 550, Title: "Fight Club", Year: 1999, Source: "edgefake"},
		{Kind: domain.KindMovie, TMDBID: 999, Title: "Not Added", Year: 2020},
		{Kind: domain.KindSeries, TMDBID: 1399, TVDBID: 121361, Title: "Some Series", Year: 2011},
	}}
	h := edgeServerWith(e, p)

	rr := edgeDo(t, h, http.MethodGet, "/api/v1/discover/lists", "", nil)
	if !strings.Contains(rr.Body.String(), `"id":"edge-row"`) {
		t.Fatalf("a configured provider's rows should be on offer: %s", rr.Body.String())
	}

	rr = edgeDo(t, h, http.MethodGet, "/api/v1/discover/items?list=edge-row", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("items: status = %d, body %s", rr.Code, rr.Body.String())
	}
	var got []struct {
		Kind      string `json:"kind"`
		TmdbID    int64  `json:"tmdbId"`
		TvdbID    *int64 `json:"tvdbId"`
		Title     string `json:"title"`
		Source    string `json:"source"`
		InLibrary bool   `json:"inLibrary"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %s: %v", rr.Body.String(), err)
	}
	if len(got) != 3 {
		t.Fatalf("results = %d, want 3", len(got))
	}
	if !got[0].InLibrary {
		t.Error("the movie already in the library is not marked, so the page offers to add it twice")
	}
	if got[1].InLibrary {
		t.Error("a movie that is not in the library is marked as added, so it cannot be added at all")
	}
	if got[2].TvdbID == nil || *got[2].TvdbID != 121361 {
		t.Errorf("tvdbId = %v, want the id the provider supplied", got[2].TvdbID)
	}
	if got[0].Source != "edgefake" {
		t.Errorf("source = %q, want the provider that supplied the row", got[0].Source)
	}
	if p.lastPage != 1 {
		t.Errorf("page = %d, want the first page when none is asked for", p.lastPage)
	}

	// Paging is 1-based and passes through; page 0 is nonsense and must not
	// reach the provider as 0 (page 0 is an off-by-one row at most providers).
	edgeDo(t, h, http.MethodGet, "/api/v1/discover/items?list=edge-row&page=2", "", nil)
	if p.lastPage != 2 {
		t.Errorf("page = %d, want 2", p.lastPage)
	}
	edgeDo(t, h, http.MethodGet, "/api/v1/discover/items?list=edge-row&page=0", "", nil)
	if p.lastPage < 1 {
		t.Errorf("page = %d, want a sane page for page=0", p.lastPage)
	}
}

// ---------------------------------------------------------------------------
// handlers.go — status, health, tasks
// ---------------------------------------------------------------------------

// The status payload is what somebody pastes into an issue, so every field
// has to be populated: a version with no commit, or an empty data dir, makes
// the report useless exactly when it is needed.
func TestEdgeSystemStatusDescribesThisRuntime(t *testing.T) {
	e := newAPIEnv(t)

	var got struct {
		AppName         string    `json:"appName"`
		Version         string    `json:"version"`
		Commit          string    `json:"commit"`
		GoVersion       string    `json:"goVersion"`
		Os              string    `json:"os"`
		Arch            string    `json:"arch"`
		DataDir         string    `json:"dataDir"`
		StartedAt       time.Time `json:"startedAt"`
		UptimeSeconds   int64     `json:"uptimeSeconds"`
		DbSchemaVersion int64     `json:"dbSchemaVersion"`
	}
	e.get(t, "/api/v1/system/status").expect(t, http.StatusOK).into(t, &got)

	if got.AppName != "Monarr" || got.Version != "test" || got.Commit != "cafebabe" {
		t.Errorf("build identity = %+v", got)
	}
	if got.GoVersion != runtime.Version() || got.Os != runtime.GOOS || got.Arch != runtime.GOARCH {
		t.Errorf("runtime identity = %+v", got)
	}
	if got.DataDir == "" {
		t.Error("dataDir is empty — the first question about any bug report is where the database is")
	}
	if got.DbSchemaVersion != 2 {
		t.Errorf("dbSchemaVersion = %d, want 2", got.DbSchemaVersion)
	}
	if got.StartedAt.IsZero() || got.UptimeSeconds < 59 {
		t.Errorf("startedAt = %v, uptime = %d", got.StartedAt, got.UptimeSeconds)
	}
}

// edgeBrokenDB cannot report its schema version — the shape of a database
// that is locked or mid-migration.
type edgeBrokenDB struct{}

func (edgeBrokenDB) SchemaVersion(context.Context) (int64, error) {
	return 0, errors.New("database is locked")
}

// The status page is the first thing somebody opens when the app is unwell,
// so it must not be the second thing that is broken: an unreadable schema
// version costs that one field and nothing else.
func TestEdgeSystemStatusSurvivesAnUnreadableSchemaVersion(t *testing.T) {
	h := New(Deps{
		Log: edgeQuietLog(), Health: health.NewRegistry(nil),
		DB: edgeBrokenDB{}, Version: "test", DataDir: t.TempDir(),
	}).Handler()

	rr := edgeDo(t, h, http.MethodGet, "/api/v1/system/status", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want the page to render anyway: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"dbSchemaVersion":0`) {
		t.Errorf("want the unknown version reported as 0: %s", rr.Body.String())
	}
}

// A registry with nothing in it reports ok and an empty array — never null,
// which the health page maps over. And a check with something to say must
// have it survive to the wire: a warning with no message is an amber badge
// nobody can act on.
func TestEdgeHealthReportsAnEmptyRegistryAndCarriesMessages(t *testing.T) {
	e := newAPIEnv(t)

	body := e.get(t, "/api/v1/health").expect(t, http.StatusOK).Body.String()
	if !strings.Contains(body, `"overall":"ok"`) {
		t.Errorf("an empty registry should be ok: %s", body)
	}
	if !strings.Contains(body, `"checks":[]`) {
		t.Errorf("checks must serialize as an array, not null: %s", body)
	}

	e.srv.deps.Health.Register("edge-warn", func(context.Context) health.Result {
		return health.Warn("the root folder %s is unwritable", "/data")
	})

	var got struct {
		Overall string `json:"overall"`
		Checks  []struct {
			Name      string    `json:"name"`
			Status    string    `json:"status"`
			Message   *string   `json:"message"`
			CheckedAt time.Time `json:"checkedAt"`
		} `json:"checks"`
	}
	e.get(t, "/api/v1/health").expect(t, http.StatusOK).into(t, &got)
	if got.Overall != "warning" {
		t.Errorf("overall = %q, want the worst check to win", got.Overall)
	}
	if len(got.Checks) != 1 {
		t.Fatalf("checks = %+v", got.Checks)
	}
	c := got.Checks[0]
	if c.Name != "edge-warn" || c.Status != "warning" {
		t.Errorf("check = %+v", c)
	}
	if c.Message == nil || !strings.Contains(*c.Message, "/data") {
		t.Errorf("the message must survive to the wire: %v", c.Message)
	}
	if c.CheckedAt.IsZero() {
		t.Error("a check with no timestamp cannot be told from a stale one")
	}
}

// Triggering a task is fire-and-forget (202, no body) and an unknown name is
// a 404 that names it — the UI shows that string, and "error" alone tells
// nobody which button did nothing.
func TestEdgeRunTaskAcceptsKnownWorkAndNamesUnknownWork(t *testing.T) {
	e := newAPIEnv(t)

	rr := e.post(t, "/api/v1/system/tasks/"+ScanTaskName+"/run", "").expect(t, http.StatusAccepted)
	if body := strings.TrimSpace(rr.Body.String()); body != "" {
		t.Errorf("a triggered task should answer with no body, got %q", body)
	}

	rr = e.post(t, "/api/v1/system/tasks/library.rekonsile/run", "").expect(t, http.StatusNotFound)
	if !strings.Contains(rr.Body.String(), "library.rekonsile") {
		t.Errorf("the 404 should name the task asked for: %s", rr.Body.String())
	}
}

// Triggering is fire-and-forget, so the task list is the only way to find out
// whether the work actually happened. A row that never fills in lastRunAt is
// a page that says "we ran it" and can never show that it did.
func TestEdgeTaskListReportsWhenATaskLastRan(t *testing.T) {
	e := newAPIEnv(t)
	e.post(t, "/api/v1/system/tasks/rss.sync/run", "").expect(t, http.StatusAccepted)

	type edgeTask struct {
		Name            string     `json:"name"`
		IntervalSeconds int64      `json:"intervalSeconds"`
		LastRunAt       *time.Time `json:"lastRunAt"`
		LastDurationMs  *int64     `json:"lastDurationMs"`
		NextRunAt       *time.Time `json:"nextRunAt"`
	}

	// The run is asynchronous, so poll — with a deadline, because a task that
	// never reports is exactly the failure under test.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var tasks []edgeTask
		e.get(t, "/api/v1/system/tasks").expect(t, http.StatusOK).into(t, &tasks)
		for _, task := range tasks {
			if task.Name != "rss.sync" || task.LastRunAt == nil {
				continue
			}
			if task.LastDurationMs == nil {
				t.Error("a task that has run must say how long it took")
			}
			if task.NextRunAt == nil {
				t.Error("a scheduled task must say when it runs next")
			}
			if task.IntervalSeconds != int64(time.Hour/time.Second) {
				t.Errorf("intervalSeconds = %d, want the registered interval", task.IntervalSeconds)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the task never reported a run, so the tasks page can never show one")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
