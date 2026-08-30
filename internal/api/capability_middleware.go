package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"codex-account-pool/internal/authz"
	"codex-account-pool/internal/storage"
)

func writeCapabilityError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    "authorization_error",
			"code":    code,
		},
	})
}

func (s *Server) writeSetupRequired(w http.ResponseWriter) {
	writeCapabilityError(w, http.StatusPreconditionRequired, "setup_required", "administrator setup is required")
}

// authorizeRegisteredRoute is the mandatory control-plane gate called before
// request body capture and routing. Handler-local checks remain defense in depth.
func (s *Server) authorizeRegisteredRoute(w http.ResponseWriter, r *http.Request) bool {
	required, protected := authz.RequiredForRoute(r.Method, r.URL.Path)
	if !protected {
		return true
	}
	return s.requireCapability(w, r, required)
}

func (s *Server) requireCapability(w http.ResponseWriter, r *http.Request, required authz.Capability) bool {
	// Ambient cookie identity always wins over every bearer credential. This is
	// the critical anti-confused-deputy rule for a normal portal user who happens
	// to paste or send an administrator/downstream token in the same browser.
	if user, ok := s.currentUser(r); ok {
		if !authz.Allows(user.Role, required) {
			s.auditAuthorizationDenied(required, "session_role")
			writeCapabilityError(w, http.StatusForbidden, "capability_required", "the authenticated user does not have this capability")
			return false
		}
		if !s.csrfOK(r) {
			s.auditAuthorizationDenied(required, "csrf")
			writeCapabilityError(w, http.StatusForbidden, "csrf_invalid", "invalid or missing CSRF token")
			return false
		}
		return true
	}

	// Owner-scoped portal APIs require a concrete browser user. A bearer admin
	// token has no owner id and therefore cannot be used to enumerate user data.
	if strings.HasPrefix(string(required), "portal.") {
		writeCapabilityError(w, http.StatusUnauthorized, "login_required", "login required")
		return false
	}

	hasAdmin, err := s.store.HasAdminUser(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("authorization storage unavailable"))
		return false
	}
	if !hasAdmin {
		s.writeSetupRequired(w)
		return false
	}

	if s.cfg.AdminToken != "" && constantTimeTokenEqual(adminBearerToken(r), s.cfg.AdminToken) {
		return true
	}
	if downstreamAPIKeyAttempt(r) {
		s.auditAuthorizationDenied(required, "non_admin_bearer")
		writeCapabilityError(w, http.StatusForbidden, "capability_required", "administrator capability required")
		return false
	}
	writeCapabilityError(w, http.StatusUnauthorized, "admin_login_required", "administrator login required")
	return false
}

func constantTimeTokenEqual(candidate, expected string) bool {
	return len(candidate) == len(expected) && expected != "" && subtleConstantTimeEqual(candidate, expected)
}

func subtleConstantTimeEqual(left, right string) bool {
	// Kept behind a tiny helper so all credential comparisons use identical
	// non-empty and equal-length preconditions.
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (s *Server) auditAuthorizationDenied(required authz.Capability, reason string) {
	if s == nil || s.store == nil {
		return
	}
	s.enqueueAudit(storage.AuditLogRow{
		Action: "authorization_denied", State: "denied", Reason: reason,
		Detail: "capability=" + string(required), CreatedAt: storage.Now(),
	})
}
