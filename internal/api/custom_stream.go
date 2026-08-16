package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/leakfilter"
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
type chatFunctionCallDelta struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatToolCallDelta struct {
	Index    int                   `json:"index"`
	ID       string                `json:"id"`
	Function chatFunctionCallDelta `json:"function"`
}

type chatChunkDelta struct {
	Content          *string                `json:"content"`
	ReasoningContent *string                `json:"reasoning_content"`
	Reasoning        *string                `json:"reasoning"`
	ToolCalls        []chatToolCallDelta    `json:"tool_calls"`
	FunctionCall     *chatFunctionCallDelta `json:"function_call"`
}

type chatChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta        chatChunkDelta `json:"delta"`
		FinishReason *string        `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens          int64 `json:"prompt_tokens"`
		CompletionTokens      int64 `json:"completion_tokens"`
		TotalTokens           int64 `json:"total_tokens"`
		PromptCacheHitTokens  int64 `json:"prompt_cache_hit_tokens"`
		PromptCacheMissTokens int64 `json:"prompt_cache_miss_tokens"`
		PromptTokensDetails   *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func normalizedChatToolCallDeltas(delta chatChunkDelta) []chatToolCallDelta {
	if len(delta.ToolCalls) > 0 || delta.FunctionCall == nil {
		return delta.ToolCalls
	}
	return []chatToolCallDelta{{
		Index: 0, ID: "call_legacy_function", Function: *delta.FunctionCall,
	}}
}

// chatReasoningDelta accepts the official DeepSeek spelling first and the
// OpenRouter-compatible alias second. A present official empty delta still wins.
func chatReasoningDelta(delta chatChunkDelta) string {
	if delta.ReasoningContent != nil {
		return *delta.ReasoningContent
	}
	if delta.Reasoning != nil {
		return *delta.Reasoning
	}
	return ""
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

type customSSEProtocol int

const (
	customSSEChatCompletions customSSEProtocol = iota
	customSSEResponses
	customSSEAnthropicMessages
	maxTrackedCustomSSEFrameBytes = 16 << 20
)

// customSSETerminalTracker observes raw SSE frames without delaying passthrough.
// It retains only the current frame and accepts a stream as complete only when the
// upstream protocol's real terminal event has arrived.
type customSSETerminalTracker struct {
	protocol customSSEProtocol
	pending  []byte
	terminal bool
	success  bool
}

func (t *customSSETerminalTracker) Write(p []byte) (int, error) {
	t.pending = append(t.pending, p...)
	for {
		boundary, separatorLen := sseFrameBoundary(t.pending)
		if boundary < 0 {
			break
		}
		t.observe(t.pending[:boundary])
		t.pending = append(t.pending[:0], t.pending[boundary+separatorLen:]...)
	}
	if len(t.pending) > maxTrackedCustomSSEFrameBytes {
		// An oversized frame cannot be validated safely. Keep passthrough streaming,
		// but require a later well-formed terminal frame before treating it as complete.
		t.pending = t.pending[:0]
	}
	return len(p), nil
}

func (t *customSSETerminalTracker) finish() {
	if len(t.pending) > 0 {
		t.observe(t.pending)
		t.pending = nil
	}
}

func (t *customSSETerminalTracker) observe(frame []byte) {
	if t.terminal {
		return
	}
	eventName, data := sseFrameEventData(frame)
	trimmed := strings.TrimSpace(string(data))
	if t.protocol == customSSEChatCompletions && trimmed == "[DONE]" {
		t.terminal = true
		t.success = true
		return
	}
	if trimmed == "" || trimmed == "[DONE]" {
		return
	}
	var envelope map[string]interface{}
	if json.Unmarshal(data, &envelope) != nil {
		return
	}
	eventType, _ := envelope["type"].(string)
	if eventType == "" {
		eventType = eventName
	}
	switch t.protocol {
	case customSSEChatCompletions:
		_, hasError := envelope["error"]
		if hasError || eventType == "error" {
			t.terminal = true
		}
	case customSSEResponses:
		switch eventType {
		case "response.completed":
			t.terminal = true
			t.success = true
		case "response.incomplete", "response.failed", "response.error", "error":
			t.terminal = true
		}
	case customSSEAnthropicMessages:
		switch eventType {
		case "message_stop":
			t.terminal = true
			t.success = true
		case "error":
			t.terminal = true
		}
	}
}

func streamCopyRewriteValidated(w http.ResponseWriter, body io.Reader, scrubber *streamrewrite.Matcher, protocol customSSEProtocol, privacyOptions ...bool) (bool, error) {
	tracker := &customSSETerminalTracker{protocol: protocol}
	source := io.TeeReader(body, tracker)
	privacy := len(privacyOptions) > 0 && privacyOptions[0]
	var err error
	if privacy {
		batch := newAdaptiveSSEBatch(w)
		err = forEachSSEFrame(source, func(frame []byte) error {
			out := sanitizeCustomSSEFrame(frame, protocol)
			if len(out) == 0 {
				return nil
			}
			if scrubber != nil && !scrubber.Empty() {
				out = scrubber.ReplaceAll(out)
			}
			return batch.append(out)
		})
		if err != nil {
			batch.abort()
		} else {
			err = errors.Join(err, batch.close())
		}
	} else {
		err = streamCopyRewrite(w, source, scrubber)
	}
	tracker.finish()
	if tracker.terminal {
		return true, err
	}
	writeCustomSSEInterrupted(w, protocol, err != nil)
	return false, err
}

// sanitizeCustomSSEFrame enforces the leak-scrub contract for native custom
// provider streams. Informational quota/safety fields are removed, while an error
// remains a protocol-valid terminal carrying no vendor, account, quota, or risk text.
func sanitizeCustomSSEFrame(frame []byte, protocol customSSEProtocol) []byte {
	out := frame
	switch protocol {
	case customSSEResponses:
		out = leakfilter.NewSSEFilter("codex", nil).ProcessFrameForRelay(out)
	case customSSEAnthropicMessages:
		out = leakfilter.NewSSEFilter("claude", nil).ProcessFrameForRelay(out)
	}
	if len(out) == 0 {
		return nil
	}

	eventName, data := sseFrameEventData(out)
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "[DONE]" {
		return out
	}
	var envelope map[string]interface{}
	if json.Unmarshal(data, &envelope) != nil {
		return out
	}
	eventType := streamString(envelope["type"])
	if eventType == "" {
		eventType = eventName
	}
	isError := false
	switch protocol {
	case customSSEChatCompletions:
		_, hasError := envelope["error"]
		isError = hasError || eventType == "error"
	case customSSEResponses:
		isError = eventType == "response.failed" || eventType == "response.error" || eventType == "error"
	case customSSEAnthropicMessages:
		isError = eventType == "error"
	}
	if !isError {
		return out
	}

	streamError := map[string]interface{}{
		"type": "server_error", "code": "server_error", "message": publicRetryMessage,
	}
	var terminalEvent string
	var payload map[string]interface{}
	switch protocol {
	case customSSEResponses:
		terminalEvent = "response.failed"
		payload = map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id": "resp_custom_stream_error", "object": "response", "status": "failed",
				"output": []interface{}{}, "error": streamError,
			},
		}
	case customSSEAnthropicMessages:
		terminalEvent = "error"
		streamError["type"] = "api_error"
		payload = map[string]interface{}{"type": "error", "error": streamError}
	default:
		payload = map[string]interface{}{"error": streamError}
	}
	encoded, _ := json.Marshal(payload)
	var rebuilt strings.Builder
	if terminalEvent != "" {
		rebuilt.WriteString("event: ")
		rebuilt.WriteString(terminalEvent)
		rebuilt.WriteByte('\n')
	}
	rebuilt.WriteString("data: ")
	rebuilt.Write(encoded)
	rebuilt.WriteString("\n\n")
	return []byte(rebuilt.String())
}

func writeCustomSSEInterrupted(w http.ResponseWriter, protocol customSSEProtocol, readFailed bool) {
	_ = readFailed
	streamError := map[string]interface{}{
		"type": "server_error", "code": "server_error", "message": publicRetryMessage,
	}
	var eventName string
	var payload map[string]interface{}
	switch protocol {
	case customSSEResponses:
		eventName = "response.failed"
		payload = map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id": "resp_custom_stream_error", "object": "response", "status": "failed",
				"output": []interface{}{}, "error": streamError,
			},
		}
	case customSSEAnthropicMessages:
		eventName = "error"
		payload = map[string]interface{}{"type": "error", "error": streamError}
	default:
		payload = map[string]interface{}{"error": streamError}
	}
	encoded, _ := json.Marshal(payload)
	if eventName != "" {
		_, _ = io.WriteString(w, "event: "+eventName+"\n")
	}
	_, _ = io.WriteString(w, "data: ")
	_, _ = w.Write(encoded)
	_, _ = io.WriteString(w, "\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
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
	cachedTokens := c.Usage.PromptCacheHitTokens
	if cachedTokens == 0 && c.Usage.PromptTokensDetails != nil {
		cachedTokens = c.Usage.PromptTokensDetails.CachedTokens
	}
	if cachedTokens > 0 {
		u["prompt_cache_hit_tokens"] = cachedTokens
	}
	if c.Usage.PromptCacheMissTokens > 0 {
		u["prompt_cache_miss_tokens"] = c.Usage.PromptCacheMissTokens
	}
	return u
}

// chatStreamToResponsesSSE rewrites a Chat Completions SSE stream into a Responses API
// SSE stream (response.created → per-item added/delta/done → response.completed).
func chatStreamToResponsesSSE(w http.ResponseWriter, body io.Reader, model string, scrubber *streamrewrite.Matcher, plans ...*prompt.ResponsesToolBridgePlan) map[string]interface{} {
	return chatStreamToResponsesSSEWithOptions(context.Background(), w, body, model, scrubber, bodysource.CaptureOptions{}, plans...)
}

func chatStreamToResponsesSSEWithOptions(ctx context.Context, w http.ResponseWriter, body io.Reader, model string, scrubber *streamrewrite.Matcher, options bodysource.CaptureOptions, plans ...*prompt.ResponsesToolBridgePlan) map[string]interface{} {
	preserveDeepSeekReasoning := prompt.IsDeepSeekModel(model)
	plan := prompt.NewResponsesToolBridgePlan()
	if len(plans) > 0 && plans[0] != nil {
		plan = plans[0]
	}
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

	var respID, msgItemID, reasoningItemID string
	created := false
	msgOpened := false
	msgOutputIndex := -1
	reasoningOpened := false
	reasoningOutputIndex := -1
	reasoningBuf := newStreamAccumulator(ctx, options, "codex-pool-chat-response-reasoning-*")
	defer reasoningBuf.Close()
	textBuf := newStreamAccumulator(ctx, options, "codex-pool-chat-response-text-*")
	defer textBuf.Close()
	nextOutputIndex := 0

	type toolAcc struct {
		outputIndex int
		itemID      string
		callID      string
		name        string
		identity    prompt.ResponsesToolIdentity
		opened      bool
		args        *streamAccumulator
	}
	tools := map[int]*toolAcc{}
	var toolOrder []int
	var usage map[string]interface{}
	sawDone := false
	var accumulationErr error

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
	ensureReasoningOpen := func() {
		if reasoningOpened {
			return
		}
		reasoningOutputIndex = nextOutputIndex
		nextOutputIndex++
		reasoningItemID = "rs_" + respID
		reasoningOpened = true
		emit(map[string]interface{}{
			"type": "response.output_item.added", "output_index": reasoningOutputIndex,
			"item": map[string]interface{}{"type": "reasoning", "id": reasoningItemID, "summary": []interface{}{}},
		})
	}
	openTool := func(acc *toolAcc) {
		if acc.opened {
			return
		}
		if acc.identity.Name == "" {
			acc.identity = prompt.ResponsesToolIdentity{Kind: prompt.ResponsesToolFunction, Name: acc.name}
		}
		prefix := "fc_"
		item := map[string]interface{}{
			"type": "function_call", "call_id": acc.callID, "name": acc.identity.Name,
			"arguments": "", "status": "in_progress",
		}
		switch acc.identity.Kind {
		case prompt.ResponsesToolCustom:
			prefix = "ctc_"
			item = map[string]interface{}{
				"type": "custom_tool_call", "call_id": acc.callID, "name": acc.identity.Name,
				"input": "", "status": "in_progress",
			}
		case prompt.ResponsesToolSearch:
			prefix = "tsc_"
			item = map[string]interface{}{
				"type": "tool_search_call", "call_id": acc.callID, "execution": "client",
				"arguments": map[string]interface{}{}, "status": "in_progress",
			}
		default:
			if acc.identity.Namespace != "" {
				item["namespace"] = acc.identity.Namespace
			}
		}
		if acc.identity.Kind == prompt.ResponsesToolCustom && acc.identity.Namespace != "" {
			item["namespace"] = acc.identity.Namespace
		}
		acc.itemID = prefix + respID + "_" + strconv.Itoa(acc.outputIndex)
		item["id"] = acc.itemID
		emit(map[string]interface{}{
			"type": "response.output_item.added", "output_index": acc.outputIndex, "item": item,
		})
		acc.opened = true
	}

	sc := newChatSSEScanner(body)
scanLoop:
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "data:") && strings.TrimSpace(line[len("data:"):]) == "[DONE]" {
			sawDone = true
			break
		}
		data, ok := chatChunkData(line)
		if !ok {
			continue
		}
		// A Chat-Completions stream may fail after its HTTP status and headers
		// have already committed. Preserve that terminal error instead of treating
		// its lack of `choices` as an empty successful assistant response.
		var raw map[string]interface{}
		if json.Unmarshal([]byte(data), &raw) == nil {
			if _, failed := raw["error"].(map[string]interface{}); failed {
				ensureCreated("")
				emit(map[string]interface{}{
					"type": "response.failed",
					"response": map[string]interface{}{
						"id": respID, "object": "response", "status": "failed", "model": model,
						"error": map[string]interface{}{
							"type": "server_error", "code": "server_error", "message": publicRetryMessage,
						},
					},
				})
				return usage
			}
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
			if reasoning := chatReasoningDelta(choice.Delta); preserveDeepSeekReasoning && reasoning != "" {
				ensureReasoningOpen()
				reasoning = scrubber.ReplaceString(reasoning)
				if accumulationErr = reasoningBuf.WriteString(reasoning); accumulationErr != nil {
					break scanLoop
				}
				emit(map[string]interface{}{
					"type": "response.reasoning_summary_text.delta", "item_id": reasoningItemID,
					"output_index": reasoningOutputIndex, "delta": reasoning,
				})
			}
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				ensureMsgOpen()
				txt := scrubber.ReplaceString(*choice.Delta.Content)
				if accumulationErr = textBuf.WriteString(txt); accumulationErr != nil {
					break scanLoop
				}
				emit(map[string]interface{}{
					"type": "response.output_text.delta", "item_id": msgItemID,
					"output_index": msgOutputIndex, "content_index": 0, "delta": txt,
				})
			}
			for _, tc := range normalizedChatToolCallDeltas(choice.Delta) {
				acc := tools[tc.Index]
				if acc == nil {
					acc = &toolAcc{outputIndex: nextOutputIndex, args: newStreamAccumulator(ctx, options, "codex-pool-chat-tool-args-*")}
					defer acc.args.Close()
					nextOutputIndex++
					tools[tc.Index] = acc
					toolOrder = append(toolOrder, tc.Index)
				}
				if tc.ID != "" {
					acc.callID = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
					if identity, ok := plan.ResolveChatName(acc.name); ok {
						acc.identity = identity
					} else {
						acc.identity = prompt.ResponsesToolIdentity{Kind: prompt.ResponsesToolFunction, Name: acc.name}
					}
					openTool(acc)
				}
				if tc.Function.Arguments != "" {
					arg := scrubber.ReplaceString(tc.Function.Arguments)
					if accumulationErr = acc.args.WriteString(arg); accumulationErr != nil {
						break scanLoop
					}
					if acc.identity.Kind == prompt.ResponsesToolFunction {
						openTool(acc)
						emit(map[string]interface{}{
							"type": "response.function_call_arguments.delta", "item_id": acc.itemID,
							"output_index": acc.outputIndex, "delta": arg,
						})
					}
				}
			}
		}
	}
	if sc.Err() != nil || accumulationErr != nil || !sawDone {
		ensureCreated("")
		emit(map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id": respID, "object": "response", "status": "failed", "model": model,
				"error": map[string]interface{}{
					"type": "server_error", "code": "server_error", "message": publicRetryMessage,
				},
			},
		})
		return usage
	}

	ensureCreated("")
	// No text and no tool calls: emit a well-formed empty assistant message.
	if !msgOpened && len(toolOrder) == 0 {
		ensureMsgOpen()
	}
	ordered := make([]interface{}, nextOutputIndex)
	if reasoningOpened {
		reasoning, err := reasoningBuf.String()
		if err != nil {
			emit(map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id": respID, "object": "response", "status": "failed", "model": model,
					"error": map[string]interface{}{"type": "server_error", "code": "server_error", "message": publicRetryMessage},
				},
			})
			return usage
		}
		emit(map[string]interface{}{
			"type": "response.reasoning_summary_text.done", "item_id": reasoningItemID,
			"output_index": reasoningOutputIndex, "text": reasoning,
		})
		reasoningItem := prompt.DeepSeekReasoningItem(reasoningItemID, reasoning)
		emit(map[string]interface{}{"type": "response.output_item.done", "output_index": reasoningOutputIndex, "item": reasoningItem})
		ordered[reasoningOutputIndex] = reasoningItem
	}
	if msgOpened {
		finalText, err := textBuf.String()
		if err != nil {
			emit(map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id": respID, "object": "response", "status": "failed", "model": model,
					"error": map[string]interface{}{"type": "server_error", "code": "server_error", "message": publicRetryMessage},
				},
			})
			return usage
		}
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
		openTool(acc)
		args, err := acc.args.String()
		if err != nil {
			emit(map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id": respID, "object": "response", "status": "failed", "model": model,
					"error": map[string]interface{}{"type": "server_error", "code": "server_error", "message": publicRetryMessage},
				},
			})
			return usage
		}
		var completed map[string]interface{}
		switch acc.identity.Kind {
		case prompt.ResponsesToolCustom:
			input := chatCustomToolInput(args)
			if input != "" {
				emit(map[string]interface{}{
					"type": "response.custom_tool_call_input.delta", "item_id": acc.itemID, "call_id": acc.callID,
					"output_index": acc.outputIndex, "delta": input,
				})
			}
			emit(map[string]interface{}{
				"type": "response.custom_tool_call_input.done", "item_id": acc.itemID, "call_id": acc.callID,
				"output_index": acc.outputIndex, "input": input,
			})
			completed = map[string]interface{}{
				"type": "custom_tool_call", "id": acc.itemID, "call_id": acc.callID,
				"name": acc.identity.Name, "input": input, "status": "completed",
			}
			if acc.identity.Namespace != "" {
				completed["namespace"] = acc.identity.Namespace
			}
		case prompt.ResponsesToolSearch:
			completed = map[string]interface{}{
				"type": "tool_search_call", "id": acc.itemID, "call_id": acc.callID,
				"execution": "client", "arguments": decodeJSONAny(args), "status": "completed",
			}
		default:
			emit(map[string]interface{}{
				"type": "response.function_call_arguments.done", "item_id": acc.itemID,
				"output_index": acc.outputIndex, "arguments": args,
			})
			completed = map[string]interface{}{
				"type": "function_call", "id": acc.itemID, "call_id": acc.callID,
				"name": acc.identity.Name, "arguments": args, "status": "completed",
			}
			if acc.identity.Namespace != "" {
				completed["namespace"] = acc.identity.Namespace
			}
		}
		emit(map[string]interface{}{"type": "response.output_item.done", "output_index": acc.outputIndex, "item": completed})
		ordered[acc.outputIndex] = completed
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

func chatCustomToolInput(arguments string) string {
	if object, ok := decodeJSONAny(arguments).(map[string]interface{}); ok {
		if input, ok := object["input"].(string); ok {
			return input
		}
	}
	return arguments
}

func decodeJSONAny(raw string) interface{} {
	if strings.TrimSpace(raw) == "" {
		return map[string]interface{}{}
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var value interface{}
	if err := dec.Decode(&value); err != nil {
		return raw
	}
	return value
}

// chatStreamToAnthropicSSE rewrites a Chat Completions SSE stream into an Anthropic
// Messages SSE stream (message_start → content_block_* → message_delta → message_stop).
func chatStreamToAnthropicSSE(w http.ResponseWriter, body io.Reader, model string, scrubber *streamrewrite.Matcher) map[string]interface{} {
	return chatStreamToAnthropicSSEWithOptions(context.Background(), w, body, model, scrubber, bodysource.CaptureOptions{})
}

func chatStreamToAnthropicSSEWithOptions(ctx context.Context, w http.ResponseWriter, body io.Reader, model string, scrubber *streamrewrite.Matcher, options bodysource.CaptureOptions) map[string]interface{} {
	preserveDeepSeekReasoning := prompt.IsDeepSeekModel(model)
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
	reasoningBlock := -1
	reasoningSigned := false
	reasoningBuf := newStreamAccumulator(ctx, options, "codex-pool-chat-anthropic-reasoning-*")
	defer reasoningBuf.Close()
	textBlock := -1
	type toolBlock struct {
		blockIndex int
		id         string
		name       string
		args       *streamAccumulator
	}
	toolBlocks := map[int]*toolBlock{}
	toolOrder := make([]int, 0, 4)
	finish := ""
	var usage map[string]interface{}
	sawDone := false
	var accumulationErr error

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
			if openBlock == reasoningBlock && !reasoningSigned {
				reasoning, err := reasoningBuf.String()
				if err != nil {
					accumulationErr = err
					return
				}
				emit("content_block_delta", map[string]interface{}{
					"index": reasoningBlock,
					"delta": map[string]interface{}{"type": "signature_delta", "signature": prompt.EncodeDeepSeekReasoningContent(reasoning)},
				})
				reasoningSigned = true
			}
			emit("content_block_stop", map[string]interface{}{"index": openBlock})
			openBlock = -1
		}
	}

	sc := newChatSSEScanner(body)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "data:") && strings.TrimSpace(line[len("data:"):]) == "[DONE]" {
			sawDone = true
			break
		}
		data, ok := chatChunkData(line)
		if !ok {
			continue
		}
		// The process-local Codex bridge can commit an SSE wait heartbeat before the
		// scheduler later reports a terminal error. Preserve that error as a real
		// Anthropic SSE error event instead of interpreting the frame as an empty Chat
		// chunk and ending with a misleading successful message_stop.
		var envelope map[string]interface{}
		if json.Unmarshal([]byte(data), &envelope) == nil {
			if _, ok := envelope["error"].(map[string]interface{}); ok {
				emit("error", map[string]interface{}{"error": map[string]interface{}{
					"type": "api_error", "code": "server_error", "message": publicRetryMessage,
				}})
				return usage
			}
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
			if reasoning := chatReasoningDelta(choice.Delta); preserveDeepSeekReasoning && reasoning != "" {
				if reasoningBlock < 0 {
					closeOpen()
					if accumulationErr != nil {
						break
					}
					reasoningBlock = nextBlock
					nextBlock++
					emit("content_block_start", map[string]interface{}{
						"index":         reasoningBlock,
						"content_block": map[string]interface{}{"type": "thinking", "thinking": ""},
					})
					openBlock = reasoningBlock
				} else if openBlock != reasoningBlock {
					accumulationErr = errors.New("custom chat stream interleaved reasoning after another content block")
					break
				}
				reasoning = scrubber.ReplaceString(reasoning)
				if accumulationErr = reasoningBuf.WriteString(reasoning); accumulationErr != nil {
					break
				}
				emit("content_block_delta", map[string]interface{}{
					"index": reasoningBlock,
					"delta": map[string]interface{}{"type": "thinking_delta", "thinking": reasoning},
				})
			}
			if choice.Delta.Content != nil && *choice.Delta.Content != "" {
				if textBlock < 0 {
					closeOpen()
					if accumulationErr != nil {
						break
					}
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
			for _, tc := range normalizedChatToolCallDeltas(choice.Delta) {
				tb := toolBlocks[tc.Index]
				if tb == nil {
					tb = &toolBlock{blockIndex: -1, args: newStreamAccumulator(ctx, options, "codex-pool-chat-anthropic-tool-args-*")}
					defer tb.args.Close()
					toolBlocks[tc.Index] = tb
					toolOrder = append(toolOrder, tc.Index)
					if tc.ID != "" {
						tb.id = tc.ID
					}
					if tc.Function.Name != "" {
						tb.name = tc.Function.Name
					}
				} else {
					if tc.ID != "" {
						tb.id = tc.ID
					}
					if tc.Function.Name != "" {
						tb.name = tc.Function.Name
					}
				}
				if tc.Function.Arguments != "" {
					arg := scrubber.ReplaceString(tc.Function.Arguments)
					if accumulationErr = tb.args.WriteString(arg); accumulationErr != nil {
						break
					}
				}
			}
			if accumulationErr != nil {
				break
			}
		}
		if accumulationErr != nil {
			break
		}
	}
	if sc.Err() != nil || accumulationErr != nil || !sawDone {
		emit("error", map[string]interface{}{"error": map[string]interface{}{
			"type": "api_error", "code": "server_error", "message": publicRetryMessage,
		}})
		return usage
	}

	ensureStarted("")
	closeOpen()
	if accumulationErr != nil {
		emit("error", map[string]interface{}{"error": map[string]interface{}{
			"type": "api_error", "code": "server_error", "message": publicRetryMessage,
		}})
		return usage
	}
	for _, index := range toolOrder {
		tb := toolBlocks[index]
		tb.blockIndex = nextBlock
		nextBlock++
		if tb.id == "" {
			tb.id = "call_" + strconv.Itoa(index)
		}
		arguments, err := tb.args.String()
		if err != nil {
			emit("error", map[string]interface{}{"error": map[string]interface{}{
				"type": "api_error", "code": "server_error", "message": publicRetryMessage,
			}})
			return usage
		}
		emit("content_block_start", map[string]interface{}{"index": tb.blockIndex, "content_block": map[string]interface{}{
			"type": "tool_use", "id": tb.id, "name": tb.name, "input": map[string]interface{}{},
		}})
		if arguments != "" {
			emit("content_block_delta", map[string]interface{}{"index": tb.blockIndex, "delta": map[string]interface{}{
				"type": "input_json_delta", "partial_json": arguments,
			}})
		}
		emit("content_block_stop", map[string]interface{}{"index": tb.blockIndex})
	}
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
