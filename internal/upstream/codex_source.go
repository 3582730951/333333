package upstream

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/capability"
	"github.com/tidwall/sjson"
)

const maxCodexPatchFieldBytes int64 = 4 << 20

// normalizeCodexSource applies Responses leaf normalization as composite spans.
func normalizeCodexSource(client *Client, spec *Request, upstreamBaseURL string, compact bool) (bool, error) {
	meta := spec.BodyMeta
	if meta == nil || spec.Body == nil || meta.Size != spec.Body.Size() || meta.ObjectEnd <= 0 {
		return false, nil
	}
	usesAPIKey := AccountUsesAPIKey(spec.Token)
	// The nested client-only cache breakpoint cannot be expressed as a top-level
	// composite patch. Select the targeted RawMessage sanitizer only for bodies the
	// single-pass metadata scanner positively marked; all ordinary large requests
	// keep the zero-copy/spooled source path.
	if meta.PromptCacheBreakpoint {
		return false, nil
	}
	responsesLite, liteEnvelope := false, false
	if !usesAPIKey {
		var supported bool
		var err error
		responsesLite, liteEnvelope, supported, err = codexRequestUsesResponsesLiteSource(*spec)
		if err != nil {
			return false, err
		}
		if !supported {
			return false, nil
		}
	}
	patches := make([]bodysource.JSONFieldPatch, 0, 20)
	if liteEnvelope {
		if _, present := meta.Fields["tools"]; present {
			switch meta.Kinds["tools"] {
			case 'n':
				patches = append(patches, bodysource.JSONFieldPatch{Name: "tools", Delete: true})
			case '[':
				raw, ok, err := requestSpan(spec.Body, meta.Fields["tools"], maxCodexPatchFieldBytes)
				if err != nil {
					return false, err
				}
				var tools []json.RawMessage
				if !ok || json.Unmarshal(raw, &tools) != nil || len(tools) != 0 {
					return false, nil
				}
				patches = append(patches, bodysource.JSONFieldPatch{Name: "tools", Delete: true})
			default:
				return false, nil
			}
		}
		if _, present := meta.Fields["instructions"]; present {
			raw := meta.Scalars["instructions"]
			if raw == nil {
				return false, nil
			}
			var instructions *string
			if json.Unmarshal(raw, &instructions) != nil || instructions != nil && *instructions != "" {
				return false, nil
			}
			patches = append(patches, bodysource.JSONFieldPatch{Name: "instructions", Delete: true})
		}
	}
	if !compact {
		if !responsesLite {
			instructionsOK := false
			switch kind, present := meta.Kinds["instructions"]; {
			case !present:
			case kind == '"':
				if raw := meta.Scalars["instructions"]; raw == nil {
					instructionsOK = true
				} else {
					var instructions string
					instructionsOK = json.Unmarshal(raw, &instructions) == nil && strings.TrimSpace(instructions) != ""
				}
			case kind == 'n':
			default:
				return false, nil
			}
			if !instructionsOK {
				patches = append(patches, bodysource.JSONFieldPatch{Name: "instructions", Value: []byte(`"You are a coding agent."`)})
			}
		}
		wantStore := codexStoreValue(upstreamBaseURL)
		kind, present := meta.Kinds["store"]
		if present && kind != 't' && kind != 'f' && kind != 'n' {
			return false, nil
		}
		storeOK := present && (wantStore && kind == 't' || !wantStore && kind == 'f')
		if !storeOK {
			value := []byte("false")
			if wantStore {
				value = []byte("true")
			}
			patches = append(patches, bodysource.JSONFieldPatch{Name: "store", Value: value})
		}
	}
	if responsesLite {
		kind, present := meta.Kinds["parallel_tool_calls"]
		if present && kind != 't' && kind != 'f' && kind != 'n' {
			return false, nil
		}
		if !present || kind != 'f' {
			patches = append(patches, bodysource.JSONFieldPatch{Name: "parallel_tool_calls", Value: []byte("false")})
		}
	} else if !compact {
		// The classic Responses backend rejects an orphan parallel_tool_calls
		// option. Preserve it byte-for-byte when real tools are present so Claude
		// Code Skills/MCP/built-in tools retain their parallel execution behavior.
		if _, present := meta.Fields["parallel_tool_calls"]; present {
			empty, known, err := codexSourceToolsEmpty(*spec)
			if err != nil {
				return false, err
			}
			if known && empty {
				patches = append(patches, bodysource.JSONFieldPatch{Name: "parallel_tool_calls", Delete: true})
			}
		}
	}
	if spec.CodexResponsesWebSocket {
		patches = append(patches,
			bodysource.JSONFieldPatch{Name: "type", Value: []byte(`"response.create"`)},
			bodysource.JSONFieldPatch{Name: "stream", Value: []byte("true")},
		)
		if !responsesLite {
			if _, present := meta.Fields["tools"]; !present {
				patches = append(patches, bodysource.JSONFieldPatch{Name: "tools", Value: []byte("[]")})
			}
		}
		if _, present := meta.Fields["tool_choice"]; !present {
			patches = append(patches, bodysource.JSONFieldPatch{Name: "tool_choice", Value: []byte(`"auto"`)})
		}
		if !responsesLite {
			empty, known, err := codexSourceToolsEmpty(*spec)
			if err != nil {
				return false, err
			}
			_, parallelPresent := meta.Fields["parallel_tool_calls"]
			if (!known || !empty) && !parallelPresent {
				patches = append(patches, bodysource.JSONFieldPatch{Name: "parallel_tool_calls", Value: []byte("false")})
			}
		}
		if _, present := meta.Fields["include"]; !present {
			patches = append(patches, bodysource.JSONFieldPatch{Name: "include", Value: []byte("[]")})
		}
	}
	if reasoning, ok, err := requestSpan(spec.Body, meta.Fields["reasoning"], maxCodexPatchFieldBytes); err != nil {
		return false, err
	} else if ok {
		var probe struct {
			Effort  string `json:"effort"`
			Context string `json:"context"`
		}
		if json.Unmarshal(reasoning, &probe) != nil {
			return false, nil
		}
		updated, changed := reasoning, false
		if responsesLite && probe.Context != "all_turns" {
			updated, err = sjson.SetBytes(updated, "context", "all_turns")
			if err != nil {
				return false, nil
			}
			changed = true
		}
		if probe.Effort == "ultra" {
			updated, err = sjson.SetBytes(updated, "effort", "max")
			if err != nil {
				return false, nil
			}
			changed = true
		}
		if changed {
			patches = append(patches, bodysource.JSONFieldPatch{Name: "reasoning", Value: updated})
		}
	} else if _, present := meta.Fields["reasoning"]; present {
		return false, nil
	} else if responsesLite {
		patches = append(patches, bodysource.JSONFieldPatch{Name: "reasoning", Value: []byte(`{"context":"all_turns"}`)})
	}
	if kind, present := meta.Kinds["prompt_cache_retention"]; present && kind != 'n' {
		patches = append(patches, bodysource.JSONFieldPatch{Name: "prompt_cache_retention", Delete: true})
	}
	if _, present := meta.Fields["prompt_cache_options"]; present {
		patches = append(patches, bodysource.JSONFieldPatch{Name: "prompt_cache_options", Delete: true})
	}
	if !usesAPIKey {
		if kind, present := meta.Kinds["max_output_tokens"]; present && kind != 'n' {
			patches = append(patches, bodysource.JSONFieldPatch{Name: "max_output_tokens", Delete: true})
		}
		metadata := client.newCodexRequestMetadataWithResponsesLite(*spec, responsesLite)
		spec.codexMetadata = &metadata
		if looksLikeCodexGeneratedUUID(meta.PromptCacheKey) {
			promptCacheKey := metadata.threadID
			if metadata.profile.promptCacheKeyBySession {
				promptCacheKey = metadata.sessionID
			}
			encoded, marshalErr := json.Marshal(promptCacheKey)
			if marshalErr != nil {
				return false, marshalErr
			}
			patches = append(patches, bodysource.JSONFieldPatch{Name: "prompt_cache_key", Value: encoded})
		}
		if !compact {
			clientMetadata, ok, err := codexClientMetadataSourceValue(*spec, metadata)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
			patches = append(patches, bodysource.JSONFieldPatch{Name: "client_metadata", Value: clientMetadata})
		}
	}
	// `generate` is a WebSocket frame control, not an HTTP Responses parameter.
	// Metadata above intentionally observes it first so generate:false retains its
	// prewarm request_kind even when this request is being bridged to HTTPS.
	if !spec.CodexResponsesWebSocket {
		if _, present := meta.Fields["generate"]; present {
			patches = append(patches, bodysource.JSONFieldPatch{Name: "generate", Delete: true})
		}
	}
	for _, name := range []string{"thread_id", "session_id", "conversation_id", "window_id", "parent_thread_id", "parent_turn_id", "forked_from_thread_id", "turn_metadata", "turn_state"} {
		if _, present := meta.Fields[name]; present {
			patches = append(patches, bodysource.JSONFieldPatch{Name: name, Delete: true})
		}
	}
	patched, err := bodysource.PatchTopLevel(spec.Body, *meta, patches)
	if err != nil {
		return false, err
	}
	spec.Body = patched
	return true, nil
}

func codexRequestUsesResponsesLiteSource(spec Request) (lite, envelope, supported bool, err error) {
	meta := spec.BodyMeta
	if meta == nil || spec.Body == nil {
		return false, false, false, nil
	}
	if meta.Kinds["model"] == '"' && meta.Scalars["model"] == nil && meta.Model == "" {
		return false, false, false, nil
	}
	if !capability.CodexUsesResponsesLite(meta.Model) {
		return false, false, true, nil
	}
	if span, present := meta.Fields["client_metadata"]; present {
		raw, ok, readErr := requestSpan(spec.Body, span, maxCodexPatchFieldBytes)
		if readErr != nil {
			return false, false, false, readErr
		}
		if !ok {
			return false, false, false, nil
		}
		var metadata map[string]json.RawMessage
		if json.Unmarshal(raw, &metadata) == nil {
			var marker string
			if json.Unmarshal(metadata[codexWSResponsesLiteMetadata], &marker) == nil && strings.EqualFold(strings.TrimSpace(marker), "true") {
				// The explicit marker is authoritative for incremental Lite frames.
				// Empty top-level compatibility fields can be deleted as source
				// patches. Non-empty injected instructions/tools require ordered input
				// prefix construction, so deliberately select the materialized fallback.
				requiresEnvelopeFallback := false
				hasEmptyCompatibilityField := false
				if _, present := meta.Fields["tools"]; present {
					empty, known, emptyErr := codexSourceToolsEmpty(spec)
					if emptyErr != nil {
						return false, false, false, emptyErr
					}
					if !known {
						return false, false, false, nil
					}
					requiresEnvelopeFallback = !empty
					hasEmptyCompatibilityField = empty
				}
				if _, present := meta.Fields["instructions"]; present {
					rawInstructions := meta.Scalars["instructions"]
					if rawInstructions == nil {
						return false, false, false, nil
					}
					var instructions *string
					if json.Unmarshal(rawInstructions, &instructions) != nil {
						return false, false, false, nil
					}
					if instructions != nil && *instructions != "" {
						requiresEnvelopeFallback = true
					} else {
						hasEmptyCompatibilityField = true
					}
				}
				if requiresEnvelopeFallback {
					return true, false, false, nil
				}
				return true, hasEmptyCompatibilityField, true, nil
			}
		}
	}
	if meta.FirstInputItem.Length == 0 {
		return false, false, true, nil
	}
	first, ok, readErr := requestSpan(spec.Body, meta.FirstInputItem, maxCodexPatchFieldBytes)
	if readErr != nil {
		return false, false, false, readErr
	}
	if !ok {
		return false, false, false, nil
	}
	var item struct {
		Type  string          `json:"type"`
		Role  string          `json:"role"`
		Tools json.RawMessage `json:"tools"`
	}
	if json.Unmarshal(first, &item) != nil || item.Type != "additional_tools" || item.Role != "developer" {
		return false, false, true, nil
	}
	var tools []json.RawMessage
	if json.Unmarshal(item.Tools, &tools) != nil {
		return false, false, true, nil
	}
	return true, true, true, nil
}

func codexSourceToolsEmpty(spec Request) (bool, bool, error) {
	meta := spec.BodyMeta
	span, present := meta.Fields["tools"]
	if !present || meta.Kinds["tools"] == 'n' {
		return true, true, nil
	}
	if meta.Kinds["tools"] != '[' {
		return false, true, nil
	}
	raw, ok, err := requestSpan(spec.Body, span, maxCodexPatchFieldBytes)
	if err != nil || !ok {
		return false, ok, err
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return false, true, nil
	}
	return len(tools) == 0, true, nil
}

func codexClientMetadataSourceValue(spec Request, metadata codexRequestMetadata) ([]byte, bool, error) {
	out := []byte(`{}`)
	if span, present := spec.BodyMeta.Fields["client_metadata"]; present {
		if spec.BodyMeta.Kinds["client_metadata"] != '{' {
			return nil, false, nil
		}
		raw, ok, err := requestSpan(spec.Body, span, maxCodexPatchFieldBytes)
		if err != nil || !ok {
			return nil, false, err
		}
		out = bytes.TrimSpace(raw)
	}
	var err error
	for _, key := range []string{"installation_id", codexInstallationIDMetadataKey, "session_id", "thread_id", "turn_id", "parent_turn_id", "window_id", "x-codex-window-id", codexSubagentHeader, "parent_thread_id", "x-codex-parent-thread-id", "forked_from_thread_id", "x-codex-turn-metadata", "x-codex-turn-state", codexWSResponsesLiteMetadata, codexWSRequestStartMetadata} {
		out, err = sjson.DeleteBytes(out, key)
		if err != nil {
			return nil, false, nil
		}
	}
	for _, field := range []struct{ path, value string }{{codexInstallationIDMetadataKey, metadata.installationID}, {"session_id", metadata.sessionID}, {"thread_id", metadata.threadID}, {"x-codex-window-id", metadata.windowID}} {
		out, err = sjson.SetBytes(out, field.path, field.value)
		if err != nil {
			return nil, false, nil
		}
	}
	for _, field := range []struct{ path, value string }{{"turn_id", metadata.turnID}, {"parent_turn_id", metadata.parentTurnID}, {codexSubagentHeader, metadata.subagent}, {"x-codex-parent-thread-id", metadata.parentThreadID}, {"x-codex-turn-metadata", metadata.turnMetadata}} {
		if field.value == "" {
			continue
		}
		out, err = sjson.SetBytes(out, field.path, field.value)
		if err != nil {
			return nil, false, nil
		}
	}
	if spec.CodexResponsesWebSocket {
		if metadata.turnState != "" {
			out, err = sjson.SetBytes(out, "x-codex-turn-state", metadata.turnState)
			if err != nil {
				return nil, false, nil
			}
		}
		if metadata.responsesLite {
			out, err = sjson.SetBytes(out, codexWSResponsesLiteMetadata, "true")
			if err != nil {
				return nil, false, nil
			}
		}
		out, err = sjson.SetBytes(out, codexWSRequestStartMetadata, strconv.FormatInt(time.Now().UnixMilli(), 10))
		if err != nil {
			return nil, false, nil
		}
	}
	return out, true, nil
}

func requestSpan(source bodysource.BodySource, span bodysource.Span, limit int64) ([]byte, bool, error) {
	if span.Length <= 0 {
		return nil, false, nil
	}
	if span.Length > limit {
		return nil, false, nil
	}
	view, err := bodysource.Slice(source, span.Offset, span.Length)
	if err != nil {
		return nil, false, err
	}
	raw, err := bodysource.ReadAll(view)
	return raw, err == nil, err
}
