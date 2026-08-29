package api

import (
	"crypto/subtle"
	"net/http"
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
	}
	if user, ok := s.currentUser(r); ok {
		payload["auth"] = userView(user, "session")
		payload["role"] = user.Role
		return payload
	}
	if s.cfg.AdminToken != "" {
		if subtle.ConstantTimeCompare([]byte(adminBearerToken(r)), []byte(s.cfg.AdminToken)) == 1 {
			payload["auth"] = map[string]interface{}{
				"id": "", "email": "admin", "name": "Admin", "role": "admin", "via": "admin_token", "authed": true,
			}
			payload["role"] = "admin"
		}
		return payload
	}
	if s.hasAdminUser(r.Context()) {
		return payload
	}
	payload["auth"] = map[string]interface{}{
		"id": "", "email": "", "name": "", "role": "admin", "via": "open", "authed": false,
	}
	payload["role"] = "admin"
	return payload
}
