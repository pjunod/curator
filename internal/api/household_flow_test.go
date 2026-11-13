package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/bus"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const householdTestPassword = "local-password-for-tests"

func householdHTTP(t *testing.T, h http.Handler, method, path, token string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	raw := ""
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(data)
	}
	req := httptest.NewRequest(method, "https://curator.example"+path, strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}
func householdHTTPAccount(t *testing.T) (http.Handler, *sqlite.DB, string) {
	t.Helper()
	h, db := newLibraryServer(t, stubProvider{configured: true})
	rr := householdHTTP(t, h, "PUT", "/api/v1/settings", "", map[string]any{"authUsername": "owner", "authPassword": householdTestPassword, "authRequired": true})
	if rr.Code != 204 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	return h, db, householdHTTPLogin(t, h, "owner", householdTestPassword)
}
func householdHTTPLogin(t *testing.T, h http.Handler, name, password string) string {
	t.Helper()
	rr := householdHTTP(t, h, "POST", "/api/v1/auth/sessions", "", map[string]string{"username": name, "password": password, "device": "regression"})
	if rr.Code != 200 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	var result struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(rr.Body.Bytes(), &result) != nil || result.Token == "" {
		t.Fatal("session missing")
	}
	return result.Token
}
func TestHouseholdMemberRoutesAndLocalRecovery(t *testing.T) {
	h, db, owner := householdHTTPAccount(t)
	rr := householdHTTP(t, h, "POST", "/api/v1/household/invitations", owner, map[string]string{"password": householdTestPassword})
	if rr.Code != 201 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	var invite struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &invite); err != nil {
		t.Fatal(err)
	}
	rr = householdHTTP(t, h, "POST", "/api/v1/auth/invitations/accept", "", map[string]string{"token": invite.Token, "username": "member", "displayName": "Household member", "password": householdTestPassword})
	if rr.Code != 201 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	member := householdHTTPLogin(t, h, "member", householdTestPassword)
	for _, route := range []struct{ method, path string }{{"GET", "/api/v1/settings"}, {"GET", "/api/v1/library"}, {"GET", "/api/v1/rootfolders"}, {"GET", "/api/v1/downloads"}, {"GET", "/api/v1/events"}, {"GET", "/metrics"}, {"GET", "/api/v1/system/status"}, {"GET", "/api/v1/metadata/preview"}, {"POST", "/api/v1/auth/pair"}, {"GET", "/api/v1/household/accounts"}, {"GET", "/api/v1/auth/cinema/connection"}} {
		rr := householdHTTP(t, h, route.method, route.path, member, nil)
		if rr.Code != 403 {
			t.Errorf("member %s %s = %d; wanted owner denial", route.method, route.path, rr.Code)
		}
	}
	rr = householdHTTP(t, h, "GET", "/api/v1/household/catalogue?kind=movie&q=Fight", member, nil)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "qualityProfile") || strings.Contains(rr.Body.String(), "rootFolder") {
		t.Fatal("unsafe catalogue", rr.Code, rr.Body.String())
	}
	rr = householdHTTP(t, h, "POST", "/api/v1/household/requests", member, map[string]any{"kind": "movie", "tmdbId": 550, "seasons": []int{}})
	if rr.Code != 201 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	var request domain.HouseholdRequest
	if err := json.Unmarshal(rr.Body.Bytes(), &request); err != nil {
		t.Fatal(err)
	}
	rr = householdHTTP(t, h, "PUT", fmt.Sprintf("/api/v1/household/requests/%d/note", request.ID), owner, map[string]string{"note": "private owner note"})
	if rr.Code != 204 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	rr = householdHTTP(t, h, "GET", fmt.Sprintf("/api/v1/household/requests/%d", request.ID), member, nil)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), "private owner note") || strings.Contains(rr.Body.String(), "policyJson") {
		t.Fatal("private owner data in member request", rr.Body.String())
	}
	// Mixed old settings must fail before changing even unrelated saved values.
	before, _ := db.GetMeta(context.Background(), "app_name")
	rr = householdHTTP(t, h, "PUT", "/api/v1/settings", owner, map[string]any{"authRequired": false, "appName": "must not commit"})
	if rr.Code != 400 {
		t.Fatal("legacy settings reopened auth", rr.Code)
	}
	after, _ := db.GetMeta(context.Background(), "app_name")
	if after != before {
		t.Fatal("rejected mixed settings partially wrote app name")
	}
	rr = householdHTTP(t, h, "GET", "/api/v1/auth/info", "", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"cinema":false`) {
		t.Fatal("standalone install requires Cinema", rr.Body.String())
	}
	a, err := db.AccountByUsername(context.Background(), "member")
	if err != nil {
		t.Fatal(err)
	}
	rr = householdHTTP(t, h, "PATCH", fmt.Sprintf("/api/v1/household/accounts/%d", a.ID), owner, map[string]any{"revision": a.Revision, "role": "member", "status": "suspended", "password": householdTestPassword})
	if rr.Code != 204 {
		t.Fatal(rr.Code, rr.Body.String())
	}
	rr = householdHTTP(t, h, "GET", "/api/v1/household/requests", member, nil)
	if rr.Code != 401 {
		t.Fatal("suspended device still authorized", rr.Code)
	}
	rr = householdHTTP(t, h, "GET", "/api/v1/system/status", owner, nil)
	if rr.Code != 200 {
		t.Fatal("member suspension revoked unrelated owner", rr.Code)
	}
}
func TestHouseholdRejectsCrossOriginAndExplicitBadCredentials(t *testing.T) {
	h, _, owner := householdHTTPAccount(t)
	req := httptest.NewRequest("PUT", "https://curator.example/api/v1/auth/account", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+owner)
	req.Header.Set("Origin", "http://curator.example")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatal("cross-scheme mutation accepted", rr.Code)
	}
	open, _ := newLibraryServer(t, stubProvider{configured: true})
	rr = householdHTTP(t, open, "GET", "/api/v1/system/status", "cts_"+strings.Repeat("0", 64), nil)
	if rr.Code != 401 {
		t.Fatal("invalid session fell back to anonymous owner", rr.Code)
	}
}

type householdPrivateEvent struct{ Secret string }

func (householdPrivateEvent) EventType() string { return "private-owner-event" }
func TestHouseholdRevokedOwnerStreamStopsBeforePayload(t *testing.T) {
	_, db, owner := householdHTTPAccount(t)
	b := bus.New(nil)
	defer b.Close()
	s := New(Deps{Store: db, Settings: db, Bus: b})
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+owner)
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	reader := bufio.NewReader(res.Body)
	if _, err = reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err = db.DropAccountSession(ctx, digestSession(owner)); err != nil {
		t.Fatal(err)
	}
	b.Publish(householdPrivateEvent{Secret: "private-owner-path"})
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal("revoked event stream did not terminate", err)
	}
	if strings.Contains(string(data), "private-owner-path") {
		t.Fatal("revoked stream received owner data")
	}
}
