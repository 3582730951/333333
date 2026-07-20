package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/accountprovider"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/routing"
)

type requestedClaudeModelContextKey struct{}

func withRequestedClaudeModel(ctx context.Context, model capability.RequestedClaudeModel) context.Context {
	return context.WithValue(ctx, requestedClaudeModelContextKey{}, model)
}

func requestedClaudeModelFromContext(ctx context.Context) capability.RequestedClaudeModel {
	model, _ := ctx.Value(requestedClaudeModelContextKey{}).(capability.RequestedClaudeModel)
	return model
}

// resolveClaudeProviders implements strict primary/standby routing. Temporary
// availability is intentionally irrelevant: an enabled official Claude account
// keeps auto traffic on the official provider even while cooled, quarantined,
// rate-limited, or lacking a healthy egress.
func (s *Server) resolveClaudeProviders(ctx context.Context, r *http.Request, pol downstreamPolicy) ([]string, string, error) {
	providers, err := claudeAllowedProviders(r, pol)
	if err != nil {
		return nil, "", err
	}
	hint := normalizeProviderHintLoose(r.Header.Get("X-Pool-Provider"))
	if strings.TrimSpace(r.Header.Get("X-Pool-Provider")) == "" {
		hint = normalizeProviderHintLoose(pol.ProviderHint)
	}
	if hint != "" && hint != "auto" {
		return providers, hint, nil
	}
	accounts, err := s.store.ListAccounts(ctx)
	if err != nil {
		return nil, "auto", fmt.Errorf("load account snapshot for Claude standby routing: %w", err)
	}
	group := strings.TrimSpace(pol.Group)
	if group == "" {
		group = s.cfg.DefaultGroup
	}
	for _, account := range accounts {
		provider := strings.TrimSpace(account.Provider)
		if provider == "" {
			if token, tokenErr := s.store.GetToken(ctx, account.ID); tokenErr == nil {
				provider = accountprovider.InferProviderFromToken(token)
			}
		}
		if strings.EqualFold(provider, "claude") && account.Status == "active" && account.GroupName == group {
			return []string{"claude"}, "auto", nil
		}
	}
	return []string{"kiro"}, "auto", nil
}

func namespaceClaudeAffinity(base routing.AffinityKey, routeMode, contextMode string) routing.AffinityKey {
	if base.Hash == "" {
		return base
	}
	// Preserve legacy/official standard affinities. Provider filtering is enough
	// to invalidate an old automatic Kiro binding when Claude becomes primary.
	if contextMode == "" && routeMode != "kiro" {
		return base
	}
	if contextMode == "" {
		contextMode = "standard"
	}
	return routing.AffinityFromKey("claude_route:"+routeMode+":"+contextMode+":"+base.Hash, base.Source)
}

func kiroAffinityWait(ctx context.Context, s *Server, providers []string) time.Duration {
	if len(providers) != 1 || !strings.EqualFold(providers[0], "kiro") {
		return 0
	}
	millis := s.settingInt(ctx, "kiro_affinity_wait_millis", 1500)
	if millis < 1 {
		millis = 1500
	}
	return time.Duration(millis) * time.Millisecond
}
