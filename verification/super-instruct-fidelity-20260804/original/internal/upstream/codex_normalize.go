package upstream

import (
	"bytes"
	"encoding/json"
	"strings"

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
//   - "parallel_tool_calls": classic Responses accepts this option only when at
//     least one top-level tool is present. Claude Code legitimately sends the
//     option alongside tools, so it is preserved there; when tools are absent,
//     null, or an empty array the orphan option is removed to prevent the
//     upstream 400. Responses Lite has a different wire contract and always
//     serializes false because its tools live in an additional_tools input item.
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
func normalizeCodexResponsesBody(raw []byte, upstreamBaseURL string, responsesLite bool) []byte {
	// The Lite header changes more than validation flags: the official client moves
	// the model-visible tool schemas and base instructions into developer input items
	// and omits both top-level members. Fold gateway-added fields back into a native
	// Lite envelope before the leaf normalizations below. Classic bodies never set
	// responsesLite and are deliberately left on the classic contract.
	if responsesLite {
		raw = normalizeCodexResponsesLiteEnvelope(raw)
	}
	wantStore := codexStoreValue(upstreamBaseURL)

	// Probe only the validated fields to decide whether any mutation is needed. The
	// struct decode correctly handles JSON escapes (e.g. "\n" → real newline) that a
	// string scan cannot, and ignores all other keys.
	var probe struct {
		Instructions      *string         `json:"instructions"`
		Store             *bool           `json:"store"`
		ParallelToolCalls json.RawMessage `json:"parallel_tool_calls"`
		Tools             json.RawMessage `json:"tools"`
		Reasoning         *struct {
			Context string `json:"context"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		// Malformed JSON passes through unchanged — the upstream will 400 with a parse
		// error, which is the correct signal (not a relay-injected field error).
		return raw
	}

	instructionsOK := responsesLite || (probe.Instructions != nil && strings.TrimSpace(*probe.Instructions) != "")
	storeOK := probe.Store != nil && *probe.Store == wantStore
	parallelOK := false
	if responsesLite {
		parallelOK = bytes.Equal(bytes.TrimSpace(probe.ParallelToolCalls), []byte("false"))
	} else {
		toolsEmpty, toolsKnown := codexRawToolsEmpty(probe.Tools)
		parallelOK = len(probe.ParallelToolCalls) == 0 || !toolsKnown || !toolsEmpty
	}
	reasoningContextOK := !responsesLite || (probe.Reasoning != nil && probe.Reasoning.Context == "all_turns")
	if instructionsOK && storeOK && parallelOK && reasoningContextOK {
		// Already matches the real-client shape: forward byte-identical.
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
		if responsesLite {
			if !set("parallel_tool_calls", false) {
				return raw
			}
		} else {
			var err error
			out, err = sjson.DeleteBytes(out, "parallel_tool_calls")
			if err != nil {
				return raw
			}
		}
	}
	if !reasoningContextOK {
		if !set("reasoning.context", "all_turns") {
			return raw
		}
	}
	return out
}

// codexRawToolsEmpty reports whether a top-level Responses tools value is
// definitely absent/null/a valid empty array. Unknown or malformed future shapes
// are not treated as empty: preserving them lets the upstream validate the actual
// tool payload instead of hiding a client error behind this compatibility guard.
func codexRawToolsEmpty(raw json.RawMessage) (empty, known bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return true, true
	}
	if trimmed[0] != '[' {
		return false, false
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(trimmed, &tools); err != nil {
		return false, false
	}
	return len(tools) == 0, true
}

// normalizeCodexResponsesLiteCompactBody applies only the Lite fields shared by
// normal and compact requests. Compact has its own schema, so it must not gain
// normal-turn fields such as store or client_metadata.
func normalizeCodexResponsesLiteCompactBody(raw []byte) []byte {
	raw = normalizeCodexResponsesLiteEnvelope(raw)
	var probe struct {
		ParallelToolCalls *bool `json:"parallel_tool_calls"`
		Reasoning         *struct {
			Context string `json:"context"`
		} `json:"reasoning"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return raw
	}
	out := raw
	var err error
	if probe.ParallelToolCalls == nil || *probe.ParallelToolCalls {
		out, err = sjson.SetBytes(out, "parallel_tool_calls", false)
		if err != nil {
			return raw
		}
	}
	if probe.Reasoning == nil || probe.Reasoning.Context != "all_turns" {
		out, err = sjson.SetBytes(out, "reasoning.context", "all_turns")
		if err != nil {
			return raw
		}
	}
	return out
}

// normalizeCodexResponsesLiteEnvelope folds gateway-added top-level instructions or
// tools back into an already-Lite request. The Lite transport starts input with:
//
//	{"type":"additional_tools","role":"developer","tools":[...]}
//
// followed by a developer message carrying non-empty base instructions. Classic
// requests are deliberately not upgraded here; they remain on the non-Lite contract.
func normalizeCodexResponsesLiteEnvelope(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}

	input, ok := codexResponsesInputItems(fields["input"])
	if !ok || len(input) == 0 || codexResponseItemType(input[0]) != "additional_tools" {
		return raw
	}

	topToolsRaw, hasTopTools := fields["tools"]
	var topTools []json.RawMessage
	if hasTopTools && !bytes.Equal(bytes.TrimSpace(topToolsRaw), []byte("null")) {
		if err := json.Unmarshal(topToolsRaw, &topTools); err != nil {
			return raw
		}
	}

	instructionsRaw, hasInstructions := fields["instructions"]
	var instructions *string
	if hasInstructions {
		if err := json.Unmarshal(instructionsRaw, &instructions); err != nil {
			return raw
		}
	}

	// This is the official shape already. Returning the original bytes is important
	// for cache-prefix stability and for exact passthrough of future input item fields.
	if !hasTopTools && !hasInstructions {
		return raw
	}

	if hasTopTools && len(topTools) > 0 {
		var firstFields map[string]json.RawMessage
		if err := json.Unmarshal(input[0], &firstFields); err != nil {
			return raw
		}
		var existing []json.RawMessage
		if existingRaw, present := firstFields["tools"]; present {
			if err := json.Unmarshal(existingRaw, &existing); err != nil {
				return raw
			}
		}
		merged := append(existing, topTools...)
		updated, err := sjson.SetRawBytes(input[0], "tools", marshalCodexRawArray(merged))
		if err != nil {
			return raw
		}
		input[0] = updated
	}

	if instructions != nil && *instructions != "" {
		developer, err := json.Marshal(struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}{
			Type: "message",
			Role: "developer",
			Content: []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}{{Type: "input_text", Text: *instructions}},
		})
		if err != nil {
			return raw
		}
		withDeveloper := make([]json.RawMessage, 0, len(input)+1)
		withDeveloper = append(withDeveloper, input[0], developer)
		input = append(withDeveloper, input[1:]...)
	}

	out, err := sjson.SetRawBytes(raw, "input", marshalCodexRawArray(input))
	if err != nil {
		return raw
	}
	for _, key := range []string{"instructions", "tools"} {
		if _, present := fields[key]; !present {
			continue
		}
		out, err = sjson.DeleteBytes(out, key)
		if err != nil {
			return raw
		}
	}
	return out
}

func codexResponsesInputItems(raw json.RawMessage) ([]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, false
	}
	return items, true
}

func codexResponseItemType(raw json.RawMessage) string {
	var item struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &item) != nil {
		return ""
	}
	return item.Type
}

func marshalCodexRawArray(items []json.RawMessage) json.RawMessage {
	var out bytes.Buffer
	out.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			out.WriteByte(',')
		}
		out.Write(bytes.TrimSpace(item))
	}
	out.WriteByte(']')
	return out.Bytes()
}

// codexRawIsNull reports whether a shared-map field value is absent or the JSON
// literal null. It reproduces the *json.RawMessage decode semantics used by the
// original per-field probes, where a null value is treated as "no field to change".
func codexRawIsNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// normalizeCodexReasoningEffortForWire mirrors codex-rs'
// reasoning_effort_for_request conversion. `ultra` remains a valid CLI/catalog
// setting for models that support automatic delegation, but the Responses API wire
// value is `max`. Other efforts and malformed/non-object payloads stay byte-identical.
func normalizeCodexReasoningEffortForWire(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	return normalizeCodexReasoningEffortForWireWithFields(raw, fields)
}

// normalizeCodexReasoningEffortForWireWithFields is the core taking the top-level
// object already parsed once at the dispatch choke point, so the whole body is not
// re-unmarshalled just to probe reasoning.effort. A nil map (non-object body) is a
// no-op, matching the wrapper's malformed-JSON passthrough.
func normalizeCodexReasoningEffortForWireWithFields(raw []byte, fields map[string]json.RawMessage) []byte {
	reasoningRaw, ok := fields["reasoning"]
	if !ok {
		return raw
	}
	var reasoning struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(reasoningRaw, &reasoning); err != nil || reasoning.Effort != "ultra" {
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
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	return stripCodexResponsesPromptCacheRetentionWithFields(raw, fields)
}

func stripCodexResponsesPromptCacheRetentionWithFields(raw []byte, fields map[string]json.RawMessage) []byte {
	if codexRawIsNull(fields["prompt_cache_retention"]) {
		return raw
	}
	out, err := sjson.DeleteBytes(raw, "prompt_cache_retention")
	if err != nil {
		return raw
	}
	return out
}

// stripCodexResponsesMaxOutputTokens removes the public Responses API output limit
// at the ChatGPT Codex transport boundary. The OAuth/WHAM endpoint rejects this field
// with "Unsupported parameter: max_output_tokens", while the official Codex client
// simply omits it. Callers deliberately apply this only to non-API-key accounts.
// sjson deletes the one leaf without round-tripping large tool/context integers.
func stripCodexResponsesMaxOutputTokens(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	return stripCodexResponsesMaxOutputTokensWithFields(raw, fields)
}

func stripCodexResponsesMaxOutputTokensWithFields(raw []byte, fields map[string]json.RawMessage) []byte {
	if codexRawIsNull(fields["max_output_tokens"]) {
		return raw
	}
	out, err := sjson.DeleteBytes(raw, "max_output_tokens")
	if err != nil {
		return raw
	}
	return out
}

// stripCodexResponsesHTTPGenerate removes the Responses WebSocket frame control
// before an HTTP/SSE request is sent. `generate` is valid on response.create /
// response.append WebSocket frames, but it is not part of either the classic
// Responses or ApiCompactionInput HTTP schema; WHAM rejects even a false/null value
// with "Unsupported parameter: generate". The caller must inspect the field first
// when it needs to classify a generate:false frame as a prewarm request.
func stripCodexResponsesHTTPGenerate(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}
	return stripCodexResponsesHTTPGenerateWithFields(raw, fields)
}

func stripCodexResponsesHTTPGenerateWithFields(raw []byte, fields map[string]json.RawMessage) []byte {
	if _, present := fields["generate"]; !present {
		return raw
	}
	out, err := sjson.DeleteBytes(raw, "generate")
	if err != nil {
		return raw
	}
	return out
}
