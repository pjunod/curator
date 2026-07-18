package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doWithHeader(t *testing.T, h http.Handler, method, path, body, hdr, val string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set(hdr, val)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestAuthHardening: default open; once credentials exist and auth turns
// on, /api/v1 wants the API key or a login session; login flow works.
func TestAuthHardening(t *testing.T) {
	h, db := newLibraryServer(t, stubProvider{configured: true})
	ctx := context.Background()

	// Seed an API key like main does at startup.
	if err := db.SetMeta(ctx, APIKeySetting, "k-123"); err != nil {
		t.Fatal(err)
	}

	// Open by default.
	if rr := do(t, h, "GET", "/api/v1/system/status", ""); rr.Code != http.StatusOK {
		t.Fatalf("default open: %d", rr.Code)
	}

	// Enabling auth without credentials is refused.
	if rr := do(t, h, "PUT", "/api/v1/settings", `{"authRequired":true}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("auth without creds: %d", rr.Code)
	}

	// Set credentials, then enable.
	rr := do(t, h, "PUT", "/api/v1/settings", `{"authUsername":"paul","authPassword":"hunter2","authRequired":true}`)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("enable auth: %d %s", rr.Code, rr.Body.String())
	}

	// Now the API is gated…
	if rr := do(t, h, "GET", "/api/v1/system/status", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("gated: %d", rr.Code)
	}
	// …but the API key opens it (header and query forms)…
	if rr := doWithHeader(t, h, "GET", "/api/v1/system/status", "", "X-Api-Key", "k-123"); rr.Code != http.StatusOK {
		t.Fatalf("api key: %d", rr.Code)
	}
	if rr := do(t, h, "GET", "/api/v1/system/status?apikey=k-123", ""); rr.Code != http.StatusOK {
		t.Fatalf("query key: %d", rr.Code)
	}
	// …bad login rejected…
	if rr := do(t, h, "POST", "/api/v1/auth/login", `{"username":"paul","password":"wrong"}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad login: %d", rr.Code)
	}
	// …good login sets a session cookie that opens it.
	rr = do(t, h, "POST", "/api/v1/auth/login", `{"username":"paul","password":"hunter2"}`)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("login: %d %s", rr.Code, rr.Body.String())
	}
	cookie := rr.Header().Get("Set-Cookie")
	if !strings.Contains(cookie, sessionCookie+"=") {
		t.Fatalf("no session cookie: %q", cookie)
	}
	if rr := doWithHeader(t, h, "GET", "/api/v1/system/status", "", "Cookie", cookie); rr.Code != http.StatusOK {
		t.Fatalf("session: %d", rr.Code)
	}

	// The SPA shell stays reachable (login page must load).
	if rr := do(t, h, "GET", "/", ""); rr.Code != http.StatusOK {
		t.Fatalf("spa gated: %d", rr.Code)
	}

	// Logout kills the session.
	if rr := doWithHeader(t, h, "POST", "/api/v1/auth/logout", "", "Cookie", cookie); rr.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", rr.Code)
	}
	if rr := doWithHeader(t, h, "GET", "/api/v1/system/status", "", "Cookie", cookie); rr.Code != http.StatusUnauthorized {
		t.Fatalf("session survived logout: %d", rr.Code)
	}
}

func TestMetricsOptIn(t *testing.T) {
	h, _ := newLibraryServer(t, stubProvider{configured: true})

	// Dark by default.
	if rr := do(t, h, "GET", "/metrics", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("metrics default: %d", rr.Code)
	}

	t.Setenv("MONARR_METRICS", "true")
	rr := do(t, h, "GET", "/metrics", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics enabled: %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"monarr_build_info", "monarr_media_items{kind=\"movie\"}", "monarr_uptime_seconds"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}
