package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

const anthropicContext1MBeta = "context-1m-2025-08-07"

func isModelNotFoundError(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusNotFound {
		return false
	}
	lower := bytes.ToLower(body)
	return bytes.Contains(lower, []byte("model_not_found")) ||
		(bytes.Contains(lower, []byte("model")) && bytes.Contains(lower, []byte("not found")))
}

func (s *Server) rejectAccountModel(ctx context.Context, account storage.Account, model string, status int) {
	_ = s.store.SetModelCapabilityState(ctx, account.ID, model, capability.AvailabilityUnsupported, capability.Context1MUnknown, "", modelEvidenceSource(account, "rejected"))
	_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
		AccountID: account.ID, AccountLabel: account.Label,
		Action: "model_capability_rejected", State: "unsupported", Reason: "model_not_found",
		Detail: fmt.Sprintf("requested_model=%s http_status=%d", model, status),
	})
	if s.scheduler != nil {
		s.scheduler.InvalidateAccountCache()
	}
}

func (s *Server) verifyAccountModel(ctx context.Context, account storage.Account, model, contextMode string) {
	contextState, source := capability.Context1MUnknown, ""
	if strings.EqualFold(strings.TrimSpace(contextMode), "1m") {
		contextState, source = capability.Context1MSupported, "runtime_inference"
	}
	_ = s.store.SetModelCapabilityState(ctx, account.ID, model, capability.AvailabilityVerified, contextState, source, modelEvidenceSource(account, "inference"))
}

func modelEvidenceSource(account storage.Account, evidence string) string {
	provider := strings.ToLower(strings.TrimSpace(account.Provider))
	if provider == "" {
		provider = "unknown"
	}
	return provider + "_runtime_" + strings.ToLower(strings.TrimSpace(evidence))
}

func anthropicContext1MRequested(header http.Header) bool {
	for _, value := range header.Values("Anthropic-Beta") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), anthropicContext1MBeta) {
				return true
			}
		}
	}
	return false
}

func (s *Server) handleCapabilitySelectionError(ctx context.Context, w http.ResponseWriter, err error, anthropicShape bool, group, provider, requestedModel, contextMode string) bool {
	if err == nil || (!errors.Is(err, scheduler.ErrNoAccount) && !errors.Is(err, scheduler.ErrStrictUnavailable)) {
		return false
	}
	var noAccount *scheduler.NoAccountError
	modelUnsupported := errors.As(err, &noAccount) && noAccount.Counters.ModelUnsupported > 0
	context1M := strings.EqualFold(strings.TrimSpace(contextMode), "1m")
	if !context1M && !modelUnsupported {
		return false
	}

	code := "model_fallback_required"
	action := "model_fallback_required"
	requestedWindow := int64(0)
	if context1M {
		code = "claude_context_1m_unavailable"
		action = "context_1m_rejected"
		requestedWindow = 1000000
	}
	fallbackModel, fallbackCommand, fallbackWindow := s.capabilityFallback(ctx, group, provider, requestedModel, context1M)
	message := "The requested model is unavailable for the active account pool. Switch models manually and retry."
	if context1M {
		message = "The requested 1M context window is unavailable for the active account pool. Switch to the standard context model manually and retry."
	}
	errorObject := map[string]interface{}{
		"message":                  message,
		"type":                     "invalid_request_error",
		"code":                     code,
		"requested_model":          requestedModel,
		"fallback_model":           fallbackModel,
		"fallback_command":         fallbackCommand,
		"requested_context_window": requestedWindow,
		"fallback_context_window":  fallbackWindow,
		"native_context_window":    fallbackWindow,
		"manual_switch_required":   true,
	}
	if !anthropicShape {
		errorObject["param"] = "model"
	}
	// A user-group fallback probe writes into a private attempt buffer and may
	// immediately succeed on the next authorized target. Route-attempt diagnostics
	// already record that internal miss; auditing it here produced two durable rows
	// per probe and filled the 20K support-log window with failures the CLI never
	// received. Emit one semantic row only for the terminal selection result.
	//
	// model_capability_rejected remains reserved for rejectAccountModel, where an
	// upstream model_not_found response is real account-scoped capability evidence.
	if !userGroupFallbackProbe(ctx) {
		_ = s.store.InsertAuditLog(ctx, storage.AuditLogRow{
			Action: action,
			State:  "rejected",
			Reason: code,
			Detail: fmt.Sprintf("provider=%s requested_model=%s fallback_model=%s requested_context_window=%d fallback_context_window=%d manual_switch_required=true",
				provider, requestedModel, fallbackModel, requestedWindow, fallbackWindow),
		})
	}
	if anthropicShape {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"type": "error", "error": errorObject})
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": errorObject})
	}
	return true
}

func (s *Server) capabilityFallback(ctx context.Context, group, provider, requestedModel string, context1M bool) (string, string, int64) {
	parsed, _ := capability.ParseRequestedClaudeModel(requestedModel)
	baseModel := capability.ClaudeFacingKiroModelID(firstNonEmpty(parsed.BaseModel, requestedModel))
	if context1M {
		family := capability.ClaudeModelFamily(baseModel)
		commandModel := family
		if commandModel == "" {
			commandModel = baseModel
		}
		return baseModel, "/model " + commandModel, 200000
	}
	caps, _ := s.store.ListRoutableCapabilities(ctx, group)
	provider = strings.ToLower(provider)
	if strings.Contains(provider, "claude") || strings.Contains(provider, "kiro") || strings.HasPrefix(strings.ToLower(baseModel), "claude-") || capability.ClaudeModelFamily(baseModel) != "" {
		fallback := capability.SuggestedClaudeFallback(baseModel, caps)
		if fallback == "" {
			fallback = capability.ClaudeModelFamily(baseModel)
		}
		fallback = capability.ClaudeFacingKiroModelID(fallback)
		window := capabilityWindow(caps, fallback)
		if window == 0 {
			window = 200000
		}
		return fallback, modelCommand(fallback), window
	}
	fallback := suggestedCodexFallback(baseModel, caps)
	return fallback, modelCommand(fallback), capabilityWindow(caps, fallback)
}

func modelCommand(model string) string {
	if strings.TrimSpace(model) == "" {
		return ""
	}
	return "/model " + strings.TrimSpace(model)
}

func capabilityWindow(caps []storage.ModelCapability, model string) int64 {
	for _, c := range caps {
		if strings.EqualFold(strings.TrimSpace(c.ModelSlug), strings.TrimSpace(model)) {
			if c.NativeContextWindow > 0 {
				return c.NativeContextWindow
			}
			return c.NativeMaxContextWindow
		}
	}
	return 0
}

func suggestedCodexFallback(requested string, caps []storage.ModelCapability) string {
	requested = strings.ToLower(strings.TrimSpace(requested))
	tier := codexQualityTier(requested)
	best := ""
	for _, c := range caps {
		candidate := strings.ToLower(strings.TrimSpace(c.ModelSlug))
		if !strings.HasPrefix(candidate, "gpt-") || !naturalModelLess(candidate, requested) || codexQualityTier(candidate) != tier {
			continue
		}
		if best == "" || naturalModelLess(best, candidate) {
			best = c.ModelSlug
		}
	}
	return best
}

func codexQualityTier(model string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(model)), "-")
	if len(parts) > 2 {
		last := parts[len(parts)-1]
		if last == "sol" || last == "terra" || last == "luna" || last == "mini" || last == "nano" {
			return last
		}
	}
	return "base"
}
