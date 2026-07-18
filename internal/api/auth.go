package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	apigen "github.com/monarr-media/monarr/internal/api/gen"
)

// Auth hardening (Phase 5): opt-in. When the auth_required setting is
// "true", /api/v1 demands either the API key (X-Api-Key header / ?apikey=)
// or a session cookie from POST /auth/login. The SPA shell and static
// assets stay open so the login page can load; the compat personalities
// have their own X-Api-Key gate.
const (
	// AuthRequiredSetting toggles native-API auth ("true"/"false").
	AuthRequiredSetting = "auth_required"
	authUserSetting     = "auth_username"
	authHashSetting     = "auth_password_hash" // hex(sha256(salt||password))
	authSaltSetting     = "auth_password_salt"

	sessionCookie = "monarr_session"
	sessionTTL    = 30 * 24 * time.Hour
)

// sessions is a process-lifetime store: token → expiry. Restarting the
// server logs everyone out, which is fine for a self-hosted app.
type sessions struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func (s *sessions) create() string {
	buf := make([]byte, 24)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	s.mu.Lock()
	if s.m == nil {
		s.m = map[string]time.Time{}
	}
	// Opportunistic sweep.
	now := time.Now()
	for t, exp := range s.m {
		if now.After(exp) {
			delete(s.m, t)
		}
	}
	s.m[token] = now.Add(sessionTTL)
	s.mu.Unlock()
	return token
}

func (s *sessions) valid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[token]
	return ok && time.Now().Before(exp)
}

func (s *sessions) drop(token string) {
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

var liveSessions sessions

func hashPassword(salt, password string) string {
	sum := sha256.Sum256([]byte(salt + password))
	return hex.EncodeToString(sum[:])
}

// readSettingOr returns a setting or fallback when unset (or when the
// server has no settings store wired, as in some tests).
func (s *Server) readSettingOr(ctx context.Context, key, fallback string) string {
	if s.deps.Settings == nil {
		return fallback
	}
	v, err := s.deps.Settings.GetMeta(ctx, key)
	if err != nil || v == "" {
		return fallback
	}
	return v
}

// authRequired reports whether native-API auth is on.
func (s *Server) authRequired(ctx context.Context) bool {
	return s.readSettingOr(ctx, AuthRequiredSetting, "false") == "true"
}

// authenticated reports whether this request carries valid credentials.
func (s *Server) authenticated(r *http.Request) bool {
	apiKey := s.readSetting(r.Context(), APIKeySetting)
	if apiKey != "" {
		got := r.Header.Get("X-Api-Key")
		if got == "" {
			got = r.URL.Query().Get("apikey")
		}
		if got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(apiKey)) == 1 {
			return true
		}
	}
	if c, err := r.Cookie(sessionCookie); err == nil && liveSessions.valid(c.Value) {
		return true
	}
	return false
}

// authGuard wraps the /api/v1 handler; login/logout stay reachable.
func (s *Server) authGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		if !s.authRequired(r.Context()) || s.authenticated(r) {
			next.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "authentication required")
	})
}

// Login implements POST /auth/login.
func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	var body apigen.LoginJSONRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	ctx := r.Context()
	wantUser := s.readSettingOr(ctx, authUserSetting, "")
	salt := s.readSettingOr(ctx, authSaltSetting, "")
	wantHash := s.readSettingOr(ctx, authHashSetting, "")
	if wantUser == "" || wantHash == "" {
		writeError(w, http.StatusUnauthorized, "no credentials configured")
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(body.Username), []byte(wantUser)) == 1
	passOK := subtle.ConstantTimeCompare(
		[]byte(hashPassword(salt, body.Password)), []byte(wantHash)) == 1
	if !userOK || !passOK {
		writeError(w, http.StatusUnauthorized, "bad credentials")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: liveSessions.create(), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	w.WriteHeader(http.StatusNoContent)
}

// Logout implements POST /auth/logout.
func (s *Server) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		liveSessions.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	w.WriteHeader(http.StatusNoContent)
}

// applyAuthSettings handles the auth fields of PUT /settings.
func (s *Server) applyAuthSettings(ctx context.Context, body apigen.SettingsUpdate) error {
	if body.AuthUsername != nil && *body.AuthUsername != "" {
		if err := s.deps.Settings.SetMeta(ctx, authUserSetting, strings.TrimSpace(*body.AuthUsername)); err != nil {
			return err
		}
	}
	if body.AuthPassword != nil && *body.AuthPassword != "" {
		buf := make([]byte, 16)
		_, _ = rand.Read(buf)
		salt := hex.EncodeToString(buf)
		if err := s.deps.Settings.SetMeta(ctx, authSaltSetting, salt); err != nil {
			return err
		}
		if err := s.deps.Settings.SetMeta(ctx, authHashSetting, hashPassword(salt, *body.AuthPassword)); err != nil {
			return err
		}
	}
	if body.AuthRequired != nil {
		val := "false"
		if *body.AuthRequired {
			// Refuse to lock the door with no key under it: credentials
			// must exist before auth turns on (the API key always works).
			if s.readSettingOr(ctx, authUserSetting, "") == "" {
				return errors.New("set a username and password before enabling authentication")
			}
			val = "true"
		}
		if err := s.deps.Settings.SetMeta(ctx, AuthRequiredSetting, val); err != nil {
			return err
		}
	}
	return nil
}
