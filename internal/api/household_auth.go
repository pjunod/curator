package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
)

type principal struct {
	Account       domain.Account
	Method        string
	Machine       bool
	Authenticated bool
}

// accessRole is the authority of this session, independent of the permanent
// account role. Integration sessions can never inherit local owner authority.
func (p principal) accessRole() string {
	if p.Account.Role == "owner" && (p.Method == "local" || p.Method == "machine" || p.Method == "open") {
		return "owner"
	}
	return "member"
}

type principalContextKey struct{}

func currentPrincipal(r *http.Request) principal {
	p, _ := r.Context().Value(principalContextKey{}).(principal)
	return p
}
func digestSession(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func newHouseholdToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "cts_" + hex.EncodeToString(buf), nil
}
func requestSessionToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer cts_") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		return c.Value
	}
	return ""
}
func (s *Server) sessionPrincipal(r *http.Request) (principal, error) {
	if s.deps.Store != nil {
		token := requestSessionToken(r)
		if strings.HasPrefix(token, "cts_") {
			session, err := s.deps.Store.AccountSession(r.Context(), digestSession(token))
			if err != nil {
				return principal{}, err
			}
			// Optional integration sessions are accepted only by their explicit grant
			// validator and retain household-only session authority.
			if session.Method == "cinema" {
				if err = s.validateCinemaSession(r.Context(), session); err != nil {
					return principal{}, err
				}
			} else if session.Method != "local" {
				return principal{}, sqlite.ErrNotFound
			}
			return principal{Account: session.Account, Method: session.Method, Authenticated: true}, nil
		}
	}
	if s.authenticated(r) {
		return principal{Account: domain.Account{Role: "owner", Status: "active"}, Method: "machine", Machine: true, Authenticated: true}, nil
	}
	return principal{}, sqlite.ErrNotFound
}
func publicHouseholdRoute(r *http.Request) bool {
	if r.Method == http.MethodGet && (r.URL.Path == "/api/v1/auth/cinema/callback" || r.URL.Path == "/api/v1/auth/cinema/browser") {
		return true
	}
	if r.Method == http.MethodPost && (r.URL.Path == "/api/v1/auth/cinema/start" || r.URL.Path == "/api/v1/auth/cinema/native" || r.URL.Path == "/api/v1/auth/cinema/native/result") {
		return true
	}
	return r.Method == http.MethodPost && (r.URL.Path == "/api/v1/auth/login" || r.URL.Path == "/api/v1/auth/logout" || r.URL.Path == "/api/v1/auth/sessions" || r.URL.Path == "/api/v1/auth/invitations/accept") || r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/info"
}
func memberHouseholdRoute(r *http.Request) bool {
	path := r.URL.Path
	if path == "/api/v1/auth/cinema/link" && (r.Method == http.MethodGet || r.Method == http.MethodPost || r.Method == http.MethodDelete) {
		return true
	}
	if path == "/api/v1/auth/me" && r.Method == http.MethodGet || path == "/api/v1/auth/account" && r.Method == http.MethodPut {
		return true
	}
	if (path == "/api/v1/household/discovery/seeds" || path == "/api/v1/household/discovery/shelf") && r.Method == http.MethodGet {
		return true
	}
	if path == "/api/v1/household/catalogue" && r.Method == http.MethodGet || path == "/api/v1/household/requests" && (r.Method == http.MethodGet || r.Method == http.MethodPost) {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/household/"), "/")
	if len(parts) == 2 && (parts[0] == "requests" || parts[0] == "titles") && r.Method == http.MethodGet {
		return true
	}
	return len(parts) == 3 && parts[0] == "requests" && (parts[2] == "join" || parts[2] == "withdraw") && r.Method == http.MethodPost
}
func (s *Server) sameOriginMutation(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	scheme := "http"
	if s.secureSessionRequest(r) {
		scheme = "https"
	}
	return len(r.Header.Values("Origin")) == 1 && err == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) && u.User == nil && u.Opaque == "" && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}
func decodeHouseholdBody(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil || dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "invalid request body")
		return false
	}
	return true
}
func validAccountName(name string) bool {
	if len(name) < 1 || len(name) > 80 || !utf8.ValidString(name) {
		return false
	}
	for _, c := range name {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}
func validAccountPassword(password string) bool {
	return len(password) >= 12 && len(password) <= 1024 && utf8.ValidString(password)
}

// Bound expensive password work and failed attempts without retaining passwords.
var householdPasswordSlots = make(chan struct{}, 2)

type householdLoginFailures struct {
	sync.Mutex
	entries map[string][]time.Time
}

func (s *Server) beginHouseholdPassword(r *http.Request) (func(bool), bool) {
	return beginHouseholdAttempt(r, &s.loginFailures)
}
func beginHouseholdAttempt(r *http.Request, failures *householdLoginFailures) (func(bool), bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	now := time.Now()
	failures.Lock()
	if failures.entries == nil {
		failures.entries = map[string][]time.Time{}
	}
	for key, times := range failures.entries {
		keep := times[:0]
		for _, at := range times {
			if now.Sub(at) < time.Minute {
				keep = append(keep, at)
			}
		}
		if len(keep) == 0 {
			delete(failures.entries, key)
		} else {
			failures.entries[key] = keep
		}
	}
	blocked := len(failures.entries[host]) >= 8 || len(failures.entries) >= 4096
	failures.Unlock()
	if blocked {
		return nil, false
	}
	select {
	case householdPasswordSlots <- struct{}{}:
	case <-r.Context().Done():
		return nil, false
	default:
		return nil, false
	}
	return func(success bool) {
		<-householdPasswordSlots
		if !success {
			failures.Lock()
			failures.entries[host] = append(failures.entries[host], time.Now())
			failures.Unlock()
		}
	}, true
}
func (s *Server) loginHousehold(w http.ResponseWriter, r *http.Request, username, password, device string, issueToken bool) {
	if s.deps.Store == nil {
		writeError(w, 503, "account storage is unavailable")
		return
	}
	if !s.sameOriginMutation(r) {
		writeError(w, 403, "sign in from this Curator installation")
		return
	}
	finish, ok := s.beginHouseholdPassword(r)
	if !ok {
		writeError(w, 429, "sign-in capacity is busy; try again shortly")
		return
	}
	success := false
	defer func() { finish(success) }()
	if err := s.deps.Store.EnsureInitialHouseholdOwner(r.Context()); err != nil {
		writeError(w, 503, "account storage is unavailable")
		return
	}
	account, err := s.deps.Store.AccountByUsername(r.Context(), username)
	if err != nil && !errors.Is(err, sqlite.ErrNotFound) {
		writeError(w, 503, "account storage is unavailable")
		return
	}
	if err != nil || account.Status != "active" {
		_, _ = verifyPassword(passwordHashPrefix+strings.Repeat("0", 32)+"$"+strings.Repeat("0", 64), "", password)
		writeError(w, 401, "bad credentials")
		return
	}
	valid, legacy := verifyPassword(account.PasswordHash, account.LegacySalt, password)
	if !valid {
		writeError(w, 401, "bad credentials")
		return
	}
	if legacy {
		upgraded, e := newPasswordHash(password)
		if e != nil {
			writeError(w, 503, "sign-in could not be completed")
			return
		}
		if e = s.deps.Store.UpgradeAccountPassword(r.Context(), account, upgraded); e != nil {
			writeError(w, 503, "sign-in could not be completed")
			return
		}
		account, err = s.deps.Store.AccountByID(r.Context(), account.ID)
		if err != nil || account.PasswordHash != upgraded {
			writeError(w, 401, "credentials changed; sign in again")
			return
		}
	}
	token, err := newHouseholdToken()
	if err != nil {
		writeError(w, 503, "sign-in could not be completed")
		return
	}
	if len(device) > 120 {
		device = device[:120]
	}
	expires := time.Now().Add(sessionTTL)
	if err = s.deps.Store.CreateAccountSession(r.Context(), digestSession(token), account, device, expires); err != nil {
		writeError(w, 409, "credentials changed; sign in again")
		return
	}
	success = true
	_ = s.deps.Store.PruneExpiredHouseholdCredentials(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	if issueToken {
		writeJSON(w, 200, struct {
			Token     string         `json:"token"`
			Account   domain.Account `json:"account"`
			ExpiresAt time.Time      `json:"expiresAt"`
		}{token, account, expires})
		return
	}
	s.setLocalSessionCookie(w, r, token, int(sessionTTL.Seconds()))
	w.WriteHeader(204)
}
func (s *Server) GetCurrentAccount(w http.ResponseWriter, r *http.Request) {
	p := currentPrincipal(r)
	if !p.Authenticated {
		writeError(w, 401, "sign in to your local Curator account")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, struct {
		Account    domain.Account `json:"account"`
		Method     string         `json:"method"`
		Machine    bool           `json:"machine"`
		AccessRole string         `json:"accessRole"`
	}{p.Account, p.Method, p.Machine, p.accessRole()})
}
func (s *Server) GetHouseholdSignInInfo(w http.ResponseWriter, r *http.Request) {
	established := false
	cinemaConfigured := false
	if s.deps.Store != nil {
		value, err := s.deps.Store.HouseholdEstablished(r.Context())
		if err != nil {
			writeError(w, 503, "sign-in information unavailable")
			return
		}
		established = value
		if c, e := s.deps.Store.CinemaConnection(r.Context()); e == nil && c.ClientSecret != "" {
			cinemaConfigured = true
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, struct {
		Required  bool `json:"required"`
		Household bool `json:"household"`
		Cinema    bool `json:"cinema"`
	}{s.authRequired(r.Context()), established, cinemaConfigured})
}

func (s *Server) CreateAccountSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Device   string `json:"device"`
	}
	if !decodeHouseholdBody(w, r, &body) {
		return
	}
	s.loginHousehold(w, r, body.Username, body.Password, body.Device, true)
}
func (s *Server) requireLocalAccount(w http.ResponseWriter, r *http.Request, owner bool) (domain.Account, bool) {
	p, ok := s.householdActor(w, r)
	if !ok {
		return domain.Account{}, false
	}
	if !p.Authenticated || p.Machine || p.Method != "local" || p.Account.ID <= 0 || owner && p.Account.Role != "owner" {
		writeError(w, 403, "sign in with an authorized local Curator account")
		return domain.Account{}, false
	}
	return p.Account, true
}
func (s *Server) reauthHousehold(w http.ResponseWriter, r *http.Request, owner bool, password string) (domain.Account, bool) {
	a, ok := s.requireLocalAccount(w, r, owner)
	if !ok {
		return a, false
	}
	finish, ok := s.beginHouseholdPassword(r)
	if !ok {
		writeError(w, 429, "sign-in capacity is busy; try again shortly")
		return a, false
	}
	success := false
	defer func() { finish(success) }()
	fresh, err := s.deps.Store.AccountByID(r.Context(), a.ID)
	if err != nil || fresh.CredentialGeneration != a.CredentialGeneration || fresh.Status != "active" {
		writeError(w, 401, "sign in again")
		return a, false
	}
	success, _ = verifyPassword(fresh.PasswordHash, fresh.LegacySalt, password)
	if !success {
		writeError(w, 401, "confirm your current Curator password")
		return a, false
	}
	return fresh, true
}
func (s *Server) ListHouseholdAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireLocalAccount(w, r, true); !ok {
		return
	}
	values, err := s.deps.Store.ListAccounts(r.Context())
	if err != nil {
		writeError(w, 503, "accounts are unavailable")
		return
	}
	writeJSON(w, 200, values)
}
func (s *Server) CreateHouseholdInvitation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !decodeHouseholdBody(w, r, &body) {
		return
	}
	owner, ok := s.reauthHousehold(w, r, true, body.Password)
	if !ok {
		return
	}
	token, err := newHouseholdToken()
	if err != nil {
		writeError(w, 503, "invitation could not be created")
		return
	}
	expires := time.Now().Add(7 * 24 * time.Hour)
	if err = s.deps.Store.CreateHouseholdInvite(r.Context(), owner, digestSession(requestSessionToken(r)), digestSession(token), expires); err != nil {
		writeError(w, 409, "invitation could not be created")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 201, struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
	}{token, expires})
}
func (s *Server) AcceptHouseholdInvitation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token       string `json:"token"`
		Username    string `json:"username"`
		DisplayName string `json:"displayName"`
		Password    string `json:"password"`
	}
	if !decodeHouseholdBody(w, r, &body) {
		return
	}
	body.Username = strings.ToLower(strings.TrimSpace(body.Username))
	body.DisplayName = strings.TrimSpace(body.DisplayName)
	if !s.sameOriginMutation(r) || !validAccountName(body.Username) || !validAccountPassword(body.Password) || len(body.DisplayName) > 100 || !utf8.ValidString(body.DisplayName) {
		writeError(w, 400, "choose a valid username and a password of at least 12 characters")
		return
	}
	if body.DisplayName == "" {
		body.DisplayName = body.Username
	}
	finish, ok := s.beginHouseholdPassword(r)
	if !ok {
		writeError(w, 429, "account setup is busy; try again shortly")
		return
	}
	success := false
	defer func() { finish(success) }()
	hash, err := newPasswordHash(body.Password)
	if err != nil {
		writeError(w, 503, "account could not be created")
		return
	}
	a, err := s.deps.Store.AcceptHouseholdInvite(r.Context(), digestSession(body.Token), body.Username, body.DisplayName, hash)
	if err != nil {
		writeError(w, 409, "invitation or username is unavailable")
		return
	}
	success = true
	writeJSON(w, 201, a)
}
func (s *Server) updateHouseholdAccount(w http.ResponseWriter, r *http.Request, id int64, owner bool) {
	var body struct {
		Revision    int64   `json:"revision"`
		Role        *string `json:"role"`
		Status      *string `json:"status"`
		DisplayName *string `json:"displayName"`
		NewPassword string  `json:"newPassword"`
		Password    string  `json:"password"`
	}
	if !decodeHouseholdBody(w, r, &body) {
		return
	}
	actor, ok := s.reauthHousehold(w, r, owner, body.Password)
	if !ok {
		return
	}
	if !owner {
		id = actor.ID
	}
	a, err := s.deps.Store.AccountByID(r.Context(), id)
	if err != nil {
		writeError(w, 404, "account unavailable")
		return
	}
	if body.Revision <= 0 || body.Revision != a.Revision {
		writeError(w, 409, "account changed; refresh it before saving")
		return
	}
	role, status, display := a.Role, a.Status, a.DisplayName
	if body.Role != nil {
		role = *body.Role
	}
	if body.Status != nil {
		status = *body.Status
	}
	if body.DisplayName != nil {
		display = strings.TrimSpace(*body.DisplayName)
	}
	if len(display) < 1 || len(display) > 100 || !utf8.ValidString(display) {
		writeError(w, 400, "choose a display name of at most 100 characters")
		return
	}
	hash := ""
	if body.NewPassword != "" {
		if !validAccountPassword(body.NewPassword) {
			writeError(w, 400, "use a password of at least 12 characters")
			return
		}
		hash, err = newPasswordHash(body.NewPassword)
		if err != nil {
			writeError(w, 503, "password could not be saved")
			return
		}
	}
	if err = s.deps.Store.UpdateAccount(r.Context(), actor, digestSession(requestSessionToken(r)), id, a.Revision, role, status, display, hash); err != nil {
		if errors.Is(err, sqlite.ErrLastOwner) {
			writeError(w, 409, err.Error())
		} else {
			writeError(w, 409, "account changed; review it again")
		}
		return
	}
	w.WriteHeader(204)
}
func (s *Server) UpdateOwnAccount(w http.ResponseWriter, r *http.Request) {
	s.updateHouseholdAccount(w, r, 0, false)
}
func (s *Server) UpdateHouseholdAccount(w http.ResponseWriter, r *http.Request, id int64) {
	s.updateHouseholdAccount(w, r, id, true)
}

func presentedCredentials(r *http.Request) bool {
	_, cookieErr := r.Cookie(sessionCookie)
	return cookieErr == nil || r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" || r.URL.Query().Get("apikey") != ""
}
func (s *Server) ownerStreamAuthorized(r *http.Request) bool {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	probe := r.WithContext(ctx)
	p, err := s.sessionPrincipal(probe)
	if err == nil {
		return p.accessRole() == "owner"
	}
	return errors.Is(err, sqlite.ErrNotFound) && !presentedCredentials(probe) && !s.authRequired(ctx)
}

// RecoverLocalOwner is for the host operator CLI, never an HTTP endpoint.
func RecoverLocalOwner(ctx context.Context, db *sqlite.DB, username, password string) error {
	if !validAccountPassword(password) {
		return errors.New("use a password of 12 to 1024 bytes")
	}
	hash, err := newPasswordHash(password)
	if err != nil {
		return err
	}
	return db.ResetLocalOwnerPassword(ctx, username, hash)
}
