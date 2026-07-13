package prompt

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	openAIReasoningEnvelopePrefix = "pool-openai-reasoning-v1:"
	toolResultErrorMarker         = "[pool:tool-result-error]"
	codexToolNameLimit            = 64
	codexCallIDLimit              = 64
)

// AnthropicResponsesRequest is the native bridge product used by Claude Code's
// Messages endpoint. Body is already a Responses request; ToolNames reverses any
// deterministic shortening required by the Codex wire contract.
type AnthropicResponsesRequest struct {
	Body      []byte
	ToolNames map[string]string // wire name -> original Claude Code name
}

// AnthropicRequestToResponses converts a Claude Messages request directly to the
// Codex Responses wire shape. Keeping this conversion direct is important: routing
// through Chat Completions loses encrypted reasoning items, typed image/file parts,
// error-bearing tool results, and exact tool-call ordering on multi-turn agent loops.
func AnthropicRequestToResponses(raw []byte) (AnthropicResponsesRequest, error) {
	root, err := decodeJSONMapUseNumber(raw)
	if err != nil {
		return AnthropicResponsesRequest{}, err
	}

	originalToWire, wireToOriginal := anthropicResponsesToolNameMaps(root["tools"])
	input, err := anthropicMessagesToResponsesInput(root["messages"], originalToWire)
	if err != nil {
		return AnthropicResponsesRequest{}, err
	}

	model, _ := root["model"].(string)
	out := map[string]interface{}{
		"model":               model,
		"instructions":        anthropicResponsesSystem(root["system"]),
		"input":               input,
		"tools":               anthropicResponsesTools(root["tools"], originalToWire),
		"parallel_tool_calls": anthropicResponsesParallelTools(root["tool_choice"]),
		"reasoning": map[string]interface{}{
			"effort":  anthropicResponsesEffort(root),
			"summary": "auto",
		},
		"include": []interface{}{"reasoning.encrypted_content"},
		"store":   false,
		// The ChatGPT Codex backend is streaming-only. A non-streaming Claude request
		// is aggregated back to one Messages JSON object by the response bridge.
		"stream": true,
	}
	if choice := anthropicResponsesToolChoice(root["tool_choice"], originalToWire); choice != nil {
		out["tool_choice"] = choice
	}
	if key := anthropicResponsesSessionCacheKey(root["metadata"]); key != "" {
		out["prompt_cache_key"] = key
	}

	body, err := json.Marshal(out)
	if err != nil {
		return AnthropicResponsesRequest{}, err
	}
	return AnthropicResponsesRequest{Body: body, ToolNames: wireToOriginal}, nil
}

func decodeJSONMapUseNumber(raw []byte) (map[string]interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root map[string]interface{}
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	return root, nil
}

func anthropicResponsesSystem(value interface{}) string {
	parts := make([]string, 0, 4)
	appendPart := func(text string) {
		text = stripLeadingAnthropicBillingHeader(text)
		if text != "" {
			parts = append(parts, text)
		}
	}
	switch current := value.(type) {
	case string:
		appendPart(current)
	case []interface{}:
		for _, item := range current {
			block, _ := item.(map[string]interface{})
			if block == nil || stringOr(block["type"], "text") != "text" {
				continue
			}
			appendPart(stringOr(block["text"], ""))
		}
	}
	return strings.Join(parts, "\n\n")
}

func stripLeadingAnthropicBillingHeader(text string) string {
	const prefix = "x-anthropic-billing-header:"
	if !strings.HasPrefix(text, prefix) {
		return text
	}
	lineEnd := strings.IndexAny(text, "\r\n")
	if lineEnd < 0 {
		return ""
	}
	rest := text[lineEnd:]
	rest = strings.TrimPrefix(rest, "\r\n")
	rest = strings.TrimPrefix(rest, "\n")
	rest = strings.TrimPrefix(rest, "\r")
	rest = strings.TrimPrefix(rest, "\r\n")
	rest = strings.TrimPrefix(rest, "\n")
	rest = strings.TrimPrefix(rest, "\r")
	return rest
}

func anthropicMessagesToResponsesInput(value interface{}, names map[string]string) ([]interface{}, error) {
	messages, _ := value.([]interface{})
	input := make([]interface{}, 0, len(messages)*2)
	for _, item := range messages {
		message, _ := item.(map[string]interface{})
		if message == nil {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(stringOr(message["role"], "user")))
		if role == "system" {
			role = "developer"
		}
		if role != "assistant" && role != "developer" {
			role = "user"
		}
		messageStart := len(input)
		parts := make([]interface{}, 0, 4)
		flush := func() {
			if len(parts) == 0 {
				return
			}
			input = append(input, map[string]interface{}{
				"type": "message", "role": role, "content": parts,
			})
			parts = make([]interface{}, 0, 4)
		}
		appendText := func(text string) {
			typ := "input_text"
			if role == "assistant" {
				typ = "output_text"
			}
			parts = append(parts, map[string]interface{}{"type": typ, "text": text})
		}

		switch content := message["content"].(type) {
		case string:
			appendText(content)
		case []interface{}:
			for _, rawBlock := range content {
				block, _ := rawBlock.(map[string]interface{})
				if block == nil {
					continue
				}
				switch stringOr(block["type"], "") {
				case "text":
					appendText(stringOr(block["text"], ""))
				case "image":
					if image := anthropicImageToResponsesPart(block); image != nil {
						parts = append(parts, image)
					}
				case "document":
					if file := anthropicDocumentToResponsesPart(block); file != nil {
						parts = append(parts, file)
					}
				case "tool_use":
					flush()
					name := stringOr(block["name"], "")
					if wire := names[name]; wire != "" {
						name = wire
					}
					arguments, err := json.Marshal(firstPresent(block["input"], map[string]interface{}{}))
					if err != nil {
						return nil, err
					}
					input = append(input, map[string]interface{}{
						"type": "function_call", "call_id": shortenCodexCallID(stringOr(block["id"], "")),
						"name": name, "arguments": string(arguments),
					})
				case "tool_result":
					flush()
					input = append(input, map[string]interface{}{
						"type": "function_call_output", "call_id": shortenCodexCallID(stringOr(block["tool_use_id"], "")),
						"output": anthropicToolResultToResponsesOutput(block),
					})
				case "thinking", "redacted_thinking":
					if reasoning := openAIReasoningItemFromAnthropicBlock(block); reasoning != nil {
						flush()
						input = append(input, reasoning)
					}
				}
			}
		}
		flush()

		// A replayed reasoning item is valid only when the same assistant turn has
		// a following assistant message or function call. Removing an orphan avoids
		// the upstream "reasoning item without its required following item" error.
		if role == "assistant" {
			follower := false
			for index := len(input) - 1; index >= messageStart; index-- {
				entry, _ := input[index].(map[string]interface{})
				if entry == nil {
					continue
				}
				if entry["type"] == "reasoning" {
					if !follower {
						input = append(input[:index], input[index+1:]...)
					}
					continue
				}
				if entry["type"] == "function_call" || entry["role"] == "assistant" {
					follower = true
				}
			}
		}
	}
	return input, nil
}

func anthropicImageToResponsesPart(block map[string]interface{}) interface{} {
	source, _ := block["source"].(map[string]interface{})
	if source == nil {
		return nil
	}
	if stringOr(source["type"], "") == "url" {
		url := stringOr(source["url"], "")
		if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
			return map[string]interface{}{"type": "input_image", "image_url": url}
		}
		return nil
	}
	data := stringOr(firstPresent(source["data"], source["base64"]), "")
	if data == "" {
		return nil
	}
	media := stringOr(firstPresent(source["media_type"], source["mime_type"]), "image/png")
	if media == "" {
		media = "image/png"
	}
	return map[string]interface{}{"type": "input_image", "image_url": "data:" + media + ";base64," + data}
}

func anthropicDocumentToResponsesPart(block map[string]interface{}) interface{} {
	source, _ := block["source"].(map[string]interface{})
	if source == nil {
		return nil
	}
	filename := stringOr(firstPresent(block["title"], block["filename"]), "document.pdf")
	switch stringOr(source["type"], "") {
	case "url":
		url := stringOr(source["url"], "")
		if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
			return map[string]interface{}{"type": "input_file", "file_url": url, "filename": filename}
		}
	case "base64":
		data := stringOr(source["data"], "")
		if data != "" {
			media := stringOr(source["media_type"], "application/pdf")
			return map[string]interface{}{"type": "input_file", "file_data": "data:" + media + ";base64," + data, "filename": filename}
		}
	}
	return nil
}

func anthropicToolResultToResponsesOutput(block map[string]interface{}) interface{} {
	content := block["content"]
	isError, _ := block["is_error"].(bool)
	if !isError {
		if text, ok := content.(string); ok {
			return text
		}
	}
	output := make([]interface{}, 0, 4)
	if isError {
		output = append(output, map[string]interface{}{"type": "input_text", "text": toolResultErrorMarker})
	}
	appendFallback := func(value interface{}) {
		raw, _ := json.Marshal(value)
		output = append(output, map[string]interface{}{"type": "input_text", "text": string(raw)})
	}
	switch current := content.(type) {
	case string:
		output = append(output, map[string]interface{}{"type": "input_text", "text": current})
	case []interface{}:
		for _, item := range current {
			part, _ := item.(map[string]interface{})
			if part == nil {
				appendFallback(item)
				continue
			}
			switch stringOr(part["type"], "") {
			case "text":
				output = append(output, map[string]interface{}{"type": "input_text", "text": stringOr(part["text"], "")})
			case "image":
				if image := anthropicImageToResponsesPart(part); image != nil {
					output = append(output, image)
				} else {
					appendFallback(part)
				}
			case "document":
				if file := anthropicDocumentToResponsesPart(part); file != nil {
					output = append(output, file)
				} else {
					appendFallback(part)
				}
			default:
				appendFallback(part)
			}
		}
	case nil:
	default:
		appendFallback(current)
	}
	return output
}

func anthropicResponsesTools(value interface{}, names map[string]string) []interface{} {
	tools, _ := value.([]interface{})
	out := make([]interface{}, 0, len(tools))
	for _, item := range tools {
		tool, _ := item.(map[string]interface{})
		if tool == nil || stringOr(tool["type"], "") == "BatchTool" {
			continue
		}
		typ := stringOr(tool["type"], "")
		if strings.HasPrefix(typ, "web_search_") {
			web := map[string]interface{}{"type": "web_search"}
			if domains, ok := tool["allowed_domains"].([]interface{}); ok && len(domains) > 0 {
				web["filters"] = map[string]interface{}{"allowed_domains": domains}
			}
			if location, ok := tool["user_location"].(map[string]interface{}); ok {
				web["user_location"] = location
			}
			out = append(out, web)
			continue
		}
		name := stringOr(tool["name"], "")
		if name == "" {
			continue
		}
		if wire := names[name]; wire != "" {
			name = wire
		}
		definition := map[string]interface{}{
			"type": "function", "name": name, "strict": false,
			"parameters": cleanAnthropicResponsesSchema(tool["input_schema"]),
		}
		if description := stringOr(tool["description"], ""); description != "" {
			definition["description"] = description
		}
		out = append(out, definition)
	}
	return out
}

func cleanAnthropicResponsesSchema(value interface{}) interface{} {
	var clean func(interface{}) interface{}
	clean = func(current interface{}) interface{} {
		switch typed := current.(type) {
		case map[string]interface{}:
			out := make(map[string]interface{}, len(typed))
			for key, child := range typed {
				switch key {
				case "$schema", "cache_control", "defer_loading":
					continue
				}
				out[key] = clean(child)
			}
			return out
		case []interface{}:
			out := make([]interface{}, len(typed))
			for index, child := range typed {
				out[index] = clean(child)
			}
			return out
		default:
			return current
		}
	}
	root, _ := clean(value).(map[string]interface{})
	if root == nil {
		root = map[string]interface{}{}
	}
	if stringOr(root["type"], "") == "" {
		root["type"] = "object"
	}
	if root["type"] == "object" {
		if _, ok := root["properties"].(map[string]interface{}); !ok {
			root["properties"] = map[string]interface{}{}
		}
	}
	return root
}

func anthropicResponsesToolChoice(value interface{}, names map[string]string) interface{} {
	choice, _ := value.(map[string]interface{})
	if choice == nil {
		return "auto"
	}
	switch stringOr(choice["type"], "auto") {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		name := stringOr(choice["name"], "")
		if wire := names[name]; wire != "" {
			name = wire
		}
		if name != "" {
			return map[string]interface{}{"type": "function", "name": name}
		}
	}
	return "auto"
}

func anthropicResponsesParallelTools(value interface{}) bool {
	choice, _ := value.(map[string]interface{})
	if choice == nil {
		return true
	}
	disabled, _ := choice["disable_parallel_tool_use"].(bool)
	return !disabled
}

func anthropicResponsesEffort(root map[string]interface{}) string {
	if effort := anthropicReasoningEffort(root); effort != "" {
		// Claude Code calls its strongest session setting "max"; Codex Responses
		// exposes that same tier as "xhigh".
		if effort == "max" {
			return "xhigh"
		}
		return effort
	}
	if thinking, _ := root["thinking"].(map[string]interface{}); thinking != nil {
		switch stringOr(thinking["type"], "") {
		case "adaptive", "auto":
			return "xhigh"
		case "disabled":
			return "low"
		}
	}
	return "medium"
}

func anthropicResponsesSessionCacheKey(value interface{}) string {
	metadata, _ := value.(map[string]interface{})
	if metadata == nil {
		return ""
	}
	seed := stringOr(firstPresent(metadata["session_id"], metadata["user_id"]), "")
	if strings.TrimSpace(seed) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(seed))
	return "claude_" + hex.EncodeToString(sum[:])[:24]
}

func anthropicResponsesToolNameMaps(value interface{}) (map[string]string, map[string]string) {
	tools, _ := value.([]interface{})
	originals := make([]string, 0, len(tools))
	for _, item := range tools {
		tool, _ := item.(map[string]interface{})
		if name := stringOr(mapGet(tool, "name"), ""); name != "" {
			originals = append(originals, name)
		}
	}
	used := map[string]bool{}
	originalToWire := map[string]string{}
	wireToOriginal := map[string]string{}
	for _, original := range originals {
		candidate := original
		if len(candidate) > codexToolNameLimit {
			if strings.HasPrefix(candidate, "mcp__") {
				if index := strings.LastIndex(candidate, "__"); index > 0 {
					candidate = "mcp__" + candidate[index+2:]
				}
			}
			if len(candidate) > codexToolNameLimit {
				candidate = candidate[:codexToolNameLimit]
			}
		}
		base := candidate
		for suffix := 1; used[candidate]; suffix++ {
			tail := fmt.Sprintf("_%d", suffix)
			limit := codexToolNameLimit - len(tail)
			candidate = base
			if len(candidate) > limit {
				candidate = candidate[:limit]
			}
			candidate += tail
		}
		used[candidate] = true
		originalToWire[original] = candidate
		wireToOriginal[candidate] = original
	}
	return originalToWire, wireToOriginal
}

func shortenCodexCallID(id string) string {
	if len(id) <= codexCallIDLimit {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	suffix := "_" + hex.EncodeToString(sum[:8])
	return id[:codexCallIDLimit-len(suffix)] + suffix
}

func openAIReasoningItemFromAnthropicBlock(block map[string]interface{}) map[string]interface{} {
	var envelope string
	switch stringOr(block["type"], "") {
	case "thinking":
		envelope = stringOr(block["signature"], "")
	case "redacted_thinking":
		envelope = stringOr(block["data"], "")
	}
	payload := strings.TrimPrefix(envelope, openAIReasoningEnvelopePrefix)
	if payload == envelope || payload == "" {
		return nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	item, err := decodeJSONMapUseNumber(decoded)
	if err != nil || item["type"] != "reasoning" {
		return nil
	}
	return item
}

func anthropicBlockFromOpenAIReasoningItem(item map[string]interface{}) interface{} {
	if item == nil || item["type"] != "reasoning" {
		return nil
	}
	text := openAIReasoningSummaryText(item)
	encrypted := stringOr(item["encrypted_content"], "")
	if encrypted != "" {
		raw, err := json.Marshal(item)
		if err != nil {
			return nil
		}
		envelope := openAIReasoningEnvelopePrefix + base64.RawURLEncoding.EncodeToString(raw)
		if text == "" {
			return map[string]interface{}{"type": "redacted_thinking", "data": envelope}
		}
		return map[string]interface{}{"type": "thinking", "thinking": text, "signature": envelope}
	}
	if text != "" {
		return map[string]interface{}{"type": "thinking", "thinking": text}
	}
	return nil
}

func openAIReasoningSummaryText(item map[string]interface{}) string {
	var result strings.Builder
	summary, _ := item["summary"].([]interface{})
	for _, rawPart := range summary {
		part, _ := rawPart.(map[string]interface{})
		if part == nil {
			continue
		}
		typ := stringOr(part["type"], "")
		if typ == "summary_text" || typ == "reasoning_text" || typ == "" {
			result.WriteString(stringOr(part["text"], ""))
		}
	}
	return result.String()
}

// ResponsesToAnthropicResponse converts a complete Responses object to a Claude
// Messages object, preserving reasoning as an opaque replayable thinking envelope.
func ResponsesToAnthropicResponse(raw []byte, requestedModel string, toolNames map[string]string) ([]byte, error) {
	root, err := decodeJSONMapUseNumber(raw)
	if err != nil {
		return nil, err
	}
	if wrapped, ok := root["response"].(map[string]interface{}); ok {
		root = wrapped
	}
	if err := responsesTerminalError(root); err != nil {
		return nil, err
	}

	content := make([]interface{}, 0, 4)
	hasTools := false
	output, _ := root["output"].([]interface{})
	for _, rawItem := range output {
		item, _ := rawItem.(map[string]interface{})
		if item == nil {
			continue
		}
		switch stringOr(item["type"], "") {
		case "reasoning":
			if block := anthropicBlockFromOpenAIReasoningItem(item); block != nil {
				content = append(content, block)
			}
		case "message":
			parts, _ := item["content"].([]interface{})
			for _, rawPart := range parts {
				part, _ := rawPart.(map[string]interface{})
				if part == nil {
					continue
				}
				typ := stringOr(part["type"], "")
				text := stringOr(firstPresent(part["text"], part["refusal"]), "")
				if (typ == "output_text" || typ == "refusal" || typ == "text") && text != "" {
					content = append(content, map[string]interface{}{"type": "text", "text": text})
				}
			}
		case "function_call":
			name := stringOr(item["name"], "")
			if original := toolNames[name]; original != "" {
				name = original
			}
			input, parseErr := responsesFunctionArguments(item["arguments"])
			if parseErr != nil {
				if stringOr(root["status"], "") == "completed" {
					return nil, fmt.Errorf("invalid Codex function arguments for %q: %w", name, parseErr)
				}
				input = map[string]interface{}{}
			}
			input = sanitizeClaudeToolInput(name, input)
			callID := stringOr(firstPresent(item["call_id"], item["id"]), "")
			content = append(content, map[string]interface{}{
				"type": "tool_use", "id": shortenCodexCallID(callID), "name": name, "input": input,
			})
			hasTools = true
		}
	}
	if len(content) == 0 {
		if text := stringOr(root["output_text"], ""); text != "" {
			content = append(content, map[string]interface{}{"type": "text", "text": text})
		}
	}

	model := stringOr(root["model"], requestedModel)
	if model == "" {
		model = requestedModel
	}
	id := stringOr(root["id"], "msg_pool_codex")
	response := map[string]interface{}{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": responsesAnthropicStopReason(root, hasTools), "stop_sequence": nil,
		"usage": responsesAnthropicUsage(root["usage"]),
	}
	return json.Marshal(response)
}

func responsesTerminalError(root map[string]interface{}) error {
	status := strings.ToLower(stringOr(root["status"], ""))
	if status != "failed" && root["type"] != "response.failed" {
		return nil
	}
	message := "Codex upstream response failed"
	if detail, ok := root["error"].(map[string]interface{}); ok {
		message = stringOr(detail["message"], message)
	}
	return errors.New(message)
}

func responsesFunctionArguments(value interface{}) (map[string]interface{}, error) {
	if object, ok := value.(map[string]interface{}); ok {
		return object, nil
	}
	raw := strings.TrimSpace(stringOr(value, ""))
	if raw == "" {
		return map[string]interface{}{}, nil
	}
	decoded, err := decodeJSONMapUseNumber([]byte(raw))
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

func sanitizeClaudeToolInput(name string, input map[string]interface{}) map[string]interface{} {
	if name == "Read" && stringOr(input["pages"], "-") == "" {
		delete(input, "pages")
	}
	return input
}

func responsesAnthropicStopReason(root map[string]interface{}, hasTools bool) string {
	if hasTools {
		return "tool_use"
	}
	status := stringOr(root["status"], "")
	if status == "incomplete" {
		details, _ := root["incomplete_details"].(map[string]interface{})
		reason := stringOr(mapGet(details, "reason"), "")
		if reason == "max_output_tokens" || reason == "max_tokens" || reason == "" {
			return "max_tokens"
		}
	}
	if sequence := stringOr(root["stop_sequence"], ""); sequence != "" {
		return "stop_sequence"
	}
	return "end_turn"
}

// ResponsesUsageToAnthropic exposes Responses usage with Anthropic's disjoint
// fresh/cache buckets. Exported for the streaming bridge.
func ResponsesUsageToAnthropic(value interface{}) map[string]interface{} {
	return responsesAnthropicUsage(value)
}

func responsesAnthropicUsage(value interface{}) map[string]interface{} {
	usage, _ := value.(map[string]interface{})
	if usage == nil {
		return map[string]interface{}{"input_tokens": int64(0), "output_tokens": int64(0)}
	}
	input := toInt64(firstPresent(usage["input_tokens"], usage["prompt_tokens"]))
	output := toInt64(firstPresent(usage["output_tokens"], usage["completion_tokens"]))
	cacheRead := toInt64(usage["cache_read_input_tokens"])
	cacheWrite := toInt64(usage["cache_creation_input_tokens"])
	if details, ok := usage["input_tokens_details"].(map[string]interface{}); ok {
		if cacheRead == 0 {
			cacheRead = toInt64(details["cached_tokens"])
		}
		if cacheWrite == 0 {
			cacheWrite = toInt64(details["cache_write_tokens"])
		}
	}
	fresh := input - cacheRead - cacheWrite
	if fresh < 0 {
		fresh = 0
	}
	out := map[string]interface{}{"input_tokens": fresh, "output_tokens": output}
	if cacheRead > 0 {
		out["cache_read_input_tokens"] = cacheRead
	}
	if cacheWrite > 0 {
		out["cache_creation_input_tokens"] = cacheWrite
	}
	if creation := usage["cache_creation"]; creation != nil {
		out["cache_creation"] = creation
	}
	return out
}

// EncodeOpenAIReasoningItem creates the opaque thinking signature used by the
// streaming bridge once the final reasoning output item arrives.
func EncodeOpenAIReasoningItem(item map[string]interface{}) string {
	if item == nil || item["type"] != "reasoning" {
		return ""
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return ""
	}
	return openAIReasoningEnvelopePrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// OpenAIReasoningSummaryText returns all visible summary parts from a reasoning item.
func OpenAIReasoningSummaryText(item map[string]interface{}) string {
	return openAIReasoningSummaryText(item)
}
