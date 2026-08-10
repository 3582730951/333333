package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"codex-account-pool/internal/jsonview"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/virtual"
)

const (
	// Claude Code 2.1.226 computes its normal 200K auto-compaction boundary by
	// reserving at most 20K output tokens and then a further 13K compaction
	// buffer. Keep the relay-generated reactive error at the same boundary so a
	// Pro account behaves like the first-party client even when the downstream
	// selected a virtual [1m] alias. A genuinely selected 1M account bypasses the
	// relay guard entirely and lets the upstream/client own compaction.
	claudeStandardOutputReserveCap = int64(20_000)
	claudeStandardCompactBuffer    = int64(13_000)

	// Kiro's published summarization policy starts at 80% of the selected
	// model's context window. The current Google agent stack uses a 50% default
	// compression threshold; use it for Antigravity-backed Claude routes.
	kiroAutoCompactPercent        = int64(80)
	antigravityAutoCompactPercent = int64(50)
)

type claudeAutoCompactPlan struct {
	RequestedModel string
	ResolvedModel  string
	Provider       string
	EstimatedInput int64
	NativeWindow   int64
	EffectiveLimit int64
	OutputReserve  int64
	Virtual1M      bool
	TriggerPolicy  string
}

func (s *Server) selectedClaudeAutoCompactPlan(ctx context.Context, raw []byte, lease scheduler.Lease, resolvedModel, contextMode string, virtual1M bool) (claudeAutoCompactPlan, bool) {
	if s == nil || s.store == nil || len(raw) == 0 {
		return claudeAutoCompactPlan{}, false
	}
	capRow, ok := s.selectedClaudeCapability(ctx, lease.Account.ID, resolvedModel)
	if !ok {
		return claudeAutoCompactPlan{}, false
	}
	requested := requestedClaudeModelFromContext(ctx).RequestedModel
	if requested == "" {
		requested = resolvedModel
	}
	return buildClaudeAutoCompactPlan(raw, requested, resolvedModel, lease.Account.Provider, capRow, strings.EqualFold(contextMode, "1m"), virtual1M)
}

func (s *Server) selectedClaudeCapability(ctx context.Context, accountID, model string) (storage.ModelCapability, bool) {
	rows, err := s.store.ListCapabilities(ctx, accountID)
	if err != nil {
		return storage.ModelCapability{}, false
	}
	for _, row := range rows {
		if strings.EqualFold(strings.TrimSpace(row.ModelSlug), strings.TrimSpace(model)) {
			return row, true
		}
	}
	for _, row := range rows {
		if claudeRouteModelsEquivalent(row.ModelSlug, model) {
			return row, true
		}
	}
	return storage.ModelCapability{}, false
}

func buildClaudeAutoCompactPlan(raw []byte, requestedModel, resolvedModel, provider string, capRow storage.ModelCapability, extendedContext, virtual1M bool) (claudeAutoCompactPlan, bool) {
	window := capRow.NativeContextWindow
	if extendedContext && !virtual1M {
		window = capRow.NativeMaxContextWindow
	}
	if window <= 0 {
		window = capRow.NativeMaxContextWindow
	}
	if window <= 0 {
		return claudeAutoCompactPlan{}, false
	}
	// The scheduler already proved the selected account/model can route this native
	// 1M request. Do not invent an earlier relay boundary: forward it unchanged and
	// let Claude Code plus the real upstream enforce their own context contract.
	if !virtual1M && window >= 1_000_000 {
		return claudeAutoCompactPlan{}, false
	}

	percent := capRow.EffectiveContextWindowPercent
	if percent <= 0 || percent > 100 {
		percent = 100
	}
	effectiveWindow := window * percent / 100
	if effectiveWindow <= 0 {
		return claudeAutoCompactPlan{}, false
	}
	trigger, triggerPolicy := providerClaudeAutoCompactLimit(provider, window, effectiveWindow)
	claudeWindowPolicy := strings.HasPrefix(triggerPolicy, "claude_code_")
	if capRow.AutoCompactTokenLimit > 0 && capRow.AutoCompactTokenLimit < trigger {
		trigger = capRow.AutoCompactTokenLimit
		triggerPolicy += "+account_capability_limit"
	}
	outputReserve := jsonview.Get(raw, "max_tokens").Int()
	// The Claude standard-window policy already includes the exact bounded output
	// reserve used by Claude Code. Other policies reserve the full requested
	// output when it would be more conservative than their quality percentage.
	if outputReserve > 0 && !claudeWindowPolicy {
		if outputReserve >= effectiveWindow {
			trigger = 1
		} else {
			inputLimit := effectiveWindow - outputReserve
			if inputLimit < trigger {
				trigger = inputLimit
			}
		}
	}
	if trigger <= 0 {
		trigger = 1
	}
	estimated := virtual.EstimateClaudeTokensJSON(raw)
	if estimated <= trigger {
		return claudeAutoCompactPlan{}, false
	}
	return claudeAutoCompactPlan{
		RequestedModel: requestedModel,
		ResolvedModel:  resolvedModel,
		Provider:       provider,
		EstimatedInput: estimated,
		NativeWindow:   window,
		EffectiveLimit: trigger,
		OutputReserve:  outputReserve,
		Virtual1M:      virtual1M,
		TriggerPolicy:  triggerPolicy,
	}, true
}

func providerClaudeAutoCompactLimit(provider string, nativeWindow, effectiveWindow int64) (int64, string) {
	if effectiveWindow <= 0 {
		return 1, "invalid_window_guard"
	}
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "claude":
		// The first-party client derives this from the model's configured default
		// output allowance, not from one request's max_tokens field. Current Claude
		// models therefore take the full 20K bounded reserve even when a synthetic
		// test or third-party client asks for a very small response.
		limit := effectiveWindow - claudeStandardOutputReserveCap - claudeStandardCompactBuffer
		if limit < 1 {
			limit = 1
		}
		return limit, "claude_code_standard_window"
	case "kiro":
		return maxClaudeInt64(1, effectiveWindow*kiroAutoCompactPercent/100), "kiro_official_80pct"
	case "antigravity":
		return maxClaudeInt64(1, effectiveWindow*antigravityAutoCompactPercent/100), "antigravity_google_50pct"
	default:
		// Unknown future Claude-capable providers get Kiro's balanced 80% policy
		// instead of inheriting a potentially unsafe 90% synthetic window.
		return maxClaudeInt64(1, effectiveWindow*kiroAutoCompactPercent/100), "provider_default_80pct"
	}
}

func maxClaudeInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func writeClaudeAutoCompactRequired(w http.ResponseWriter, plan claudeAutoCompactPlan) {
	contextMode := "native"
	if plan.Virtual1M {
		contextMode = "virtual_1m"
	}
	w.Header().Set("X-MiCliProxy-Context-Status", "compact_required")
	w.Header().Set("X-MiCliProxy-Context-Mode", contextMode)
	w.Header().Set("X-MiCliProxy-Context-Limit", fmt.Sprintf("%d", plan.EffectiveLimit))
	w.Header().Set("X-MiCliProxy-Native-Context-Window", fmt.Sprintf("%d", plan.NativeWindow))
	w.Header().Set("X-MiCliProxy-Context-Estimated-Input", fmt.Sprintf("%d", plan.EstimatedInput))
	w.Header().Set("X-MiCliProxy-Context-Output-Reserve", fmt.Sprintf("%d", plan.OutputReserve))
	w.Header().Set("X-MiCliProxy-Context-Policy", plan.TriggerPolicy)
	w.Header().Set("X-MiCliProxy-Auto-Compact", "claude_code_reactive")
	w.Header().Set("X-MiCliProxy-Resolved-Provider", plan.Provider)
	w.Header().Set("X-Pool-Resolved-Provider", plan.Provider)
	w.Header().Set("X-Pool-Resolved-Model", plan.ResolvedModel)
	message := fmt.Sprintf(
		"Prompt is too long: %d tokens > %d; requested_model=%s resolved_model=%s provider=%s; Claude Code should automatically compact the conversation and retry",
		plan.EstimatedInput, plan.EffectiveLimit, plan.RequestedModel, plan.ResolvedModel, plan.Provider,
	)
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "invalid_request_error",
			"code":    "context_length_exceeded",
			"message": message,
		},
	})
}
