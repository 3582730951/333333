package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	poolconfig "codex-account-pool/internal/config"
)

type gatewayTargetAction string

const (
	gatewayTargetIntercept gatewayTargetAction = "intercept"
	gatewayTargetForward   gatewayTargetAction = "forward"
	gatewayTargetBlock     gatewayTargetAction = "block"
)

type gatewayTargetDecision struct {
	Action gatewayTargetAction
	Reason string
}

type GatewayPolicy struct {
	InterceptHosts         []string `json:"intercept_hosts"`
	ForwardHosts           []string `json:"forward_hosts"`
	BlockedHostPatterns    []string `json:"blocked_host_patterns"`
	UnknownTargetPolicy    string   `json:"unknown_target_policy"`
	DisableNonessentialEnv bool     `json:"disable_nonessential_env"`
	StrictLinuxDefault     bool     `json:"strict_linux_default"`
}

func defaultGatewayPolicy() GatewayPolicy {
	return GatewayPolicy{
		InterceptHosts:         poolconfig.DefaultClaudeGatewayInterceptHosts(),
		ForwardHosts:           poolconfig.DefaultClaudeGatewayForwardHosts(),
		BlockedHostPatterns:    poolconfig.DefaultClaudeGatewayBlockedHostPatterns(),
		UnknownTargetPolicy:    "forward",
		DisableNonessentialEnv: false,
		StrictLinuxDefault:     true,
	}
}

func effectiveGatewayPolicy(policy GatewayPolicy) GatewayPolicy {
	if gatewayPolicyIsZero(policy) {
		return defaultGatewayPolicy()
	}
	policy.UnknownTargetPolicy = normalizeGatewayUnknownTargetPolicy(policy.UnknownTargetPolicy)
	return policy
}

func gatewayPolicyIsZero(policy GatewayPolicy) bool {
	return len(policy.InterceptHosts) == 0 &&
		len(policy.ForwardHosts) == 0 &&
		len(policy.BlockedHostPatterns) == 0 &&
		strings.TrimSpace(policy.UnknownTargetPolicy) == "" &&
		!policy.DisableNonessentialEnv &&
		!policy.StrictLinuxDefault
}

func classifyGatewayTarget(target, poolURL string, policies ...GatewayPolicy) gatewayTargetDecision {
	policy := defaultGatewayPolicy()
	if len(policies) > 0 {
		policy = effectiveGatewayPolicy(policies[0])
	}
	host := normalizeTargetHost(target)
	if host == "" {
		return gatewayTargetDecision{Action: gatewayTargetBlock, Reason: "empty host"}
	}
	if gatewayHostMatches(host, policy.BlockedHostPatterns) {
		return gatewayTargetDecision{Action: gatewayTargetBlock, Reason: "nonessential telemetry/update traffic"}
	}
	if gatewayHostMatches(host, policy.InterceptHosts) {
		return gatewayTargetDecision{Action: gatewayTargetIntercept}
	}
	if poolHost := poolURLHost(poolURL); poolHost != "" && host == poolHost {
		return gatewayTargetDecision{Action: gatewayTargetForward}
	}
	if gatewayHostMatches(host, policy.ForwardHosts) {
		return gatewayTargetDecision{Action: gatewayTargetForward}
	}
	if policy.UnknownTargetPolicy == "forward" {
		return gatewayTargetDecision{Action: gatewayTargetForward}
	}
	return gatewayTargetDecision{Action: gatewayTargetBlock, Reason: "gateway policy blocks unknown target"}
}

func shouldRewriteGatewayRequest(path string) bool {
	path = strings.SplitN(path, "?", 2)[0]
	return path == "/v1/messages"
}

func normalizeTargetHost(target string) string {
	target = strings.TrimSpace(strings.ToLower(target))
	if target == "" {
		return ""
	}
	if strings.Contains(target, "://") {
		if u, err := url.Parse(target); err == nil {
			target = u.Host
		}
	}
	if host, _, err := net.SplitHostPort(target); err == nil {
		return strings.Trim(host, "[]")
	}
	return strings.Trim(strings.Split(target, ":")[0], "[]")
}

func poolURLHost(poolURL string) string {
	u, err := url.Parse(strings.TrimSpace(poolURL))
	if err != nil {
		return ""
	}
	return normalizeTargetHost(u.Host)
}

func gatewayHostMatches(host string, patterns []string) bool {
	host = strings.TrimSuffix(strings.ToLower(normalizeTargetHost(host)), ".")
	if host == "" {
		return false
	}
	for _, pattern := range patterns {
		if gatewayHostPatternMatches(host, pattern) {
			return true
		}
	}
	return false
}

func gatewayHostPatternMatches(host, pattern string) bool {
	pattern = strings.TrimSuffix(strings.ToLower(normalizeTargetHost(pattern)), ".")
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if strings.Contains(pattern, "*") {
		return wildcardGatewayHostMatch(pattern, host)
	}
	if strings.Contains(pattern, ".") {
		return host == pattern || strings.HasSuffix(host, "."+pattern)
	}
	return strings.Contains(host, pattern)
}

func wildcardGatewayHostMatch(pattern, host string) bool {
	parts := strings.Split(pattern, "*")
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		idx := strings.Index(host[pos:], part)
		if idx < 0 {
			return false
		}
		if i == 0 && !strings.HasPrefix(pattern, "*") && idx != 0 {
			return false
		}
		pos += idx + len(part)
	}
	if !strings.HasSuffix(pattern, "*") {
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] != "" {
				return strings.HasSuffix(host, parts[i])
			}
		}
	}
	return true
}

func normalizeGatewayUnknownTargetPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "forward":
		return "forward"
	default:
		return "block"
	}
}

func (p *Proxy) gatewayPolicy() GatewayPolicy {
	identity, err := p.cache.Get("claude")
	if err != nil {
		log.Printf("gateway policy fetch failed: %v; using defaults", err)
		return defaultGatewayPolicy()
	}
	if identity == nil || identity.Virtual == nil {
		return defaultGatewayPolicy()
	}
	return effectiveGatewayPolicy(identity.Virtual.GatewayPolicy)
}

func cachedGatewayPolicy() (GatewayPolicy, bool) {
	path, err := runtimeIdentityPath()
	if err != nil {
		return GatewayPolicy{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return GatewayPolicy{}, false
	}
	var cached CachedIdentity
	if err := json.Unmarshal(data, &cached); err != nil {
		return GatewayPolicy{}, false
	}
	if cached.Virtual == nil {
		return GatewayPolicy{}, false
	}
	return effectiveGatewayPolicy(cached.Virtual.GatewayPolicy), true
}

func logBlockedGatewayTarget(target, reason string) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	dir := filepath.Join(home, ".claude-gateway")
	if err := os.MkdirAll(dir, gatewayPrivateDirMode); err != nil {
		return
	}
	if err := chmodGatewayPrivateDir(dir); err != nil {
		return
	}
	line := fmt.Sprintf("%s target=%s reason=%s\n", time.Now().Format(time.RFC3339), target, reason)
	path := filepath.Join(dir, "blocked.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, gatewayConfigFileMode)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
	_ = os.Chmod(path, gatewayConfigFileMode)
}
