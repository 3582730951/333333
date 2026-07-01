package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
)

// userauth.go is the end-user authentication layer for the multi-user portal:
// register/login/logout/me + the cookie session and CSRF plumbing, plus the
// helpers (currentUser/requireUser) that the user-scoped and admin handlers use.
// It coexists with the legacy single admin_token (Bearer) auth — a request bearing
// the configured admin_token is still treated as an admin (see adminAllowed).

const (
	sessionCookieName = "cp_session" // HttpOnly session token
	csrfCookieName    = "cp_csrf"    // JS-readable double-submit CSRF token
	csrfHeaderName    = "X-CP-CSRF"
	sessionTTL        = 30 * 24 * time.Hour
	minPasswordLen    = 8
)

// dummyPasswordHash is verified against when the email is unknown / has no password,
// so a login attempt takes the same time whether or not the account exists (closes
// the account-enumeration timing oracle). Computed once at startup.
var dummyPasswordHash, _ = hashPassword("cp-constant-time-dummy")

// hasAdminUser reports whether the portal has been bootstrapped with an admin (used
// to lock down anonymous open-mode /admin once a real admin exists). Fails open
// (false) on a DB error to preserve zero-config bootstrap.
func (s *Server) hasAdminUser(ctx context.Context) bool {
	ok, err := s.store.HasAdminUser(ctx)
	return err == nil && ok
}

// ── login throttle (per-IP) ──

type loginThrottle struct {
	mu   sync.Mutex
	hits map[string]*loginAttempt
}

type loginAttempt struct {
	count        int
	blockedUntil int64
}

const (
	loginMaxFailures = 10
	loginBlockSecs   = 15 * 60
)

func newLoginThrottle() *loginThrottle { return &loginThrottle{hits: map[string]*loginAttempt{}} }

func (l *loginThrottle) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.hits[ip]
	return a == nil || a.blockedUntil <= storage.Now()
}

func (l *loginThrottle) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.hits[ip]
	if a == nil {
		a = &loginAttempt{}
		l.hits[ip] = a
	}
	a.count++
	if a.count >= loginMaxFailures {
		a.blockedUntil = storage.Now() + loginBlockSecs
		a.count = 0
	}
}

func (l *loginThrottle) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, ip)
}

// ── helpers ──

// requestIsHTTPS reports whether the request reached us over TLS (directly or via a
// terminating reverse proxy), so session cookies are marked Secure only when they
// will actually be sent back (the panel is often served over plain HTTP).
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func clientIP(r *http.Request) string {
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return xff
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	return host
}

// userView is the JSON shape returned to the SPA for the authenticated identity.
// via is "session" (logged-in user), "admin_token" (legacy Bearer), or "open"
// (no admin_token configured — historical open-admin deployment).
func userView(u storage.User, via string) map[string]interface{} {
	return map[string]interface{}{
		"id": u.ID, "email": u.Email, "name": u.Name,
		"role": u.Role, "status": u.Status, "via": via, "authed": true,
	}
}

// currentUser resolves the session cookie to an active user (ok=false when there is
// no valid session). It performs no writes beyond lazily expiring a stale session.
func (s *Server) currentUser(r *http.Request) (storage.User, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return storage.User{}, false
	}
	sess, ok, err := s.store.GetUserSession(r.Context(), hashAPIKey(c.Value))
	if err != nil || !ok {
		return storage.User{}, false
	}
	u, ok, err := s.store.GetUser(r.Context(), sess.UserID)
	if err != nil || !ok || u.Status != "active" {
		return storage.User{}, false
	}
	return u, true
}

// csrfOK enforces double-submit CSRF on unsafe, cookie-authenticated requests: the
// JS-readable cp_csrf cookie value must match the X-CP-CSRF header. Safe methods and
// Bearer-token (non-ambient) requests are exempt.
func (s *Server) csrfOK(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	cookie, err := r.Cookie(csrfCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(r.Header.Get(csrfHeaderName))) == 1
}

// requireUser resolves the session user for a /user/* endpoint, writing 401 when not
// logged in and 403 on a failed CSRF check (for unsafe methods).
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (storage.User, bool) {
	u, ok := s.currentUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, errors.New("login required"))
		return storage.User{}, false
	}
	if !s.csrfOK(r) {
		writeError(w, http.StatusForbidden, errors.New("invalid or missing CSRF token"))
		return storage.User{}, false
	}
	return u, true
}

// startSession creates a session row + sets the session and CSRF cookies.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u storage.User) error {
	tok, err := randomToken(32)
	if err != nil {
		return err
	}
	csrf, err := randomToken(16)
	if err != nil {
		return err
	}
	now := storage.Now()
	if err := s.store.CreateUserSession(r.Context(), storage.UserSession{
		TokenHash: hashAPIKey(tok), UserID: u.ID, UserAgent: r.UserAgent(),
		CreatedAt: now, ExpiresAt: now + int64(sessionTTL/time.Second),
	}); err != nil {
		return err
	}
	secure := requestIsHTTPS(r)
	maxAge := int(sessionTTL / time.Second)
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure, MaxAge: maxAge})
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: csrf, Path: "/", HttpOnly: false, SameSite: http.SameSiteLaxMode, Secure: secure, MaxAge: maxAge})
	return nil
}

func clearCookie(w http.ResponseWriter, name string, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: name == sessionCookieName, SameSite: http.SameSiteLaxMode, Secure: secure, MaxAge: -1})
}

// ── handlers ──

func (s *Server) handleAuthRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		writeError(w, http.StatusBadRequest, errors.New("a valid email is required"))
		return
	}
	if len(req.Password) < minPasswordLen {
		writeError(w, http.StatusBadRequest, fmt.Errorf("password must be at least %d characters", minPasswordLen))
		return
	}
	count, err := s.store.CountUsers(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// The very first user always bootstraps (and becomes admin); afterwards, honor the
	// admin's allow_registration toggle.
	if count > 0 && !s.flagEnabled(r.Context(), "allow_registration", true) {
		writeError(w, http.StatusForbidden, errors.New("registration is disabled"))
		return
	}
	if _, exists, err := s.store.GetUserByEmail(r.Context(), email); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	} else if exists {
		writeError(w, http.StatusConflict, errors.New("email already registered"))
		return
	}
	ph, err := hashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	role := "user"
	if count == 0 {
		role = "admin"
	}
	u := storage.User{ID: generatedID("usr"), Email: email, Name: strings.TrimSpace(req.Name), Role: role, Status: "active", PasswordHash: ph}
	if err := s.store.UpsertUser(r.Context(), u); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Save first admin credentials to passwd.txt
	if count == 0 {
		line := fmt.Sprintf("Admin: %s\nPassword: %s\n", email, req.Password)
		_ = os.WriteFile("passwd.txt", []byte(line), 0600)
	}
	if err := s.startSession(w, r, u); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, userView(u, "session"))
}

func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	ip := clientIP(r)
	if !s.login.allow(ip) {
		writeError(w, http.StatusTooManyRequests, errors.New("too many login attempts; try again later"))
		return
	}
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	u, ok, err := s.store.GetUserByEmail(r.Context(), email)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// Always run one PBKDF2 verify (against a dummy hash when the account is absent)
	// so timing does not reveal whether the email exists.
	verifyHash := dummyPasswordHash
	if ok && u.PasswordHash != "" {
		verifyHash = u.PasswordHash
	}
	passOK := verifyPassword(req.Password, verifyHash)
	if !ok || u.PasswordHash == "" || !passOK {
		s.login.fail(ip)
		writeError(w, http.StatusUnauthorized, errors.New("invalid email or password"))
		return
	}
	if u.Status != "active" {
		writeError(w, http.StatusForbidden, errors.New("account disabled"))
		return
	}
	s.login.reset(ip)
	if err := s.startSession(w, r, u); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, userView(u, "session"))
}

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		_ = s.store.DeleteUserSession(r.Context(), hashAPIKey(c.Value))
	}
	secure := requestIsHTTPS(r)
	clearCookie(w, sessionCookieName, secure)
	clearCookie(w, csrfCookieName, secure)
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// handleAuthMe reports the current identity for the SPA. A session user is returned
// directly; otherwise a configured+matching admin_token reports an admin identity,
// and a deployment with no admin_token reports the historical "open" admin so the
// panel works out of the box. Anything else is 401 (the SPA shows login/register).
func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	if u, ok := s.currentUser(r); ok {
		writeJSON(w, http.StatusOK, userView(u, "session"))
		return
	}
	allowReg := s.flagEnabled(r.Context(), "allow_registration", true)
	if s.cfg.AdminToken != "" {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") == s.cfg.AdminToken {
			writeJSON(w, http.StatusOK, map[string]interface{}{"id": "", "email": "admin", "name": "Admin", "role": "admin", "via": "admin_token", "authed": true})
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"authed": false, "allow_registration": allowReg})
		return
	}
	// No admin_token configured. Until an admin is registered the panel is usable open
	// (zero-config bootstrap); once an admin exists, anonymous access requires login.
	if s.hasAdminUser(r.Context()) {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"authed": false, "allow_registration": allowReg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": "", "email": "", "name": "", "role": "admin", "via": "open", "authed": false, "allow_registration": allowReg})
}
