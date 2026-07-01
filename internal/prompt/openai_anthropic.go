package prompt

import (
	"encoding/json"
	"strings"
)

// openai_anthropic.go bridges the Anthropic **Messages API** (/v1/messages — what
// Claude Code speaks) and OpenAI **Chat Completions** (a custom provider such as
// DeepSeek). It is the request/response (non-streaming) half; the streaming converter
// lives in package api (ChatCompletionStreamToAnthropicSSE). These mirror, in reverse,
// the existing ChatCompletionToAnthropic / AnthropicToChatCompletion in anthropic.go.

// AnthropicRequestToChatCompletion converts an Anthropic Messages request into a Chat
// Completions request: system -> a system message; content blocks (text / tool_use /
// tool_result) -> chat messages and tool_calls / tool-role messages; tools
// (input_schema) -> OpenAI function tools.
func AnthropicRequestToChatCompletion(raw []byte) ([]byte, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	out := map[string]interface{}{}
	if m, ok := root["model"].(string); ok {
		out["model"] = m
	}
	if n := firstNum(root, "max_tokens", "max_completion_tokens"); n > 0 {
		out["max_tokens"] = n
	}
	if v, ok := root["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := root["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := root["stream"].(bool); ok && v {
		out["stream"] = true
	}
	if ss := toStringSlice(root["stop_sequences"]); len(ss) > 0 {
		out["stop"] = ss
	}

	messages := make([]interface{}, 0, 8)
	if sys := anthropicSystemToText(root["system"]); strings.TrimSpace(sys) != "" {
		messages = append(messages, map[string]interface{}{"role": "system", "content": sys})
	}
	for _, mi := range toSlice(root["messages"]) {
		m, ok := mi.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		messages = append(messages, anthropicMessageToChat(role, m["content"])...)
	}
	out["messages"] = messages

	if tools := anthropicToolsToChat(root["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := anthropicToolChoiceToChat(root["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}
	return json.Marshal(out)
}

// anthropicSystemToText flattens an Anthropic `system` (string or array of text
// blocks) into a single string.
func anthropicSystemToText(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		var parts []string
		for _, b := range t {
			if bm, ok := b.(map[string]interface{}); ok {
				if s, ok := bm["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// anthropicMessageToChat converts one Anthropic message (role + content) into one or
// more Chat Completions messages. An assistant turn with tool_use blocks becomes an
// assistant message carrying tool_calls; a user turn's tool_result blocks become
// tool-role messages (emitted BEFORE any user text so they immediately follow the
// assistant tool_calls turn, as the Chat Completions API requires).
func anthropicMessageToChat(role string, content interface{}) []interface{} {
	switch c := content.(type) {
	case string:
		return []interface{}{map[string]interface{}{"role": role, "content": c}}
	case []interface{}:
		var textParts []string
		var toolCalls []interface{}
		var toolResults []interface{}
		for _, b := range c {
			bm, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			switch bm["type"] {
			case "text":
				if s, ok := bm["text"].(string); ok {
					textParts = append(textParts, s)
				}
			case "tool_use":
				args, _ := json.Marshal(bm["input"])
				toolCalls = append(toolCalls, map[string]interface{}{
					"id":   stringOr(bm["id"], ""),
					"type": "function",
					"function": map[string]interface{}{
						"name":      stringOr(bm["name"], ""),
						"arguments": string(args),
					},
				})
			case "tool_result":
				toolResults = append(toolResults, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": stringOr(bm["tool_use_id"], ""),
					"content":      anthropicToolResultContentToText(bm["content"]),
				})
			case "image":
				// Dropped: a plain text-only Chat Completions provider cannot consume it.
			}
		}
		text := strings.Join(textParts, "")
		if role == "assistant" {
			msg := map[string]interface{}{"role": "assistant"}
			if len(toolCalls) > 0 {
				msg["tool_calls"] = toolCalls
				if text != "" {
					msg["content"] = text
				} else {
					msg["content"] = nil
				}
			} else {
				msg["content"] = text
			}
			return []interface{}{msg}
		}
		out := make([]interface{}, 0, len(toolResults)+1)
		out = append(out, toolResults...)
		if text != "" {
			out = append(out, map[string]interface{}{"role": role, "content": text})
		}
		return out
	}
	return nil
}

// anthropicToolResultContentToText flattens an Anthropic tool_result `content`
// (string or array of text/image blocks) into plain text.
func anthropicToolResultContentToText(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		return chatContentToText(t)
	}
	return ""
}

// anthropicToolsToChat maps Anthropic tools (input_schema) to OpenAI function tools.
// Typed server tools (web_search_*, …) without an input_schema are dropped.
func anthropicToolsToChat(v interface{}) []interface{} {
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
		name := stringOr(tm["name"], "")
		if name == "" {
			continue
		}
		schema, hasSchema := tm["input_schema"]
		if !hasSchema {
			if _, typed := tm["type"]; typed {
				continue // typed server tool
			}
		}
		fn := map[string]interface{}{"name": name}
		if d, ok := tm["description"]; ok {
			fn["description"] = d
		}
		if hasSchema {
			fn["parameters"] = schema
		} else {
			fn["parameters"] = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, map[string]interface{}{"type": "function", "function": fn})
	}
	return out
}

func anthropicToolChoiceToChat(v interface{}) interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}
	switch stringOr(m["type"], "") {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		if name := stringOr(m["name"], ""); name != "" {
			return map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": name}}
		}
	}
	return nil
}

// ChatCompletionToAnthropicResponse converts a (non-streaming) Chat Completions
// response into an Anthropic Messages response: assistant text -> a text block, tool
// calls -> tool_use blocks, finish_reason -> stop_reason, usage -> input/output_tokens.
func ChatCompletionToAnthropicResponse(raw []byte, model string) ([]byte, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, nil
	}
	id, _ := root["id"].(string)
	if id == "" {
		id = "msg_pool"
	}
	if model == "" {
		if m, ok := root["model"].(string); ok {
			model = m
		}
	}
	finish := ""
	if choices, ok := root["choices"].([]interface{}); ok && len(choices) > 0 {
		if ch, ok := choices[0].(map[string]interface{}); ok {
			finish = stringOr(ch["finish_reason"], "")
		}
	}
	text, toolCalls := chatChoiceTextAndToolCalls(root)
	content := make([]interface{}, 0, 1+len(toolCalls))
	if text != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": text})
	}
	for _, tc := range toolCalls {
		tcm, ok := tc.(map[string]interface{})
		if !ok {
			continue
		}
		fn, _ := tcm["function"].(map[string]interface{})
		content = append(content, map[string]interface{}{
			"type":  "tool_use",
			"id":    stringOr(tcm["id"], ""),
			"name":  stringOr(mapGet(fn, "name"), ""),
			"input": parseJSONObject(mapGet(fn, "arguments")),
		})
	}
	if len(content) == 0 {
		content = append(content, map[string]interface{}{"type": "text", "text": ""})
	}
	resp := map[string]interface{}{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   FinishToStopReason(finish, len(toolCalls) > 0),
		"stop_sequence": nil,
	}
	if u, ok := root["usage"].(map[string]interface{}); ok {
		resp["usage"] = ChatUsageToAnthropic(u)
	}
	return json.Marshal(resp)
}

// FinishToStopReason maps an OpenAI finish_reason to an Anthropic stop_reason (the
// inverse of StopReasonToFinish). Exported for reuse by the streaming converter.
func FinishToStopReason(finish string, hasTools bool) string {
	switch finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop":
		return "end_turn"
	}
	if hasTools {
		return "tool_use"
	}
	return "end_turn"
}

// ChatUsageToAnthropic maps Chat Completions usage to Anthropic usage field names.
func ChatUsageToAnthropic(u map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"input_tokens":  chatPromptTokens(u),
		"output_tokens": toInt64(u["completion_tokens"]),
	}
	if cached := chatCachedTokens(u); cached > 0 {
		out["cache_read_input_tokens"] = cached
		out["prompt_cache_hit_tokens"] = cached
	}
	if miss := toInt64(u["prompt_cache_miss_tokens"]); miss > 0 {
		out["prompt_cache_miss_tokens"] = miss
	}
	return out
}
