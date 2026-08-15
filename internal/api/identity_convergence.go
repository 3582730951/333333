package api

import (
	"context"
	"strings"

	"codex-account-pool/internal/identity"
)

func (s *Server) identityConvergenceMode(ctx context.Context) string {
	mode := strings.ToLower(strings.TrimSpace(s.settingString(ctx, "identity_convergence_mode", s.cfg.IdentityConvergenceMode)))
	if mode == "full" {
		return mode
	}
	return "off"
}

func (s *Server) virtualIdentity(ctx context.Context, accountID, osHint string) identity.Identity {
	return identity.ForOSWithConvergence(s.identitySecret(), accountID, osHint, s.identityConvergenceMode(ctx))
}
