package api

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	apigen "github.com/pjunod/monarr/internal/api/gen"
)

type authMemorySettings struct {
	values map[string]string
	fail   bool
}

func (m *authMemorySettings) GetMeta(_ context.Context, key string) (string, error) {
	return m.values[key], nil
}
func (m *authMemorySettings) SetMeta(_ context.Context, key, value string) error {
	if m.fail {
		return errors.New("storage unavailable")
	}
	m.values[key] = value
	return nil
}
func legacyAuthSettings() *authMemorySettings {
	salt := "0123456789abcdef0123456789abcdef"
	sum := sha256.Sum256([]byte(salt + "old-password"))
	return &authMemorySettings{values: map[string]string{authUserSetting: "owner", authSaltSetting: salt, authHashSetting: hex.EncodeToString(sum[:])}}
}
func authTestServer(m SettingsStore) *Server {
	return New(Deps{Settings: m, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}
func loginAuth(s *Server, user, password string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"`+user+`","password":"`+password+`"}`))
	w := httptest.NewRecorder()
	s.Login(w, r)
	return w
}

func TestLegacyLoginMigratesOnlyOnSuccess(t *testing.T) {
	m := legacyAuthSettings()
	old := m.values[authHashSetting]
	s := authTestServer(m)
	for _, creds := range [][2]string{{"owner", "bad"}, {"other", "old-password"}} {
		if rr := loginAuth(s, creds[0], creds[1]); rr.Code != http.StatusUnauthorized {
			t.Fatalf("bad credentials: %d", rr.Code)
		}
		if m.values[authHashSetting] != old {
			t.Fatal("failed login changed credentials")
		}
	}
	if rr := loginAuth(s, "owner", "old-password"); rr.Code != http.StatusNoContent {
		t.Fatalf("legacy login: %d", rr.Code)
	}
	upgraded := m.values[authHashSetting]
	if !strings.HasPrefix(upgraded, passwordHashPrefix) {
		t.Fatal("legacy hash not upgraded")
	}
	// The embedded salt is sufficient even after the obsolete salt is removed,
	// including a fresh process/server using the same persisted settings.
	delete(m.values, authSaltSetting)
	if rr := loginAuth(authTestServer(m), "owner", "old-password"); rr.Code != http.StatusNoContent {
		t.Fatalf("upgraded login: %d", rr.Code)
	}
	if m.values[authHashSetting] != upgraded {
		t.Fatal("current hash unnecessarily rewritten")
	}
	if rr := loginAuth(s, "owner", "wrong"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password accepted: %d", rr.Code)
	}
}

func TestMigrationWriteFailureRetainsUsableCredentials(t *testing.T) {
	m := legacyAuthSettings()
	old := m.values[authHashSetting]
	m.fail = true
	s := authTestServer(m)
	if rr := loginAuth(s, "owner", "old-password"); rr.Code != http.StatusNoContent {
		t.Fatalf("storage failure locked out login: %d", rr.Code)
	}
	if m.values[authHashSetting] != old {
		t.Fatal("failed write changed credentials")
	}
	m.fail = false
	if rr := loginAuth(s, "owner", "old-password"); rr.Code != http.StatusNoContent || m.values[authHashSetting] == old {
		t.Fatal("migration did not retry")
	}
}

func TestPasswordChangeIsOneAtomicHashWrite(t *testing.T) {
	m := legacyAuthSettings()
	s := authTestServer(m)
	password := "new-password"
	body := apigen.SettingsUpdate{AuthPassword: &password}
	old := m.values[authHashSetting]
	m.fail = true
	if err := s.applyAuthSettings(context.Background(), body); err == nil {
		t.Fatal("write failure swallowed")
	}
	if valid, _ := verifyPassword(m.values[authHashSetting], m.values[authSaltSetting], "old-password"); !valid || m.values[authHashSetting] != old {
		t.Fatal("failed update broke old credentials")
	}
	m.fail = false
	if err := s.applyAuthSettings(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	first := m.values[authHashSetting]
	if valid, legacy := verifyPassword(first, "", password); !valid || legacy {
		t.Fatal("new credential not a valid KDF")
	}
	if valid, _ := verifyPassword(first, "", "old-password"); valid {
		t.Fatal("old password survived change")
	}
	if err := s.applyAuthSettings(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if first == m.values[authHashSetting] {
		t.Fatal("password salt reused")
	}
}

func TestMalformedPasswordHashesFailClosed(t *testing.T) {
	for _, hash := range []string{"", strings.Repeat("0", 64), "$unknown$600000$", passwordHashPrefix, "$pbkdf2-sha256$999999999999$", passwordHashPrefix + strings.Repeat("0", 32) + "$00", passwordHashPrefix + "zz$" + strings.Repeat("0", 64)} {
		if valid, _ := verifyPassword(hash, "", "anything"); valid {
			t.Fatalf("accepted malformed hash %q", hash)
		}
	}
}

func TestSessionCookieTransportBoundary(t *testing.T) {
	m := legacyAuthSettings()
	s := authTestServer(m)
	s.deps.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32"), netip.MustParsePrefix("2001:db8::10/128")}
	for _, tc := range []struct {
		name, peer, proto string
		tls, secure       bool
	}{
		{"LAN HTTP", "192.0.2.20:1234", "", false, false},
		{"TLS", "192.0.2.20:1234", "http", true, true},
		{"untrusted spoof", "192.0.2.20:1234", "https", false, false},
		{"trusted HTTPS", "192.0.2.10:1234", "https", false, true},
		{"trusted HTTP", "192.0.2.10:1234", "http", false, false},
		{"trusted absent", "192.0.2.10:1234", "", false, false},
		{"ambiguous chain", "192.0.2.10:1234", "https, http", false, false},
		{"IPv6 proxy", "[2001:db8::10]:1234", "https", false, true},
		{"invalid peer", "192.0.2.10", "https", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"owner","password":"old-password"}`))
			r.RemoteAddr = tc.peer
			if tc.proto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			w := httptest.NewRecorder()
			s.Login(w, r)
			cookies := w.Result().Cookies()
			if w.Code != http.StatusNoContent || len(cookies) != 1 {
				t.Fatalf("login: %d %v", w.Code, cookies)
			}
			c := cookies[0]
			if c.Secure != tc.secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
				t.Fatalf("cookie: %+v", c)
			}
			if !liveSessions.valid(c.Value) {
				t.Fatal("no session")
			}
			r.AddCookie(c)
			w = httptest.NewRecorder()
			s.Logout(w, r)
			deleted := w.Result().Cookies()[0]
			if deleted.Secure != tc.secure || deleted.MaxAge != -1 || deleted.Path != c.Path || deleted.SameSite != c.SameSite || !deleted.HttpOnly {
				t.Fatalf("logout: %+v", deleted)
			}
			if liveSessions.valid(c.Value) {
				t.Fatal("session survived logout")
			}
		})
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Add("X-Forwarded-Proto", "https")
	r.Header.Add("X-Forwarded-Proto", "http")
	if s.secureSessionRequest(r) {
		t.Fatal("trusted duplicate scheme headers")
	}
	s.deps.TrustedProxies = nil
	r.Header.Set("X-Forwarded-Proto", "https")
	if s.secureSessionRequest(r) {
		t.Fatal("proxy trusted by default")
	}
}
