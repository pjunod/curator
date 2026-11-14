package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pjunod/monarr/internal/domain"
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
