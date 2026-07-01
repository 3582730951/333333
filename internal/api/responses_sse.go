package api

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"codex-account-pool/internal/streamrewrite"
)

// responsesStreamToChatSSE transforms an OpenAI Responses API SSE stream (what the
// Codex/ChatGPT backend emits) into an OpenAI Chat Completions chunk stream, so a
// third-party OpenAI-compatible client (Cline, Roo, opencode, …) hitting
// /v1/chat/completions against a GPT model gets the chat.completion.chunk frames it
// can parse — including streamed tool calls — instead of raw Responses events. Text
// deltas are scrubbed. It mirrors anthropicStreamToChatSSE (the Claude path), keeping
// the two compat streams consistent.
func responsesStreamToChatSSE(w http.ResponseWriter, body io.Reader, model string, scrubber *streamrewrite.Matcher) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	chatID := "chatcmpl-codex"
	roleSent := false
	// Map a Responses output_index → the chat tool_call index we present downstream.
	toolIndexByOutput := map[int]int{}
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
	ensureRole := func() {
		if !roleSent {
			emit(map[string]interface{}{"role": "assistant"}, nil)
			roleSent = true
		}
	}
	done := func() {
		finish := "stop"
		if sawToolCall {
			finish = "tool_calls"
		}
		emit(map[string]interface{}{}, finish)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	outputIndexOf := func(ev map[string]interface{}) int {
		if v, ok := ev["output_index"].(float64); ok {
			return int(v)
		}
		return 0
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
		var ev map[string]interface{}
		if json.Unmarshal([]byte(data), &ev) != nil {
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
			if item == nil || item["type"] != "function_call" {
				continue
			}
			ensureRole()
			idx := nextToolIdx
			nextToolIdx++
			toolIndexByOutput[outputIndexOf(ev)] = idx
			sawToolCall = true
			callID := asString(item["call_id"])
			if callID == "" {
				callID = asString(item["id"])
			}
			emit(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
				"index": idx,
				"id":    callID,
				"type":  "function",
				"function": map[string]interface{}{
					"name":      asString(item["name"]),
					"arguments": "",
				},
			}}}, nil)
		case "response.function_call_arguments.delta":
			delta, ok := ev["delta"].(string)
			if !ok || delta == "" {
				continue
			}
			idx, ok := toolIndexByOutput[outputIndexOf(ev)]
			if !ok {
				idx = 0
			}
			emit(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{
				"index": idx,
				"function": map[string]interface{}{
					"arguments": scrubber.ReplaceString(delta),
				},
			}}}, nil)
		case "response.completed", "response.incomplete":
			done()
			return
		case "response.failed", "error", "response.error":
			done()
			return
		}
	}
	done()
}
