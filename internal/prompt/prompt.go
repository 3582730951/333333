package prompt

import (
	"encoding/json"
	"errors"
	"strings"
)

func ShouldRewrite(systemPrompt string, isCompaction bool, applyToCompaction bool) bool {
	if strings.TrimSpace(systemPrompt) == "" {
		return false
	}
	if isCompaction && !applyToCompaction {
		return false
	}
	return true
}

func InjectResponsesSystemPrompt(raw []byte, systemPrompt string) ([]byte, bool, error) {
	systemPrompt = strings.TrimSpace(systemPrompt)
	if systemPrompt == "" {
		return raw, false, nil
	}
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false, err
	}
	if existing, ok := root["instructions"].(string); ok && strings.TrimSpace(existing) != "" {
		if !strings.HasPrefix(existing, systemPrompt) {
			root["instructions"] = systemPrompt + "\n\n" + existing
		}
	} else {
		root["instructions"] = systemPrompt
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func InjectChatSystemPrompt(raw []byte, systemPrompt string) ([]byte, bool, error) {
	systemPrompt = strings.TrimSpace(systemPrompt)
	if systemPrompt == "" {
		return raw, false, nil
	}
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, false, err
	}
	messages, _ := root["messages"].([]interface{})
	systemMessage := map[string]interface{}{"role": "system", "content": systemPrompt}
	if len(messages) > 0 {
		if first, ok := messages[0].(map[string]interface{}); ok && first["role"] == "system" {
			if content, ok := first["content"].(string); ok && strings.TrimSpace(content) != "" {
				first["content"] = systemPrompt + "\n\n" + content
			} else {
				first["content"] = systemPrompt
			}
			out, err := json.Marshal(root)
			return out, err == nil, err
		}
	}
	root["messages"] = append([]interface{}{systemMessage}, messages...)
	out, err := json.Marshal(root)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func ChatCompletionToResponses(raw []byte) ([]byte, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	messages, ok := root["messages"].([]interface{})
	if !ok {
		return nil, errors.New("chat completions request missing messages array")
	}
	delete(root, "messages")
	root["input"] = chatMessagesToResponsesInput(messages)
	// Translate tool calling so a third-party OpenAI-compatible client (Cline, Roo,
	// opencode, …) can actually use tools against a Codex/Responses account. The
	// Responses API uses FLAT function tools ({type,name,parameters}), function_call /
	// function_call_output input items, and a flatter tool_choice — not Chat
	// Completions' nested {function:{…}} + assistant.tool_calls + tool-role messages.
	// Without this the tools array reached the upstream in the wrong shape (or not at
	// all), so tool use silently never fired.
	if tools, ok := root["tools"]; ok {
		if conv := chatToolsToResponses(tools); len(conv) > 0 {
			root["tools"] = conv
		} else {
			delete(root, "tools")
		}
	}
	if tc, ok := root["tool_choice"]; ok {
		root["tool_choice"] = chatToolChoiceToResponses(tc)
	}
	for _, drop := range []string{"n", "logprobs", "top_logprobs"} {
		delete(root, drop)
	}
	return json.Marshal(root)
}

// chatMessagesToResponsesInput converts a Chat Completions messages array into a
// Responses API input array, expanding tool calling into the Responses item shapes:
//   - assistant.tool_calls → one {type:"function_call", call_id, name, arguments} item
//     each (plus the assistant text message when it carried content);
//   - role:"tool" result → {type:"function_call_output", call_id, output};
//   - plain system/user/assistant text messages pass through unchanged (the Responses
//     API accepts {role, content} items, so existing text-only chat is unaffected).
func chatMessagesToResponsesInput(messages []interface{}) []interface{} {
	out := make([]interface{}, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			out = append(out, m)
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "tool":
			out = append(out, map[string]interface{}{
				"type":    "function_call_output",
				"call_id": stringOr(firstPresent(msg["tool_call_id"], msg["id"]), ""),
				"output":  chatContentToText(msg["content"]),
			})
		case "assistant":
			toolCalls, _ := msg["tool_calls"].([]interface{})
			if c := msg["content"]; c != nil && chatContentToText(c) != "" {
				out = append(out, map[string]interface{}{"role": "assistant", "content": c})
			} else if len(toolCalls) == 0 {
				out = append(out, msg) // assistant turn with neither text nor tools: keep as-is
			}
			for _, tc := range toolCalls {
				tcm, ok := tc.(map[string]interface{})
				if !ok {
					continue
				}
				fn, _ := tcm["function"].(map[string]interface{})
				out = append(out, map[string]interface{}{
					"type":      "function_call",
					"call_id":   stringOr(tcm["id"], ""),
					"name":      stringOr(mapGet(fn, "name"), ""),
					"arguments": stringOr(mapGet(fn, "arguments"), ""),
				})
			}
		default:
			out = append(out, msg)
		}
	}
	return out
}

// chatToolsToResponses flattens Chat Completions function tools
// ({type:"function",function:{name,description,parameters}}) into the Responses API's
// flat tool shape ({type:"function",name,description,parameters}). Typed built-in
// tools (web_search, …) and already-flat tools pass through unchanged.
func chatToolsToResponses(v interface{}) []interface{} {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]interface{}, 0, len(arr))
	for _, t := range arr {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]interface{})
		if !ok {
			out = append(out, tm) // typed built-in or already flat
			continue
		}
		conv := map[string]interface{}{"type": "function", "name": stringOr(mapGet(fn, "name"), "")}
		if d, ok := fn["description"]; ok {
			conv["description"] = d
		}
		if params, ok := fn["parameters"]; ok {
			conv["parameters"] = params
		}
		if strict, ok := fn["strict"]; ok {
			conv["strict"] = strict
		}
		out = append(out, conv)
	}
	return out
}

// chatToolChoiceToResponses maps a Chat Completions tool_choice to the Responses form.
// The string forms (auto/none/required) are accepted by both; a forced-function object
// {type:"function",function:{name}} flattens to {type:"function",name}.
func chatToolChoiceToResponses(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		return t
	case map[string]interface{}:
		if fn, ok := t["function"].(map[string]interface{}); ok {
			if name := stringOr(mapGet(fn, "name"), ""); name != "" {
				return map[string]interface{}{"type": "function", "name": name}
			}
		}
	}
	return v
}

// firstPresent returns the first non-nil argument, or nil.
func firstPresent(vs ...interface{}) interface{} {
	for _, v := range vs {
		if v != nil {
			return v
		}
	}
	return nil
}

func ResponsesToChatCompletion(raw []byte, requestID, model string) ([]byte, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, nil
	}
	content := extractResponseText(root)
	toolCalls := extractResponseToolCalls(root)
	if model == "" {
		if m, ok := root["model"].(string); ok {
			model = m
		}
	}
	if requestID == "" {
		if id, ok := root["id"].(string); ok {
			requestID = id
		}
	}
	if requestID == "" {
		requestID = "chatcmpl-pool"
	}
	message := map[string]interface{}{"role": "assistant"}
	finish := "stop"
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		finish = "tool_calls"
		// OpenAI sets content to null when the turn is purely tool calls; keep any text.
		if content != "" {
			message["content"] = content
		} else {
			message["content"] = nil
		}
	} else {
		message["content"] = content
	}
	resp := map[string]interface{}{
		"id":      requestID,
		"object":  "chat.completion",
		"created": root["created_at"],
		"model":   model,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			},
		},
	}
	if usage, ok := root["usage"]; ok {
		resp["usage"] = usage
	}
	return json.Marshal(resp)
}

// extractResponseToolCalls pulls function_call items out of a Responses API response's
// output array and shapes them as OpenAI chat tool_calls, so a third-party client sees
// the tool calls it expects instead of them vanishing (the old text-only extraction).
func extractResponseToolCalls(root map[string]interface{}) []interface{} {
	output, ok := root["output"].([]interface{})
	if !ok {
		return nil
	}
	var calls []interface{}
	for _, item := range output {
		m, ok := item.(map[string]interface{})
		if !ok || m["type"] != "function_call" {
			continue
		}
		calls = append(calls, map[string]interface{}{
			"id":   stringOr(firstPresent(m["call_id"], m["id"]), ""),
			"type": "function",
			"function": map[string]interface{}{
				"name":      stringOr(mapGet(m, "name"), ""),
				"arguments": stringOr(mapGet(m, "arguments"), ""),
			},
		})
	}
	return calls
}

func extractResponseText(root map[string]interface{}) string {
	if text, ok := root["output_text"].(string); ok {
		return text
	}
	if output, ok := root["output"].([]interface{}); ok {
		var parts []string
		for _, item := range output {
			msg, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			if content, ok := msg["content"].([]interface{}); ok {
				for _, c := range content {
					part, ok := c.(map[string]interface{})
					if !ok {
						continue
					}
					if text, ok := part["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// EnsureResponsesWebSearchTool guarantees a web search tool of the given type is
// present in a /v1/responses request body, enabling 联网搜索 over the account
// session (AT) path without API-key/organization verification. It is a no-op if
// a tool of that type already exists. toolType defaults to "web_search".
func EnsureResponsesWebSearchTool(raw []byte, toolType string) ([]byte, error) {
	if strings.TrimSpace(toolType) == "" {
		toolType = "web_search"
	}
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	var tools []interface{}
	if existing, ok := root["tools"].([]interface{}); ok {
		tools = existing
		for _, t := range tools {
			if tm, ok := t.(map[string]interface{}); ok {
				if ts, _ := tm["type"].(string); ts == toolType {
					return raw, nil
				}
			}
		}
	}
	tools = append(tools, map[string]interface{}{"type": toolType})
	root["tools"] = tools
	return json.Marshal(root)
}

// SupportsExtendedPromptCache reports whether a model supports OpenAI's extended
// ("24h") prompt-cache retention. Per the prompt-caching guide the extended policy is
// available for the gpt-5.x family (incl. every *-codex variant) and gpt-4.1; older
// models (gpt-4o, etc.) only support the short in-memory policy, where setting a
// retention value is unnecessary (and for some, rejected). A prefix test covers the
// whole supported family without enumerating each point release.
func SupportsExtendedPromptCache(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(m, "gpt-5") || strings.HasPrefix(m, "gpt-4.1")
}

// NormalizePromptCacheRetention returns the only retention values this gateway may
// auto-inject. Downstream-supplied request fields are handled separately and pass
// through unchanged; this is only for operator config/runtime settings.
func NormalizePromptCacheRetention(retention string) string {
	switch strings.TrimSpace(retention) {
	case "24h":
		return "24h"
	case "in_memory":
		return "in_memory"
	default:
		return ""
	}
}

// EnsureResponsesPromptCacheRetention sets prompt_cache_retention on a /responses body
// when the field is absent, leaving an explicit downstream value untouched. Extended
// ("24h") retention keeps a prompt-cache prefix warm for up to 24h instead of the
// ~5–10min in-memory default — the single biggest lever on cache-hit rate for a
// rotating account pool: a conversation that resumes after a gap, or that fails over
// and later returns to a previously-used account, can still hit a warm cache rather
// than reprocessing the whole prefix uncached. prompt_cache_retention is a known
// top-level /responses field and is NOT part of the cacheable prompt prefix, so adding
// it never perturbs existing cache hits. retention="" is a no-op.
func EnsureResponsesPromptCacheRetention(raw []byte, retention string) []byte {
	retention = NormalizePromptCacheRetention(retention)
	if retention == "" {
		return raw
	}
	// Cheap scan first: if the field is already present, forward byte-identical so a
	// downstream-chosen policy wins and we avoid a needless re-marshal.
	if bytesContainsKey(raw, "prompt_cache_retention") {
		return raw
	}
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw
	}
	if _, ok := root["prompt_cache_retention"]; ok {
		return raw
	}
	root["prompt_cache_retention"] = retention
	out, err := json.Marshal(root)
	if err != nil {
		return raw
	}
	return out
}

func bytesContainsKey(raw []byte, key string) bool {
	return strings.Contains(string(raw), `"`+key+`"`)
}
