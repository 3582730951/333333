package api

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/storage"
)

const (
	adminSetupTTL        = 10 * time.Minute
	setupNonceCookieName = "cp_setup_nonce"
	setupNonceHeaderName = "X-CP-Setup-Nonce"
)

func (s *Server) adminSetupTokenMAC(plaintext string) string {
	return storage.AdminSetupTokenMAC(s.identitySecretCached, plaintext)
}

func remoteAdminSetupEnabled() bool {
	enabled, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("CODEX_POOL_ALLOW_REMOTE_SETUP")))
	return enabled
}

func (s *Server) setupRequestAllowed(r *http.Request) bool {
	// A loopback TCP peer is not sufficient evidence when a reverse proxy is
	// running on the same host: without an explicit trusted-proxy entry, an
	// attacker could send X-Forwarded-For and be mistaken for a local caller.
	// Direct loopback requests remain convenient; proxied requests must resolve
	// to loopback through a configured trusted proxy.
	remote := remoteIP(r.RemoteAddr)
	if remote != nil && remote.IsLoopback() {
		forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")) != "" ||
			strings.TrimSpace(r.Header.Get("X-Real-IP")) != "" ||
			strings.TrimSpace(r.Header.Get("CF-Connecting-IP")) != ""
		if !forwarded {
			return true
		}
		if !s.isTrustedProxyIP(remote) {
			return false
		}
		resolved := net.ParseIP(strings.TrimSpace(s.clientIP(r)))
		return resolved != nil && resolved.IsLoopback()
	}
	// Remote setup is an explicit emergency workflow and never runs over cleartext.
	return remoteAdminSetupEnabled() && s.requestIsHTTPS(r)
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	status, err := s.store.AdminSetupStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("setup status unavailable"))
		return
	}
	nonce, err := randomToken(24)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("setup nonce unavailable"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: setupNonceCookieName, Value: nonce, Path: "/", HttpOnly: false,
		SameSite: http.SameSiteStrictMode, Secure: s.requestIsHTTPS(r), MaxAge: int(adminSetupTTL / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"required": status.Required, "expires_at": status.ExpiresAt, "loopback_only": !remoteAdminSetupEnabled(),
	})
}

func setupNonceOK(r *http.Request, bodyNonce string) bool {
	cookie, err := r.Cookie(setupNonceCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return false
	}
	provided := firstNonEmpty(strings.TrimSpace(bodyNonce), strings.TrimSpace(r.Header.Get(setupNonceHeaderName)))
	return len(provided) == len(cookie.Value) && subtle.ConstantTimeCompare([]byte(provided), []byte(cookie.Value)) == 1
}

func (s *Server) auditAdminSetup(state, reason string) {
	if s == nil || s.store == nil {
		return
	}
	s.enqueueAudit(storage.AuditLogRow{
		Action: "admin_setup_claim", State: state, Reason: reason,
		Detail: "setup_token_redacted=true", CreatedAt: storage.Now(),
	})
}

func (s *Server) handleSetupClaimAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !s.setupRequestAllowed(r) {
		s.auditAdminSetup("denied", "loopback_or_https_required")
		writeError(w, http.StatusForbidden, errors.New("admin setup is restricted to loopback"))
		return
	}
	var req struct {
		SetupToken     string `json:"setup_token"`
		BootstrapNonce string `json:"bootstrap_nonce"`
		Email          string `json:"email"`
		Password       string `json:"password"`
		Name           string `json:"name"`
	}
	if err := decodeJSONRequestBody(r.Body, &req, adminJSONBodyLimit); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !setupNonceOK(r, req.BootstrapNonce) {
		s.auditAdminSetup("denied", "invalid_nonce")
		writeError(w, http.StatusForbidden, errors.New("invalid or missing setup nonce"))
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	if email == "" || !strings.Contains(email, "@") {
		writeError(w, http.StatusBadRequest, errors.New("a valid email is required"))
		return
	}
	if len(req.Password) < minPasswordLen {
		writeError(w, http.StatusBadRequest, errors.New("password does not meet minimum length"))
		return
	}
	passwordHash, err := hashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("password verifier unavailable"))
		return
	}
	sessionToken, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("session token unavailable"))
		return
	}
	csrfToken, err := randomToken(16)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("CSRF token unavailable"))
		return
	}
	now := storage.Now()
	adminID := generatedID("usr")
	adminCandidate := storage.User{
		ID: adminID, Email: email, Name: strings.TrimSpace(req.Name), PasswordHash: passwordHash,
	}
	sessionCandidate := storage.UserSession{
		TokenHash: hashAPIKey(sessionToken), UserID: adminID, UserAgent: r.UserAgent(),
		CreatedAt: now, ExpiresAt: now + int64(sessionTTL/time.Second),
	}
	legacyRecovery := s.cfg.AdminToken != "" && constantTimeTokenEqual(adminBearerToken(r), s.cfg.AdminToken)
	var admin storage.User
	if legacyRecovery {
		admin, err = s.store.ClaimAdminWithRecoveryCredential(r.Context(), adminCandidate, sessionCandidate, now)
	} else {
		admin, err = s.store.ClaimAdminSetup(r.Context(), s.adminSetupTokenMAC(req.SetupToken), adminCandidate, sessionCandidate, now)
	}
	if err != nil {
		status := http.StatusUnauthorized
		reason := "invalid_token"
		switch {
		case errors.Is(err, storage.ErrAdminSetupExpired):
			status, reason = http.StatusGone, "expired"
		case errors.Is(err, storage.ErrAdminSetupLocked):
			status, reason = http.StatusLocked, "locked"
		case errors.Is(err, storage.ErrAdminSetupCompleted):
			status, reason = http.StatusConflict, "already_completed"
		case errors.Is(err, storage.ErrAdminSetupUnavailable):
			status, reason = http.StatusConflict, "not_provisioned"
		case !errors.Is(err, storage.ErrAdminSetupInvalid):
			status, reason = http.StatusInternalServerError, "storage_error"
		}
		s.auditAdminSetup("denied", reason)
		writeError(w, status, errors.New("admin setup claim failed: "+reason))
		return
	}
	secure := s.requestIsHTTPS(r)
	maxAge := int(sessionTTL / time.Second)
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: sessionToken, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure, MaxAge: maxAge})
	http.SetCookie(w, &http.Cookie{Name: csrfCookieName, Value: csrfToken, Path: "/", HttpOnly: false, SameSite: http.SameSiteLaxMode, Secure: secure, MaxAge: maxAge})
	http.SetCookie(w, &http.Cookie{Name: setupNonceCookieName, Value: "", Path: "/", HttpOnly: false,
		SameSite: http.SameSiteStrictMode, Secure: s.requestIsHTTPS(r), MaxAge: -1})
	completionReason := "one_time_claim"
	if legacyRecovery {
		completionReason = "legacy_admin_token_recovery"
	}
	s.auditAdminSetup("completed", completionReason)
	writeJSON(w, http.StatusCreated, userView(admin, "session"))
}
