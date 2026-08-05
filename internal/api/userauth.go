package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
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
// (true) on a DB error so an unavailable authorization database fails closed.
func (s *Server) hasAdminUser(ctx context.Context) bool {
	ok, err := s.store.HasAdminUser(ctx)
	return err != nil || ok
}

// ── login throttle (per-IP) ──

type loginThrottle struct {
	mu          sync.Mutex
	hits        map[string]*loginAttempt
	nextCleanup int64
	now         func() int64
}

type loginAttempt struct {
	count        int
	blockedUntil int64
	lastSeen     int64
}

const (
	loginMaxFailures        = 10
	loginBlockSecs          = 15 * 60
	loginThrottleTTLSeconds = 30 * 60
	loginThrottleSweepSecs  = 60
	loginThrottleMaxEntries = 8192
)

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{hits: map[string]*loginAttempt{}, now: storage.Now}
}

func (l *loginThrottle) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.currentTime()
	l.cleanupExpired(now)
	a := l.hits[ip]
	if a == nil {
		return len(l.hits) < loginThrottleMaxEntries || l.evictOldestInactive(now)
	}
	a.lastSeen = now
	return a.blockedUntil <= now
}

func (l *loginThrottle) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.currentTime()
	l.cleanupExpired(now)
	a := l.hits[ip]
	if a == nil {
		if len(l.hits) >= loginThrottleMaxEntries && !l.evictOldestInactive(now) {
			return
		}
		a = &loginAttempt{lastSeen: now}
		l.hits[ip] = a
	}
	a.lastSeen = now
	a.count++
	if a.count >= loginMaxFailures {
		a.blockedUntil = now + loginBlockSecs
		a.count = 0
	}
}

func (l *loginThrottle) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, ip)
}

func (l *loginThrottle) currentTime() int64 {
	if l.now != nil {
		return l.now()
	}
	return storage.Now()
}

// cleanupExpired is called with l.mu held. It runs at most once per sweep interval
// and never removes an active block, so TTL cleanup preserves the 15-minute ban.
func (l *loginThrottle) cleanupExpired(now int64) {
	if now < l.nextCleanup {
		return
	}
	for ip, attempt := range l.hits {
		if attempt == nil || (attempt.blockedUntil <= now && now-attempt.lastSeen >= loginThrottleTTLSeconds) {
			delete(l.hits, ip)
		}
	}
	l.nextCleanup = now + loginThrottleSweepSecs
}

// evictOldestInactive is called with l.mu held. Active blocks are never eligible:
// capacity pressure may forget an old partial failure count, but it must not shorten
// an established ban. It returns whether one slot was reclaimed.
func (l *loginThrottle) evictOldestInactive(now int64) bool {
	oldestIP := ""
	oldestSeen := int64(0)
	found := false
	for ip, attempt := range l.hits {
		if attempt == nil {
			delete(l.hits, ip)
			return true
		}
		if attempt.blockedUntil > now {
			continue
		}
		if !found || attempt.lastSeen < oldestSeen || (attempt.lastSeen == oldestSeen && ip < oldestIP) {
			oldestIP = ip
			oldestSeen = attempt.lastSeen
			found = true
		}
	}
	if !found {
		return false
	}
	delete(l.hits, oldestIP)
	return true
}

// ── helpers ──

// requestIsHTTPS reports whether the request reached us over TLS (directly or via a
// terminating reverse proxy), so session cookies are marked Secure only when they
// will actually be sent back (the panel is often served over plain HTTP).
func (s *Server) requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !s.isTrustedProxyRequest(r) {
		return false
	}
	proto := lastForwardedValue(r.Header.Get("X-Forwarded-Proto"))
	return strings.EqualFold(proto, "https")
}

func (s *Server) clientIP(r *http.Request) string {
	remote := remoteIP(r.RemoteAddr)
	if remote == nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	if !s.isTrustedProxyIP(remote) {
		return remote.String()
	}
	forwardedFor := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if forwardedFor == "" {
		for _, header := range []string{"CF-Connecting-IP", "X-Real-IP"} {
			if ip := net.ParseIP(strings.TrimSpace(r.Header.Get(header))); ip != nil {
				return ip.String()
			}
		}
		return remote.String()
	}
	values := strings.Split(forwardedFor, ",")
	candidate := remote
	for i := len(values) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(values[i]))
		if ip == nil {
			return remote.String()
		}
		candidate = ip
		if !s.isTrustedProxyIP(ip) {
			return ip.String()
		}
	}
	return candidate.String()
}

func (s *Server) isTrustedProxyRequest(r *http.Request) bool {
	return s.isTrustedProxyIP(remoteIP(r.RemoteAddr))
}

func (s *Server) isTrustedProxyIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, entry := range s.cfg.TrustedProxyCIDRs {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if trustedIP := net.ParseIP(entry); trustedIP != nil && trustedIP.Equal(ip) {
			return true
		}
		if _, network, err := net.ParseCIDR(entry); err == nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func remoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		host = strings.Trim(strings.TrimSpace(remoteAddr), "[]")
	}
	return net.ParseIP(host)
}

func lastForwardedValue(raw string) string {
	values := strings.Split(raw, ",")
	for i := len(values) - 1; i >= 0; i-- {
		if value := strings.TrimSpace(values[i]); value != "" {
			return value
		}
	}
	return ""
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
	secure := s.requestIsHTTPS(r)
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
	ph, err := hashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	u, err := s.store.CreateUserWithBootstrap(r.Context(), storage.User{
		ID: generatedID("usr"), Email: email, Name: strings.TrimSpace(req.Name), Status: "active", PasswordHash: ph,
	}, s.flagEnabled(r.Context(), "allow_registration", true))
	if errors.Is(err, storage.ErrUserEmailExists) {
		writeError(w, http.StatusConflict, errors.New("email already registered"))
		return
	}
	if errors.Is(err, storage.ErrRegistrationClosed) {
		writeError(w, http.StatusForbidden, errors.New("registration is disabled"))
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
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
	ip := s.clientIP(r)
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
	secure := s.requestIsHTTPS(r)
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
		if subtle.ConstantTimeCompare([]byte(adminBearerToken(r)), []byte(s.cfg.AdminToken)) == 1 {
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
