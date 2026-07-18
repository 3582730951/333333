package upstream_error_rules

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"codex-account-pool/internal/storage"
)

const (
	MatchAny = "any"
	MatchAll = "all"

	AccountActionBuiltin         = "builtin"
	AccountActionNone            = "none"
	AccountActionCooldown        = "cooldown"
	AccountActionCooldownRecheck = "cooldown_recheck"
	AccountActionQuarantine      = "quarantine"
	// AccountActionAutoContinue marks matching traffic for the auto-continue subsystem:
	// on a truncated stream the relay re-issues once with a "continue" turn instead of
	// surfacing the truncation. It never punishes the account (the account is healthy;
	// the upstream merely stopped early), so for account fate it behaves like "none".
	AccountActionAutoContinue = "auto_continue"

	DownstreamActionBuiltin             = "builtin"
	DownstreamActionFailover            = "failover"
	DownstreamActionPass                = "pass"
	DownstreamActionCustomError         = "custom_error"
	DownstreamActionNeutralize          = "neutralize"
	DownstreamActionIdleStream          = "idle_stream"
	DownstreamActionIntercept           = "intercept"
	DownstreamActionHideSafetyBuffering = "hide_safety_buffering"
	// DownstreamActionHeartbeatFinish absorbs a matched upstream error on a streaming
	// request by emitting a single provider-appropriate keepalive frame (Codex
	// response.in_progress / Claude ping) and then closing the stream cleanly, instead
	// of surfacing the error. It positively simulates an upstream heartbeat-then-done so
	// a lenient client sees a benign empty stream rather than a hard failure.
	DownstreamActionHeartbeatFinish = "heartbeat_finish"
)

type MatchInput struct {
	Provider   string
	Entrypoint string
	Model      string
	Status     int
	Header     http.Header
	Body       []byte
	Streaming  bool
}

type MatchResult struct {
	Rule             storage.UpstreamErrorRule `json:"rule"`
	AccountAction    string                    `json:"account_action"`
	DownstreamAction string                    `json:"downstream_action"`
}

type ResponsePreview struct {
	Status           int    `json:"status"`
	Body             string `json:"body"`
	DownstreamAction string `json:"downstream_action"`
}

func Match(rules []storage.UpstreamErrorRule, in MatchInput) (MatchResult, bool) {
	ordered := append([]storage.UpstreamErrorRule(nil), rules...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority == ordered[j].Priority {
			if ordered[i].CreatedAt == ordered[j].CreatedAt {
				return ordered[i].ID < ordered[j].ID
			}
			return ordered[i].CreatedAt < ordered[j].CreatedAt
		}
		return ordered[i].Priority < ordered[j].Priority
	})
	for _, rule := range ordered {
		if !rule.Enabled {
			continue
		}
		if !scopeMatches(rule.Providers, in.Provider) || !scopeMatches(rule.Entrypoints, in.Entrypoint) || !modelMatches(rule.ModelPatterns, in.Model) {
			continue
		}
		if !conditionsMatch(rule, in) {
			continue
		}
		return MatchResult{Rule: rule, AccountAction: normalizeAccountAction(rule.AccountAction), DownstreamAction: normalizeDownstreamAction(rule.DownstreamAction)}, true
	}
	return MatchResult{}, false
}

func Preview(rule storage.UpstreamErrorRule, in MatchInput) ResponsePreview {
	action := normalizeDownstreamAction(rule.DownstreamAction)
	switch action {
	case DownstreamActionCustomError:
		status := rule.ResponseStatus
		if status == 0 {
			status = http.StatusBadGateway
		}
		msg := strings.TrimSpace(rule.CustomMessage)
		if msg == "" {
			msg = "Upstream provider returned an error"
		}
		body, _ := json.Marshal(map[string]interface{}{"error": map[string]interface{}{"message": msg}})
		return ResponsePreview{Status: status, Body: string(body), DownstreamAction: action}
	case DownstreamActionPass:
		status := in.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		return ResponsePreview{Status: status, Body: string(in.Body), DownstreamAction: action}
	case DownstreamActionNeutralize:
		return ResponsePreview{Status: http.StatusServiceUnavailable, Body: `{"error":{"message":"upstream temporarily unavailable"}}`, DownstreamAction: action}
	case DownstreamActionIdleStream:
		msg := strings.TrimSpace(rule.CustomMessage)
		if msg == "" {
			msg = "upstream rule matched; keeping stream alive"
		}
		return ResponsePreview{Status: http.StatusOK, Body: ": " + msg + "\n\n", DownstreamAction: action}
	case DownstreamActionHeartbeatFinish:
		// One keepalive frame then a clean close. The concrete frame is provider-
		// specific and rendered at relay time; the preview shows the intent.
		return ResponsePreview{Status: http.StatusOK, Body: "event: ping\ndata: {\"type\":\"ping\"}\n\n", DownstreamAction: action}
	default:
		return ResponsePreview{Status: in.Status, Body: "", DownstreamAction: action}
	}
}

func normalizeAccountAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case AccountActionNone, AccountActionCooldown, AccountActionCooldownRecheck, AccountActionQuarantine, AccountActionAutoContinue:
		return strings.ToLower(strings.TrimSpace(action))
	default:
		return AccountActionBuiltin
	}
}

func normalizeDownstreamAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case DownstreamActionFailover, DownstreamActionPass, DownstreamActionCustomError, DownstreamActionNeutralize, DownstreamActionIdleStream, DownstreamActionIntercept, DownstreamActionHideSafetyBuffering, DownstreamActionHeartbeatFinish:
		return strings.ToLower(strings.TrimSpace(action))
	default:
		return DownstreamActionBuiltin
	}
}

func scopeMatches(allowed []string, value string) bool {
	if len(allowed) == 0 {
		return true
	}
	value = strings.ToLower(strings.TrimSpace(value))
	for _, item := range allowed {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == value {
			return true
		}
		if (item == "chatgpt" && value == "codex") || (item == "codex" && value == "chatgpt") {
			return true
		}
		if item == "openai_compatible" && value != "" && value != "codex" && value != "chatgpt" && value != "claude" {
			return true
		}
	}
	return false
}

func modelMatches(patterns []string, model string) bool {
	if len(patterns) == 0 {
		return true
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, pattern := range patterns {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if pattern == "*" || globMatch(pattern, model) {
			return true
		}
	}
	return false
}

func conditionsMatch(rule storage.UpstreamErrorRule, in MatchInput) bool {
	statusConfigured := len(rule.StatusCodes) > 0
	keywordConfigured := len(rule.BodyKeywords) > 0
	if !statusConfigured && !keywordConfigured {
		return true
	}
	statusOK := false
	if statusConfigured {
		for _, status := range rule.StatusCodes {
			if status == in.Status {
				statusOK = true
				break
			}
		}
	}
	keywordOK := false
	if keywordConfigured {
		body := string(in.Body)
		if !rule.KeywordCaseSensitive {
			body = strings.ToLower(body)
		}
		for _, kw := range rule.BodyKeywords {
			kw = strings.TrimSpace(kw)
			if !rule.KeywordCaseSensitive {
				kw = strings.ToLower(kw)
			}
			if kw != "" && strings.Contains(body, kw) {
				keywordOK = true
				break
			}
		}
	}
	if strings.EqualFold(strings.TrimSpace(rule.MatchMode), MatchAll) {
		if statusConfigured && !statusOK {
			return false
		}
		if keywordConfigured && !keywordOK {
			return false
		}
		return true
	}
	return (statusConfigured && statusOK) || (keywordConfigured && keywordOK)
}

func globMatch(pattern, value string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	pos := 0
	if parts[0] != "" {
		if !strings.HasPrefix(value, parts[0]) {
			return false
		}
		pos = len(parts[0])
	}
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if part == "" {
			continue
		}
		idx := strings.Index(value[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	last := parts[len(parts)-1]
	if last != "" && !strings.HasSuffix(value, last) {
		return false
	}
	return true
}
