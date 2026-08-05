package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"codex-account-pool/internal/cf"
	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/storage"

	"github.com/tidwall/gjson"
)

// codexGoalHoldsNonAuthoritativeQuotaSignal is the narrow account-stickiness
// boundary for an active Codex Goal turn. Local snapshots and ordinary rate-limit
// responses are telemetry, not proof that the CLI has reached its terminal usage
// state. Cloudflare responses are excluded because they describe the selected
// network exit rather than the account's subscription.
func codexGoalHoldsNonAuthoritativeQuotaSignal(ctx context.Context, status int, header http.Header, body []byte) bool {
	if !codexGoalQuotaGraceFromContext(ctx) || leakfilter.IsAuthoritativeCodexUsageLimit(status, body) {
		return false
	}
	if cf.Detect(status, header, body).Matched {
		return false
	}
	return status == http.StatusTooManyRequests || usageLimitSignal(body)
}

func codexGoalAuthoritativeUsageLimit(ctx context.Context, status int, body []byte) bool {
	return codexGoalQuotaGraceFromContext(ctx) && leakfilter.IsAuthoritativeCodexUsageLimit(status, body)
}

func authoritativeCodexUsageLimitCode(body []byte) string {
	outerType := strings.TrimSpace(gjson.GetBytes(body, "type").String())
	var code string
	if outerType == "response.failed" {
		code = strings.TrimSpace(gjson.GetBytes(body, "response.error.code").String())
	} else if outerType == "" || outerType == "error" {
		code = strings.TrimSpace(gjson.GetBytes(body, "error.type").String())
	}
	switch code {
	case "usage_limit_reached", "usage_not_included", "insufficient_quota":
		return code
	default:
		return ""
	}
}

// writeCodexGoalUsageLimitTerminal preserves only the fixed machine-readable
// terminal that stable Codex consumes. It intentionally omits account identifiers,
// plan details, reset timestamps, quota headers, and upstream prose.
func writeCodexGoalUsageLimitTerminal(w http.ResponseWriter, body []byte, stream bool) {
	code := authoritativeCodexUsageLimitCode(body)
	if code == "" {
		code = "usage_limit_reached"
	}
	const message = "The upstream account usage limit has been reached."
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"type": code, "code": code, "message": message},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	response := map[string]interface{}{
		"object": "response",
		"status": "failed",
		"error":  map[string]string{"code": code, "message": message},
	}
	_ = writeSSEEvent(w, "response.failed", map[string]interface{}{
		"type": "response.failed", "response": response,
	})
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *Server) auditCodexGoalQuotaDecision(ctx context.Context, accountID, state, reason string, status int) {
	if s == nil || s.store == nil {
		return
	}
	_ = s.store.InsertAuditLog(context.WithoutCancel(ctx), storage.AuditLogRow{
		AccountID: accountID,
		Action:    "codex_goal_quota_decision",
		State:     strings.TrimSpace(state),
		Reason:    strings.TrimSpace(reason),
		Detail:    http.StatusText(status),
	})
}
