package upstream

import (
	"encoding/json"
	"strings"

	"codex-account-pool/internal/capability"
	"github.com/tidwall/sjson"
)

// azureResponsesMarkers mirrors other_codex's matches_azure_responses_base_url
// (codex-api/src/provider.rs): the Codex client sets store:true ONLY when the upstream
// base URL is an Azure OpenAI responses endpoint, and store:false otherwise (e.g. the
// default chatgpt.com WHAM backend). The WHAM backend REJECTS store:true with
// 400 {"detail":"Store must be set to false"}, so the value is not cosmetic.
var azureResponsesMarkers = []string{
	"openai.azure.",
	"cognitiveservices.azure.",
	"aoai.azure.",
	"azure-api.",
	"azurefd.",
	"windows.net/openai",
}

// codexStoreValue returns the value the real Codex client would put in the responses
// request's "store" field for the given upstream base URL: true for an Azure responses
// endpoint, false otherwise. Verified against other_codex client.rs:781
// (store: provider.is_azure_responses_endpoint()).
func codexStoreValue(upstreamBaseURL string) bool {
	u := strings.ToLower(upstreamBaseURL)
	for _, m := range azureResponsesMarkers {
		if strings.Contains(u, m) {
			return true
		}
	}
	return false
}

// normalizeCodexResponsesBody ensures a Codex /responses request body matches the shape
// the real Codex client always sends, on the fields the WHAM backend hard-validates:
//
//   - "instructions": a non-empty top-level string for the classic Responses
//     contract. Responses Lite is the deliberate exception: codex-rs moves the
//     base instructions/tools into developer input items and omits the optional
//     top-level field. An explicitly supplied value is preserved; a missing one
//     must not be synthesized because that changes the real-client wire shape.
//     The classic backend returns
//     400 {"detail":"Instructions are required"} when absent/empty (other_codex:
//     ResponsesApiRequest.instructions). The relay normally forwards the downstream's own
//     instructions (or InjectResponsesSystemPrompt backfills one), but a client may send
//     instructions:"" or omit it — those would 400 on all three transports (WS/sidecar/HTTP).
//     BACKFILL-IF-MISSING: a client-supplied non-empty value is never overwritten.
//
//   - "store": the exact bool the real client sends — true only for an Azure responses
//     endpoint, false for the default chatgpt.com WHAM backend (other_codex client.rs:781,
//     store: provider.is_azure_responses_endpoint()). The WHAM backend returns
//     400 {"detail":"Store must be set to false"} when store:true, and a missing store is
//     also a fingerprint tell (the real client ALWAYS serializes it — store: bool with no
//     skip_serializing_if). FORCE-TO-CORRECT-VALUE: unlike instructions, a downstream client
//     that wrongly sends store:true must be corrected, not preserved.
//
//   - Responses Lite reasoning context: current GPT-5.6 models require
//     reasoning.context="all_turns" whenever the Responses Lite header/metadata is
//     present. codex-rs sets this in build_reasoning; omitting it makes the live
//     backend reject an otherwise valid request with HTTP 400. The gateway keeps
//     effort/summary intact and only adds or corrects the context member.
//
// DELIBERATELY NOT NORMALIZED HERE: "stream". The WHAM backend is streaming-only and a
// non-streaming (stream:false) request is rejected with the SAME 400 {"detail":"Store must
// be set to false"} even when store:false is present (stored-response mode conflicts with
// the mandatory store:false) — so stream:false, not the store value, is that 400's real
// trigger and is the next link in the masked-bug chain. We still do NOT force stream here,
// because this choke point also sees the third-party-chat path that intentionally sends a
// non-streaming request and reads back a COMPLETE JSON Responses object to convert into a
// Chat Completion (server.go isChat && !isStreamRequest → ResponsesToChatCompletion);
// forcing stream:true would hand that path an SSE stream and break it. The streaming
// requirement is therefore satisfied per-caller instead: real Codex clients always stream
// (buildCodexWebSocketCreatePayload hard-sets it), and the synthetic health-test probe sets
// stream:true itself (server.go adminHealthTest).
//
// This single normalization step at the dispatch choke point (client.Do) guarantees these
// fields before transport selection, so WS/sidecar/HTTP all benefit. It continues the
// Session-21 "masked next bug" chain: the sidecar Content-Type fix exposed the instructions
// 400, which once fixed exposed this store 400.
//
// BYTE-FIDELITY: when the body already matches all required fields, the ORIGINAL bytes are
// returned unchanged — no
// unmarshal/marshal round-trip that would reorder JSON keys or normalize whitespace (the
// relay's byte-for-byte forwarding contract — TestGatewayPreservesResponsesToolItemsWhenVirtual2MEnabled).
// We decide via a cheap two-field probe decode and only re-marshal when we must mutate.
func normalizeCodexResponsesBody(raw []byte, upstreamBaseURL string) []byte {
	wantStore := codexStoreValue(upstreamBaseURL)

	// Probe ONLY the two validated fields to decide whether any mutation is needed. The
	// struct decode correctly handles JSON escapes (e.g. "\n" → real newline) that a
	// string scan cannot, and ignores all other keys.
	var probe struct {
		Model             string  `json:"model"`
		Instructions      *string `json:"instructions"`
		Store             *bool   `json:"store"`
		ParallelToolCalls *bool   `json:"parallel_tool_calls"`
		Reasoning         *struct {
			Context string `json:"context"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		// Malformed JSON passes through unchanged — the upstream will 400 with a parse
		// error, which is the correct signal (not a relay-injected field error).
		return raw
	}

	responsesLite := capability.CodexUsesResponsesLite(probe.Model)
	instructionsOK := responsesLite || (probe.Instructions != nil && strings.TrimSpace(*probe.Instructions) != "")
	storeOK := probe.Store != nil && *probe.Store == wantStore
	parallelOK := !responsesLite || (probe.ParallelToolCalls != nil && !*probe.ParallelToolCalls)
	reasoningContextOK := !responsesLite || (probe.Reasoning != nil && probe.Reasoning.Context == "all_turns")
	if instructionsOK && storeOK && parallelOK && reasoningContextOK {
		// Already matches the real-client shape on both fields: forward byte-identical.
		return raw
	}

	// At least one field needs correction. Edit only the required leaves: input,
	// tools, instructions supplied by the client, previous_response_id, and all
	// other context must remain byte-identical (including integer literals that do
	// not fit in float64).
	out := raw
	set := func(path string, value interface{}) bool {
		var err error
		out, err = sjson.SetBytes(out, path, value)
		return err == nil
	}
	if !instructionsOK {
		// The same generic prompt the health-test probe uses. The classic backend
		// validates PRESENCE, not exact content. Responses Lite never reaches this
		// branch because its instructions are carried in developer input items.
		if !set("instructions", "You are a coding agent.") {
			return raw
		}
	}
	if !storeOK {
		// Force the exact value the real client sends for this upstream (false for WHAM).
		if !set("store", wantStore) {
			return raw
		}
	}
	if !parallelOK {
		if !set("parallel_tool_calls", false) {
			return raw
		}
	}
	if !reasoningContextOK {
		if !set("reasoning.context", "all_turns") {
			return raw
		}
	}
	return out
}

// normalizeCodexReasoningEffortForWire mirrors codex-rs'
// reasoning_effort_for_request conversion. `ultra` remains a valid CLI/catalog
// setting for models that support automatic delegation, but the Responses API wire
// value is `max`. Other efforts and malformed/non-object payloads stay byte-identical.
func normalizeCodexReasoningEffortForWire(raw []byte) []byte {
	var probe struct {
		Reasoning *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.Reasoning == nil || probe.Reasoning.Effort != "ultra" {
		return raw
	}
	// Change exactly the official wire field.  Re-marshalling the whole body via
	// map[string]interface{} would round large integer tool/context values through
	// float64 even though the mapping is unrelated to them.
	out, err := sjson.SetBytes(raw, "reasoning.effort", "max")
	if err != nil {
		return raw
	}
	return out
}

func stripCodexResponsesPromptCacheRetention(raw []byte) []byte {
	var probe struct {
		PromptCacheRetention *json.RawMessage `json:"prompt_cache_retention"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.PromptCacheRetention == nil {
		return raw
	}
	out, err := sjson.DeleteBytes(raw, "prompt_cache_retention")
	if err != nil {
		return raw
	}
	return out
}
