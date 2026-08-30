package api

import (
	"net/http"

	"codex-account-pool/internal/authz"
)

// consoleBootstrap is intentionally a tiny identity-only projection. Tokens,
// cookies, CSRF values, account records and credentials are never serialized into
// the HTML shell.
func (s *Server) consoleBootstrap(r *http.Request) map[string]interface{} {
	payload := map[string]interface{}{
		"auth":               nil,
		"role":               "",
		"allow_registration": s.flagEnabled(r.Context(), "allow_registration", true),
		"ui_experience_v2":   s.flagEnabled(r.Context(), "ui_experience_v2", true),
		"setup_required":     false,
	}
	setupStatus, setupErr := s.store.AdminSetupStatus(r.Context())
	if setupErr == nil {
		payload["setup_required"] = setupStatus.Required
	}
	if user, ok := s.currentUser(r); ok {
		payload["auth"] = userView(user, "session")
		payload["role"] = user.Role
		return payload
	}
	if setupErr != nil || setupStatus.Required {
		return payload
	}
	if s.cfg.AdminToken != "" {
		if constantTimeTokenEqual(adminBearerToken(r), s.cfg.AdminToken) {
			payload["auth"] = map[string]interface{}{
				"id": "", "email": "admin", "name": "Admin", "role": "admin", "via": "admin_token", "authed": true,
				"capabilities": authz.ForRole("admin"),
			}
			payload["role"] = "admin"
		}
		return payload
	}
	return payload
}
