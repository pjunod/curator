package api

import (
	"context"
	"encoding/json"
	"github.com/pjunod/monarr/internal/domain"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type cinemaFixture struct {
	mu                              sync.Mutex
	active                          map[string]bool
	counter                         int
	issuer, client, secret, subject string
	server                          *httptest.Server
}

func newCinemaFixture(t *testing.T) *cinemaFixture {
	t.Helper()
	f := &cinemaFixture{active: map[string]bool{}, issuer: "stable-cinema-issuer", client: "registered-curator-client", secret: strings.Repeat("secret", 8), subject: "permanent-user-subject"}
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/api/v1/delegation/info" {
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.issuer, "scope": "household-requests", "client": map[string]string{"id": f.client, "redirect": "https://curator.example/api/v1/auth/cinema/callback"}})
			return
		}
		var b map[string]string
		if json.NewDecoder(r.Body).Decode(&b) != nil || b["clientId"] != f.client || b["clientSecret"] != f.secret {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/api/v1/delegation/exchange":
			if b["code"] != "one-use-code" || b["verifier"] == "" || b["redirect"] != "https://curator.example/api/v1/auth/cinema/callback" {
				w.WriteHeader(401)
				return
			}
			f.counter++
			grant := "grant-" + strings.Repeat("g", f.counter)
			f.active[grant] = true
			f.identity(w, grant, true)
		case "/api/v1/delegation/introspect":
			f.identity(w, "", f.active[b["grant"]])
		case "/api/v1/delegation/revoke":
			delete(f.active, b["grant"])
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	previous := cinemaHTTPClient
	client := *previous
	client.Transport = f.server.Client().Transport
	cinemaHTTPClient = &client
	t.Cleanup(func() { cinemaHTTPClient = previous; f.server.Close() })
	return f
}
func (f *cinemaFixture) identity(w http.ResponseWriter, grant string, active bool) {
	_ = json.NewEncoder(w).Encode(map[string]any{"active": active, "iss": f.issuer, "sub": f.subject, "aud": f.client, "scope": "household-requests", "displayName": "Cinema person", "expiresAt": int64(9999999999), "grant": grant})
}
func cinemaStart(t *testing.T, h http.Handler, path, token string, body any) (string, *http.Cookie) {
	t.Helper()
	rr := householdHTTP(t, h, "POST", path, token, body)
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	var value struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	cookies := rr.Result().Cookies()
	if len(cookies) == 0 {
		return value.State, nil
	}
	return value.State, cookies[0]
}
func cinemaCallbackTest(t *testing.T, h http.Handler, f *cinemaFixture, state, token string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/auth/cinema/callback?" + url.Values{"state": {state}, "iss": {f.issuer}, "code": {"one-use-code"}}.Encode()
	if cookie == nil {
		return householdHTTP(t, h, "GET", path, token, nil)
	}
	return householdHTTP(t, h, "GET", path, token, nil, cookie)
}
func TestCinemaExplicitLinkScopedSignInAndOutageUnlink(t *testing.T) {
	h, db, owner := householdHTTPAccount(t)
	f := newCinemaFixture(t)
	rr := householdHTTP(t, h, "PUT", "/api/v1/auth/cinema/connection", owner, map[string]string{"password": householdTestPassword, "baseUrl": f.server.URL, "clientId": f.client, "clientSecret": f.secret, "redirect": "https://curator.example/api/v1/auth/cinema/callback"})
	if rr.Code != 204 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	unlinkedState, unlinkedCookie := cinemaStart(t, h, "/api/v1/auth/cinema/start", "", nil)
	unlinked := cinemaCallbackTest(t, h, f, unlinkedState, "", unlinkedCookie)
	if unlinked.Header().Get("Location") != "/login?cinema=unlinked" {
		t.Fatal("unlinked identity silently merged or created a local account", unlinked.Header())
	}
	accounts, e := db.ListAccounts(context.Background())
	if e != nil || len(accounts) != 1 {
		t.Fatal("Cinema sign-in changed the local account directory", e)
	}
	state, cookie := cinemaStart(t, h, "/api/v1/auth/cinema/link", owner, map[string]string{"password": householdTestPassword})
	bad := cinemaCallbackTest(t, h, f, state, owner, nil)
	if bad.Header().Get("Location") != "/login?cinema=failed" {
		t.Fatal("unbound callback accepted")
	}
	rr = cinemaCallbackTest(t, h, f, state, owner, cookie)
	if rr.Header().Get("Location") != "/account?cinema=linked" {
		t.Fatal("explicit link failed", rr.Code, rr.Header())
	}
	rr = cinemaCallbackTest(t, h, f, state, owner, cookie)
	if rr.Header().Get("Location") != "/login?cinema=failed" {
		t.Fatal("callback replay accepted")
	}
	link, err := db.CinemaLinkForSubject(context.Background(), f.issuer, f.subject)
	if err != nil || link.Grant != "" {
		t.Fatal("link persisted a reusable login instead of identity", err)
	}
	state, cookie = cinemaStart(t, h, "/api/v1/auth/cinema/start", "", nil)
	rr = cinemaCallbackTest(t, h, f, state, "", cookie)
	if rr.Header().Get("Location") != "/requests" {
		t.Fatal("linked sign-in failed", rr.Header())
	}
	var sessionCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "monarr_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("derived session missing")
	}
	derived := sessionCookie.Value
	rr = householdHTTP(t, h, "GET", "/api/v1/auth/me", derived, nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"accessRole":"member"`) || !strings.Contains(rr.Body.String(), `"role":"owner"`) {
		t.Fatal("effective authority confused with local owner role", rr.Body.String())
	}
	rr = householdHTTP(t, h, "GET", "/api/v1/settings", derived, nil)
	if rr.Code != 403 {
		t.Fatal("Cinema inherited Curator owner authority", rr.Code)
	}
	rr = householdHTTP(t, h, "POST", "/api/v1/household/requests", derived, map[string]any{"kind": "movie", "tmdbId": 550, "seasons": []int{}})
	if rr.Code != 201 {
		t.Fatal("scoped household request failed", rr.Code, rr.Body.String())
	}
	var request domain.HouseholdRequest
	_ = json.Unmarshal(rr.Body.Bytes(), &request)
	f.server.Close()
	rr = householdHTTP(t, h, "GET", "/api/v1/household/requests", derived, nil)
	if rr.Code != 503 {
		t.Fatal("Cinema outage did not fail derived session closed", rr.Code)
	}
	rr = householdHTTP(t, h, "GET", "/api/v1/system/status", owner, nil)
	if rr.Code != 200 {
		t.Fatal("Cinema outage broke local owner", rr.Code)
	}
	rr = householdHTTP(t, h, "DELETE", "/api/v1/auth/cinema/link", owner, map[string]string{"password": householdTestPassword})
	if rr.Code != 204 {
		t.Fatal("local unlink depended on Cinema availability", rr.Code, rr.Body.String())
	}
	rr = householdHTTP(t, h, "GET", "/api/v1/household/requests", derived, nil)
	if rr.Code != 401 {
		t.Fatal("unlinked derived session survived", rr.Code)
	}
	saved, err := db.HouseholdRequest(context.Background(), request.ID, link.AccountID, true)
	if err != nil || !saved.Subscribed {
		t.Fatal("unlink lost local request history", err)
	}
	rr = householdHTTP(t, h, "GET", "/api/v1/system/status", owner, nil)
	if rr.Code != 200 {
		t.Fatal("unlink revoked local session", rr.Code)
	}
	pending, err := db.CinemaRevocations(context.Background())
	if err != nil || len(pending) < 2 {
		t.Fatal("remote revocation was not queued durably during outage", err)
	}
}
func TestCinemaNativeProofBoundHandoff(t *testing.T) {
	h, db, owner := householdHTTPAccount(t)
	f := newCinemaFixture(t)
	rr := householdHTTP(t, h, "PUT", "/api/v1/auth/cinema/connection", owner, map[string]string{"password": householdTestPassword, "baseUrl": f.server.URL, "clientId": f.client, "clientSecret": f.secret, "redirect": "https://curator.example/api/v1/auth/cinema/callback"})
	if rr.Code != 204 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	state, cookie := cinemaStart(t, h, "/api/v1/auth/cinema/link", owner, map[string]string{"password": householdTestPassword})
	rr = cinemaCallbackTest(t, h, f, state, owner, cookie)
	if rr.Header().Get("Location") != "/account?cinema=linked" {
		t.Fatal("link failed")
	}
	verifier := strings.Repeat("a", 64)
	state, _ = cinemaStart(t, h, "/api/v1/auth/cinema/native", "", map[string]string{"challenge": cinemaChallenge(verifier)})
	rr = householdHTTP(t, h, "GET", "/api/v1/auth/cinema/browser?state="+url.QueryEscape(state), "", nil)
	if rr.Code != 303 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	cookie = rr.Result().Cookies()[0]
	rr = cinemaCallbackTest(t, h, f, state, "", cookie)
	completionURL, e := url.Parse(rr.Header().Get("Location"))
	if e != nil {
		t.Fatal(e)
	}
	completion := completionURL.Query().Get("code")
	if completionURL.Scheme != "monarr" || completionURL.Host != "cinema-complete" || len(completion) < 32 {
		t.Fatal("native consent failed", rr.Header())
	}
	rr = householdHTTP(t, h, "POST", "/api/v1/auth/cinema/native/result", "", map[string]string{"state": state, "verifier": strings.Repeat("b", 64)})
	if rr.Code != 409 {
		t.Fatal("wrong application verifier accepted", rr.Code)
	}
	rr = householdHTTP(t, h, "POST", "/api/v1/auth/cinema/native/result", "", map[string]string{"state": state, "verifier": verifier})
	if rr.Code != 409 {
		t.Fatal("original device polled victim consent without receiving completion", rr.Code)
	}
	rr = householdHTTP(t, h, "POST", "/api/v1/auth/cinema/native/result", "", map[string]string{"state": state, "verifier": strings.Repeat("b", 64), "completion": completion})
	if rr.Code != 409 {
		t.Fatal("consenting device completed another device's proof", rr.Code)
	}
	rr = householdHTTP(t, h, "POST", "/api/v1/auth/cinema/native/result", "", map[string]string{"state": state, "verifier": verifier, "completion": completion})
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	var result struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &result)
	if result.Token == "" {
		t.Fatal("native device token missing")
	}
	rr = householdHTTP(t, h, "POST", "/api/v1/auth/cinema/native/result", "", map[string]string{"state": state, "verifier": verifier, "completion": completion})
	if rr.Code != 409 {
		t.Fatal("native handoff replay accepted", rr.Code)
	}
	session, err := db.AccountSession(context.Background(), digestSession(result.Token))
	if err != nil || session.Method != "cinema" || session.IntegrationGrant == "" {
		t.Fatal("device lost its distinct grant", err)
	}
}

func TestCinemaStartBudgetDoesNotLockOutLocalSignIn(t *testing.T) {
	h, _, owner := householdHTTPAccount(t)
	for n := 0; n < 10; n++ {
		rr := householdHTTP(t, h, "POST", "/api/v1/auth/cinema/start", "", nil)
		if rr.Code != 503 && rr.Code != 429 {
			t.Fatal(rr.Code, rr.Body.String())
		}
	}
	_ = owner
	householdHTTPLogin(t, h, "owner", householdTestPassword)
}
