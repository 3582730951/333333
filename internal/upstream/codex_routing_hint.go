package upstream

import (
	"encoding/json"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// codexRoutingHintHeaderValue builds the x-codex-routing-hint value the official
// Codex CLI sends on every codex-backend request (codex-rs core/src/client.rs
// build_routing_hint_header): "model={model}" plus ";tier={tier}" when the body
// carries an explicit service_tier. The value is derived from the FINAL upstream
// body — this relay does not remap codex models, so the body model is the slug
// actually sent to the codex backend. It returns "" when no model can be derived
// or the hint would not be a valid header value: the official client never emits
// the header without a model, so omitting it beats sending a fabricated hint.
//
// BodyMeta (zero-copy/spooled) requests expose the model directly; service_tier
// is not a tracked BodyMeta scalar, so those requests produce a model-only hint,
// which is exactly what codex-rs sends when service_tier is None.
func codexRoutingHintHeaderValue(spec Request) string {
	var probe struct {
		Model       string `json:"model"`
		ServiceTier string `json:"service_tier"`
	}
	if spec.BodyMeta != nil {
		probe.Model = spec.BodyMeta.Model
	} else if err := json.Unmarshal(requestBody(spec), &probe); err != nil {
		return ""
	}
	model := strings.TrimSpace(probe.Model)
	if model == "" || strings.ContainsAny(model, ";=") {
		return ""
	}
	hint := "model=" + model
	if tier := strings.TrimSpace(probe.ServiceTier); tier != "" && !strings.ContainsAny(tier, ";=") {
		hint += ";tier=" + tier
	}
	if !httpguts.ValidHeaderFieldValue(hint) {
		return ""
	}
	return hint
}
