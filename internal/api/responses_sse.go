package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/streamrewrite"
)

// responsesStreamToChatSSE transforms an OpenAI Responses API SSE stream (what the
// Codex/ChatGPT backend emits) into an OpenAI Chat Completions chunk stream, so a
// third-party OpenAI-compatible client (Cline, Roo, opencode, …) hitting
// /v1/chat/completions against a GPT model gets the chat.completion.chunk frames it
// can parse — including streamed tool calls — instead of raw Responses events. Text
// deltas are scrubbed. It mirrors anthropicStreamToChatSSE (the Claude path), keeping
// the two compat streams consistent.
func responsesStreamToChatSSE(w http.ResponseWriter, body io.Reader, model string, includeUsage bool, scrubber *streamrewrite.Matcher) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	chatID := "chatcmpl-codex"
	roleSent := false
	// Map a Responses output_index → the chat tool_call index we present downstream.
	type responseToolStream struct {
		index           int
		kind            prompt.ResponsesToolKind
		customSawDelta  bool
		customCompleted bool
		argumentsSent   bool
	}
	toolByOutput := map[int]*responseToolStream{}
	toolByItemID := map[string]*responseToolStream{}
	bridgePlan := prompt.NewResponsesToolBridgePlan()
	nextToolIdx := 0
	sawToolCall := false

	emit := func(delta map[string]interface{}, finish interface{}) {
		chunk := map[string]interface{}{
			"id":     chatID,
			"object": "chat.completion.chunk",
			"model":  model,
			"choices": []interface{}{
				map[string]interface{}{"index": 0, "delta": delta, "finish_reason": finish},
			},
		}
		b, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	emitUsage := func(response map[string]interface{}) {
		if !includeUsage {
			return
		}
		usage, ok := responsesUsageToChat(response["usage"])
		if !ok {
			return
		}
		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": []interface{}{},
			"usage":   usage,
		}
		b, _ := json.Marshal(chunk)
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	ensureRole := func() {
		if !roleSent {
			emit(map[string]interface{}{"role": "assistant"}, nil)
			roleSent = true
		}
	}
	done := func(response map[string]interface{}) {
		finish := "stop"
		if sawToolCall {
			finish = "tool_calls"
		}
		emit(map[string]interface{}{}, finish)
		if response != nil {
			emitUsage(response)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	outputIndexOf := func(ev map[string]interface{}) int {
		switch v := ev["output_index"].(type) {
		case float64:
			return int(v)
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return int(n)
			}
		}
		return 0
	}
	escapeJSONFragment := func(value string) string {
		quoted := strconv.Quote(value)
		return quoted[1 : len(quoted)-1]
	}
	emitToolArguments := func(state *responseToolStream, arguments string) {
		if state == nil || arguments == "" {
			return
		}
		emit(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
			"index": state.index,
			"function": map[string]interface{}{
				"arguments": scrubber.ReplaceString(arguments),
			},
		}}}, nil)
		state.argumentsSent = true
	}
	ensureTool := func(outputIndex int, item map[string]interface{}) *responseToolStream {
		if existing := toolByOutput[outputIndex]; existing != nil {
			if id := asString(item["id"]); id != "" {
				toolByItemID[id] = existing
			}
			return existing
		}
		var identity prompt.ResponsesToolIdentity
		switch asString(item["type"]) {
		case "function_call":
			identity = prompt.ResponsesToolIdentity{Kind: prompt.ResponsesToolFunction, Namespace: asString(item["namespace"]), Name: asString(item["name"])}
		case "custom_tool_call":
			identity = prompt.ResponsesToolIdentity{Kind: prompt.ResponsesToolCustom, Namespace: asString(item["namespace"]), Name: asString(item["name"])}
		case "tool_search_call":
			if !strings.EqualFold(asString(item["execution"]), "client") {
				return nil
			}
			identity = prompt.ResponsesToolIdentity{Kind: prompt.ResponsesToolSearch, Name: "tool_search", Execution: "client"}
		default:
			return nil
		}
		ensureRole()
		state := &responseToolStream{index: nextToolIdx, kind: identity.Kind}
		nextToolIdx++
		toolByOutput[outputIndex] = state
		if id := asString(item["id"]); id != "" {
			toolByItemID[id] = state
		}
		sawToolCall = true
		callID := asString(item["call_id"])
		if callID == "" {
			callID = asString(item["id"])
		}
		emit(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
			"index": state.index,
			"id":    callID,
			"type":  "function",
			"function": map[string]interface{}{
				"name":      bridgePlan.EnsureChatName(identity),
				"arguments": "",
			},
		}}}, nil)
		return state
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" || data == "[DONE]" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(data))
		dec.UseNumber()
		var ev map[string]interface{}
		if dec.Decode(&ev) != nil {
			continue
		}
		switch ev["type"] {
		case "response.created":
			if resp, ok := ev["response"].(map[string]interface{}); ok {
				if id, ok := resp["id"].(string); ok && id != "" {
					chatID = id
				}
			}
			ensureRole()
		case "response.output_text.delta":
			if txt, ok := ev["delta"].(string); ok && txt != "" {
				ensureRole()
				emit(map[string]interface{}{"content": scrubber.ReplaceString(txt)}, nil)
			}
		case "response.output_item.added":
			item, _ := ev["item"].(map[string]interface{})
			if item != nil {
				ensureTool(outputIndexOf(ev), item)
			}
		case "response.function_call_arguments.delta":
			delta, ok := ev["delta"].(string)
			if !ok || delta == "" {
				continue
			}
			emitToolArguments(toolByOutput[outputIndexOf(ev)], delta)
		case "response.custom_tool_call_input.delta":
			delta, _ := ev["delta"].(string)
			state := toolByOutput[outputIndexOf(ev)]
			if state == nil {
				state = toolByItemID[asString(ev["item_id"])]
			}
			if state == nil || state.kind != prompt.ResponsesToolCustom || delta == "" {
				continue
			}
			prefix := ""
			if !state.customSawDelta {
				prefix = `{"input":"`
				state.customSawDelta = true
			}
			emitToolArguments(state, prefix+escapeJSONFragment(delta))
		case "response.custom_tool_call_input.done":
			state := toolByOutput[outputIndexOf(ev)]
			if state == nil {
				state = toolByItemID[asString(ev["item_id"])]
			}
			if state == nil || state.kind != prompt.ResponsesToolCustom || state.customCompleted {
				continue
			}
			if state.customSawDelta {
				emitToolArguments(state, `"}`)
			} else {
				emitToolArguments(state, `{"input":"`+escapeJSONFragment(asString(ev["input"]))+`"}`)
			}
			state.customCompleted = true
		case "response.output_item.done":
			item, _ := ev["item"].(map[string]interface{})
			if item == nil {
				continue
			}
			state := ensureTool(outputIndexOf(ev), item)
			if state == nil {
				continue
			}
			switch state.kind {
			case prompt.ResponsesToolCustom:
				if !state.customCompleted {
					input := asString(item["input"])
					if state.customSawDelta {
						emitToolArguments(state, `"}`)
					} else {
						emitToolArguments(state, `{"input":"`+escapeJSONFragment(input)+`"}`)
					}
					state.customCompleted = true
				}
			case prompt.ResponsesToolSearch:
				if !state.argumentsSent {
					rawArguments, _ := json.Marshal(item["arguments"])
					emitToolArguments(state, string(rawArguments))
				}
			}
		case "response.completed", "response.incomplete":
			response, _ := ev["response"].(map[string]interface{})
			done(response)
			return
		case "response.failed", "error", "response.error":
			done(nil)
			return
		}
	}
	done(nil)
}

func responsesUsageToChat(value interface{}) (map[string]interface{}, bool) {
	source, ok := value.(map[string]interface{})
	if !ok {
		return nil, false
	}
	usage := map[string]interface{}{
		"prompt_tokens":     source["input_tokens"],
		"completion_tokens": source["output_tokens"],
		"total_tokens":      source["total_tokens"],
	}
	if details, ok := source["input_tokens_details"].(map[string]interface{}); ok {
		if cached, exists := details["cached_tokens"]; exists {
			usage["prompt_tokens_details"] = map[string]interface{}{"cached_tokens": cached}
		}
	}
	if details, ok := source["output_tokens_details"].(map[string]interface{}); ok {
		if reasoning, exists := details["reasoning_tokens"]; exists {
			usage["completion_tokens_details"] = map[string]interface{}{"reasoning_tokens": reasoning}
		}
	}
	return usage, true
}
