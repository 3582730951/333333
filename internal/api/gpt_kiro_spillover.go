package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"strings"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/scheduler"
)

// autoKiroGPTBridge is the request-side representation Kiro needs. Kiro speaks
// the Anthropic-shaped conversation envelope; a Responses request additionally
// keeps its tool-name restoration plan for the output-side bridge.
type autoKiroGPTBridge struct {
	anthropicBody []byte
	responses     *prompt.ResponsesChatBridgeResult
}

const autoKiroGPTFallbackModel = "gpt-5.6-sol"

// tryServeAutoKiroGPT implements the GPT fair-pool admission policy:
//
//   - only an effective provider hint of "auto" is eligible;
//   - exact Kiro GPT-5.6 ids keep their model; another Codex model uses Kiro's
//     documented gpt-5.6-sol fallback when Kiro wins the fair selection;
//   - Kiro joins Codex as an equal, fair-scheduled candidate when the current
//     group's GPT policy admits it;
//   - a Codex winner or an unavailable Kiro pool falls through to the existing
//     native Codex route.
//
// The temporary fair selection deliberately uses the same account scheduler that
// owns the normal route. It does not make Kiro a priority provider: a selected
// Codex lease is immediately released and the native Codex path below performs the
// actual request. A Kiro winner is served here through the compatibility bridge.
func (s *Server) tryServeAutoKiroGPT(w http.ResponseWriter, r *http.Request, raw []byte, meta *bodysource.BodyMeta, model, group string, isChat, isCompact bool, pol downstreamPolicy) bool {
	kiroModel, eligible := autoKiroGPTModelForCodex(model)
	if s.scheduler == nil || effectiveGatewayProviderHint(r, pol) != "auto" || !eligible {
		return false
	}
	// Kiro accepts self-contained conversation history, not native Codex response
	// state. A previous_response_id / turn-state must stay on its account-bound
	// Codex route. Compaction and WebSocket requests are likewise handled by the
	// native Codex path only. Plan Mode also stays native: its collaboration
	// marker controls Codex's native planning/reasoning behavior and cannot be
	// represented losslessly by the Chat/Anthropic compatibility bridge.
	if isCompact || codexPlanModeRequest(raw) || (!isChat && r.URL.Path != "/v1/responses") || serverSideStateWithMeta(r.URL.Path, r, raw, meta) {
		return false
	}
	kiroCfg := s.effectiveKiroConfig(r.Context())
	kiroRoute := scheduler.Route{
		Group: group, Provider: "kiro", Model: kiroModel,
		KiroEndpointAllowlist: kiroCfg.KiroEndpointAllowlist,
		KiroDefaultRegion:     kiroCfg.KiroDefaultAPIRegion,
	}
	// When the group has no structurally compatible Kiro account, a fair-pool
	// selection can only choose Codex and is pure duplicate scheduling. This cheap
	// cached gate leaves real Kiro pressure/failover behavior unchanged.
	if candidates, countErr := s.scheduler.StructuralCandidateCount(r.Context(), kiroRoute); countErr != nil || candidates == 0 {
		return false
	}

	pressure, err := s.scheduler.ProviderPressureSnapshot(r.Context(), group, "codex", model)
	if err != nil {
		// Pressure is advisory. Preserve the current Codex behavior when a
		// diagnostic snapshot cannot be read rather than turning an observability
		// failure into a routing outage.
		log.Printf("[KIRO-GPT-SPILLOVER] pressure snapshot failed group=%s model=%s: %v", group, model, err)
		return false
	}
	if !pressure.ShouldAdmitKiroFairly() {
		return false
	}

	affinity := codexSelectionAffinityWithMeta(r, raw, meta, affinityWithMeta(r, raw, meta), group)
	// Fair scheduling is only for the first unbound request. Once an affinity
	// binding exists for this conversation and it targets a Kiro account, honor
	// the sticky binding so context is preserved across turns. Without this,
	// every turn re-runs the coin-flip and may land on a different Kiro account,
	// which changes the conversationId and loses any per-account upstream state.
	fairScheduling := true
	if affinity.Hash != "" {
		if bound, bindErr := s.store.GetAffinityBinding(r.Context(), affinity.Hash); bindErr == nil &&
			strings.EqualFold(strings.TrimSpace(bound.Provider), "kiro") {
			fairScheduling = false
		}
	}

	route := scheduler.Route{
		Group:                 group,
		AllowedProviders:      []string{"codex", "kiro"},
		Affinity:              affinity,
		FairScheduling:        fairScheduling,
		KiroEndpointAllowlist: kiroCfg.KiroEndpointAllowlist,
		KiroDefaultRegion:     kiroCfg.KiroDefaultAPIRegion,
		Model:                 model,
		KiroFallbackModel:     kiroModel,
		EstimatedTokens:       estimatedTokensWithMeta(raw, meta),
	}

	// Select normally queues a request when every matching account is temporarily
	// saturated. Auto admission needs an immediate fair-pool snapshot instead, so
	// a busy Kiro pool or a fair Codex winner simply follows the existing Codex
	// route. WithoutCancel retains request values but presents a nil Done channel
	// to Scheduler's queue gate.
	lease, err := s.scheduler.Select(context.WithoutCancel(r.Context()), route)
	if err != nil {
		if !errors.Is(err, scheduler.ErrNoAccount) {
			log.Printf("[KIRO-GPT-SPILLOVER] Kiro selection failed group=%s model=%s: %v", group, model, err)
		}
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(lease.Account.Provider), "kiro") {
		// Codex won the same fair selection. Its real lease is acquired by the
		// native path below, where CPA/session mapping and retry handling remain
		// authoritative; do not hold a speculative reservation across that path.
		lease.Release()
		return false
	}
	// Conversion can allocate in proportion to the full request. Defer it until a
	// Kiro account actually wins: under Codex-only pressure, eagerly building the
	// bridge for every admitted candidate retained several extra million-token body
	// copies and could exhaust the gateway before the fair scheduler chose Codex.
	bridge, ok := buildAutoKiroGPTBridge(raw, isChat, kiroModel)
	if !ok {
		lease.Release()
		// The native Codex path remains lossless for any request shape that cannot
		// be represented through Kiro's Chat/Anthropic compatibility bridge.
		return false
	}
	resolvedKiroModel := firstNonEmpty(lease.ResolvedModel, kiroModel)
	if !strings.EqualFold(strings.TrimSpace(model), resolvedKiroModel) {
		w.Header().Set("X-Pool-Kiro-Fallback-From", strings.TrimSpace(model))
	}

	if bridge.responses == nil {
		s.kiroChatWithLease(w, r, bridge.anthropicBody, resolvedKiroModel, affinity, lease, false, nil)
		return true
	}
	// Preserve the request-side Responses bridge diagnostics in the Kiro usage row
	// and surface the same losses to the downstream Responses client.
	r = withResponsesCompatibilityLosses(r, bridge.responses.CompatibilityLosses)
	writer, err := newKiroResponsesBridgeWriter(r.Context(), w, isStreamRequest(raw), resolvedKiroModel, bridge.responses.Plan, bridge.responses.CompatibilityLosses, s.responseBodyCaptureOptions(r.Context()))
	if err != nil {
		lease.Release()
		writeError(w, http.StatusInternalServerError, err)
		return true
	}
	s.kiroChatWithLease(writer, r, bridge.anthropicBody, resolvedKiroModel, affinity, lease, false, nil)
	writer.finish()
	return true
}

const codexPlanModeMarker = "<collaboration_mode>plan</collaboration_mode>"

// codexPlanModeRequest identifies the official Codex plan-mode developer item.
// Keep this as an allocation-free marker scan because raw is already the
// captured request body and this check runs on every eligible GPT request.
func codexPlanModeRequest(raw []byte) bool {
	return bytes.Contains(raw, []byte(codexPlanModeMarker))
}

func autoKiroGPTModelForCodex(model string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" || isClaudeModel(model) {
		return "", false
	}
	if capability.KiroSupportsGPTModel(model) {
		return model, true
	}
	return autoKiroGPTFallbackModel, true
}

func buildAutoKiroGPTBridge(raw []byte, isChat bool, kiroModel string) (autoKiroGPTBridge, bool) {
	// The bridge is only sent after the fair scheduler selected a Kiro account.
	// Rewrite its model here, not on the shared native Codex request: a Codex
	// winner still receives the exact downstream model.
	raw = setForcedModel(raw, kiroModel)
	if isChat {
		anthropic, err := prompt.ChatCompletionToAnthropic(raw)
		if err != nil {
			return autoKiroGPTBridge{}, false
		}
		return autoKiroGPTBridge{anthropicBody: anthropic}, true
	}
	responses, err := prompt.ResponsesRequestToChatCompletionBridge(raw)
	if err != nil {
		return autoKiroGPTBridge{}, false
	}
	anthropic, err := prompt.ChatCompletionToAnthropic(responses.Body)
	if err != nil {
		return autoKiroGPTBridge{}, false
	}
	return autoKiroGPTBridge{anthropicBody: anthropic, responses: &responses}, true
}

// effectiveGatewayProviderHint mirrors the header-over-key behavior used by the
// Claude route policy. Invalid explicit headers never activate automatic Kiro
// spillover; the legacy native Codex path remains the conservative fallback.
func effectiveGatewayProviderHint(r *http.Request, pol downstreamPolicy) string {
	if raw := strings.TrimSpace(r.Header.Get("X-Pool-Provider")); raw != "" {
		if hint, ok := normalizeProviderHint(raw); ok {
			return hint
		}
		return ""
	}
	return normalizeProviderHintLoose(pol.ProviderHint)
}
