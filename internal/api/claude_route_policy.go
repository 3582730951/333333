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

func claudeRouteModelsEquivalent(left, right string) bool {
	leftParsed, leftErr := capability.ParseRequestedClaudeModel(left)
	rightParsed, rightErr := capability.ParseRequestedClaudeModel(right)
	if leftErr == nil {
		left = leftParsed.BaseModel
	}
	if rightErr == nil {
		right = rightParsed.BaseModel
	}
	leftCanonical, leftKiro := capability.KiroCanonicalModel(left)
	rightCanonical, rightKiro := capability.KiroCanonicalModel(right)
	if leftKiro && rightKiro {
		return leftCanonical == rightCanonical
	}
	return strings.EqualFold(strings.TrimSpace(left), strings.TrimSpace(right))
}

// resolveClaudeProviders is the OpenAI Chat Completions compatibility policy.
// That surface currently has response bridges for official Claude and Kiro only;
// Antigravity is therefore not an executable provider for it yet. Native Claude
// Code Messages uses resolveClaudeMessageProviders below.
func (s *Server) resolveClaudeProviders(ctx context.Context, r *http.Request, pol downstreamPolicy) ([]string, string, error) {
	return s.resolveClaudeProvidersForSurface(ctx, r, pol, false)
}

// resolveClaudeMessageProviders implements the native /v1/messages contract:
// the account group is the routing boundary, and every provider in that group
// that can execute the exact requested model is eligible. The scheduler performs
// the account-scoped capability check; this layer must not hard-code Kiro merely
// because the group has no official Claude account.
func (s *Server) resolveClaudeMessageProviders(ctx context.Context, r *http.Request, pol downstreamPolicy) ([]string, string, error) {
	return s.resolveClaudeProvidersForSurface(ctx, r, pol, true)
}

func (s *Server) resolveClaudeProvidersForSurface(ctx context.Context, r *http.Request, pol downstreamPolicy, nativeMessages bool) ([]string, string, error) {
	providers, err := claudeAllowedProviders(r, pol)
	if err != nil {
		return nil, "", err
	}
	hint := normalizeProviderHintLoose(r.Header.Get("X-Pool-Provider"))
	if strings.TrimSpace(r.Header.Get("X-Pool-Provider")) == "" {
		hint = normalizeProviderHintLoose(pol.ProviderHint)
	}
	if hint != "" && hint != "auto" {
		if hint == "antigravity" && !nativeMessages {
			return nil, "", fmt.Errorf("provider antigravity is available on native /v1/messages, not the OpenAI Chat Completions Claude bridge")
		}
		return providers, hint, nil
	}
	if nativeMessages {
		return []string{"claude", "kiro", "antigravity"}, "auto", nil
	}

	// Keep the established Chat Completions compatibility behavior: an enabled
	// official Claude account remains primary and Kiro is its standby. Temporary
	// availability is intentionally irrelevant to that provider decision.
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

func kiroAffinityWait(_ context.Context, _ *Server, _ []string) time.Duration {
	// Do not add a proxy-side pacing delay to Kiro traffic. Exact conversation
	// affinity is enforced by the binding itself; when that account is immediately
	// available it is selected without sleeping, and otherwise normal scheduler
	// failover/error semantics apply.
	return 0
}
