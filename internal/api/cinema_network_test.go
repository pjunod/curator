package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type cinemaTransportFunc func(*http.Request) (*http.Response, error)

func (f cinemaTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func cinemaTLSClientForTest(t *testing.T, server *httptest.Server) {
	t.Helper()
	previous := cinemaHTTPClient
	client := *previous
	client.Transport = server.Client().Transport
	cinemaHTTPClient = &client
	t.Cleanup(func() { cinemaHTTPClient = previous })
}

func TestCinemaEndpointRejectsAmbiguousBasesAndPreservesPrefix(t *testing.T) {
	for _, base := range []string{"https://:443", "https://cinema.example?", "https://cinema.example?next=x", "https://cinema.example/#fragment", "https://user:secret@cinema.example", "http://cinema.example", "https:///path"} {
		if _, err := cinemaEndpoint(base, "info", nil); err == nil {
			t.Errorf("ambiguous base accepted: %s", base)
		}
	}
	if _, err := cinemaEndpoint("https://cinema.example", "../other", nil); err == nil {
		t.Fatal("caller-controlled operation accepted")
	}
	value, err := cinemaEndpoint("https://cinema.example/reverse%20proxy/", "info", url.Values{"clientId": {"client&next=other"}})
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	if target.Host != "cinema.example" || target.EscapedPath() != "/reverse%20proxy/api/v1/delegation/info" || target.Query().Get("clientId") != "client&next=other" || target.Query().Has("next") {
		t.Fatal("endpoint escaped its fixed prefix/query", value)
	}
	encoded, err := cinemaEndpoint("https://cinema.example/tenant%2F/", "exchange", nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(encoded)
	if err != nil || parsed.EscapedPath() != "/tenant%2F/api/v1/delegation/exchange" {
		t.Fatal("encoded prefix changed destination path", encoded, err)
	}
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/cinema/api/v1/delegation/introspect" {
			t.Error("reverse proxy prefix lost", r.URL.Path)
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	cinemaTLSClientForTest(t, server)
	if err = cinemaCall(context.Background(), domain.CinemaConnection{BaseURL: server.URL + "/cinema"}, "introspect", map[string]string{"grant": "fixture"}, nil); err != nil {
		t.Fatal("valid self-hosted TLS destination refused", err)
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected outbound count", calls.Load())
	}
	for _, base := range []string{"https://:443", "https://cinema.example?"} {
		if err = cinemaCall(context.Background(), domain.CinemaConnection{BaseURL: base}, "introspect", nil, nil); err == nil {
			t.Fatal("stored/revocation base bypassed validation")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("malformed base caused outbound IO")
	}
}
func TestCinemaConnectionRequiresLocalOwnerProofBeforeOutboundIO(t *testing.T) {
	h, db, owner := householdHTTPAccount(t)
	invitation := householdHTTP(t, h, "POST", "/api/v1/household/invitations", owner, map[string]string{"password": householdTestPassword})
	var invite struct{ Token string }
	if err := json.Unmarshal(invitation.Body.Bytes(), &invite); err != nil {
		t.Fatal(err)
	}
	response := householdHTTP(t, h, "POST", "/api/v1/auth/invitations/accept", "", map[string]string{"token": invite.Token, "username": "member", "displayName": "Member", "password": householdTestPassword})
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	member := householdHTTPLogin(t, h, "member", householdTestPassword)
	previous := cinemaHTTPClient
	client := *previous
	var calls atomic.Int64
	client.Transport = cinemaTransportFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected outbound request")
	})
	cinemaHTTPClient = &client
	defer func() { cinemaHTTPClient = previous }()
	body := map[string]string{"password": householdTestPassword, "baseUrl": "https://cinema.example", "clientId": strings.Repeat("a", 32), "clientSecret": strings.Repeat("b", 64), "redirect": "https://curator.example/api/v1/auth/cinema/callback"}
	for _, tc := range []struct {
		token, password string
		status          int
	}{{"", householdTestPassword, 401}, {member, householdTestPassword, 403}, {owner, "wrong-password", 401}} {
		body["password"] = tc.password
		rr := householdHTTP(t, h, "PUT", "/api/v1/auth/cinema/connection", tc.token, body)
		if rr.Code != tc.status {
			t.Fatal("unexpected authority response", rr.Code, rr.Body.String())
		}
	}
	body["password"] = householdTestPassword
	account, err := db.AccountByUsername(context.Background(), "owner")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "https://curator.example/api/v1/auth/cinema/connection", strings.NewReader(string(raw)))
	req = req.WithContext(context.WithValue(req.Context(), principalContextKey{}, principal{Account: account, Method: "cinema", Authenticated: true}))
	rr := httptest.NewRecorder()
	(&Server{deps: Deps{Store: db}}).SaveCinemaConnection(rr, req)
	if rr.Code != 403 {
		t.Fatal("integration-derived owner chose outbound destination", rr.Code, rr.Body.String())
	}
	body["password"] = householdTestPassword
	for _, base := range []string{"https://:443", "https://cinema.example?"} {
		body["baseUrl"] = base
		rr := householdHTTP(t, h, "PUT", "/api/v1/auth/cinema/connection", owner, body)
		if rr.Code != 400 {
			t.Fatal("malformed destination reached IO", rr.Code, rr.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized or malformed configuration reached outbound IO", calls.Load())
	}
}
func TestCinemaRedirectNeverReachesSecondOriginOrForwardsSecrets(t *testing.T) {
	var secondCalls, firstCalls atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(204)
	}))
	defer second.Close()
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		status := http.StatusFound
		if r.Method == http.MethodPost {
			status = http.StatusTemporaryRedirect
		}
		http.Redirect(w, r, second.URL+"/capture", status)
	}))
	defer first.Close()
	cinemaTLSClientForTest(t, first)
	endpoint, err := cinemaEndpoint(first.URL, "info", url.Values{"clientId": {"registered-client"}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := cinemaHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 302 {
		t.Fatal("info redirect was followed", res.StatusCode)
	}
	if err = cinemaCall(context.Background(), domain.CinemaConnection{BaseURL: first.URL}, "exchange", map[string]string{"clientSecret": "never-forward-this-secret", "grant": "never-forward-this-grant"}, nil); err == nil {
		t.Fatal("secret-bearing redirect was accepted")
	}
	if firstCalls.Load() != 2 || secondCalls.Load() != 0 {
		t.Fatal("redirect reached another origin", firstCalls.Load(), secondCalls.Load())
	}
}

func TestCinemaUnavailableDependenciesAndInvalidBodiesCannotIssueIdentity(t *testing.T) {
	missing := New(Deps{})
	for _, test := range []struct {
		name   string
		status int
		run    func(http.ResponseWriter, *http.Request)
	}{
		{"start", 503, missing.StartCinemaSignIn}, {"browser", 503, missing.OpenCinemaBrowser}, {"callback", 303, missing.CinemaCallback}, {"native result", 503, missing.FinishCinemaNative}, {"local session", 503, missing.CreateAccountSession}, {"identity", 401, missing.GetCurrentAccount},
	} {
		t.Run(test.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "https://curator.example/", strings.NewReader(`{}`))
			test.run(rr, req)
			householdExpect(t, rr, test.status)
			for _, c := range rr.Result().Cookies() {
				if c.Name == sessionCookie && c.Value != "" {
					t.Fatal("unavailable storage issued identity")
				}
			}
		})
	}
	h, _, owner := householdHTTPAccount(t)
	for _, route := range []struct{ method, path string }{
		{"PUT", "/api/v1/auth/cinema/connection"}, {"POST", "/api/v1/auth/cinema/link"}, {"DELETE", "/api/v1/auth/cinema/link"}, {"POST", "/api/v1/auth/cinema/native"}, {"POST", "/api/v1/auth/cinema/native/result"}, {"POST", "/api/v1/auth/sessions"}, {"POST", "/api/v1/household/invitations"}, {"POST", "/api/v1/auth/invitations/accept"}, {"PUT", "/api/v1/auth/account"},
	} {
		req := httptest.NewRequest(route.method, "https://curator.example"+route.path, strings.NewReader(`{"unexpected":"must not be ignored"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+owner)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		householdExpect(t, rr, 400)
	}
}

func TestCinemaConfigurationVerificationFailuresPreserveStandaloneAccess(t *testing.T) {
	for _, scenario := range []string{"network unavailable", "wrong registered callback", "rejected client credentials", "malformed introspection"} {
		t.Run(scenario, func(t *testing.T) {
			h, db, owner := householdHTTPAccount(t)
			fixture := newCinemaFixture(t)
			client := *cinemaHTTPClient
			transport := client.Transport
			var introspections atomic.Int64
			client.Transport = cinemaTransportFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/v1/delegation/info" {
					if scenario == "network unavailable" {
						return nil, errors.New("fixture transport unavailable")
					}
					if scenario == "wrong registered callback" {
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"issuer":"stable-cinema-issuer","scope":"household-requests","client":{"id":"registered-curator-client","redirect":"https://wrong.example/api/v1/auth/cinema/callback"}}`))}, nil
					}
				}
				if r.URL.Path == "/api/v1/delegation/introspect" {
					introspections.Add(1)
					code, body := http.StatusUnauthorized, `{"active":false}`
					if scenario == "malformed introspection" {
						code = http.StatusOK
						body = `{"active":false} {}`
					}
					return &http.Response{StatusCode: code, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
				}
				return transport.RoundTrip(r)
			})
			cinemaHTTPClient = &client
			rr := householdHTTP(t, h, "PUT", "/api/v1/auth/cinema/connection", owner, map[string]string{"password": householdTestPassword, "baseUrl": fixture.server.URL, "clientId": fixture.client, "clientSecret": fixture.secret, "redirect": "https://curator.example/api/v1/auth/cinema/callback"})
			status := 409
			if scenario == "network unavailable" {
				status = 503
			}
			householdExpect(t, rr, status)
			if scenario == "wrong registered callback" && introspections.Load() != 0 {
				t.Fatal("mismatched callback reached credential verification")
			}
			if _, err := db.CinemaConnection(context.Background()); !errors.Is(err, sqlite.ErrNotFound) {
				t.Fatal("failed verification persisted integration authority", err)
			}
			householdExpect(t, householdHTTP(t, h, "GET", "/api/v1/system/status", owner, nil), 200)
			info := householdResult[map[string]any](t, householdHTTP(t, h, "GET", "/api/v1/auth/info", "", nil), 200)
			if info["cinema"] != false {
				t.Fatal("failed integration advertised sign-in", info)
			}
		})
	}
}

func TestCinemaInitiationRequiresHTTPSAndExactBoundCallback(t *testing.T) {
	h, db, owner := householdHTTPAccount(t)
	fixture := newCinemaFixture(t)
	householdExpect(t, householdHTTP(t, h, "PUT", "/api/v1/auth/cinema/connection", owner, map[string]string{"password": householdTestPassword, "baseUrl": fixture.server.URL, "clientId": fixture.client, "clientSecret": fixture.secret, "redirect": "https://curator.example/api/v1/auth/cinema/callback"}), 204)
	info := householdResult[map[string]any](t, householdHTTP(t, h, "GET", "/api/v1/auth/info", "", nil), 200)
	if info["cinema"] != true {
		t.Fatal("verified optional integration not advertised", info)
	}
	req := httptest.NewRequest("POST", "http://curator.example/api/v1/auth/cinema/start", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	householdExpect(t, rr, 400)

	var exchanges atomic.Int64
	previous := cinemaHTTPClient
	client := *previous
	transport := client.Transport
	client.Transport = cinemaTransportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/delegation/exchange" {
			exchanges.Add(1)
		}
		return transport.RoundTrip(r)
	})
	cinemaHTTPClient = &client
	// Each rejected callback starts from an otherwise live, bound handoff.
	state, cookie := cinemaStart(t, h, "/api/v1/auth/cinema/link", owner, map[string]string{"password": householdTestPassword})
	rr = cinemaCallbackTest(t, h, fixture, state, "", cookie)
	if rr.Header().Get("Location") != "/login?cinema=failed" || exchanges.Load() != 0 {
		t.Fatal("missing initiating local session reached exchange", rr.Header(), exchanges.Load())
	}
	query := url.Values{"state": {state}, "iss": {fixture.issuer}, "code": {strings.Repeat("c", 257)}}
	rr = householdHTTP(t, h, "GET", "/api/v1/auth/cinema/callback?"+query.Encode(), owner, nil, cookie)
	if rr.Header().Get("Location") != "/login?cinema=failed" || exchanges.Load() != 0 {
		t.Fatal("oversized code reached exchange", rr.Header(), exchanges.Load())
	}
	rr = cinemaCallbackTest(t, h, fixture, state, owner, cookie)
	if rr.Header().Get("Location") != "/account?cinema=linked" || exchanges.Load() != 1 {
		t.Fatal("invalid callback claimed otherwise usable handoff", rr.Header(), exchanges.Load())
	}
	state, cookie = cinemaStart(t, h, "/api/v1/auth/cinema/start", "", nil)
	oversized := strings.Repeat("s", 257)
	if _, err := db.W.ExecContext(context.Background(), `UPDATE cinema_handoffs SET state_digest=? WHERE state_digest=?`, digestSession(oversized), digestSession(state)); err != nil {
		t.Fatal(err)
	}
	query = url.Values{"state": {oversized}, "iss": {fixture.issuer}, "code": {"one-use-code"}}
	rr = householdHTTP(t, h, "GET", "/api/v1/auth/cinema/callback?"+query.Encode(), "", nil, cookie)
	if rr.Header().Get("Location") != "/login?cinema=failed" || exchanges.Load() != 1 {
		t.Fatal("oversized bound state reached exchange", rr.Header(), exchanges.Load())
	}
	if _, err := db.W.ExecContext(context.Background(), `UPDATE cinema_handoffs SET state_digest=? WHERE state_digest=?`, digestSession(state), digestSession(oversized)); err != nil {
		t.Fatal(err)
	}
	rr = cinemaCallbackTest(t, h, fixture, state, "", cookie)
	if rr.Header().Get("Location") != "/requests" || exchanges.Load() != 2 {
		t.Fatal("state length rejection consumed valid consent", rr.Header(), exchanges.Load())
	}

	req = httptest.NewRequest("POST", "https://curator.example/api/v1/auth/sessions", strings.NewReader(`{"username":"owner","password":"`+householdTestPassword+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://other.example")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	householdExpect(t, rr, 403)
}
