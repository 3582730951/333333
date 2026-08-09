package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"codex-account-pool/internal/routing"
)

const claudeCacheDiagnosticsHeader = "X-Codex-Claude-Cache-Diagnostics"

// applyClaudeCacheDiagnostics opts the turn into Anthropic's cache-diagnosis beta.
//
// claudeCacheDiagnosticsHeader is an INTERNAL control header: the upstream builder reads it
// off the downstream header set (upstream/anthropic.go applyClaudeHeaders) to decide whether
// to append the cache-diagnosis beta. It is never written into the upstream request itself —
// both Claude header builders construct their header set from scratch — so the gateway's own
// header name does not reach api.anthropic.com.
//
// It must still be stripped when the feature is disabled. Otherwise a downstream client can
// set it and drive the account's anthropic-beta set from outside the operator's control; the
// active beta set is part of what Anthropic fingerprints per request (it is called out as a
// prompt-affecting parameter in the cache-diagnostics docs), so letting an untrusted caller
// toggle it makes an account's beta set vary for reasons the operator never configured.
func (s *Server) applyClaudeCacheDiagnostics(ctx context.Context, headers http.Header, body []byte, affinity routing.AffinityKey) []byte {
	if !s.claudeCacheDiagnosticsEnabled(ctx) {
		headers.Del(claudeCacheDiagnosticsHeader)
		return body
	}
	headers.Set(claudeCacheDiagnosticsHeader, "1")
	key := strings.TrimSpace(affinity.Hash)
	if key == "" {
		return body
	}
	s.claudeCacheDiagMu.Lock()
	prev := s.claudeCacheDiagPrev[key]
	s.claudeCacheDiagMu.Unlock()
	if prev == "" {
		return body
	}
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	diag, _ := root["diagnostics"].(map[string]interface{})
	if diag == nil {
		diag = map[string]interface{}{}
		root["diagnostics"] = diag
	}
	diag["previous_message_id"] = prev
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func (s *Server) rememberClaudeCacheDiagnosticsMessageID(affinity routing.AffinityKey, body []byte) {
	key := strings.TrimSpace(affinity.Hash)
	if key == "" {
		return
	}
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return
	}
	id, _ := root["id"].(string)
	if id == "" {
		return
	}
	s.claudeCacheDiagMu.Lock()
	s.claudeCacheDiagPrev[key] = id
	s.claudeCacheDiagMu.Unlock()
}

func claudeDiagnosticsMissReason(body []byte) string {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	return findClaudeDiagnosticsMissReason(root)
}

func findClaudeDiagnosticsMissReason(v interface{}) string {
	m, ok := v.(map[string]interface{})
	if !ok {
		if arr, ok := v.([]interface{}); ok {
			for _, item := range arr {
				if reason := findClaudeDiagnosticsMissReason(item); reason != "" {
					return reason
				}
			}
		}
		return ""
	}
	if reason, _ := m["diagnostics_miss_reason"].(string); reason != "" {
		return reason
	}
	if reason, _ := m["miss_reason"].(string); reason != "" {
		return reason
	}
	for _, key := range []string{"system_changed", "tools_changed", "messages_changed", "unavailable"} {
		if b, _ := m[key].(bool); b {
			return key
		}
	}
	for _, child := range m {
		if reason := findClaudeDiagnosticsMissReason(child); reason != "" {
			return reason
		}
	}
	return ""
}

func withClaudeDiagnosticsMissReason(ctx context.Context, body []byte) context.Context {
	reason := claudeDiagnosticsMissReason(body)
	if reason == "" {
		return ctx
	}
	diag := usageDiagnosticsFromCtx(ctx)
	diag.DiagnosticsMissReason = reason
	return withUsageDiagnostics(ctx, diag)
}
