package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"codex-account-pool/internal/bodysource"
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
	arguments  *streamAccumulator
	started    bool
	ready      bool
	closed     bool
	fallback   string
}

type responsesAnthropicReasoningStream struct {
	blockIndex int
	item       map[string]interface{}
	text       *streamAccumulator
	started    bool
	closed     bool
}

type responsesAnthropicWebSearchStream struct {
	id          string
	outputIndex string
	query       string
	results     []interface{}
	ready       bool
	closed      bool
}

// responsesStreamToAnthropicSSE is the native Codex Responses -> Claude Messages
// stream bridge. Unlike the old Responses -> Chat -> Messages chain, it preserves
// encrypted reasoning, parallel tool identity, complete tool arguments, cache usage,
// incomplete status, and upstream error events.
func responsesStreamToAnthropicSSE(w http.ResponseWriter, body io.Reader, requestedModel string, toolNames map[string]string, inheritModelTools map[string]bool, scrubber *streamrewrite.Matcher) map[string]interface{} {
	return responsesStreamToAnthropicSSEWithOptions(context.Background(), w, body, requestedModel, toolNames, inheritModelTools, scrubber, bodysource.CaptureOptions{})
}

func responsesStreamToAnthropicSSEWithOptions(ctx context.Context, w http.ResponseWriter, body io.Reader, requestedModel string, toolNames map[string]string, inheritModelTools map[string]bool, scrubber *streamrewrite.Matcher, options bodysource.CaptureOptions) map[string]interface{} {
	return responsesStreamToAnthropicSSEWithOptionsAndEstimate(ctx, w, body, requestedModel, toolNames, inheritModelTools, scrubber, options, 1)
}

// responsesStreamToAnthropicSSEWithOptionsAndEstimate starts an Anthropic stream
// before Responses has terminal metering. The estimate is intentionally provisional:
// response.completed remains authoritative and is repeated in message_delta.
func responsesStreamToAnthropicSSEWithOptionsAndEstimate(ctx context.Context, w http.ResponseWriter, body io.Reader, requestedModel string, toolNames map[string]string, inheritModelTools map[string]bool, scrubber *streamrewrite.Matcher, options bodysource.CaptureOptions, estimatedInputTokens int64) map[string]interface{} {
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
	webSearchByKey := map[string]*responsesAnthropicWebSearchStream{}
	webSearches := make([]*responsesAnthropicWebSearchStream, 0, 2)
	webSearchRequests := 0
	nextToolToFlush := 0
	// Claude Code treats a completed tool_use block as executable immediately.
	// Keep client-managed calls behind the native Responses terminal boundary so
	// every call from one assistant turn is delivered as one complete batch.
	// Otherwise a fast first result can race later calls from the same turn.
	toolTurnFinalized := false
	bridgePlan := prompt.NewResponsesToolBridgePlan()
	var accumulationErr error
	defer func() {
		for _, state := range tools {
			_ = state.arguments.Close()
		}
		for _, state := range reasoning {
			_ = state.text.Close()
		}
	}()

	ensureStarted := func() {
		if started {
			return
		}
		provisionalInputTokens := responsesProvisionalInputTokens(estimatedInputTokens)
		startUsage := map[string]interface{}{"input_tokens": provisionalInputTokens, "output_tokens": int64(0)}
		if usage != nil {
			for key, value := range usage {
				if key != "output_tokens" {
					startUsage[key] = value
				}
			}
		}
		// Some Responses transports emit a created/in-progress usage skeleton with
		// zero input tokens. It is not final metering, so keep the non-zero
		// provisional value until response.completed supplies the authority.
		if anthropicStreamUsageInt64(startUsage["input_tokens"]) <= 0 {
			startUsage["input_tokens"] = provisionalInputTokens
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
		emit("content_block_start", map[string]interface{}{
			"index": textBlock, "content_block": map[string]interface{}{"type": "text", "text": ""},
		})
		return textBlock
	}

	lookupToolItem := func(event, item map[string]interface{}) *responsesAnthropicToolStream {
		hasIdentity := false
		for _, value := range []interface{}{
			mapValue(event, "item_id"), mapValue(item, "id"),
			mapValue(event, "call_id"), mapValue(item, "call_id"),
		} {
			if text := streamString(value); text != "" {
				hasIdentity = true
				if state := toolsByKey["id:"+text]; state != nil {
					return state
				}
			}
		}
		for _, value := range []interface{}{mapValue(event, "output_index"), mapValue(item, "output_index")} {
			if index, ok := jsonNumberString(value); ok {
				hasIdentity = true
				if state := toolsByKey["output:"+index]; state != nil {
					return state
				}
			}
		}
		if !hasIdentity && len(tools) == 1 {
			return tools[0]
		}
		return nil
	}
	lookupTool := func(event map[string]interface{}) *responsesAnthropicToolStream {
		return lookupToolItem(event, nil)
	}
	hydrateTool := func(state *responsesAnthropicToolStream, event, item map[string]interface{}, kind prompt.ResponsesToolKind) {
		if state == nil {
			return
		}
		state.kind = kind
		itemID := streamString(firstNonEmptyInterface(mapValue(item, "id"), mapValue(event, "item_id")))
		callID := streamString(firstNonEmptyInterface(mapValue(item, "call_id"), mapValue(event, "call_id")))
		if itemID != "" {
			state.itemID = itemID
			toolsByKey["id:"+itemID] = state
		}
		if callID != "" {
			state.id = callID
			toolsByKey["id:"+callID] = state
		} else if state.id == "" && itemID != "" {
			state.id = itemID
		}
		if name := streamString(mapValue(item, "name")); name != "" {
			state.name = name
		}
		if namespace := streamString(mapValue(item, "namespace")); namespace != "" {
			state.namespace = namespace
		}
		if kind == prompt.ResponsesToolSearch {
			state.name = "tool_search"
		}
		for _, value := range []interface{}{mapValue(event, "output_index"), mapValue(item, "output_index")} {
			if index, ok := jsonNumberString(value); ok {
				toolsByKey["output:"+index] = state
			}
		}
	}
	startTool := func(state *responsesAnthropicToolStream) {
		if state == nil || state.started {
			return
		}
		closeText()
		ensureStarted()
		if state.blockIndex < 0 {
			state.blockIndex = nextBlock
			nextBlock++
		}
		state.started = true
		hasTools = true
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
		arguments := fallback
		if arguments == "" {
			var err error
			arguments, err = state.arguments.String()
			if err != nil {
				accumulationErr = err
				return
			}
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
	var flushReadyTools func()
	flushReadyTools = func() {
		// response.output_item.done means an individual call is complete, not
		// that the assistant turn is complete.  Do not let the Messages client
		// execute a subset of a parallel tool turn while Responses is still
		// emitting its remaining items.
		if !toolTurnFinalized {
			return
		}
		// A Responses reasoning item can span multiple frames. Keep every
		// Anthropic content block strictly serial by waiting until all reasoning
		// items observed so far have closed before opening a tool_use block.
		for _, state := range reasoning {
			if !state.closed {
				return
			}
		}
		for nextToolToFlush < len(tools) {
			state := tools[nextToolToFlush]
			if state.closed {
				nextToolToFlush++
				continue
			}
			if !state.ready {
				return
			}
			// Some Codex streams only reveal the stable name/call ID on
			// output_item.done or response.output. Emitting an empty Anthropic
			// tool_use would make Claude Code unable to route the result.
			if state.name == "" || state.id == "" {
				return
			}
			flushToolArguments(state, state.fallback)
			if accumulationErr != nil {
				return
			}
			nextToolToFlush++
		}
	}
	markToolReady := func(state *responsesAnthropicToolStream, fallback string) {
		if state == nil || state.closed {
			return
		}
		if fallback != "" {
			state.fallback = fallback
		}
		state.ready = true
		flushReadyTools()
	}
	registerTool := func(event, item map[string]interface{}, kind prompt.ResponsesToolKind) *responsesAnthropicToolStream {
		if state := lookupToolItem(event, item); state != nil {
			hydrateTool(state, event, item, kind)
			return state
		}
		state := &responsesAnthropicToolStream{
			blockIndex: -1,
			kind:       kind,
			arguments:  newStreamAccumulator(ctx, options, "codex-pool-responses-tool-args-*"),
		}
		tools = append(tools, state)
		hydrateTool(state, event, item, kind)
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
		state := &responsesAnthropicReasoningStream{
			blockIndex: -1, item: item,
			text: newStreamAccumulator(ctx, options, "codex-pool-responses-reasoning-*"),
		}
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
	var flushReadyWebSearches func()
	flushDeferredContent := func() {
		if flushReadyWebSearches != nil {
			flushReadyWebSearches()
		}
		flushReadyTools()
	}
	startReasoning := func(state *responsesAnthropicReasoningStream) {
		if state == nil || state.started || state.closed {
			return
		}
		closeText()
		ensureStarted()
		if state.blockIndex < 0 {
			state.blockIndex = nextBlock
			nextBlock++
		}
		state.started = true
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
			flushDeferredContent()
			return
		}
		if !state.started && visible == "" && signature != "" {
			closeText()
			ensureStarted()
			if state.blockIndex < 0 {
				state.blockIndex = nextBlock
				nextBlock++
			}
			state.started = true
			emit("content_block_start", map[string]interface{}{
				"index": state.blockIndex,
				"content_block": map[string]interface{}{
					"type": "redacted_thinking", "data": signature,
				},
			})
			emit("content_block_stop", map[string]interface{}{"index": state.blockIndex})
			state.closed = true
			flushDeferredContent()
			return
		}
		startReasoning(state)
		seen, err := state.text.String()
		if err != nil {
			accumulationErr = err
			return
		}
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
			}
		}
		if signature != "" {
			emit("content_block_delta", map[string]interface{}{
				"index": state.blockIndex, "delta": map[string]interface{}{"type": "signature_delta", "signature": signature},
			})
		}
		emit("content_block_stop", map[string]interface{}{"index": state.blockIndex})
		state.closed = true
		flushDeferredContent()
	}

	lookupWebSearch := func(event, item map[string]interface{}) *responsesAnthropicWebSearchStream {
		hasIdentity := false
		for _, value := range []interface{}{
			mapValue(event, "item_id"), mapValue(item, "id"),
			mapValue(event, "call_id"), mapValue(item, "call_id"),
		} {
			if id := streamString(value); id != "" {
				hasIdentity = true
				if state := webSearchByKey["id:"+id]; state != nil {
					return state
				}
			}
		}
		for _, value := range []interface{}{mapValue(event, "output_index"), mapValue(item, "output_index")} {
			if index, ok := jsonNumberString(value); ok {
				hasIdentity = true
				if state := webSearchByKey["output:"+index]; state != nil {
					return state
				}
			}
		}
		if !hasIdentity && len(webSearches) == 1 {
			return webSearches[0]
		}
		return nil
	}
	hydrateWebSearch := func(state *responsesAnthropicWebSearchStream, event, item map[string]interface{}) {
		if state == nil {
			return
		}
		for _, value := range []interface{}{
			mapValue(item, "id"), mapValue(event, "item_id"),
			mapValue(item, "call_id"), mapValue(event, "call_id"),
		} {
			if id := streamString(value); id != "" {
				if state.id == "" {
					state.id = id
				}
				webSearchByKey["id:"+id] = state
			}
		}
		for _, value := range []interface{}{mapValue(event, "output_index"), mapValue(item, "output_index")} {
			if index, ok := jsonNumberString(value); ok {
				if state.outputIndex == "" {
					state.outputIndex = index
				}
				webSearchByKey["output:"+index] = state
			}
		}
		if query := prompt.ResponsesWebSearchQuery(item); query != "" {
			state.query = query
		}
		if results := prompt.ResponsesWebSearchResults(item); len(results) > 0 {
			state.results = results
		}
		if state.query != "" || len(state.results) > 0 {
			state.ready = true
		}
	}
	registerWebSearch := func(event, item map[string]interface{}) *responsesAnthropicWebSearchStream {
		if state := lookupWebSearch(event, item); state != nil {
			hydrateWebSearch(state, event, item)
			return state
		}
		state := &responsesAnthropicWebSearchStream{}
		webSearches = append(webSearches, state)
		hydrateWebSearch(state, event, item)
		return state
	}
	emitWebSearch := func(state *responsesAnthropicWebSearchStream) {
		if state == nil || state.closed || !state.ready {
			return
		}
		closeText()
		ensureStarted()
		id := state.id
		if id == "" {
			id = "ws_stream_" + state.outputIndex
			if state.outputIndex == "" {
				id = "ws_stream_" + strconv.Itoa(webSearchRequests+1)
			}
		}
		toolUseID := prompt.ResponsesWebSearchToolUseID(id)
		useIndex := nextBlock
		nextBlock++
		emit("content_block_start", map[string]interface{}{
			"index": useIndex,
			"content_block": map[string]interface{}{
				"type": "server_tool_use", "id": toolUseID, "name": "web_search", "input": map[string]interface{}{},
			},
		})
		if state.query != "" {
			partial, _ := json.Marshal(map[string]interface{}{"query": state.query})
			emit("content_block_delta", map[string]interface{}{
				"index": useIndex,
				"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": scrubber.ReplaceString(string(partial))},
			})
		}
		emit("content_block_stop", map[string]interface{}{"index": useIndex})

		resultIndex := nextBlock
		nextBlock++
		emit("content_block_start", map[string]interface{}{
			"index": resultIndex,
			"content_block": map[string]interface{}{
				"type": "web_search_tool_result", "tool_use_id": toolUseID, "content": state.results,
			},
		})
		emit("content_block_stop", map[string]interface{}{"index": resultIndex})
		state.closed = true
		webSearchRequests++
	}
	flushReadyWebSearches = func() {
		for _, state := range reasoning {
			if !state.closed {
				return
			}
		}
		for _, state := range webSearches {
			emitWebSearch(state)
		}
	}

	toolItemArguments := func(kind prompt.ResponsesToolKind, item map[string]interface{}) string {
		var value interface{}
		if kind == prompt.ResponsesToolCustom {
			value = mapValue(item, "input")
		} else {
			value = mapValue(item, "arguments")
		}
		if text, ok := value.(string); ok {
			return text
		}
		if value == nil {
			return ""
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
	hydrateTerminalTools := func(response map[string]interface{}) {
		output, _ := mapValue(response, "output").([]interface{})
		for index, rawItem := range output {
			item, _ := rawItem.(map[string]interface{})
			if item == nil {
				continue
			}
			var kind prompt.ResponsesToolKind
			switch streamString(mapValue(item, "type")) {
			case "function_call":
				kind = prompt.ResponsesToolFunction
			case "custom_tool_call":
				kind = prompt.ResponsesToolCustom
			case "tool_search_call":
				execution := strings.TrimSpace(streamString(mapValue(item, "execution")))
				if execution != "" && !strings.EqualFold(execution, "client") {
					continue
				}
				kind = prompt.ResponsesToolSearch
			default:
				continue
			}
			outputIndex := interface{}(index)
			if _, ok := jsonNumberString(mapValue(item, "output_index")); ok {
				outputIndex = mapValue(item, "output_index")
			}
			event := map[string]interface{}{"output_index": outputIndex}
			state := lookupToolItem(event, item)
			if state == nil {
				state = registerTool(event, item, kind)
			} else {
				hydrateTool(state, event, item, kind)
			}
			markToolReady(state, toolItemArguments(kind, item))
		}
	}
	hydrateTerminalWebSearches := func(response map[string]interface{}) {
		output, _ := mapValue(response, "output").([]interface{})
		for index, rawItem := range output {
			item, _ := rawItem.(map[string]interface{})
			if item == nil || streamString(mapValue(item, "type")) != "web_search_call" {
				continue
			}
			outputIndex := interface{}(index)
			if _, ok := jsonNumberString(mapValue(item, "output_index")); ok {
				outputIndex = mapValue(item, "output_index")
			}
			state := registerWebSearch(map[string]interface{}{"output_index": outputIndex}, item)
			hydrateWebSearch(state, nil, item)
		}
		flushReadyWebSearches()
	}

	emitTerminal := func(response map[string]interface{}, eventType string) {
		if terminal {
			return
		}
		terminal = true
		if response != nil {
			if terminalError := responsesStreamError(response); terminalError != "" {
				emit("error", map[string]interface{}{"error": map[string]interface{}{
					"type": "api_error", "code": "server_error", "message": publicRetryMessage,
				}})
				return
			}
			if response["usage"] != nil {
				usage = prompt.ResponsesUsageToAnthropic(response["usage"])
			}
			hydrateTerminalTools(response)
			hydrateTerminalWebSearches(response)
		}
		for _, state := range reasoning {
			finishReasoning(state, state.item)
			if accumulationErr != nil {
				return
			}
		}
		closeText()
		flushReadyWebSearches()
		for _, state := range webSearches {
			if !state.ready {
				state.closed = true
			}
		}
		// All terminal output has now been hydrated and every reasoning block
		// has been closed.  Releasing client tools here gives Claude Code one
		// atomic, fully indexed tool_use turn.
		toolTurnFinalized = true
		for _, state := range tools {
			if state.name == "" || state.id == "" {
				// A terminal response that still cannot identify a pending call
				// cannot produce a routable Claude tool_use. Skip only that
				// unresolved placeholder so later complete calls still flow.
				state.ready = true
				state.closed = true
				continue
			}
			markToolReady(state, "")
			if accumulationErr != nil {
				return
			}
		}
		flushReadyTools()
		if accumulationErr != nil {
			return
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
		if usage == nil {
			usage = map[string]interface{}{"input_tokens": responsesProvisionalInputTokens(estimatedInputTokens), "output_tokens": int64(0)}
		}
		if webSearchRequests > 0 {
			usage["server_tool_use"] = map[string]interface{}{"web_search_requests": webSearchRequests}
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
				execution := strings.TrimSpace(streamString(mapValue(item, "execution")))
				if execution == "" || strings.EqualFold(execution, "client") {
					registerTool(event, item, prompt.ResponsesToolSearch)
				}
			case "web_search_call":
				registerWebSearch(event, item)
			case "reasoning":
				getReasoning(event, item)
			}
		case "response.web_search_call.searching", "response.web_search_call.in_progress", "response.web_search_call.completed":
			registerWebSearch(event, nil)
		case "response.function_call_arguments.delta":
			if state := lookupTool(event); state != nil {
				accumulationErr = state.arguments.WriteString(streamString(event["delta"]))
			}
		case "response.function_call_arguments.done":
			item, _ := event["item"].(map[string]interface{})
			state := lookupToolItem(event, item)
			if state != nil {
				hydrateTool(state, event, item, state.kind)
			}
			fallback := streamString(firstNonEmptyInterface(event["arguments"], mapValueFromNested(event, "item", "arguments")))
			markToolReady(state, fallback)
		case "response.custom_tool_call_input.delta":
			if state := lookupTool(event); state != nil && state.kind == prompt.ResponsesToolCustom {
				accumulationErr = state.arguments.WriteString(streamString(event["delta"]))
			}
		case "response.custom_tool_call_input.done":
			item, _ := event["item"].(map[string]interface{})
			state := lookupToolItem(event, item)
			if state != nil {
				hydrateTool(state, event, item, state.kind)
			}
			fallback := streamString(firstNonEmptyInterface(event["input"], mapValueFromNested(event, "item", "input")))
			markToolReady(state, fallback)
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.reasoning.delta":
			state := getReasoning(event, nil)
			delta := streamString(event["delta"])
			if delta != "" {
				startReasoning(state)
				if err := state.text.WriteString(delta); err != nil {
					accumulationErr = err
					return
				}
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
				state := lookupToolItem(event, item)
				if state == nil {
					state = registerTool(event, item, prompt.ResponsesToolFunction)
				} else {
					hydrateTool(state, event, item, prompt.ResponsesToolFunction)
				}
				markToolReady(state, streamString(mapValue(item, "arguments")))
			case "custom_tool_call":
				state := lookupToolItem(event, item)
				if state == nil {
					state = registerTool(event, item, prompt.ResponsesToolCustom)
				} else {
					hydrateTool(state, event, item, prompt.ResponsesToolCustom)
				}
				markToolReady(state, streamString(mapValue(item, "input")))
			case "tool_search_call":
				execution := strings.TrimSpace(streamString(mapValue(item, "execution")))
				if execution != "" && !strings.EqualFold(execution, "client") {
					break
				}
				state := lookupToolItem(event, item)
				if state == nil {
					state = registerTool(event, item, prompt.ResponsesToolSearch)
				} else {
					hydrateTool(state, event, item, prompt.ResponsesToolSearch)
				}
				arguments, _ := json.Marshal(mapValue(item, "arguments"))
				markToolReady(state, string(arguments))
			case "web_search_call":
				state := registerWebSearch(event, item)
				hydrateWebSearch(state, event, item)
				flushReadyWebSearches()
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
			emit("error", map[string]interface{}{"error": map[string]interface{}{
				"type": "api_error", "code": "server_error", "message": publicRetryMessage,
			}})
		}
	}

	streamErr := forEachSSEFrameWithOptions(ctx, body, options, func(frame []byte) error {
		process(frame)
		return accumulationErr
	})
	if streamErr != nil || accumulationErr != nil || !terminal {
		emit("error", map[string]interface{}{"error": map[string]interface{}{
			"type": "api_error", "code": "server_error", "message": publicRetryMessage,
		}})
	}
	return usage
}

func responsesProvisionalInputTokens(estimated int64) int64 {
	if estimated > 0 {
		return estimated
	}
	return 1
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
