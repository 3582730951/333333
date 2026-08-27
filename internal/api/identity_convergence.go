package api

import (
	"context"
	"strings"

	"codex-account-pool/internal/identity"
)

// identityConvergenceMode normalizes the operator's device-convergence policy. An
// unset or unrecognized value means "account": one virtual device per account, stable
// across egress. Only an explicit "off" restores the legacy per-egress derivation,
// which mints a fresh installation id for every exit an account touches.
func (s *Server) identityConvergenceMode(ctx context.Context) string {
	switch strings.ToLower(strings.TrimSpace(s.settingString(ctx, "identity_convergence_mode", s.cfg.IdentityConvergenceMode))) {
	case "full":
		return "full"
	case "off":
		return "off"
	default:
		return "account"
	}
}

func (s *Server) virtualIdentity(ctx context.Context, accountID, osHint string) identity.Identity {
	return identity.ForOSWithConvergence(s.identitySecret(), accountID, osHint, s.identityConvergenceMode(ctx))
}
