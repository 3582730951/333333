package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/streamrewrite"
)

type responsesAnthropicToolStream struct {
	blockIndex int
	id         string
	itemID     string
	name       string
	namespace  string
	kind       prompt.ResponsesToolKind
	arguments  strings.Builder
	started    bool
	closed     bool
}

type responsesAnthropicReasoningStream struct {
	blockIndex int
	item       map[string]interface{}
	text       strings.Builder
	started    bool
	closed     bool
}

// responsesStreamToAnthropicSSE is the native Codex Responses -> Claude Messages
// stream bridge. Unlike the old Responses -> Chat -> Messages chain, it preserves
// encrypted reasoning, parallel tool identity, complete tool arguments, cache usage,
// incomplete status, and upstream error events.
func responsesStreamToAnthropicSSE(w http.ResponseWriter, body io.Reader, requestedModel string, toolNames map[string]string, inheritModelTools map[string]bool, scrubber *streamrewrite.Matcher) map[string]interface{} {
	flusher, _ := w.(http.Flusher)
	emit := func(event string, payload map[string]interface{}) {
		payload["type"] = event
		encoded, _ := json.Marshal(payload)
		_, _ = io.WriteString(w, "event: "+event+"\n")
		_, _ = io.WriteString(w, "data: ")
		_, _ = w.Write(encoded)
		_, _ = io.WriteString(w, "\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}

	messageID := "msg_pool_codex"
	model := requestedModel
	started := false
	terminal := false
	sawContent := false
	sawText := false
	nextBlock := 0
	textBlock := -1
	textOpen := false
	hasTools := false
	var usage map[string]interface{}
	toolsByKey := map[string]*responsesAnthropicToolStream{}
	tools := make([]*responsesAnthropicToolStream, 0, 4)
	reasoningByKey := map[string]*responsesAnthropicReasoningStream{}
	reasoning := make([]*responsesAnthropicReasoningStream, 0, 2)
	bridgePlan := prompt.NewResponsesToolBridgePlan()

	ensureStarted := func() {
		if started {
			return
		}
		startUsage := map[string]interface{}{"input_tokens": int64(0), "output_tokens": int64(0)}
		if usage != nil {
			for key, value := range usage {
				if key != "output_tokens" {
					startUsage[key] = value
				}
			}
		}
		emit("message_start", map[string]interface{}{"message": map[string]interface{}{
			"id": messageID, "type": "message", "role": "assistant", "model": model,
			"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil, "usage": startUsage,
		}})
		started = true
	}
	closeText := func() {
		if !textOpen {
			return
		}
		emit("content_block_stop", map[string]interface{}{"index": textBlock})
		textOpen = false
		textBlock = -1
	}
	ensureText := func() int {
		ensureStarted()
		if textOpen {
			return textBlock
		}
		textBlock = nextBlock
		nextBlock++
		textOpen = true
		sawContent = true
		emit("content_block_start", map[string]interface{}{
			"index": textBlock, "content_block": map[string]interface{}{"type": "text", "text": ""},
		})
		return textBlock
	}

	toolKey := func(event, item map[string]interface{}) string {
		for _, value := range []interface{}{event["item_id"], mapValue(item, "id"), event["call_id"], mapValue(item, "call_id")} {
			if text := streamString(value); text != "" {
				return "id:" + text
			}
		}
		if value, ok := jsonNumberString(event["output_index"]); ok {
			return "output:" + value
		}
		return "output:0"
	}
	lookupTool := func(event map[string]interface{}) *responsesAnthropicToolStream {
		if key := toolKey(event, nil); toolsByKey[key] != nil {
			return toolsByKey[key]
		}
		if value, ok := jsonNumberString(event["output_index"]); ok {
			return toolsByKey["output:"+value]
		}
		if len(tools) == 1 {
			return tools[0]
		}
		return nil
	}
	startTool := func(state *responsesAnthropicToolStream) {
		if state == nil || state.started {
			return
		}
		closeText()
		ensureStarted()
		state.started = true
		hasTools = true
		sawContent = true
		name := state.name
		if original := toolNames[name]; original != "" {
			name = original
		}
		if state.namespace != "" {
			name = bridgePlan.EnsureChatName(prompt.ResponsesToolIdentity{Kind: state.kind, Namespace: state.namespace, Name: name})
		}
		state.name = name
		emit("content_block_start", map[string]interface{}{
			"index":         state.blockIndex,
			"content_block": map[string]interface{}{"type": "tool_use", "id": state.id, "name": name, "input": map[string]interface{}{}},
		})
	}
	flushToolArguments := func(state *responsesAnthropicToolStream, fallback string) {
		if state == nil || state.closed {
			return
		}
		startTool(state)
		arguments := state.arguments.String()
		if arguments == "" {
			arguments = fallback
		}
		if state.kind == prompt.ResponsesToolCustom {
			encoded, _ := json.Marshal(map[string]interface{}{"input": arguments})
			arguments = string(encoded)
		} else if arguments == "" {
			arguments = "{}"
		}
		arguments = sanitizeAnthropicStreamToolArguments(state.name, arguments, inheritModelTools)
		emit("content_block_delta", map[string]interface{}{
			"index": state.blockIndex, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": scrubber.ReplaceString(arguments)},
		})
		emit("content_block_stop", map[string]interface{}{"index": state.blockIndex})
		state.closed = true
	}
	registerTool := func(event, item map[string]interface{}, kind prompt.ResponsesToolKind) *responsesAnthropicToolStream {
		state := &responsesAnthropicToolStream{
			blockIndex: nextBlock, itemID: streamString(mapValue(item, "id")), kind: kind,
			id:        streamString(firstNonEmptyInterface(mapValue(item, "call_id"), mapValue(item, "id"))),
			name:      streamString(mapValue(item, "name")),
			namespace: streamString(mapValue(item, "namespace")),
		}
		if kind == prompt.ResponsesToolSearch {
			state.name = "tool_search"
		}
		nextBlock++
		key := toolKey(event, item)
		toolsByKey[key] = state
		if state.itemID != "" {
			toolsByKey["id:"+state.itemID] = state
		}
		if value, ok := jsonNumberString(event["output_index"]); ok {
			toolsByKey["output:"+value] = state
		}
		tools = append(tools, state)
		startTool(state)
		return state
	}

	reasoningKey := func(event, item map[string]interface{}) string {
		for _, value := range []interface{}{event["item_id"], mapValue(item, "id")} {
			if text := streamString(value); text != "" {
				return "id:" + text
			}
		}
		if value, ok := jsonNumberString(event["output_index"]); ok {
			return "output:" + value
		}
		return "output:0"
	}
	getReasoning := func(event, item map[string]interface{}) *responsesAnthropicReasoningStream {
		key := reasoningKey(event, item)
		if state := reasoningByKey[key]; state != nil {
			return state
		}
		state := &responsesAnthropicReasoningStream{blockIndex: nextBlock, item: item}
		nextBlock++
		reasoningByKey[key] = state
		if itemID := streamString(mapValue(item, "id")); itemID != "" {
			reasoningByKey["id:"+itemID] = state
		}
		if value, ok := jsonNumberString(event["output_index"]); ok {
			reasoningByKey["output:"+value] = state
		}
		reasoning = append(reasoning, state)
		return state
	}
	startReasoning := func(state *responsesAnthropicReasoningStream) {
		if state == nil || state.started || state.closed {
			return
		}
		closeText()
		ensureStarted()
		state.started = true
		sawContent = true
		emit("content_block_start", map[string]interface{}{
			"index": state.blockIndex, "content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
		})
	}
	finishReasoning := func(state *responsesAnthropicReasoningStream, final map[string]interface{}) {
		if state == nil || state.closed {
			return
		}
		if final != nil {
			state.item = final
		}
		visible := prompt.OpenAIReasoningSummaryText(state.item)
		signature := prompt.EncodeOpenAIReasoningItem(state.item)
		if !state.started && visible == "" && signature == "" {
			state.closed = true
			return
		}
		if !state.started && visible == "" && signature != "" {
			closeText()
			ensureStarted()
			state.started = true
			sawContent = true
			emit("content_block_start", map[string]interface{}{
				"index": state.blockIndex,
				"content_block": map[string]interface{}{
					"type": "redacted_thinking", "data": signature,
				},
			})
			emit("content_block_stop", map[string]interface{}{"index": state.blockIndex})
			state.closed = true
			return
		}
		startReasoning(state)
		seen := state.text.String()
		if visible != "" {
			missing := visible
			if strings.HasPrefix(visible, seen) {
				missing = visible[len(seen):]
			} else if seen != "" {
				missing = ""
			}
			if missing != "" {
				emit("content_block_delta", map[string]interface{}{
					"index": state.blockIndex, "delta": map[string]interface{}{"type": "thinking_delta", "thinking": scrubber.ReplaceString(missing)},
				})
				state.text.WriteString(missing)
			}
		}
		if signature != "" {
			emit("content_block_delta", map[string]interface{}{
				"index": state.blockIndex, "delta": map[string]interface{}{"type": "signature_delta", "signature": signature},
			})
		}
		emit("content_block_stop", map[string]interface{}{"index": state.blockIndex})
		state.closed = true
	}

	emitTerminal := func(response map[string]interface{}, eventType string) {
		if terminal {
			return
		}
		terminal = true
		if response != nil {
			if terminalError := responsesStreamError(response); terminalError != "" {
				emit("error", map[string]interface{}{"error": map[string]interface{}{"type": "api_error", "message": terminalError}})
				return
			}
			usage = prompt.ResponsesUsageToAnthropic(response["usage"])
		}
		for _, state := range reasoning {
			finishReasoning(state, state.item)
		}
		closeText()
		for _, state := range tools {
			flushToolArguments(state, "")
		}
		ensureStarted()
		stopReason := "end_turn"
		if hasTools {
			stopReason = "tool_use"
		} else if eventType == "response.incomplete" || streamString(mapValue(response, "status")) == "incomplete" {
			details, _ := mapValue(response, "incomplete_details").(map[string]interface{})
			reason := streamString(mapValue(details, "reason"))
			if reason == "" || reason == "max_output_tokens" || reason == "max_tokens" {
				stopReason = "max_tokens"
			}
		}
		messageUsage := map[string]interface{}{"output_tokens": int64(0)}
		for key, value := range usage {
			messageUsage[key] = value
		}
		emit("message_delta", map[string]interface{}{
			"delta": map[string]interface{}{"stop_reason": stopReason, "stop_sequence": nil}, "usage": messageUsage,
		})
		emit("message_stop", map[string]interface{}{})
	}

	process := func(frame []byte) {
		eventName, data := sseFrameEventData(frame)
		if len(data) == 0 || strings.TrimSpace(string(data)) == "[DONE]" || terminal {
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var event map[string]interface{}
		if decoder.Decode(&event) != nil {
			return
		}
		typ := streamString(event["type"])
		if typ == "" {
			typ = eventName
		}
		switch typ {
		case "response.created", "response.in_progress":
			response, _ := event["response"].(map[string]interface{})
			if id := streamString(mapValue(response, "id")); id != "" {
				messageID = id
			}
			if upstreamModel := streamString(mapValue(response, "model")); upstreamModel != "" {
				model = upstreamModel
			}
			if response != nil && response["usage"] != nil {
				usage = prompt.ResponsesUsageToAnthropic(response["usage"])
			}
			ensureStarted()
		case "response.output_text.delta", "response.refusal.delta":
			if delta := streamString(event["delta"]); delta != "" {
				sawText = true
				index := ensureText()
				emit("content_block_delta", map[string]interface{}{
					"index": index, "delta": map[string]interface{}{"type": "text_delta", "text": scrubber.ReplaceString(delta)},
				})
			}
		case "response.output_item.added":
			item, _ := event["item"].(map[string]interface{})
			switch streamString(mapValue(item, "type")) {
			case "function_call":
				registerTool(event, item, prompt.ResponsesToolFunction)
			case "custom_tool_call":
				registerTool(event, item, prompt.ResponsesToolCustom)
			case "tool_search_call":
				if strings.EqualFold(streamString(mapValue(item, "execution")), "client") {
					registerTool(event, item, prompt.ResponsesToolSearch)
				}
			case "reasoning":
				getReasoning(event, item)
			}
		case "response.function_call_arguments.delta":
			if state := lookupTool(event); state != nil {
				state.arguments.WriteString(streamString(event["delta"]))
			}
		case "response.function_call_arguments.done":
			state := lookupTool(event)
			fallback := streamString(firstNonEmptyInterface(event["arguments"], mapValueFromNested(event, "item", "arguments")))
			flushToolArguments(state, fallback)
		case "response.custom_tool_call_input.delta":
			if state := lookupTool(event); state != nil && state.kind == prompt.ResponsesToolCustom {
				state.arguments.WriteString(streamString(event["delta"]))
			}
		case "response.custom_tool_call_input.done":
			state := lookupTool(event)
			fallback := streamString(firstNonEmptyInterface(event["input"], mapValueFromNested(event, "item", "input")))
			flushToolArguments(state, fallback)
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.reasoning.delta":
			state := getReasoning(event, nil)
			delta := streamString(event["delta"])
			if delta != "" {
				startReasoning(state)
				state.text.WriteString(delta)
				emit("content_block_delta", map[string]interface{}{
					"index": state.blockIndex, "delta": map[string]interface{}{"type": "thinking_delta", "thinking": scrubber.ReplaceString(delta)},
				})
			}
		case "response.output_item.done":
			item, _ := event["item"].(map[string]interface{})
			switch streamString(mapValue(item, "type")) {
			case "reasoning":
				finishReasoning(getReasoning(event, item), item)
			case "function_call":
				state := lookupTool(event)
				if state == nil {
					state = registerTool(event, item, prompt.ResponsesToolFunction)
				}
				flushToolArguments(state, streamString(mapValue(item, "arguments")))
			case "custom_tool_call":
				state := lookupTool(event)
				if state == nil {
					state = registerTool(event, item, prompt.ResponsesToolCustom)
				}
				flushToolArguments(state, streamString(mapValue(item, "input")))
			case "tool_search_call":
				if !strings.EqualFold(streamString(mapValue(item, "execution")), "client") {
					break
				}
				state := lookupTool(event)
				if state == nil {
					state = registerTool(event, item, prompt.ResponsesToolSearch)
				}
				arguments, _ := json.Marshal(mapValue(item, "arguments"))
				flushToolArguments(state, string(arguments))
			case "message":
				if !sawText {
					if text := responsesOutputItemText(item); text != "" {
						sawText = true
						index := ensureText()
						emit("content_block_delta", map[string]interface{}{
							"index": index, "delta": map[string]interface{}{"type": "text_delta", "text": scrubber.ReplaceString(text)},
						})
					}
				}
			}
		case "response.completed", "response.incomplete":
			response, _ := event["response"].(map[string]interface{})
			emitTerminal(response, typ)
		case "response.failed", "response.error", "error":
			terminal = true
			message := responsesStreamError(event)
			if response, ok := event["response"].(map[string]interface{}); ok {
				message = responsesStreamError(response)
			}
			if message == "" {
				message = "Codex upstream stream failed"
			}
			emit("error", map[string]interface{}{"error": map[string]interface{}{"type": "api_error", "message": message}})
		}
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var frame bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimRight(line, "\r") == "" {
			process(frame.Bytes())
			frame.Reset()
			continue
		}
		frame.WriteString(line)
		frame.WriteByte('\n')
	}
	if frame.Len() > 0 {
		process(frame.Bytes())
	}
	if !terminal {
		if sawContent {
			emitTerminal(nil, "response.completed")
		} else {
			emit("error", map[string]interface{}{"error": map[string]interface{}{"type": "api_error", "message": "Codex upstream stream ended before a terminal event"}})
		}
	}
	return usage
}

func responsesOutputItemText(item map[string]interface{}) string {
	var out strings.Builder
	parts, _ := item["content"].([]interface{})
	for _, rawPart := range parts {
		part, _ := rawPart.(map[string]interface{})
		if part == nil {
			continue
		}
		typ := streamString(part["type"])
		if typ == "output_text" || typ == "refusal" || typ == "text" {
			out.WriteString(streamString(firstNonEmptyInterface(part["text"], part["refusal"])))
		}
	}
	return out.String()
}

func sanitizeAnthropicStreamToolArguments(name, raw string, inheritModelTools map[string]bool) string {
	if (name != "Read" && !inheritModelTools[name]) || strings.TrimSpace(raw) == "" {
		return raw
	}
	input, err := decodeStreamJSONMapUseNumber([]byte(raw))
	if err != nil {
		return raw
	}
	if pages, ok := input["pages"].(string); ok && pages == "" {
		delete(input, "pages")
	}
	if inheritModelTools[name] {
		delete(input, "model")
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return raw
	}
	return string(encoded)
}

func responsesStreamError(value map[string]interface{}) string {
	if value == nil {
		return ""
	}
	status := strings.ToLower(streamString(value["status"]))
	errorValue, _ := value["error"].(map[string]interface{})
	if status != "failed" && errorValue == nil {
		return ""
	}
	if message := streamString(mapValue(errorValue, "message")); message != "" {
		return message
	}
	if message := streamString(value["message"]); message != "" {
		return message
	}
	return "Codex upstream response failed"
}

func mapValue(value map[string]interface{}, key string) interface{} {
	if value == nil {
		return nil
	}
	return value[key]
}

func mapValueFromNested(value map[string]interface{}, parent, key string) interface{} {
	nested, _ := mapValue(value, parent).(map[string]interface{})
	return mapValue(nested, key)
}

func firstNonEmptyInterface(values ...interface{}) interface{} {
	for _, value := range values {
		if text, ok := value.(string); ok {
			if text != "" {
				return text
			}
			continue
		}
		if value != nil {
			return value
		}
	}
	return nil
}

func jsonNumberString(value interface{}) (string, bool) {
	switch current := value.(type) {
	case json.Number:
		return current.String(), true
	case float64:
		encoded, _ := json.Marshal(int(current))
		return string(encoded), true
	case int:
		return strconv.Itoa(current), true
	case int64:
		return strconv.FormatInt(current, 10), true
	default:
		return "", false
	}
}

// anthropicMessageJSONToSSE adapts a complete Messages object when an upstream
// Responses endpoint unexpectedly returned JSON to a streaming Claude request.
func anthropicMessageJSONToSSE(w http.ResponseWriter, raw []byte) error {
	message, err := decodeStreamJSONMapUseNumber(raw)
	if err != nil {
		return err
	}
	flusher, _ := w.(http.Flusher)
	emit := func(event string, payload map[string]interface{}) {
		payload["type"] = event
		encoded, _ := json.Marshal(payload)
		_, _ = io.WriteString(w, "event: "+event+"\ndata: ")
		_, _ = w.Write(encoded)
		_, _ = io.WriteString(w, "\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	start := cloneJSONMap(message)
	start["content"] = []interface{}{}
	start["stop_reason"] = nil
	start["stop_sequence"] = nil
	if usage, ok := start["usage"].(map[string]interface{}); ok {
		usage["output_tokens"] = float64(0)
	}
	emit("message_start", map[string]interface{}{"message": start})
	content, _ := message["content"].([]interface{})
	for index, rawBlock := range content {
		block, _ := rawBlock.(map[string]interface{})
		if block == nil {
			continue
		}
		typ := streamString(block["type"])
		switch typ {
		case "text":
			emit("content_block_start", map[string]interface{}{"index": index, "content_block": map[string]interface{}{"type": "text", "text": ""}})
			emit("content_block_delta", map[string]interface{}{"index": index, "delta": map[string]interface{}{"type": "text_delta", "text": streamString(block["text"])}})
		case "thinking":
			emit("content_block_start", map[string]interface{}{"index": index, "content_block": map[string]interface{}{"type": "thinking", "thinking": ""}})
			if text := streamString(block["thinking"]); text != "" {
				emit("content_block_delta", map[string]interface{}{"index": index, "delta": map[string]interface{}{"type": "thinking_delta", "thinking": text}})
			}
			if signature := streamString(block["signature"]); signature != "" {
				emit("content_block_delta", map[string]interface{}{"index": index, "delta": map[string]interface{}{"type": "signature_delta", "signature": signature}})
			}
		case "redacted_thinking":
			emit("content_block_start", map[string]interface{}{"index": index, "content_block": block})
		case "tool_use":
			emit("content_block_start", map[string]interface{}{"index": index, "content_block": map[string]interface{}{
				"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]interface{}{},
			}})
			arguments, _ := json.Marshal(firstNonEmptyInterface(block["input"], map[string]interface{}{}))
			emit("content_block_delta", map[string]interface{}{"index": index, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(arguments)}})
		}
		emit("content_block_stop", map[string]interface{}{"index": index})
	}
	delta := map[string]interface{}{"stop_reason": message["stop_reason"], "stop_sequence": message["stop_sequence"]}
	endUsage := map[string]interface{}{"output_tokens": float64(0)}
	if usage, ok := message["usage"].(map[string]interface{}); ok {
		for key, value := range usage {
			endUsage[key] = value
		}
	}
	emit("message_delta", map[string]interface{}{"delta": delta, "usage": endUsage})
	emit("message_stop", map[string]interface{}{})
	return nil
}

func decodeStreamJSONMapUseNumber(raw []byte) (map[string]interface{}, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]interface{}
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected trailing JSON value")
		}
		return nil, err
	}
	return value, nil
}
