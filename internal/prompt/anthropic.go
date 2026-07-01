package prompt

import (
	"encoding/json"
	"strings"
	"time"
)

// ChatCompletionToAnthropic converts an OpenAI Chat Completions request body
// into an Anthropic Messages (/v1/messages) request body, including full
// tool-calling: OpenAI `tools` (function) → Anthropic `tools` (input_schema),
// `tool_choice`, assistant `tool_calls` → `tool_use` blocks, and `tool`-role
// messages → `tool_result` blocks (consecutive results merged into one user
// turn). System/developer messages are hoisted to the top-level `system` field.
// Anthropic requires max_tokens, so a default is supplied when absent.
func ChatCompletionToAnthropic(raw []byte) ([]byte, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, err
	}
	messages, _ := root["messages"].([]interface{})

	out := map[string]interface{}{}
	if m, ok := root["model"].(string); ok {
		out["model"] = m
	}
	maxTokens := firstNum(root, "max_tokens", "max_completion_tokens")
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	out["max_tokens"] = maxTokens
	if v, ok := root["temperature"]; ok {
		out["temperature"] = v
	}
	if v, ok := root["top_p"]; ok {
		out["top_p"] = v
	}
	if v, ok := root["stream"].(bool); ok && v {
		out["stream"] = true
	}
	if stop := toStringSlice(root["stop"]); len(stop) > 0 {
		out["stop_sequences"] = stop
	}
	if tools := convertOpenAITools(root["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := convertToolChoice(root["tool_choice"]); tc != nil {
		out["tool_choice"] = tc
	}

	var systemParts []string
	anthMessages := make([]interface{}, 0, len(messages))
	appendToolResult := func(block map[string]interface{}) {
		if n := len(anthMessages); n > 0 {
			if last, ok := anthMessages[n-1].(map[string]interface{}); ok && last["role"] == "user" {
				if blocks, ok := last["content"].([]interface{}); ok && isToolResultContent(blocks) {
					last["content"] = append(blocks, block)
					return
				}
			}
		}
		anthMessages = append(anthMessages, map[string]interface{}{
			"role":    "user",
			"content": []interface{}{block},
		})
	}

	for _, mi := range messages {
		m, ok := mi.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if txt := chatContentToText(m["content"]); txt != "" {
				systemParts = append(systemParts, txt)
			}
		case "tool":
			appendToolResult(map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": stringOr(m["tool_call_id"], ""),
				"content":     chatContentToText(m["content"]),
			})
		case "assistant":
			blocks := []interface{}{}
			if txt := chatContentToText(m["content"]); txt != "" {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": txt})
			}
			for _, tc := range toSlice(m["tool_calls"]) {
				tcm, ok := tc.(map[string]interface{})
				if !ok {
					continue
				}
				fn, _ := tcm["function"].(map[string]interface{})
				blocks = append(blocks, map[string]interface{}{
					"type":  "tool_use",
					"id":    stringOr(tcm["id"], ""),
					"name":  stringOr(mapGet(fn, "name"), ""),
					"input": parseJSONObject(mapGet(fn, "arguments")),
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]interface{}{"type": "text", "text": ""})
			}
			anthMessages = append(anthMessages, map[string]interface{}{"role": "assistant", "content": blocks})
		default: // user
			anthMessages = append(anthMessages, map[string]interface{}{
				"role":    "user",
				"content": chatContentToText(m["content"]),
			})
		}
	}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	out["messages"] = anthMessages
	return json.Marshal(out)
}

// AnthropicToChatCompletion converts an Anthropic Messages response into an
// OpenAI Chat Completions response (non-streaming), mapping tool_use blocks to
// OpenAI tool_calls.
func AnthropicToChatCompletion(raw []byte, model string) ([]byte, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw, nil
	}
	text, toolCalls := anthropicContentToOpenAI(root["content"])
	if model == "" {
		if m, ok := root["model"].(string); ok {
			model = m
		}
	}
	id, _ := root["id"].(string)
	if id == "" {
		id = "chatcmpl-claude"
	}
	message := map[string]interface{}{"role": "assistant"}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if text != "" {
			message["content"] = text
		} else {
			message["content"] = nil
		}
	} else {
		message["content"] = text
	}
	resp := map[string]interface{}{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"message":       message,
				"finish_reason": StopReasonToFinish(root["stop_reason"]),
			},
		},
	}
	if u, ok := root["usage"].(map[string]interface{}); ok {
		resp["usage"] = anthropicUsageToOpenAI(u)
	}
	return json.Marshal(resp)
}

// StopReasonToFinish maps an Anthropic stop_reason to an OpenAI finish_reason.
func StopReasonToFinish(v interface{}) string {
	switch s, _ := v.(string); s {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default: // end_turn, stop_sequence, "" ...
		return "stop"
	}
}

// EnsureAnthropicCacheControl adds Anthropic prompt-cache breakpoints to a
// /v1/messages body that has none — most importantly the OpenAI-compat→Claude
// path, whose converted body would otherwise be billed at full input price every
// turn (zero cache hits). It mirrors what the native Claude Code client does: a
// breakpoint at the end of the (large, stable) system prompt and on the most
// recent turn, capped at the Anthropic limit of 4 total breakpoints. A body that
// already carries cache_control keeps at least one free slot respected: we only
// top up to 4, never remove the client's own breakpoints. ttl == "1h" requests
// the extended (1-hour) cache; anything else uses the standard 5-minute ephemeral
// cache. Model quality, reasoning, and output are unaffected — only billing drops.
func EnsureAnthropicCacheControl(body []byte, ttl string) []byte {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	existing := countCacheControl(root["system"]) + countCacheControl(root["tools"]) + countCacheControlMessages(root["messages"])
	budget := 4 - existing
	if budget <= 0 {
		return body
	}
	mk := func() map[string]interface{} {
		m := map[string]interface{}{"type": "ephemeral"}
		if ttl == "1h" {
			m["ttl"] = "1h"
		}
		return m
	}
	changed := false
	// 1) End of the system prompt — the largest stable cacheable prefix.
	if sys, ok := markSystemTail(root["system"], mk); ok {
		root["system"] = sys
		budget--
		changed = true
	}
	// 2) End of the tool definition prefix. Tool schemas are stable across turns and
	// should be preferred over marking volatile last-user content.
	hasTools := hasCacheableList(root["tools"])
	if budget > 0 {
		if markListTail(root["tools"], mk) {
			budget--
			changed = true
		}
	}
	// 3) Most recent message turn as a fallback for bodies without a stable tools
	// prefix, so simple multi-turn agent loops still reuse the prompt prefix.
	if budget > 0 && !hasTools {
		if msgs, ok := root["messages"].([]interface{}); ok && len(msgs) > 0 {
			if markMessageTail(msgs[len(msgs)-1], mk) {
				changed = true
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return out
}

func countCacheControl(system interface{}) int {
	n := 0
	if arr, ok := system.([]interface{}); ok {
		for _, b := range arr {
			if m, ok := b.(map[string]interface{}); ok {
				if _, has := m["cache_control"]; has {
					n++
				}
			}
		}
	}
	return n
}

func hasCacheableList(v interface{}) bool {
	arr, ok := v.([]interface{})
	if !ok {
		return false
	}
	for _, item := range arr {
		if _, ok := item.(map[string]interface{}); ok {
			return true
		}
	}
	return false
}

func countCacheControlMessages(messages interface{}) int {
	arr, ok := messages.([]interface{})
	if !ok {
		return 0
	}
	n := 0
	for _, msg := range arr {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if blocks, ok := m["content"].([]interface{}); ok {
			for _, b := range blocks {
				if bm, ok := b.(map[string]interface{}); ok {
					if _, has := bm["cache_control"]; has {
						n++
					}
				}
			}
		}
	}
	return n
}

// markSystemTail puts a cache_control breakpoint at the end of the system prompt,
// converting a plain-string system into a single text block when needed. Returns
// the (possibly rewritten) system value and whether a breakpoint was added.
func markSystemTail(system interface{}, mk func() map[string]interface{}) (interface{}, bool) {
	switch s := system.(type) {
	case string:
		if strings.TrimSpace(s) == "" {
			return system, false
		}
		return []interface{}{map[string]interface{}{"type": "text", "text": s, "cache_control": mk()}}, true
	case []interface{}:
		for i := len(s) - 1; i >= 0; i-- {
			if m, ok := s[i].(map[string]interface{}); ok {
				if _, has := m["cache_control"]; has {
					return system, false
				}
				m["cache_control"] = mk()
				return s, true
			}
		}
	}
	return system, false
}

func markListTail(v interface{}, mk func() map[string]interface{}) bool {
	arr, ok := v.([]interface{})
	if !ok {
		return false
	}
	for i := len(arr) - 1; i >= 0; i-- {
		if m, ok := arr[i].(map[string]interface{}); ok {
			if _, has := m["cache_control"]; has {
				return false
			}
			m["cache_control"] = mk()
			return true
		}
	}
	return false
}

// markMessageTail puts a cache_control breakpoint on the last content block of a
// message, converting a plain-string content into a single text block when needed.
func markMessageTail(msg interface{}, mk func() map[string]interface{}) bool {
	m, ok := msg.(map[string]interface{})
	if !ok {
		return false
	}
	switch c := m["content"].(type) {
	case string:
		if strings.TrimSpace(c) == "" {
			return false
		}
		m["content"] = []interface{}{map[string]interface{}{"type": "text", "text": c, "cache_control": mk()}}
		return true
	case []interface{}:
		for i := len(c) - 1; i >= 0; i-- {
			if bm, ok := c[i].(map[string]interface{}); ok {
				if _, has := bm["cache_control"]; has {
					return false
				}
				bm["cache_control"] = mk()
				return true
			}
		}
	}
	return false
}

// convertOpenAITools maps OpenAI function tools to Anthropic tools. Tools already
// in Anthropic shape (input_schema) or typed server tools (web_search, ...) are
// passed through unchanged so server-side tools keep working.
func convertOpenAITools(v interface{}) []interface{} {
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
		if _, ok := tm["input_schema"]; ok {
			out = append(out, tm)
			continue
		}
		fn, ok := tm["function"].(map[string]interface{})
		if !ok {
			if _, typed := tm["type"]; typed {
				out = append(out, tm) // typed built-in tool
			}
			continue
		}
		conv := map[string]interface{}{"name": stringOr(fn["name"], "")}
		if d, ok := fn["description"]; ok {
			conv["description"] = d
		}
		if params, ok := fn["parameters"]; ok {
			conv["input_schema"] = params
		} else {
			conv["input_schema"] = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, conv)
	}
	return out
}

// convertToolChoice maps an OpenAI tool_choice to Anthropic's form. "none"
// returns nil (omit; Anthropic decides) since there is no universal equivalent.
func convertToolChoice(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto":
			return map[string]interface{}{"type": "auto"}
		case "required":
			return map[string]interface{}{"type": "any"}
		}
	case map[string]interface{}:
		if fn, ok := t["function"].(map[string]interface{}); ok {
			if name := stringOr(fn["name"], ""); name != "" {
				return map[string]interface{}{"type": "tool", "name": name}
			}
		}
	}
	return nil
}

// anthropicContentToOpenAI splits Anthropic response content blocks into the
// concatenated assistant text and OpenAI-shaped tool_calls.
func anthropicContentToOpenAI(v interface{}) (string, []interface{}) {
	arr, ok := v.([]interface{})
	if !ok {
		return "", nil
	}
	var parts []string
	var toolCalls []interface{}
	for _, item := range arr {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		switch m["type"] {
		case "text":
			if s, ok := m["text"].(string); ok {
				parts = append(parts, s)
			}
		case "tool_use":
			args, _ := json.Marshal(m["input"])
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   stringOr(m["id"], ""),
				"type": "function",
				"function": map[string]interface{}{
					"name":      stringOr(m["name"], ""),
					"arguments": string(args),
				},
			})
		}
	}
	return strings.Join(parts, ""), toolCalls
}

func anthropicUsageToOpenAI(u map[string]interface{}) map[string]interface{} {
	in := numField(u, "input_tokens")
	out := numField(u, "output_tokens")
	return map[string]interface{}{
		"prompt_tokens":     in,
		"completion_tokens": out,
		"total_tokens":      in + out,
	}
}

// chatContentToText flattens OpenAI message content (a string, or an array of
// content parts) into plain text.
func chatContentToText(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		var parts []string
		for _, p := range t {
			if pm, ok := p.(map[string]interface{}); ok {
				if s, ok := pm["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

func toStringSlice(v interface{}) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func firstNum(m map[string]interface{}, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if n := toInt64(v); n > 0 {
				return n
			}
		}
	}
	return 0
}

func numField(m map[string]interface{}, key string) int64 {
	return toInt64(m[key])
}

func toInt64(v interface{}) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	}
	return 0
}

func stringOr(v interface{}, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func mapGet(m map[string]interface{}, k string) interface{} {
	if m == nil {
		return nil
	}
	return m[k]
}

func toSlice(v interface{}) []interface{} {
	if s, ok := v.([]interface{}); ok {
		return s
	}
	return nil
}

func isToolResultContent(blocks []interface{}) bool {
	if len(blocks) == 0 {
		return false
	}
	if b, ok := blocks[0].(map[string]interface{}); ok {
		return b["type"] == "tool_result"
	}
	return false
}

func parseJSONObject(v interface{}) interface{} {
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return map[string]interface{}{}
		}
		var obj interface{}
		if json.Unmarshal([]byte(t), &obj) == nil {
			return obj
		}
		return map[string]interface{}{}
	case map[string]interface{}:
		return t
	default:
		return map[string]interface{}{}
	}
}
