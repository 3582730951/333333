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

// custom_stream.go converts a custom provider's OpenAI Chat Completions SSE stream
// into the protocol the downstream client expects: the Responses API SSE (Codex) or
// the Anthropic Messages SSE (Claude Code). They are the streaming inverses of
// responsesStreamToChatSSE / anthropicStreamToChatSSE and emit the canonical event
// sequences those upstreams produce, so the real clients parse them unchanged. Each
// returns the upstream usage (chat-completions shape, nil if none) so the caller can
// record token usage exactly like the native streaming paths.

// chatChunk is one OpenAI `chat.completion.chunk` SSE frame. Pointers distinguish
// "absent" from zero/empty so a content-only or finish-only frame is handled correctly.
type chatChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Content   *string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
			// reasoning_content (deepseek-reasoner CoT) is intentionally ignored — only
			// the final answer in `content` is surfaced.
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens          int64 `json:"prompt_tokens"`
		CompletionTokens      int64 `json:"completion_tokens"`
		TotalTokens           int64 `json:"total_tokens"`
		PromptCacheHitTokens  int64 `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens int64 `json:"prompt_cache_miss_tokens"`
	} `json:"usage"`
}

func newChatSSEScanner(body io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	return sc
}

// chatChunkData returns the JSON payload of a `data:` line, or ("", false) for a
// non-data line, an empty keep-alive, or the terminal [DONE].
func chatChunkData(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	data := strings.TrimSpace(line[len("data:"):])
	if data == "" || data == "[DONE]" {
		return "", false
	}
	return data, true
}

func usageMap(c chatChunk) map[string]interface{} {
	if c.Usage == nil {
		return nil
	}
	u := map[string]interface{}{
		"prompt_tokens":     c.Usage.PromptTokens,
		"completion_tokens": c.Usage.CompletionTokens,
		"total_tokens":      c.Usage.TotalTokens,
	}
	if c.Usage.PromptCacheHitTokens > 0 {
		u["prompt_cache_hit_tokens"] = c.Usage.PromptCacheHitTokens
	}
	if c.Usage.PromptCacheMissTokens > 0 {
		u["prompt_cache_miss_tokens"] = c.Usage.PromptCacheMissTokens
	}
	return u
}

// chatStreamToResponsesSSE rewrites a Chat Completions SSE stream into a Responses API
// SSE stream (response.created → per-item added/delta/done → response.completed).
func chatStreamToResponsesSSE(w http.ResponseWriter, body io.Reader, model string, scrubber *streamrewrite.Matcher) map[string]interface{} {
	flusher, _ := w.(http.Flusher)
	emit := func(ev map[string]interface{}) {
		b, _ := json.Marshal(ev)
		if t, _ := ev["type"].(string); t != "" {
			_, _ = io.WriteString(w, "event: "+t+"\n")
		}
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	var respID, msgItemID string
	created := false
	msgOpened := false
	msgOutputIndex := -1
	var textBuf strings.Builder
	nextOutputIndex := 0

	type toolAcc struct {
		outputIndex int
		itemID      string
		callID      string
		name        string
		args        strings.Builder
	}
	tools := map[int]*toolAcc{}
	var toolOrder []int
	var usage map[string]interface{}

	ensureCreated := func(id string) {
		if created {
			return
		}
		respID = "resp_stream"
		if id != "" {
			respID = "resp_" + id
		}
		msgItemID = "msg_" + respID
		emit(map[string]interface{}{
			"type": "response.created",
			"response": map[string]interface{}{
				"id": respID, "object": "response", "status": "in_progress",
				"model": model, "output": []interface{}{},
			},
		})
		created = true
	}
	ensureMsgOpen := func() {
		if msgOpened {
			return
		}
		msgOutputIndex = nextOutputIndex
		nextOutputIndex++
		msgOpened = true
		emit(map[string]interface{}{
			"type": "response.output_item.added", "output_index": msgOutputIndex,
			"item": map[string]interface{}{"type": "message", "id": msgItemID, "role": "assistant", "status": "in_progress", "content": []interface{}{}},
		})
		emit(map[string]interface{}{
			"type": "response.content_part.added", "item_id": msgItemID, "output_index": msgOutputIndex,
			"content_index": 0, "part": map[string]interface{}{"type": "output_text", "text": ""},
		})
	}

	sc := newChatSSEScanner(body)
	for sc.Scan() {
		data, ok := chatChunkData(sc.Text())
		if !ok {
			continue
		}
		var c chatChunk
		if json.Unmarshal([]byte(data), &c) != nil {
			continue
		}
		ensureCreated(c.ID)
		if u := usageMap(c); u != nil {
			usage = u
		}
		for _, choice := range c.Choices {
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				ensureMsgOpen()
				txt := scrubber.ReplaceString(*choice.Delta.Content)
				textBuf.WriteString(txt)
				emit(map[string]interface{}{
					"type": "response.output_text.delta", "item_id": msgItemID,
					"output_index": msgOutputIndex, "content_index": 0, "delta": txt,
				})
			}
			for _, tc := range choice.Delta.ToolCalls {
				acc := tools[tc.Index]
				if acc == nil {
					acc = &toolAcc{outputIndex: nextOutputIndex}
					nextOutputIndex++
					tools[tc.Index] = acc
					toolOrder = append(toolOrder, tc.Index)
				}
				if tc.ID != "" {
					acc.callID = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				if acc.itemID == "" {
					acc.itemID = "fc_" + respID + "_" + strconv.Itoa(acc.outputIndex)
					emit(map[string]interface{}{
						"type": "response.output_item.added", "output_index": acc.outputIndex,
						"item": map[string]interface{}{"type": "function_call", "id": acc.itemID, "call_id": acc.callID, "name": acc.name, "arguments": "", "status": "in_progress"},
					})
				}
				if tc.Function.Arguments != "" {
					arg := scrubber.ReplaceString(tc.Function.Arguments)
					acc.args.WriteString(arg)
					emit(map[string]interface{}{
						"type": "response.function_call_arguments.delta", "item_id": acc.itemID,
						"output_index": acc.outputIndex, "delta": arg,
					})
				}
			}
		}
	}

	ensureCreated("")
	// No text and no tool calls: emit a well-formed empty assistant message.
	if !msgOpened && len(toolOrder) == 0 {
		ensureMsgOpen()
	}
	ordered := make([]interface{}, nextOutputIndex)
	if msgOpened {
		finalText := textBuf.String()
		emit(map[string]interface{}{
			"type": "response.output_text.done", "item_id": msgItemID,
			"output_index": msgOutputIndex, "content_index": 0, "text": finalText,
		})
		emit(map[string]interface{}{
			"type": "response.content_part.done", "item_id": msgItemID, "output_index": msgOutputIndex,
			"content_index": 0, "part": map[string]interface{}{"type": "output_text", "text": finalText},
		})
		msgItem := map[string]interface{}{"type": "message", "id": msgItemID, "role": "assistant", "status": "completed",
			"content": []interface{}{map[string]interface{}{"type": "output_text", "text": finalText}}}
		emit(map[string]interface{}{"type": "response.output_item.done", "output_index": msgOutputIndex, "item": msgItem})
		ordered[msgOutputIndex] = msgItem
	}
	for _, idx := range toolOrder {
		acc := tools[idx]
		args := acc.args.String()
		emit(map[string]interface{}{
			"type": "response.function_call_arguments.done", "item_id": acc.itemID,
			"output_index": acc.outputIndex, "arguments": args,
		})
		fcItem := map[string]interface{}{"type": "function_call", "id": acc.itemID, "call_id": acc.callID, "name": acc.name, "arguments": args, "status": "completed"}
		emit(map[string]interface{}{"type": "response.output_item.done", "output_index": acc.outputIndex, "item": fcItem})
		ordered[acc.outputIndex] = fcItem
	}
	output := make([]interface{}, 0, nextOutputIndex)
	for _, it := range ordered {
		if it != nil {
			output = append(output, it)
		}
	}
	resp := map[string]interface{}{"id": respID, "object": "response", "status": "completed", "model": model, "output": output}
	if usage != nil {
		resp["usage"] = prompt.ChatUsageToResponses(usage)
	}
	emit(map[string]interface{}{"type": "response.completed", "response": resp})
	return usage
}

// chatStreamToAnthropicSSE rewrites a Chat Completions SSE stream into an Anthropic
// Messages SSE stream (message_start → content_block_* → message_delta → message_stop).
func chatStreamToAnthropicSSE(w http.ResponseWriter, body io.Reader, model string, scrubber *streamrewrite.Matcher) map[string]interface{} {
	flusher, _ := w.(http.Flusher)
	emit := func(event string, payload map[string]interface{}) {
		payload["type"] = event
		b, _ := json.Marshal(payload)
		_, _ = io.WriteString(w, "event: "+event+"\n")
		_, _ = w.Write([]byte("data: "))
		_, _ = w.Write(b)
		_, _ = w.Write([]byte("\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	msgID := "msg_stream"
	started := false
	nextBlock := 0
	openBlock := -1 // currently-open content block index, -1 if none
	textBlock := -1
	type toolBlock struct {
		blockIndex int
		id         string
		name       string
	}
	toolBlocks := map[int]*toolBlock{}
	finish := ""
	var usage map[string]interface{}

	ensureStarted := func(id string) {
		if started {
			return
		}
		if id != "" {
			msgID = "msg_" + id
		}
		emit("message_start", map[string]interface{}{"message": map[string]interface{}{
			"id": msgID, "type": "message", "role": "assistant", "model": model,
			"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
		}})
		started = true
	}
	closeOpen := func() {
		if openBlock >= 0 {
			emit("content_block_stop", map[string]interface{}{"index": openBlock})
			openBlock = -1
		}
	}

	sc := newChatSSEScanner(body)
	for sc.Scan() {
		data, ok := chatChunkData(sc.Text())
		if !ok {
			continue
		}
		var c chatChunk
		if json.Unmarshal([]byte(data), &c) != nil {
			continue
		}
		ensureStarted(c.ID)
		if u := usageMap(c); u != nil {
			usage = u
		}
		for _, choice := range c.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				if textBlock < 0 {
					closeOpen()
					textBlock = nextBlock
					nextBlock++
					emit("content_block_start", map[string]interface{}{"index": textBlock, "content_block": map[string]interface{}{"type": "text", "text": ""}})
					openBlock = textBlock
				} else if openBlock != textBlock {
					closeOpen()
					openBlock = textBlock
					// Re-open is not valid in Anthropic's protocol; in practice text is
					// contiguous, so this path is defensive only.
				}
				txt := scrubber.ReplaceString(*choice.Delta.Content)
				emit("content_block_delta", map[string]interface{}{"index": textBlock, "delta": map[string]interface{}{"type": "text_delta", "text": txt}})
			}
			for _, tc := range choice.Delta.ToolCalls {
				tb := toolBlocks[tc.Index]
				if tb == nil {
					closeOpen()
					tb = &toolBlock{blockIndex: nextBlock}
					nextBlock++
					toolBlocks[tc.Index] = tb
					if tc.ID != "" {
						tb.id = tc.ID
					}
					if tc.Function.Name != "" {
						tb.name = tc.Function.Name
					}
					emit("content_block_start", map[string]interface{}{"index": tb.blockIndex, "content_block": map[string]interface{}{"type": "tool_use", "id": tb.id, "name": tb.name, "input": map[string]interface{}{}}})
					openBlock = tb.blockIndex
				} else if openBlock != tb.blockIndex {
					closeOpen()
					openBlock = tb.blockIndex
				}
				if tc.Function.Arguments != "" {
					arg := scrubber.ReplaceString(tc.Function.Arguments)
					emit("content_block_delta", map[string]interface{}{"index": tb.blockIndex, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": arg}})
				}
			}
		}
	}

	ensureStarted("")
	closeOpen()
	hasTools := len(toolBlocks) > 0
	delta := map[string]interface{}{"stop_reason": prompt.FinishToStopReason(finish, hasTools), "stop_sequence": nil}
	msgDelta := map[string]interface{}{"delta": delta}
	if usage != nil {
		msgUsage := map[string]interface{}{"output_tokens": usage["completion_tokens"]}
		if cached := prompt.ChatUsageToAnthropic(usage)["cache_read_input_tokens"]; cached != nil {
			msgUsage["cache_read_input_tokens"] = cached
		}
		msgDelta["usage"] = msgUsage
	}
	emit("message_delta", msgDelta)
	emit("message_stop", map[string]interface{}{})
	return usage
}
