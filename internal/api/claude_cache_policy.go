package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

const (
	claudeCachePolicyLegacy     = "legacy"
	claudeCachePolicyAggressive = "aggressive"
	claudeCachePolicyBalanced   = "balanced"
	claudeCachePolicyCoarseSafe = "coarse_safe"
	claudeCachePolicyStableSafe = "stable_prefix_safe"
	claudeCachePolicyMaxHit     = "max_hit"
	claudeCacheModeStableSafe   = "stable_safe"
)

type claudeCacheOptimizationRollout struct {
	Groups             []string `json:"groups"`
	APIKeyHashPrefixes []string `json:"api_key_hash_prefixes"`
	AccountIDs         []string `json:"account_ids"`
	Percent            *int     `json:"percent"`
}

func (s *Server) claudeSelectionAffinity(ctx context.Context, r *http.Request, raw, stableBody []byte, group, apiKeyHash, model string) routing.AffinityKey {
	if normalizeClaudeCacheAffinityPolicy(s.settingString(ctx, "claude_cache_affinity_policy", s.cfg.ClaudeCacheAffinityPolicy)) == claudeCachePolicyLegacy ||
		!s.claudeCacheRolloutAllows(ctx, group, apiKeyHash, "") {
		return routing.ExtractAffinityKey(r, raw)
	}
	if trueAffinity := routing.ExtractClaudeTrueAffinityKey(r, raw); trueAffinity.Hash != "" {
		return trueAffinity
	}
	if prefixHash := routing.AnthropicStablePromptPrefixHash(stableBody); prefixHash != "" {
		key := strings.Join([]string{
			"claude_cache_prefix",
			strings.TrimSpace(group),
			strings.TrimSpace(apiKeyHash),
			strings.TrimSpace(model),
			prefixHash,
		}, ":")
		return routing.AffinityFromKey(key, "cache_prefix_hash")
	}
	return routing.ExtractAffinityKey(r, raw)
}

// claudeCacheTTLForRoute picks the Claude prompt-cache TTL for a specific route. The
// configured 1h extended cache is used ONLY for true-conversation routes (a stable
// session / thread id) — long-lived turns that genuinely benefit from the longer
// retention. Collapsible routes (stable-prefix / coarse / anonymous, where several
// distinct requests can share a prefix) fall back to the 5m ephemeral cache: it bounds
// how long a shared prefix stays warm (limiting any staleness that could bleed context
// between distinct requests) and costs less to write (5m writes bill 1.25x vs 1h 2x),
// trimming the high_write_share the diagnostics flag on those routes. TTL is retention
// only — it never changes the model, the context sent, or reasoning — so real
// conversations keep their full 1h hit rate while collapsible routes get safer.
func (s *Server) claudeCacheTTLForRoute(ctx context.Context, affinity routing.AffinityKey) string {
	ttl := s.claudeCacheTTL(ctx)
	if ttl != "1h" {
		return ttl
	}
	// Route-aware differentiation is opt-in. Default OFF keeps the historical contract
	// (the configured TTL applies to every route). When enabled, only true-conversation
	// routes keep 1h; collapsible routes fall back to 5m to bound staleness / write cost.
	if !s.flagEnabled(ctx, "claude_cache_ttl_route_aware", s.cfg.ClaudeCacheTTLRouteAware) {
		return "1h"
	}
	if storage.RouteClassForAffinitySource(affinity.Source) == "true_conversation" {
		return "1h"
	}
	return ""
}

func (s *Server) claudeCacheBreakpointPolicy(ctx context.Context, affinity routing.AffinityKey, accountID, group, apiKeyHash string) string {
	mode := normalizeClaudeCacheMode(s.settingString(ctx, "claude_cache_mode", s.cfg.ClaudeCacheMode))
	policy := normalizeClaudeCacheBreakpointPolicy(s.settingString(ctx, "claude_cache_breakpoint_policy", s.cfg.ClaudeCacheBreakpointPolicy))
	if policy == claudeCachePolicyLegacy || !s.claudeCacheRolloutAllows(ctx, group, apiKeyHash, accountID) {
		return claudeCachePolicyLegacy
	}
	if mode == claudeCachePolicyMaxHit {
		return claudeCachePolicyMaxHit
	}
	if policy == claudeCachePolicyCoarseSafe {
		return claudeCachePolicyCoarseSafe
	}
	if policy == claudeCachePolicyStableSafe {
		return claudeCachePolicyStableSafe
	}
	if policy == claudeCachePolicyBalanced && storage.RouteClassForAffinitySource(affinity.Source) == "coarse" {
		return claudeCachePolicyCoarseSafe
	}
	if policy == claudeCachePolicyBalanced {
		return claudeCachePolicyStableSafe
	}
	return claudeCachePolicyAggressive
}

func normalizeClaudeCacheAffinityPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case claudeCachePolicyLegacy:
		return claudeCachePolicyLegacy
	default:
		return claudeCachePolicyBalanced
	}
}

func normalizeClaudeCacheMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case claudeCachePolicyMaxHit:
		return claudeCachePolicyMaxHit
	case "", claudeCacheModeStableSafe, claudeCachePolicyStableSafe:
		return claudeCacheModeStableSafe
	default:
		return claudeCacheModeStableSafe
	}
}

func normalizeClaudeCacheBreakpointPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case claudeCachePolicyLegacy:
		return claudeCachePolicyLegacy
	case claudeCachePolicyCoarseSafe:
		return claudeCachePolicyCoarseSafe
	case claudeCachePolicyBalanced:
		return claudeCachePolicyBalanced
	case claudeCachePolicyAggressive:
		return claudeCachePolicyAggressive
	case claudeCachePolicyStableSafe:
		return claudeCachePolicyStableSafe
	case claudeCachePolicyMaxHit:
		return claudeCachePolicyMaxHit
	default:
		return claudeCachePolicyStableSafe
	}
}

func (s *Server) claudeCacheRolloutAllows(ctx context.Context, group, apiKeyHash, accountID string) bool {
	raw := strings.TrimSpace(s.settingString(ctx, "claude_cache_optimization_rollout", s.cfg.ClaudeCacheOptimizationRollout))
	if raw == "" || raw == "{}" {
		return true
	}
	var rollout claudeCacheOptimizationRollout
	if err := json.Unmarshal([]byte(raw), &rollout); err != nil {
		return true
	}
	if len(rollout.Groups) > 0 && !stringInFoldedList(group, rollout.Groups) {
		return false
	}
	if len(rollout.APIKeyHashPrefixes) > 0 && !hasAnyPrefixFolded(apiKeyHash, rollout.APIKeyHashPrefixes) {
		return false
	}
	if accountID != "" && len(rollout.AccountIDs) > 0 && !stringInFoldedList(accountID, rollout.AccountIDs) {
		return false
	}
	percent := 100
	if rollout.Percent != nil {
		percent = *rollout.Percent
	}
	if percent <= 0 {
		return false
	}
	if percent >= 100 {
		return true
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{"claude-cache-rollout", group, apiKeyHash, accountID}, "\x00")))
	bucket := int(binary.BigEndian.Uint64(sum[:8]) % 100)
	return bucket < percent
}

func stringInFoldedList(value string, list []string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, item := range list {
		if strings.ToLower(strings.TrimSpace(item)) == value {
			return true
		}
	}
	return false
}

func hasAnyPrefixFolded(value string, prefixes []string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range prefixes {
		if p := strings.ToLower(strings.TrimSpace(prefix)); p != "" && strings.HasPrefix(value, p) {
			return true
		}
	}
	return false
}
