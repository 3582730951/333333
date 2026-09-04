package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
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

// codexIncludeWireValue is the include array the official Codex CLI always
// serializes (codex-rs client.rs:959 — ResponsesApiRequest.include has no
// skip_serializing_if): it requests server-side encrypted reasoning delivery.
var codexIncludeWireValue = []byte(`["reasoning.encrypted_content"]`)

// codexIncludeNeedsDefault reports whether a Responses include member is a wire
// deviation from that always-serialized canonical value: absent or an empty
// array. A non-empty include is a downstream parse contract and is preserved
// verbatim — forcing a different value would hand the client a stream it did
// not ask for. A non-array include is left for the upstream to validate.
func codexIncludeNeedsDefault(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return true
	}
	var items []string
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return false
	}
	if len(items) == 0 {
		return true
	}
	for _, item := range items {
		if item == "reasoning.encrypted_content" {
			return false
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
//   - "include": the official client ALWAYS serializes include:["reasoning.
//     encrypted_content"] (codex-rs client.rs:959, no skip_serializing_if). An
//     absent or empty include is a wire deviation the relay corrects to that
//     canonical value. A non-empty include is a downstream parse contract and is
//     preserved verbatim — a different value would change the event stream the
//     client asked for. This is the backfill for native pass-through; the
//     Messages->Chat->Responses bridge already emits the canonical include.
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
		Include           json.RawMessage `json:"include"`
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
	includeOK := !codexIncludeNeedsDefault(probe.Include)
	if instructionsOK && storeOK && parallelOK && reasoningContextOK && includeOK {
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
	if !includeOK {
		var err error
		out, err = sjson.SetRawBytes(out, "include", codexIncludeWireValue)
		if err != nil {
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
// tools back into an already-Lite request. The initial Lite request starts input with:
//
//	{"type":"additional_tools","role":"developer","tools":[...]}
//
// followed by a developer message carrying non-empty base instructions. A WebSocket
// continuation may instead carry the explicit Lite client_metadata marker and only
// incremental input. In both cases injected top-level instructions/tools must become
// independent prefix items rather than being dropped or left on the Lite request root.
// Classic requests are deliberately not upgraded here; they remain on the non-Lite
// contract.
func normalizeCodexResponsesLiteEnvelope(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw
	}

	input, ok := codexResponsesInputItems(fields["input"])
	if !ok {
		return raw
	}
	hasAdditionalToolsPrefix := len(input) > 0 && codexResponseItemType(input[0]) == "additional_tools"
	if !hasAdditionalToolsPrefix && !codexResponsesLiteContinuationMarker(fields) {
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

	prefixLen := 0
	if hasAdditionalToolsPrefix {
		prefixLen = 1
		// The first developer message after additional_tools is the native base
		// instruction item. Keep it ahead of gateway-injected instructions; later
		// developer items (permissions, collaboration mode, skills, environment,
		// AGENTS) retain their original relative order after the insertion.
		if len(input) > 1 && codexResponseItemIsDeveloperMessage(input[1]) {
			prefixLen = 2
		}
	}
	if hasTopTools && len(topTools) > 0 && hasAdditionalToolsPrefix {
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
	} else if hasTopTools && len(topTools) > 0 {
		additional, err := json.Marshal(struct {
			Type  string            `json:"type"`
			Role  string            `json:"role"`
			Tools []json.RawMessage `json:"tools"`
		}{
			Type:  "additional_tools",
			Role:  "developer",
			Tools: topTools,
		})
		if err != nil {
			return raw
		}
		input = append([]json.RawMessage{additional}, input...)
		prefixLen = 1
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
		withDeveloper = append(withDeveloper, input[:prefixLen]...)
		withDeveloper = append(withDeveloper, developer)
		input = append(withDeveloper, input[prefixLen:]...)
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

func codexResponseItemIsDeveloperMessage(raw json.RawMessage) bool {
	var item struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	return json.Unmarshal(raw, &item) == nil && item.Type == "message" && item.Role == "developer"
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

// stripCodexUnsupportedPromptCacheControls removes cache hints emitted by generic
// Responses clients but rejected by the ChatGPT Codex transport. The supported
// prompt_cache_key remains byte-identical. RawMessage + targeted sjson edits avoid
// rounding large schema/tool integers and run after every envelope rewrite, so a
// system/developer role conversion cannot accidentally restore the unsupported
// nested member.
func stripCodexUnsupportedPromptCacheControls(raw []byte) []byte {
	if !bytes.Contains(raw, []byte(`"prompt_cache_options"`)) && !bytes.Contains(raw, []byte(`"prompt_cache_breakpoint"`)) {
		return raw
	}
	out := raw
	var root map[string]json.RawMessage
	if json.Unmarshal(out, &root) != nil {
		return raw
	}
	if _, present := root["prompt_cache_options"]; present {
		var err error
		out, err = sjson.DeleteBytes(out, "prompt_cache_options")
		if err != nil {
			return raw
		}
		if json.Unmarshal(out, &root) != nil {
			return raw
		}
	}
	inputRaw, present := root["input"]
	if !present || !bytes.Contains(inputRaw, []byte(`"prompt_cache_breakpoint"`)) {
		return out
	}
	var input []json.RawMessage
	if json.Unmarshal(inputRaw, &input) != nil {
		return raw
	}
	changed := false
	for inputIndex, itemRaw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(itemRaw, &item) != nil {
			continue
		}
		// Both block arrays must be walked. A message item carries "content"; a
		// function_call_output item carries "output", and the automatic-breakpoint
		// writer targets input.N.output.M.prompt_cache_breakpoint for exactly that
		// shape (see applyCodexGPT56AutomaticCacheBreakpoint / the "output" kind from
		// stableCodexPrefixBreakpoint). Walking only "content" left a breakpoint
		// inside a tool-result block on the wire for OAuth accounts — the path that
		// carries the Codex CLI fingerprint — even though the field appears nowhere in
		// codex-rs. Only cache metadata is deleted; the block's text/output payload is
		// untouched, so no model context is altered.
		itemRawUpdated := itemRaw
		itemChanged := false
		for _, key := range []string{"content", "output"} {
			blocksRaw, ok := item[key]
			if !ok || !bytes.Contains(blocksRaw, []byte(`"prompt_cache_breakpoint"`)) {
				continue
			}
			var blocks []json.RawMessage
			if json.Unmarshal(blocksRaw, &blocks) != nil {
				continue
			}
			keyChanged := false
			for blockIndex, blockRaw := range blocks {
				var block map[string]json.RawMessage
				if json.Unmarshal(blockRaw, &block) != nil {
					continue
				}
				if _, exists := block["prompt_cache_breakpoint"]; !exists {
					continue
				}
				updated, err := sjson.DeleteBytes(blockRaw, "prompt_cache_breakpoint")
				if err != nil {
					return raw
				}
				blocks[blockIndex] = updated
				keyChanged = true
			}
			if !keyChanged {
				continue
			}
			updated, err := sjson.SetRawBytes(itemRawUpdated, key, marshalCodexRawArray(blocks))
			if err != nil {
				return raw
			}
			itemRawUpdated = updated
			itemChanged = true
		}
		if !itemChanged {
			continue
		}
		input[inputIndex] = itemRawUpdated
		changed = true
	}
	if !changed {
		return out
	}
	updated, err := sjson.SetRawBytes(out, "input", marshalCodexRawArray(input))
	if err != nil {
		return raw
	}
	return updated
}

func codexExplicitCacheControlsAllowed(spec Request) bool {
	return AccountUsesAPIKey(spec.Token) && spec.CodexExplicitCacheCapable &&
		!strings.EqualFold(strings.TrimSpace(spec.CodexExplicitCacheMode), "off")
}

func codexGPT56Model(model string, raw []byte) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" && len(raw) > 0 {
		var root map[string]json.RawMessage
		if json.Unmarshal(raw, &root) == nil {
			_ = json.Unmarshal(root["model"], &model)
			model = strings.ToLower(strings.TrimSpace(model))
		}
	}
	return model == "gpt-5.6" || strings.HasPrefix(model, "gpt-5.6-")
}

// applyCodexGPT56AutomaticCacheBreakpoint marks the end of the reusable prefix at
// the last stable content block before the current turn's user message, following
// OpenAI's documented multi-turn pattern (top-level implicit mode + explicit
// breakpoint after the last stable item). It never creates, moves, or converts
// messages, instructions, tools, history, or reasoning fields — only cache metadata.
//
// Placement rationale:
//   - Fresh turn (preamble + one user): the breakpoint lands on the last
//     developer/system input_text block, so a retry that edits the first user
//     message still reuses the preamble ("shared prefix != cached prefix" gotcha).
//   - Continuation turn: the breakpoint lands on the last block-bearing item before
//     the current user message (an assistant output_text block or a
//     function_call_output output block), so conversation history — not just the
//     preamble — stays cacheable. The trailing per-turn reliability envelope
//     (bare-string content) and the current user message are dynamic and excluded.
//
// mode is always "explicit": the OpenAI spec enum for PromptCacheBreakpointParam is
// ["explicit"] only, so the historical "implicit" value was schema-invalid and
// ignored by upstream.
func applyCodexGPT56AutomaticCacheBreakpoint(raw []byte) []byte {
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return raw
	}
	// A client-authored breakpoint is authoritative; auto mode supplements only
	// requests that do not already select an explicit prefix. Inspect JSON keys,
	// rather than searching bytes, so a user message that merely mentions the
	// feature name does not suppress the automatic marker.
	if _, exists := root["prompt_cache_options"]; exists || jsonRawContainsKey(root["input"], "prompt_cache_breakpoint") {
		return raw
	}
	var input []json.RawMessage
	if json.Unmarshal(root["input"], &input) != nil {
		return raw
	}
	itemIndex, kind, blockIndex := stableCodexPrefixBreakpoint(input)
	if itemIndex < 0 {
		return raw
	}
	out, err := sjson.SetBytes(raw, fmt.Sprintf("input.%d.%s.%d.prompt_cache_breakpoint.mode", itemIndex, kind, blockIndex), "explicit")
	if err != nil {
		return raw
	}
	out, err = sjson.SetBytes(out, "prompt_cache_options.mode", "implicit")
	if err != nil {
		return raw
	}
	// GPT-5.6's prompt_cache_options.ttl accepts exactly "30m" (cache read 0.1x,
	// write 1.25x). Without it the upstream default TTL is the old-model window
	// (5-10 minutes), which an agentic conversation's inter-turn gap easily
	// crosses — every resumed turn then pays a cold cache. Pinning 30m stretches
	// the hit window to the model maximum at zero context cost: ttl is pure cache
	// metadata, covered by the codexCacheOnlyMutation guard below.
	out, err = sjson.SetBytes(out, "prompt_cache_options.ttl", "30m")
	if err != nil || !codexCacheOnlyMutation(raw, out) {
		return raw
	}
	return out
}

// stableCodexPrefixBreakpoint locates the last content block that can host a cache
// breakpoint inside the STABLE prefix: the last block-bearing item before the
// current turn's user message, ignoring any trailing bare-string reliability
// envelope. kind is "content" (message input_text/output_text block) or "output"
// (function_call_output output block); returns (-1, "", -1) when no stable block
// exists.
func stableCodexPrefixBreakpoint(input []json.RawMessage) (itemIndex int, kind string, blockIndex int) {
	n := len(input)
	if n == 0 {
		return -1, "", -1
	}
	// The reliability layer appends a per-turn envelope as a trailing bare-string
	// item; it changes every turn and is not part of the reusable prefix.
	tail := n
	for tail > 0 && codexItemBareStringContent(input[tail-1]) {
		tail--
	}
	lastUser := -1
	for i := tail - 1; i >= 0; i-- {
		if codexItemRole(input[i]) == "user" {
			lastUser = i
			break
		}
	}
	if lastUser <= 0 {
		return -1, "", -1
	}
	for i := lastUser - 1; i >= 0; i-- {
		if k, idx, ok := codexItemLastCacheableBlock(input[i]); ok {
			return i, k, idx
		}
	}
	return -1, "", -1
}

// codexItemRole returns the item's lowercased role, or "" for non-message items
// (function_call, function_call_output, reasoning).
func codexItemRole(item json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(item, &m) != nil {
		return ""
	}
	var role string
	_ = json.Unmarshal(m["role"], &role)
	return strings.ToLower(strings.TrimSpace(role))
}

// codexItemBareStringContent reports whether the item's content is a plain string
// (the reliability envelope shape) rather than an array of blocks.
func codexItemBareStringContent(item json.RawMessage) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(item, &m) != nil {
		return false
	}
	raw, ok := m["content"]
	if !ok {
		return false
	}
	b := bytes.TrimSpace(raw)
	return len(b) > 0 && b[0] == '"'
}

// codexItemLastCacheableBlock returns the array kind ("content"|"output") and index
// of the last input_text/output_text block on the item that can host a breakpoint.
func codexItemLastCacheableBlock(item json.RawMessage) (kind string, blockIndex int, ok bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(item, &m) != nil {
		return "", 0, false
	}
	for _, arrayKey := range []string{"content", "output"} {
		raw, exists := m[arrayKey]
		if !exists {
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(raw, &blocks) != nil {
			continue
		}
		for i := len(blocks) - 1; i >= 0; i-- {
			var block map[string]json.RawMessage
			var blockType string
			if json.Unmarshal(blocks[i], &block) == nil && json.Unmarshal(block["type"], &blockType) == nil {
				switch strings.ToLower(strings.TrimSpace(blockType)) {
				case "input_text", "output_text":
					return arrayKey, i, true
				}
			}
		}
	}
	return "", 0, false
}

func jsonRawContainsKey(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var value interface{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return false
	}
	var visit func(interface{}) bool
	visit = func(current interface{}) bool {
		switch typed := current.(type) {
		case map[string]interface{}:
			if _, ok := typed[want]; ok {
				return true
			}
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		case []interface{}:
			for _, child := range typed {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(value)
}

// ApplyCodexGPT56AutomaticCacheBreakpoint exposes the lossless cache-metadata
// mutation to the gateway layer so diagnostics and singleflight fingerprint the
// exact body that the native API-key transport will send.
func ApplyCodexGPT56AutomaticCacheBreakpoint(raw []byte) []byte {
	return applyCodexGPT56AutomaticCacheBreakpoint(raw)
}

func codexCacheOnlyMutation(before, after []byte) bool {
	clean := func(raw []byte) (interface{}, bool) {
		var value interface{}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&value) != nil {
			return nil, false
		}
		var strip func(interface{}) interface{}
		strip = func(current interface{}) interface{} {
			switch typed := current.(type) {
			case map[string]interface{}:
				cleaned := make(map[string]interface{}, len(typed))
				for key, child := range typed {
					if key == "prompt_cache_options" || key == "prompt_cache_breakpoint" {
						continue
					}
					cleaned[key] = strip(child)
				}
				return cleaned
			case []interface{}:
				cleaned := make([]interface{}, len(typed))
				for i, child := range typed {
					cleaned[i] = strip(child)
				}
				return cleaned
			default:
				return current
			}
		}
		return strip(value), true
	}
	want, ok := clean(before)
	if !ok {
		return false
	}
	got, ok := clean(after)
	return ok && reflect.DeepEqual(want, got)
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
