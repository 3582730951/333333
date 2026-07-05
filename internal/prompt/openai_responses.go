package prompt

import (
	"encoding/json"
	"fmt"
	"strings"
)

// openai_responses.go bridges the OpenAI **Responses API** (/v1/responses — what the
// Codex CLI speaks) and OpenAI **Chat Completions** (what a custom provider such as
// DeepSeek speaks). It is the request/response (non-streaming) half; the streaming
// converter lives in package api (ChatCompletionStreamToResponsesSSE) next to the
// other SSE rewriters. These mirror, in reverse, the existing
// ChatCompletionToResponses / ResponsesToChatCompletion in prompt.go.

// ResponsesRequestToChatCompletion converts a Responses request body into a Chat
// Completions request body:
//   - instructions (string)            -> a leading system message
//   - input[] items                    -> messages[] (text passes through;
//     function_call -> assistant tool_calls; function_call_output -> tool message)
//   - flat function tools              -> Chat Completions {function:{…}} tools
//   - max_output_tokens                -> max_tokens
//   - Responses-only fields (store, previous_response_id, reasoning, include, …) dropped.
func ResponsesRequestToChatCompletion(raw []byte) ([]byte, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	out := map[string]interface{}{}
	if m, ok := root["model"].(string); ok {
		out["model"] = m
	}
	messages := make([]interface{}, 0, 8)
	if instr, ok := root["instructions"].(string); ok && strings.TrimSpace(instr) != "" {
		messages = append(messages, map[string]interface{}{"role": "system", "content": instr})
	}
	convertedMessages, err := responsesInputToChatMessages(root["input"])
	if err != nil {
		return nil, err
	}
	messages = append(messages, convertedMessages...)
	out["messages"] = messages

	if v, ok := root["stream"].(bool); ok && v {
		out["stream"] = true
	}
	if v, ok := root["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := root["top_p"]; ok {
		out["top_p"] = v
	}
	if n := firstNum(root, "max_output_tokens", "max_tokens", "max_completion_tokens"); n > 0 {
		out["max_tokens"] = n
	}
	if v, ok := root["parallel_tool_calls"]; ok {
		out["parallel_tool_calls"] = v
	}
	tools, err := responsesToolsToChat(root["tools"])
	if err != nil {
		return nil, err
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := responsesToolChoiceToChat(root["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}
	return json.Marshal(out)
}

// responsesInputToChatMessages converts a Responses `input` (a bare string, or an
// array of input items) into Chat Completions messages.
func responsesInputToChatMessages(input interface{}) ([]interface{}, error) {
	switch t := input.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		return []interface{}{map[string]interface{}{"role": "user", "content": t}}, nil
	case []interface{}:
		out := make([]interface{}, 0, len(t))
		for _, item := range t {
			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			switch itemType, _ := m["type"].(string); itemType {
			case "function_call":
				out = append(out, map[string]interface{}{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []interface{}{map[string]interface{}{
						"id":   stringOr(firstPresent(m["call_id"], m["id"]), ""),
						"type": "function",
						"function": map[string]interface{}{
							"name":      stringOr(mapGet(m, "name"), ""),
							"arguments": stringOr(mapGet(m, "arguments"), ""),
						},
					}},
				})
			case "function_call_output":
				out = append(out, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": stringOr(firstPresent(m["call_id"], m["id"]), ""),
					"content":      responsesOutputToText(m["output"]),
				})
			case "reasoning":
				// Drop reasoning (chain-of-thought) items — not part of the chat history.
			case "message", "":
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				out = append(out, map[string]interface{}{
					"role":    role,
					"content": chatContentToText(m["content"]),
				})
			default:
				return nil, responsesCompatibilityError("input item type", itemType)
			}
		}
		return out, nil
	}
	return nil, nil
}

// responsesOutputToText flattens a function_call_output `output` (string, array of
// content parts, or an object) to plain text for a Chat Completions tool message.
func responsesOutputToText(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		return chatContentToText(t)
	case map[string]interface{}:
		if s, ok := t["text"].(string); ok {
			return s
		}
		if b, err := json.Marshal(t); err == nil {
			return string(b)
		}
	}
	return ""
}

// responsesToolsToChat flattens Responses flat function tools
// ({type:"function",name,description,parameters}) into Chat Completions
// ({type:"function",function:{…}}). Typed built-in tools (web_search, …) are dropped.
func responsesToolsToChat(v interface{}) ([]interface{}, error) {
	arr, ok := v.([]interface{})
	if !ok {
		return nil, nil
	}
	out := make([]interface{}, 0, len(arr))
	for _, t := range arr {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		if _, ok := tm["function"].(map[string]interface{}); ok {
			out = append(out, tm) // already nested chat shape
			continue
		}
		if typ, _ := tm["type"].(string); typ != "" && typ != "function" {
			return nil, responsesCompatibilityError("tool type", typ)
		}
		name := stringOr(mapGet(tm, "name"), "")
		if name == "" {
			continue
		}
		fn := map[string]interface{}{"name": name}
		if d, ok := tm["description"]; ok {
			fn["description"] = d
		}
		if p, ok := tm["parameters"]; ok {
			fn["parameters"] = p
		}
		if s, ok := tm["strict"]; ok {
			fn["strict"] = s
		}
		out = append(out, map[string]interface{}{"type": "function", "function": fn})
	}
	return out, nil
}

func responsesCompatibilityError(kind, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "<empty>"
	}
	return fmt.Errorf("unsupported Responses %s %q for chat_completions bridge; configure upstream_protocol=\"responses\" or use an official Codex account for this request", kind, value)
}

func responsesToolChoiceToChat(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		return t // auto / none / required
	case map[string]interface{}:
		if stringOr(mapGet(t, "type"), "") == "function" {
			if name := stringOr(mapGet(t, "name"), ""); name != "" {
				return map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": name}}
			}
		}
	}
	return nil
}

// ChatCompletionToResponsesResponse converts a (non-streaming) Chat Completions
// response into a Responses API response, so a Responses-speaking client gets the
// shape it expects from a Chat Completions upstream. Tool calls map to function_call
// output items; text maps to a message item with an output_text content part.
func ChatCompletionToResponsesResponse(raw []byte, model string) ([]byte, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, nil
	}
	id, _ := root["id"].(string)
	if id == "" {
		id = "resp_pool"
	}
	if model == "" {
		if m, ok := root["model"].(string); ok {
			model = m
		}
	}
	text, toolCalls := chatChoiceTextAndToolCalls(root)
	output := make([]interface{}, 0, 1+len(toolCalls))
	if text != "" || len(toolCalls) == 0 {
		output = append(output, map[string]interface{}{
			"type":    "message",
			"id":      "msg_" + id,
			"role":    "assistant",
			"status":  "completed",
			"content": []interface{}{map[string]interface{}{"type": "output_text", "text": text}},
		})
	}
	for i, tc := range toolCalls {
		tcm, ok := tc.(map[string]interface{})
		if !ok {
			continue
		}
		fn, _ := tcm["function"].(map[string]interface{})
		output = append(output, map[string]interface{}{
			"type":      "function_call",
			"id":        fmt.Sprintf("fc_%s_%d", id, i),
			"call_id":   stringOr(tcm["id"], ""),
			"name":      stringOr(mapGet(fn, "name"), ""),
			"arguments": stringOr(mapGet(fn, "arguments"), ""),
			"status":    "completed",
		})
	}
	resp := map[string]interface{}{
		"id":          id,
		"object":      "response",
		"model":       model,
		"status":      "completed",
		"output":      output,
		"output_text": text,
	}
	if u, ok := root["usage"].(map[string]interface{}); ok {
		resp["usage"] = ChatUsageToResponses(u)
	}
	return json.Marshal(resp)
}

// chatChoiceTextAndToolCalls extracts choices[0].message {content, tool_calls} from a
// Chat Completions response.
func chatChoiceTextAndToolCalls(root map[string]interface{}) (string, []interface{}) {
	choices, ok := root["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return "", nil
	}
	ch, ok := choices[0].(map[string]interface{})
	if !ok {
		return "", nil
	}
	msg, ok := ch["message"].(map[string]interface{})
	if !ok {
		return "", nil
	}
	text := chatContentToText(msg["content"])
	toolCalls, _ := msg["tool_calls"].([]interface{})
	return text, toolCalls
}

// ChatUsageToResponses maps Chat Completions usage to Responses usage field names.
// Exported so the streaming converter (package api) reuses the exact same mapping.
func ChatUsageToResponses(u map[string]interface{}) map[string]interface{} {
	in := chatPromptTokens(u)
	out := toInt64(u["completion_tokens"])
	total := toInt64(u["total_tokens"])
	if total == 0 {
		total = in + out
	}
	outMap := map[string]interface{}{"input_tokens": in, "output_tokens": out, "total_tokens": total}
	if cached := chatCachedTokens(u); cached > 0 {
		outMap["input_tokens_details"] = map[string]interface{}{"cached_tokens": cached}
		outMap["prompt_cache_hit_tokens"] = cached
	}
	if miss := toInt64(u["prompt_cache_miss_tokens"]); miss > 0 {
		outMap["prompt_cache_miss_tokens"] = miss
	}
	return outMap
}

func chatPromptTokens(u map[string]interface{}) int64 {
	if v := toInt64(u["prompt_tokens"]); v > 0 {
		return v
	}
	if v := toInt64(u["input_tokens"]); v > 0 {
		return v
	}
	return toInt64(u["prompt_cache_hit_tokens"]) + toInt64(u["prompt_cache_miss_tokens"])
}

func chatCachedTokens(u map[string]interface{}) int64 {
	for _, key := range []string{"prompt_cache_hit_tokens", "cached_tokens", "input_cached_tokens", "prompt_cached_tokens", "cache_read_input_tokens"} {
		if v := toInt64(u[key]); v > 0 {
			return v
		}
	}
	for _, key := range []string{"input_tokens_details", "prompt_tokens_details"} {
		if detail, ok := u[key].(map[string]interface{}); ok {
			if v := toInt64(detail["cached_tokens"]); v > 0 {
				return v
			}
		}
	}
	return 0
}
