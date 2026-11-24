package api

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	apigen "github.com/pjunod/monarr/internal/api/gen"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
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
	authHashSetting     = "auth_password_hash" // versioned KDF; legacy hex SHA-256 accepted on login
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

// Fixed parameters bound verification cost even if a stored value is malformed.
// PBKDF2 uses the standard library, HMAC-SHA-256 and 600,000 iterations.
const passwordHashPrefix = "$pbkdf2-sha256$600000$"

func newPasswordHash(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	if err != nil {
		return "", err
	}
	return passwordHashPrefix + hex.EncodeToString(salt) + "$" + hex.EncodeToString(key), nil
}

func verifyPassword(stored, legacySalt, password string) (valid, legacy bool) {
	if strings.HasPrefix(stored, passwordHashPrefix) {
		parts := strings.Split(strings.TrimPrefix(stored, passwordHashPrefix), "$")
		if len(parts) != 2 {
			return false, false
		}
		salt, err := hex.DecodeString(parts[0])
		if err != nil || len(salt) != 16 {
			return false, false
		}
		want, err := hex.DecodeString(parts[1])
		if err != nil || len(want) != 32 {
			return false, false
		}
		got, err := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
		return err == nil && subtle.ConstantTimeCompare(got, want) == 1, false
	}
	// Compatibility only: never write another fast hash. Unknown versions fail
	// closed instead of falling back to legacy password verification.
	want, err := hex.DecodeString(stored)
	if err != nil || len(want) != sha256.Size || len(legacySalt) != 32 {
		return false, false
	}
	return subtle.ConstantTimeCompare([]byte(hashPassword(legacySalt, password)), []byte(strings.ToLower(stored))) == 1, true
}

// hashPassword is the original pre-KDF digest, retained only to verify existing
// credentials. Never write its result: new and migrated credentials must use
// newPasswordHash, with its salt and work factor embedded in the stored value.
func hashPassword(salt, password string) string {
	sum := sha256.Sum256([]byte(salt + password))
	return hex.EncodeToString(sum[:])
}

// Only the immediate, explicitly trusted proxy may assert the original scheme.
// It must overwrite X-Forwarded-Proto; ambiguous chains are not trusted.
func (s *Server) secureSessionRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, network := range s.deps.TrustedProxies {
		if network.Contains(peer.Unmap()) {
			values := r.Header.Values("X-Forwarded-Proto")
			return len(values) == 1 && strings.EqualFold(strings.TrimSpace(values[0]), "https")
		}
	}
	return false
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
	if s.deps.Store != nil {
		established, err := s.deps.Store.HouseholdEstablished(ctx)
		if err != nil || established {
			return true
		}
	}
	if s.deps.Settings == nil {
		return false
	}
	value, err := s.deps.Settings.GetMeta(ctx, AuthRequiredSetting)
	// An absent opt-in setting preserves the open default. A storage failure
	// must never turn an enabled guard off; valid credentials still work.
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return true
	}
	return value == "true"
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
	if c, err := r.Cookie(sessionCookie); s.deps.Store == nil && err == nil && liveSessions.valid(c.Value) {
		return true
	}
	return false
}

// authGuard wraps the /api/v1 handler; login/logout stay reachable.
func (s *Server) authGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.sameOriginMutation(r) {
			writeError(w, 403, "use this Curator installation's origin")
			return
		}
		if publicHouseholdRoute(r) {
			next.ServeHTTP(w, r)
			return
		}
		p, err := s.sessionPrincipal(r)
		if err != nil {
			if !errors.Is(err, sqlite.ErrNotFound) {
				writeError(w, 503, "account authorization is unavailable")
				return
			}
			if s.authRequired(r.Context()) || presentedCredentials(r) {
				writeError(w, 401, "authentication required")
				return
			}
			p = principal{Account: domain.Account{Role: "owner", Status: "active"}, Method: "open"}
		}
		if p.accessRole() != "owner" && !memberHouseholdRoute(r) {
			writeError(w, 403, "this operation requires owner access")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey{}, p)))
	})
}

// Login implements POST /auth/login.
func (s *Server) Login(w http.ResponseWriter, r *http.Request) {
	var body apigen.LoginJSONRequestBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if s.deps.Store != nil {
		s.loginHousehold(w, r, body.Username, body.Password, "Web", false)
		return
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	ctx := r.Context()
	wantUser := s.readSettingOr(ctx, authUserSetting, "")
	salt := s.readSettingOr(ctx, authSaltSetting, "")
	wantHash := s.readSettingOr(ctx, authHashSetting, "")
	if wantUser == "" || wantHash == "" {
		writeError(w, http.StatusUnauthorized, "no credentials configured")
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(body.Username), []byte(wantUser)) == 1
	passOK, legacy := verifyPassword(wantHash, salt, body.Password)
	if !userOK || !passOK {
		writeError(w, http.StatusUnauthorized, "bad credentials")
		return
	}
	if legacy {
		upgraded, err := newPasswordHash(body.Password)
		if err == nil {
			err = s.deps.Settings.SetMeta(ctx, authHashSetting, upgraded)
		}
		if err != nil {
			// Keep a successful login usable if storage is temporarily unavailable;
			// the unchanged legacy credentials can be upgraded on the next login.
			s.deps.Log.Warn("auth: password hash upgrade failed; will retry on next login")
		}
	}
	s.setLocalSessionCookie(w, r, liveSessions.create(), int(sessionTTL.Seconds()))
	w.WriteHeader(http.StatusNoContent)
}

// setLocalSessionCookie is the common standalone login/logout transport policy.
// Direct TLS and explicitly trusted HTTPS proxies always get Secure. Plain HTTP
// is an intentional trusted-LAN compatibility mode (SECURITY_CHECKS.md), not a
// confidentiality guarantee. Cinema-derived cookie issuance remains HTTPS-only.
func (s *Server) setLocalSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureSessionRequest(r), MaxAge: maxAge})
}

// Logout implements POST /auth/logout.
func (s *Server) Logout(w http.ResponseWriter, r *http.Request) {
	if expected := r.Header.Get("X-Curator-Principal"); expected != "" {
		if s.deps.Store == nil {
			writeError(w, 409, "Account changed. Refresh before continuing.")
			return
		}
		// Local logout remains available during an upstream outage; read this
		// Curator session's authority without asking Cinema to authenticate it.
		session, err := s.deps.Store.AccountSession(r.Context(), digestSession(requestSessionToken(r)))
		p := principal{Account: session.Account, Method: session.Method}
		if err != nil || expected != fmt.Sprintf("%d:%s:%s", p.Account.ID, p.Method, p.accessRole()) {
			writeError(w, 409, "Account changed. Refresh before continuing.")
			return
		}
	}

	if s.deps.Store != nil {
		if token := requestSessionToken(r); token != "" {
			if err := s.deps.Store.DropAccountSession(r.Context(), digestSession(token)); err != nil {
				writeError(w, 503, "sign-out could not be confirmed; try again")
				return
			}
		}
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		liveSessions.drop(c.Value)
	}
	s.setLocalSessionCookie(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// applyAuthSettings handles the auth fields of PUT /settings.
func (s *Server) applyAuthSettings(ctx context.Context, body apigen.SettingsUpdate) error {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.deps.Store != nil {
		var hash *string
		if body.AuthUsername != nil && *body.AuthUsername != "" && !validAccountName(strings.TrimSpace(*body.AuthUsername)) {
			return errors.New("choose a valid local username")
		}
		if body.AuthPassword != nil && *body.AuthPassword != "" {
			if len(*body.AuthPassword) > 1024 {
				return errors.New("password is too long")
			}
			value, err := newPasswordHash(*body.AuthPassword)
			if err != nil {
				return err
			}
			hash = &value
		}
		return s.deps.Store.ConfigureLocalOwner(ctx, body.AuthUsername, hash, body.AuthRequired)
	}

	if body.AuthUsername != nil && *body.AuthUsername != "" {
		if err := s.deps.Settings.SetMeta(ctx, authUserSetting, strings.TrimSpace(*body.AuthUsername)); err != nil {
			return err
		}
	}
	if body.AuthPassword != nil && *body.AuthPassword != "" {
		hash, err := newPasswordHash(*body.AuthPassword)
		if err != nil {
			return err
		}
		// Salt, parameters and key are one atomic setting write. Leave the old
		// salt alone so a failed write cannot invalidate legacy credentials.
		if err := s.deps.Settings.SetMeta(ctx, authHashSetting, hash); err != nil {
			return err
		}
	}
	if body.AuthRequired != nil {
		val := "false"
		if *body.AuthRequired {
			// Refuse to lock the door with no key under it: credentials must
			// exist before auth turns on (the API key always works).
			//
			// BOTH halves, and the hash is the one that matters. Login refuses
			// on `wantUser == "" || wantHash == ""`, so checking only the
			// username let a username with an empty password turn auth on and
			// then answer every login with "no credentials configured" — a
			// permanent lockout, and on an install with no API key the only way
			// back in was editing app_meta by hand. The two conditions have to
			// agree, so they are spelled the same way.
			user := s.readSettingOr(ctx, authUserSetting, "")
			hash := s.readSettingOr(ctx, authHashSetting, "")
			if user == "" || hash == "" {
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
