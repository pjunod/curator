package api

import (
	"crypto/tls"
	"errors"
	"github.com/pjunod/monarr/internal/app/health"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func TestHouseholdDatabaseCookieTransportAndRevocation(t *testing.T) {
	_, db, _ := householdHTTPAccount(t)
	s := New(Deps{Store: db, Settings: db, Log: edgeQuietLog(), Health: health.NewRegistry(nil), TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32")}})
	t.Cleanup(func() { s.BeginShutdown(); s.CancelRequests(); s.Wait() })
	handler := s.Handler()
	for _, tc := range []struct {
		name, peer  string
		headers     []string
		tls, secure bool
	}{
		{name: "trusted LAN HTTP", peer: "192.0.2.20:1234"},
		{name: "direct TLS", peer: "192.0.2.20:1234", headers: []string{"http"}, tls: true, secure: true},
		{name: "trusted HTTPS proxy", peer: "192.0.2.10:1234", headers: []string{"https"}, secure: true},
		{name: "spoofed HTTPS", peer: "192.0.2.20:1234", headers: []string{"https"}},
		{name: "ambiguous chain", peer: "192.0.2.10:1234", headers: []string{"https, http"}},
		{name: "duplicate headers", peer: "192.0.2.10:1234", headers: []string{"https", "http"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepare := func(path, body string) *http.Request {
				r := httptest.NewRequest(http.MethodPost, "http://curator.example"+path, strings.NewReader(body))
				r.Header.Set("Content-Type", "application/json")
				r.RemoteAddr = tc.peer
				for _, header := range tc.headers {
					r.Header.Add("X-Forwarded-Proto", header)
				}
				if tc.tls {
					r.TLS = &tls.ConnectionState{}
				}
				return r
			}
			r := prepare("/api/v1/auth/login", `{"username":"owner","password":"`+householdTestPassword+`"}`)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			cookies := w.Result().Cookies()
			if w.Code != 204 || len(cookies) != 1 {
				t.Fatal("database login", w.Code, w.Body.String())
			}
			cookie := cookies[0]
			if cookie.Secure != tc.secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
				t.Fatal("cookie transport policy", cookie)
			}
			if _, err := db.AccountSession(r.Context(), digestSession(cookie.Value)); err != nil {
				t.Fatal("no durable session", err)
			}
			logout := prepare("/api/v1/auth/logout", "")
			logout.AddCookie(cookie)
			w = httptest.NewRecorder()
			handler.ServeHTTP(w, logout)
			deleted := w.Result().Cookies()
			if w.Code != 204 || len(deleted) != 1 || deleted[0].MaxAge != -1 || deleted[0].Secure != tc.secure || !deleted[0].HttpOnly || deleted[0].SameSite != cookie.SameSite || deleted[0].Path != cookie.Path {
				t.Fatal("logout transport policy", w.Code, deleted)
			}
			if _, err := db.AccountSession(r.Context(), digestSession(cookie.Value)); !errors.Is(err, sqlite.ErrNotFound) {
				t.Fatal("durable session survived logout", err)
			}
		})
	}
}
