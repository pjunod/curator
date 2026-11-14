package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pjunod/monarr/internal/domain"
	"github.com/pjunod/monarr/internal/infra/sqlite"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const cinemaStateCookie = "curator_cinema_state"

var cinemaHTTPClient = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

type cinemaIdentity struct {
	Active      bool   `json:"active"`
	Issuer      string `json:"iss"`
	Subject     string `json:"sub"`
	Audience    string `json:"aud"`
	Scope       string `json:"scope"`
	DisplayName string `json:"displayName"`
	ExpiresAt   int64  `json:"expiresAt"`
	Grant       string `json:"grant"`
}

func cinemaHTTPS(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.ForceQuery || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.Opaque != "" {
		return nil, errors.New("use an exact HTTPS address without credentials, query or fragment")
	}
	return u, nil
}

// cinemaEndpoint accepts only the owner-configured HTTPS origin/prefix and
// this closed protocol's operations. Never concatenate a base's query/fragment
// with a path, or accept a caller-selected operation/redirect target.
func cinemaEndpoint(base, operation string, query url.Values) (string, error) {
	u, err := cinemaHTTPS(base)
	if err != nil {
		return "", err
	}
	switch operation {
	case "info", "exchange", "introspect", "revoke":
	default:
		return "", errors.New("invalid Cinema operation")
	}
	suffix := "/api/v1/delegation/" + operation
	escaped := strings.TrimRight(u.EscapedPath(), "/")
	u.Path = strings.TrimRight(u.Path, "/") + suffix
	u.RawPath = escaped + suffix
	u.RawQuery = query.Encode()
	return u.String(), nil
}
func cinemaCall(ctx context.Context, c domain.CinemaConnection, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	target, err := cinemaEndpoint(c.BaseURL, path, nil)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return errors.New("cinema connection is invalid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := cinemaHTTPClient.Do(req)
	if err != nil {
		return errors.New("cinema is unavailable; local Curator sign-in still works")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("cinema did not authorize this sign-in")
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(response.Body, 64<<10))
	if dec.Decode(out) != nil || dec.Decode(new(any)) != io.EOF {
		return errors.New("cinema returned an invalid response")
	}
	return nil
}
func cinemaValidIdentity(c domain.CinemaConnection, v cinemaIdentity) bool {
	return v.Active && v.Issuer == c.Issuer && v.Audience == c.ClientID && v.Scope == "household-requests" && len(v.Subject) >= 16 && len(v.Subject) <= 80 && v.ExpiresAt > time.Now().Unix()
}
func cinemaIntrospect(ctx context.Context, c domain.CinemaConnection, grant string) (cinemaIdentity, error) {
	var v cinemaIdentity
	err := cinemaCall(ctx, c, "introspect", map[string]string{"clientId": c.ClientID, "clientSecret": c.ClientSecret, "grant": grant}, &v)
	if err != nil {
		return v, err
	}
	if !cinemaValidIdentity(c, v) {
		return v, sqlite.ErrNotFound
	}
	return v, nil
}
func (s *Server) validateCinemaSession(ctx context.Context, session domain.AccountSession) error {
	c, err := s.deps.Store.CinemaConnection(ctx)
	if err != nil || c.ClientSecret == "" {
		return sqlite.ErrNotFound
	}
	link, err := s.deps.Store.CinemaLinkForSession(ctx, session.LinkID, session.LinkGeneration)
	if err != nil {
		return err
	}
	if link.AccountID != session.Account.ID || link.ConnectionGeneration != c.Generation || link.Issuer != c.Issuer || session.IntegrationGrant == "" {
		return sqlite.ErrNotFound
	}
	identity, err := cinemaIntrospect(ctx, c, session.IntegrationGrant)
	if err != nil {
		return err
	}
	if identity.Subject != link.Subject {
		return sqlite.ErrNotFound
	}
	return nil
}
func (s *Server) GetCinemaConnection(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireLocalAccount(w, r, true); !ok {
		return
	}
	c, err := s.deps.Store.CinemaConnection(r.Context())
	if errors.Is(err, sqlite.ErrNotFound) {
		writeJSON(w, 200, map[string]any{"configured": false})
		return
	}
	if err != nil {
		writeError(w, 503, "Cinema connection is unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"configured": c.ClientSecret != "", "baseUrl": c.BaseURL, "issuer": c.Issuer, "clientId": c.ClientID, "redirect": c.Redirect})
}
func (s *Server) SaveCinemaConnection(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Password     string `json:"password"`
		BaseURL      string `json:"baseUrl"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		Redirect     string `json:"redirect"`
		Disconnect   bool   `json:"disconnect"`
	}
	if !decodeHouseholdBody(w, r, &b) {
		return
	}
	actor, ok := s.reauthHousehold(w, r, true, b.Password)
	if !ok {
		return
	}
	var c *domain.CinemaConnection
	if !b.Disconnect {
		base, e := cinemaHTTPS(b.BaseURL)
		redirect, re := cinemaHTTPS(b.Redirect)
		if e != nil || re != nil || redirect.Path != "/api/v1/auth/cinema/callback" || len(b.BaseURL) > 2048 || len(b.ClientID) > 80 || len(b.ClientID) < 16 || len(b.ClientSecret) > 256 || len(b.ClientSecret) < 32 {
			writeError(w, 400, "use Cinema's HTTPS address, registered credentials and the exact Curator HTTPS callback")
			return
		}
		c = &domain.CinemaConnection{BaseURL: strings.TrimRight(base.String(), "/"), ClientID: b.ClientID, ClientSecret: b.ClientSecret, Redirect: redirect.String()}
		target, e := cinemaEndpoint(c.BaseURL, "info", url.Values{"clientId": {c.ClientID}})
		if e != nil {
			writeError(w, 400, "invalid Cinema address")
			return
		}
		req, e := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
		if e != nil {
			writeError(w, 400, "invalid Cinema address")
			return
		}
		res, e := cinemaHTTPClient.Do(req)
		if e != nil {
			writeError(w, 503, "Cinema connection could not be verified")
			return
		}
		defer func() { _ = res.Body.Close() }()
		var info struct {
			Issuer string `json:"issuer"`
			Scope  string `json:"scope"`
			Client struct {
				ID       string `json:"id"`
				Redirect string `json:"redirect"`
			} `json:"client"`
		}
		if res.StatusCode != 200 || json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&info) != nil || len(info.Issuer) < 16 || info.Client.ID != c.ClientID || info.Client.Redirect != c.Redirect || info.Scope != "household-requests" {
			writeError(w, 409, "Cinema's registered callback or identity does not match")
			return
		}
		c.Issuer = info.Issuer
		var check cinemaIdentity
		if e = cinemaCall(r.Context(), *c, "introspect", map[string]string{"clientId": c.ClientID, "clientSecret": c.ClientSecret, "grant": "invalid-configuration-check"}, &check); e != nil {
			writeError(w, 409, "Cinema client credentials could not be verified")
			return
		}
	}
	if e := s.deps.Store.SaveCinemaConnection(r.Context(), actor, digestSession(requestSessionToken(r)), c); e != nil {
		writeError(w, 409, "connection changed; confirm your local owner password again")
		return
	}
	w.WriteHeader(204)
}
func (s *Server) GetCinemaLink(w http.ResponseWriter, r *http.Request) {
	p, ok := s.householdActor(w, r)
	if !ok {
		return
	}
	l, err := s.deps.Store.CinemaLinkForAccount(r.Context(), p.Account.ID)
	if errors.Is(err, sqlite.ErrNotFound) {
		writeJSON(w, 200, map[string]bool{"linked": false})
		return
	}
	if err != nil {
		writeError(w, 503, "account link unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"linked": true, "issuer": l.Issuer})
}
func (s *Server) StartCinemaLink(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Password string `json:"password"`
	}
	if !decodeHouseholdBody(w, r, &b) {
		return
	}
	a, ok := s.reauthHousehold(w, r, false, b.Password)
	if !ok {
		return
	}
	s.beginCinema(w, r, "link", a, "")
}
func (s *Server) UnlinkCinema(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Password string `json:"password"`
	}
	if !decodeHouseholdBody(w, r, &b) {
		return
	}
	a, ok := s.reauthHousehold(w, r, false, b.Password)
	if !ok {
		return
	}
	if err := s.deps.Store.UnlinkCinema(r.Context(), a, digestSession(requestSessionToken(r))); err != nil {
		writeError(w, 409, "link could not be removed; refresh your account")
		return
	}
	w.WriteHeader(204)
}
func (s *Server) StartCinemaSignIn(w http.ResponseWriter, r *http.Request) {
	s.beginCinema(w, r, "signin", domain.Account{}, "")
}
func (s *Server) StartCinemaNative(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Challenge string `json:"challenge"`
	}
	if !decodeHouseholdBody(w, r, &b) {
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(b.Challenge)
	if err != nil || len(raw) != 32 {
		writeError(w, 400, "a native S256 challenge is required")
		return
	}
	s.beginCinema(w, r, "native", domain.Account{}, b.Challenge)
}
func cinemaChallenge(v string) string {
	hash := sha256.Sum256([]byte(v))
	return base64.RawURLEncoding.EncodeToString(hash[:])
}
func cinemaAuthorize(c domain.CinemaConnection, state, verifier string) string {
	q := url.Values{"clientId": {c.ClientID}, "state": {state}, "challenge": {cinemaChallenge(verifier)}}
	return c.BaseURL + "/#/curator-consent?" + q.Encode()
}
func cinemaSetState(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{Name: cinemaStateCookie, Value: token, Path: "/api/v1/auth/cinema/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: 300})
}
func (s *Server) beginCinema(w http.ResponseWriter, r *http.Request, mode string, a domain.Account, challenge string) {
	if s.deps.Store == nil {
		writeError(w, 503, "account storage unavailable")
		return
	}
	finish, allowed := beginHouseholdAttempt(r, &s.cinemaStarts)
	if !allowed {
		writeError(w, 429, "too many sign-in starts; try again shortly")
		return
	}
	defer finish(false)
	w.Header().Set("Referrer-Policy", "no-referrer")
	c, err := s.deps.Store.CinemaConnection(r.Context())
	if err != nil || c.ClientSecret == "" {
		writeError(w, 503, "Cinema sign-in is not configured; use your local Curator account")
		return
	}
	if !s.secureSessionRequest(r) {
		writeError(w, 400, "Cinema sign-in requires this Curator installation over HTTPS")
		return
	}
	state, e := newHouseholdToken()
	verifier, ve := newHouseholdToken()
	cookie, ce := newHouseholdToken()
	if e != nil || ve != nil || ce != nil {
		writeError(w, 503, "sign-in could not start")
		return
	}
	h := domain.CinemaHandoff{StateDigest: digestSession(state), Mode: mode, AccountID: a.ID, CredentialGeneration: a.CredentialGeneration, SessionDigest: digestSession(requestSessionToken(r)), ConnectionGeneration: c.Generation, Verifier: verifier, NativeChallenge: challenge, ExpiresAt: time.Now().Add(5 * time.Minute).UnixMilli()}
	if mode != "native" {
		h.CookieDigest = digestSession(cookie)
	}
	if err = s.deps.Store.CreateCinemaHandoff(r.Context(), h); err != nil {
		writeError(w, 409, "sign-in could not start; refresh the account")
		return
	}
	target := cinemaAuthorize(c, state, verifier)
	if mode == "native" {
		u, _ := url.Parse(c.Redirect)
		target = u.Scheme + "://" + u.Host + "/api/v1/auth/cinema/browser?state=" + url.QueryEscape(state)
	} else {
		cinemaSetState(w, r, cookie)
	}
	writeJSON(w, 200, map[string]string{"url": target, "state": state})
}
func (s *Server) OpenCinemaBrowser(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeError(w, 503, "account storage unavailable")
		return
	}
	state := r.URL.Query().Get("state")
	h, err := s.deps.Store.CinemaHandoff(r.Context(), digestSession(state))
	if err != nil || h.Mode != "native" {
		writeError(w, 409, "sign-in handoff is unavailable")
		return
	}
	c, err := s.deps.Store.CinemaConnection(r.Context())
	if err != nil || c.Generation != h.ConnectionGeneration {
		writeError(w, 409, "Cinema connection changed")
		return
	}
	cookie, err := newHouseholdToken()
	if err != nil {
		writeError(w, 503, "sign-in could not start")
		return
	}
	if err = s.deps.Store.OpenCinemaBrowser(r.Context(), h.StateDigest, digestSession(cookie)); err != nil {
		writeError(w, 409, "sign-in handoff was already opened")
		return
	}
	cinemaSetState(w, r, cookie)
	http.Redirect(w, r, cinemaAuthorize(c, state, h.Verifier), http.StatusSeeOther)
}
func (s *Server) CinemaCallback(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		http.Redirect(w, r, "/login?cinema=failed", http.StatusSeeOther)
		return
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	fail := func() { http.Redirect(w, r, "/login?cinema=failed", http.StatusSeeOther) }
	q := r.URL.Query()
	state := q.Get("state")
	if len(state) > 256 || len(q.Get("code")) > 256 {
		fail()
		return
	}
	h, err := s.deps.Store.CinemaHandoff(r.Context(), digestSession(state))
	cookie, e := r.Cookie(cinemaStateCookie)
	if err != nil || e != nil || subtle.ConstantTimeCompare([]byte(h.CookieDigest), []byte(digestSession(cookie.Value))) != 1 {
		fail()
		return
	}
	c, err := s.deps.Store.CinemaConnection(r.Context())
	if err != nil || c.Generation != h.ConnectionGeneration || q.Get("iss") != c.Issuer {
		fail()
		return
	}
	if h.Mode == "link" && digestSession(requestSessionToken(r)) != h.SessionDigest {
		fail()
		return
	}
	claim, err := newHouseholdToken()
	if err != nil {
		fail()
		return
	}
	if err = s.deps.Store.ClaimCinemaCallback(r.Context(), h, claim); err != nil {
		fail()
		return
	}
	h.Claim = claim
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		_ = s.deps.Store.CancelCinemaHandoff(ctx, h.StateDigest, h.Claim)
	}()
	var identity cinemaIdentity
	err = cinemaCall(r.Context(), c, "exchange", map[string]string{"clientId": c.ClientID, "clientSecret": c.ClientSecret, "code": q.Get("code"), "verifier": h.Verifier, "redirect": c.Redirect}, &identity)
	if err != nil || !cinemaValidIdentity(c, identity) || identity.Grant == "" {
		fail()
		return
	}
	owned := false
	defer func() {
		if !owned {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
			defer cancel()
			if s.deps.Store.QueueCinemaRevocation(ctx, c, identity.Grant, identity.ExpiresAt) != nil {
				_ = cinemaCall(ctx, c, "revoke", map[string]string{"clientId": c.ClientID, "clientSecret": c.ClientSecret, "grant": identity.Grant}, nil)
			}
		}
	}()
	if h.Mode == "link" {
		if err = s.deps.Store.FinishCinemaLink(r.Context(), h, domain.CinemaLink{Issuer: identity.Issuer, Subject: identity.Subject}); err != nil {
			fail()
			return
		}
		http.Redirect(w, r, "/account?cinema=linked", http.StatusSeeOther)
		return
	}
	link, err := s.deps.Store.CinemaLinkForSubject(r.Context(), identity.Issuer, identity.Subject)
	if err != nil || link.ConnectionGeneration != c.Generation {
		http.Redirect(w, r, "/login?cinema=unlinked", http.StatusSeeOther)
		return
	}
	link.Grant = identity.Grant
	link.GrantExpiresAt = identity.ExpiresAt * 1000
	if h.Mode == "native" {
		completion, e := newHouseholdToken()
		if e != nil {
			fail()
			return
		}
		h.CompletionDigest = digestSession(completion)
		if err = s.deps.Store.ReadyCinemaNative(r.Context(), h, link); err != nil {
			fail()
			return
		}
		owned = true
		http.Redirect(w, r, "monarr://cinema-complete?"+url.Values{"state": {state}, "code": {completion}}.Encode(), http.StatusSeeOther)
		return
	}
	token, err := newHouseholdToken()
	if err != nil {
		fail()
		return
	}
	if err = s.deps.Store.CreateCinemaSession(r.Context(), h, link, digestSession(token), "Cinema web", time.Now().Add(sessionTTL)); err != nil {
		fail()
		return
	}
	owned = true
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds())})
	http.Redirect(w, r, "/requests", http.StatusSeeOther)
}
func (s *Server) FinishCinemaNative(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		writeError(w, 503, "account storage unavailable")
		return
	}
	var b struct {
		State      string `json:"state"`
		Verifier   string `json:"verifier"`
		Completion string `json:"completion"`
	}
	if !decodeHouseholdBody(w, r, &b) {
		return
	}
	if len(b.State) > 256 || !validCinemaVerifier(b.Verifier) {
		writeError(w, 400, "invalid native handoff")
		return
	}
	h, err := s.deps.Store.CinemaHandoff(r.Context(), digestSession(b.State))
	if err != nil || h.Mode != "native" || subtle.ConstantTimeCompare([]byte(h.NativeChallenge), []byte(cinemaChallenge(b.Verifier))) != 1 {
		writeError(w, 409, "native handoff expired or changed")
		return
	}
	if h.ReadyLink == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if len(b.Completion) < 32 || len(b.Completion) > 256 || subtle.ConstantTimeCompare([]byte(digestSession(b.Completion)), []byte(h.CompletionDigest)) != 1 {
		writeError(w, 409, "Return from Cinema on the device that started sign-in")
		return
	}
	link, err := s.deps.Store.CinemaLinkByID(r.Context(), h.ReadyLink)
	if err != nil {
		writeError(w, 409, "linked account is unavailable")
		return
	}
	c, err := s.deps.Store.CinemaConnection(r.Context())
	if err != nil {
		writeError(w, 503, "Cinema connection unavailable")
		return
	}
	identity, err := cinemaIntrospect(r.Context(), c, h.Grant)
	if err != nil || identity.Subject != link.Subject {
		writeError(w, 503, "Cinema could not confirm the handoff; use local sign-in")
		return
	}
	link.Grant = h.Grant
	link.GrantExpiresAt = h.GrantExpiresAt
	a, err := s.deps.Store.AccountByID(r.Context(), link.AccountID)
	if err != nil {
		writeError(w, 503, "sign-in completed; refresh the account")
		return
	}
	token, err := newHouseholdToken()
	if err != nil {
		writeError(w, 503, "sign-in could not finish")
		return
	}
	expires := time.Now().Add(sessionTTL)
	if err = s.deps.Store.CreateCinemaSession(r.Context(), h, link, digestSession(token), "Cinema native", expires); err != nil {
		writeError(w, 409, "native handoff was already used or changed")
		return
	}
	writeJSON(w, 200, map[string]any{"token": token, "account": a, "expiresAt": expires})
}
func (s *Server) RunCinemaRevocations(ctx context.Context) {
	if s.deps.Store == nil {
		return
	}
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		_ = s.deps.Store.QueueDeadCinemaSessions(ctx)
		rows, err := s.deps.Store.CinemaRevocations(ctx)
		if err == nil {
			for _, r := range rows {
				if s.deps.Store.DeferCinemaRevocation(ctx, r.ID) != nil {
					continue
				}
				c := domain.CinemaConnection{BaseURL: r.BaseURL, ClientID: r.ClientID, ClientSecret: r.ClientSecret}
				if cinemaCall(ctx, c, "revoke", map[string]string{"clientId": c.ClientID, "clientSecret": c.ClientSecret, "grant": r.Grant}, nil) == nil {
					_ = s.deps.Store.FinishCinemaRevocation(ctx, r.ID)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func validCinemaVerifier(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, c := range []byte(value) {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("-._~", rune(c)) {
			return false
		}
	}
	return true
}
