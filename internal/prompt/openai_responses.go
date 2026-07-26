package prompt

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// openai_responses.go bridges the OpenAI **Responses API** (/v1/responses — what the
// Codex CLI speaks) and OpenAI **Chat Completions** (what a custom provider such as
// DeepSeek speaks). It is the request/response (non-streaming) half; the streaming
// converter lives in package api (ChatCompletionStreamToResponsesSSE) next to the
// other SSE rewriters. These mirror, in reverse, the existing
// ChatCompletionToResponses / ResponsesToChatCompletion in prompt.go.

const (
	LossResponsesHostedToolOmitted        = "responses_hosted_tool_omitted"
	LossResponsesServerToolSearchOmitted  = "responses_server_tool_search_omitted"
	LossResponsesIncludeOmitted           = "responses_include_omitted"
	LossResponsesStructuredToolOutputJSON = "responses_structured_tool_output_json"
	LossResponsesHistoryItemJSON          = "responses_history_item_json"
	LossResponsesToolChoiceDowngraded     = "responses_tool_choice_downgraded"
)

type ResponsesToolKind string

const (
	ResponsesToolFunction ResponsesToolKind = "function"
	ResponsesToolCustom   ResponsesToolKind = "custom"
	ResponsesToolSearch   ResponsesToolKind = "tool_search"
)

// ResponsesToolIdentity is the original stable Responses identity hidden behind a
// Chat-Completions-compatible function name.
type ResponsesToolIdentity struct {
	Kind      ResponsesToolKind
	Namespace string
	Name      string
	Execution string
}

// ResponsesToolBridgePlan is request-scoped conversion state. Its maps are purposely
// unexported so it cannot be serialized into journals or persisted by accident.
type ResponsesToolBridgePlan struct {
	byChatName map[string]ResponsesToolIdentity
	byIdentity map[string]string
}

type ResponsesChatBridgeResult struct {
	Body                []byte
	Plan                *ResponsesToolBridgePlan
	CompatibilityLosses []string
}

func NewResponsesToolBridgePlan() *ResponsesToolBridgePlan {
	return &ResponsesToolBridgePlan{
		byChatName: map[string]ResponsesToolIdentity{},
		byIdentity: map[string]string{},
	}
}

func (p *ResponsesToolBridgePlan) ResolveChatName(name string) (ResponsesToolIdentity, bool) {
	if p == nil {
		return ResponsesToolIdentity{}, false
	}
	identity, ok := p.byChatName[name]
	return identity, ok
}

func (p *ResponsesToolBridgePlan) ChatName(kind ResponsesToolKind, namespace, name string) (string, bool) {
	if p == nil {
		return "", false
	}
	alias, ok := p.byIdentity[responsesToolIdentityKey(ResponsesToolIdentity{Kind: kind, Namespace: namespace, Name: name})]
	return alias, ok
}

// EnsureChatName registers an original Responses tool identity and returns the
// deterministic Chat-compatible function name used for it. It is primarily useful
// to response-only stream adapters that did not see the original request.
func (p *ResponsesToolBridgePlan) EnsureChatName(identity ResponsesToolIdentity) string {
	return p.ensureAlias(identity)
}

func responsesToolIdentityKey(identity ResponsesToolIdentity) string {
	return string(identity.Kind) + "\x00" + identity.Namespace + "\x00" + identity.Name
}

func (p *ResponsesToolBridgePlan) ensureAlias(identity ResponsesToolIdentity) string {
	if p == nil {
		return identity.Name
	}
	key := responsesToolIdentityKey(identity)
	if alias := p.byIdentity[key]; alias != "" {
		return alias
	}
	base := sanitizeChatToolName(identity.Name)
	forceHash := identity.Namespace != ""
	if identity.Namespace != "" {
		base = sanitizeChatToolName(identity.Namespace + "__" + identity.Name)
	}
	if base == "" {
		base = "tool"
	}
	alias := truncateChatToolName(base, 64)
	if existing, collision := p.byChatName[alias]; forceHash || (collision && responsesToolIdentityKey(existing) != key) {
		sum := sha256.Sum256([]byte(key))
		suffix := fmt.Sprintf("_%x", sum[:6])
		alias = truncateChatToolName(base, 64-len(suffix)) + suffix
	}
	// A truncated direct name can still collide with a previously allocated hashed
	// alias. Extend the deterministic digest until the map is unambiguous.
	if existing, collision := p.byChatName[alias]; collision && responsesToolIdentityKey(existing) != key {
		sum := sha256.Sum256([]byte("collision\x00" + key))
		suffix := fmt.Sprintf("_%x", sum[:8])
		alias = truncateChatToolName(base, 64-len(suffix)) + suffix
	}
	p.byIdentity[key] = alias
	p.byChatName[alias] = identity
	return alias
}

func sanitizeChatToolName(value string) string {
	var out strings.Builder
	for _, r := range strings.TrimSpace(value) {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	return strings.Trim(out.String(), "_")
}

func truncateChatToolName(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	return strings.TrimRight(value[:limit], "_")
}

type compatibilityLossSet map[string]bool

func (s compatibilityLossSet) add(loss string) {
	if strings.TrimSpace(loss) != "" {
		s[loss] = true
	}
}

func (s compatibilityLossSet) sorted() []string {
	out := make([]string, 0, len(s))
	for loss := range s {
		out = append(out, loss)
	}
	sort.Strings(out)
	return out
}

// ResponsesRequestToChatCompletion converts a Responses request body into a Chat
// Completions request body:
//   - instructions (string)            -> a leading system message
//   - input[] items                    -> messages[] (text passes through;
//     function_call -> assistant tool_calls; function_call_output -> tool message)
//   - flat function tools              -> Chat Completions {function:{…}} tools
//   - max_output_tokens                -> max_tokens
//   - Responses-only fields (store, previous_response_id, reasoning, include, …) dropped.
func ResponsesRequestToChatCompletion(raw []byte) ([]byte, error) {
	converted, err := ResponsesRequestToChatCompletionBridge(raw)
	return converted.Body, err
}

// ResponsesRequestToChatCompletionBridge is the loss-aware, request-scoped adapter.
// The compatibility wrapper above remains for callers that only need the body.
func ResponsesRequestToChatCompletionBridge(raw []byte) (ResponsesChatBridgeResult, error) {
	root, err := decodeJSONMapUseNumber(raw)
	if err != nil {
		return ResponsesChatBridgeResult{}, err
	}
	plan := NewResponsesToolBridgePlan()
	losses := compatibilityLossSet{}
	out := map[string]interface{}{}
	if m, ok := root["model"].(string); ok {
		out["model"] = m
	}
	if include := firstResponsesInclude(root["include"]); include != "" {
		losses.add(LossResponsesIncludeOmitted)
	}
	input, additionalTools := responsesLiteAdditionalTools(root["input"])
	toolSpecs := append(responseToolSlice(root["tools"]), additionalTools...)
	toolSpecs = append(toolSpecs, discoveredToolSearchTools(input)...)
	chatTools := responsesToolsToChatBridge(toolSpecs, plan, losses)
	messages := make([]interface{}, 0, 8)
	if instr, ok := root["instructions"].(string); ok && strings.TrimSpace(instr) != "" {
		messages = append(messages, map[string]interface{}{"role": "system", "content": instr})
	}
	convertedMessages, err := responsesInputToChatMessagesBridge(input, plan, losses)
	if err != nil {
		return ResponsesChatBridgeResult{}, err
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
	if len(chatTools) > 0 {
		out["tools"] = chatTools
	}
	if tc, downgraded := responsesToolChoiceToChatBridge(root["tool_choice"], plan); tc != nil {
		out["tool_choice"] = tc
		if downgraded {
			losses.add(LossResponsesToolChoiceDowngraded)
		}
	} else if root["tool_choice"] != nil {
		losses.add(LossResponsesToolChoiceDowngraded)
	}
	body, err := json.Marshal(out)
	if err != nil {
		return ResponsesChatBridgeResult{}, err
	}
	return ResponsesChatBridgeResult{Body: body, Plan: plan, CompatibilityLosses: losses.sorted()}, nil
}

func firstResponsesInclude(v interface{}) string {
	switch t := v.(type) {
	case []interface{}:
		for _, item := range t {
			if s := stringOr(item, ""); strings.TrimSpace(s) != "" {
				return s
			}
		}
	case string:
		return t
	}
	return ""
}

func responseToolSlice(value interface{}) []interface{} {
	tools, _ := value.([]interface{})
	return append([]interface{}(nil), tools...)
}

func responsesLiteAdditionalTools(input interface{}) (interface{}, []interface{}) {
	items, ok := input.([]interface{})
	if !ok || len(items) == 0 {
		return input, nil
	}
	first, _ := items[0].(map[string]interface{})
	if first == nil || stringOr(first["type"], "") != "additional_tools" {
		return input, nil
	}
	remaining := append([]interface{}(nil), items[1:]...)
	return remaining, responseToolSlice(first["tools"])
}

func discoveredToolSearchTools(input interface{}) []interface{} {
	items, _ := input.([]interface{})
	var tools []interface{}
	for _, rawItem := range items {
		item, _ := rawItem.(map[string]interface{})
		if item == nil || stringOr(item["type"], "") != "tool_search_output" {
			continue
		}
		tools = append(tools, responseToolSlice(item["tools"])...)
	}
	return tools
}

// responsesInputToChatMessages converts a Responses `input` (a bare string, or an
// array of input items) into Chat Completions messages.
func responsesInputToChatMessages(input interface{}) ([]interface{}, error) {
	return responsesInputToChatMessagesBridge(input, NewResponsesToolBridgePlan(), compatibilityLossSet{})
}

func responsesInputToChatMessagesBridge(input interface{}, plan *ResponsesToolBridgePlan, losses compatibilityLossSet) ([]interface{}, error) {
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
				identity := ResponsesToolIdentity{
					Kind: ResponsesToolFunction, Namespace: stringOr(m["namespace"], ""), Name: stringOr(m["name"], ""),
				}
				alias := plan.ensureAlias(identity)
				out = append(out, map[string]interface{}{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []interface{}{map[string]interface{}{
						"id":   stringOr(firstPresent(m["call_id"], m["id"]), ""),
						"type": "function",
						"function": map[string]interface{}{
							"name":      alias,
							"arguments": jsonValueString(m["arguments"]),
						},
					}},
				})
			case "custom_tool_call":
				identity := ResponsesToolIdentity{
					Kind: ResponsesToolCustom, Namespace: stringOr(m["namespace"], ""), Name: stringOr(m["name"], ""),
				}
				alias := plan.ensureAlias(identity)
				arguments, _ := json.Marshal(map[string]interface{}{"input": stringOr(m["input"], "")})
				out = append(out, map[string]interface{}{
					"role": "assistant", "content": nil,
					"tool_calls": []interface{}{map[string]interface{}{
						"id": stringOr(firstPresent(m["call_id"], m["id"]), ""), "type": "function",
						"function": map[string]interface{}{"name": alias, "arguments": string(arguments)},
					}},
				})
			case "tool_search_call":
				if !strings.EqualFold(stringOr(m["execution"], "client"), "client") {
					out = append(out, responsesHistoryItemAsChatMessage(m))
					losses.add(LossResponsesHistoryItemJSON)
					continue
				}
				identity := ResponsesToolIdentity{Kind: ResponsesToolSearch, Name: "tool_search", Execution: "client"}
				alias := plan.ensureAlias(identity)
				out = append(out, map[string]interface{}{
					"role": "assistant", "content": nil,
					"tool_calls": []interface{}{map[string]interface{}{
						"id": stringOr(firstPresent(m["call_id"], m["id"]), ""), "type": "function",
						"function": map[string]interface{}{"name": alias, "arguments": jsonValueString(m["arguments"])},
					}},
				})
			case "function_call_output", "custom_tool_call_output", "mcp_tool_call_output", "tool_search_output":
				if itemType == "tool_search_output" && (strings.EqualFold(stringOr(m["execution"], ""), "server") || stringOr(firstPresent(m["call_id"], m["id"]), "") == "") {
					out = append(out, responsesHistoryItemAsChatMessage(m))
					losses.add(LossResponsesHistoryItemJSON)
					continue
				}
				content, structured := responsesToolOutputChatContent(m)
				if structured {
					losses.add(LossResponsesStructuredToolOutputJSON)
				}
				out = append(out, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": stringOr(firstPresent(m["call_id"], m["id"]), ""),
					"content":      content,
				})
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
				out = append(out, responsesHistoryItemAsChatMessage(m))
				losses.add(LossResponsesHistoryItemJSON)
			}
		}
		return out, nil
	}
	return nil, nil
}

func jsonValueString(value interface{}) string {
	if text, ok := value.(string); ok {
		return text
	}
	if value == nil {
		return "{}"
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func responsesHistoryItemAsChatMessage(item map[string]interface{}) map[string]interface{} {
	envelope := map[string]interface{}{
		"version": 1,
		"type":    "responses_history_item",
		"item":    item,
	}
	raw, _ := json.Marshal(envelope)
	return map[string]interface{}{"role": "user", "content": string(raw)}
}

func responsesToolOutputChatContent(item map[string]interface{}) (string, bool) {
	if output, ok := item["output"].(string); ok && responsesToolOutputIsPlainText(item) {
		return output, false
	}
	envelope := map[string]interface{}{
		"version": 1,
		"type":    "responses_tool_output",
		"item":    item,
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return responsesOutputToText(item["output"]), false
	}
	return string(raw), true
}

func responsesToolOutputIsPlainText(item map[string]interface{}) bool {
	if _, ok := item["output"].(string); !ok {
		return false
	}
	for key := range item {
		switch key {
		case "type", "id", "call_id", "name", "output":
		default:
			return false
		}
	}
	return true
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
	return responsesToolsToChatBridge(responseToolSlice(v), NewResponsesToolBridgePlan(), compatibilityLossSet{}), nil
}

func responsesToolsToChatBridge(arr []interface{}, plan *ResponsesToolBridgePlan, losses compatibilityLossSet) []interface{} {
	out := make([]interface{}, 0, len(arr))
	seenAliases := map[string]bool{}
	appendFunction := func(identity ResponsesToolIdentity, source map[string]interface{}, parameters interface{}) {
		alias := plan.ensureAlias(identity)
		if alias == "" || seenAliases[alias] {
			return
		}
		seenAliases[alias] = true
		fn := map[string]interface{}{"name": alias}
		if description, ok := source["description"]; ok {
			fn["description"] = description
		}
		if parameters != nil {
			fn["parameters"] = parameters
		}
		if strict, ok := source["strict"]; ok {
			fn["strict"] = strict
		}
		out = append(out, map[string]interface{}{"type": "function", "function": fn})
	}
	for _, t := range arr {
		tm, ok := t.(map[string]interface{})
		if !ok {
			continue
		}
		if nested, ok := tm["function"].(map[string]interface{}); ok {
			identity := ResponsesToolIdentity{Kind: ResponsesToolFunction, Name: stringOr(nested["name"], "")}
			appendFunction(identity, nested, nested["parameters"])
			continue
		}
		typ := strings.ToLower(strings.TrimSpace(stringOr(tm["type"], "function")))
		switch typ {
		case "", "function":
			name := stringOr(tm["name"], "")
			if name != "" {
				appendFunction(ResponsesToolIdentity{Kind: ResponsesToolFunction, Name: name}, tm, tm["parameters"])
			}
		case "namespace":
			namespace := stringOr(tm["name"], "")
			children, _ := tm["tools"].([]interface{})
			for _, rawChild := range children {
				child, _ := rawChild.(map[string]interface{})
				if child == nil || stringOr(child["type"], "function") != "function" {
					losses.add(LossResponsesHostedToolOmitted)
					continue
				}
				name := stringOr(child["name"], "")
				if name != "" {
					appendFunction(ResponsesToolIdentity{Kind: ResponsesToolFunction, Namespace: namespace, Name: name}, child, child["parameters"])
				}
			}
		case "custom":
			name := stringOr(tm["name"], "")
			if name != "" {
				parameters := map[string]interface{}{
					"type":                 "object",
					"properties":           map[string]interface{}{"input": map[string]interface{}{"type": "string"}},
					"required":             []interface{}{"input"},
					"additionalProperties": false,
				}
				appendFunction(ResponsesToolIdentity{Kind: ResponsesToolCustom, Name: name}, tm, parameters)
			}
		case "tool_search":
			if !strings.EqualFold(stringOr(tm["execution"], ""), "client") {
				losses.add(LossResponsesServerToolSearchOmitted)
				continue
			}
			appendFunction(ResponsesToolIdentity{Kind: ResponsesToolSearch, Name: "tool_search", Execution: "client"}, tm, tm["parameters"])
		case "web_search", "web_search_preview", "image_generation", "image_generation_call", "file_search", "computer", "computer_use_preview":
			losses.add(LossResponsesHostedToolOmitted)
		default:
			losses.add(LossResponsesHostedToolOmitted)
		}
	}
	return out
}

func responsesCompatibilityError(kind, value string) error {
	return &CompatibilityError{Protocol: "Responses", Kind: kind, Value: value}
}

func responsesToolChoiceToChat(v interface{}) interface{} {
	choice, _ := responsesToolChoiceToChatBridge(v, NewResponsesToolBridgePlan())
	return choice
}

func responsesToolChoiceToChatBridge(v interface{}, plan *ResponsesToolBridgePlan) (interface{}, bool) {
	switch t := v.(type) {
	case string:
		return t, false // auto / none / required
	case map[string]interface{}:
		kind := ResponsesToolKind(stringOr(t["type"], "function"))
		name := stringOr(t["name"], "")
		namespace := stringOr(t["namespace"], "")
		if kind == ResponsesToolSearch && name == "" {
			name = "tool_search"
		}
		if alias, ok := plan.ChatName(kind, namespace, name); ok {
			return map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": alias}}, false
		}
		return "auto", true
	}
	return nil, v != nil
}

// ChatCompletionToResponsesResponse converts a (non-streaming) Chat Completions
// response into a Responses API response, so a Responses-speaking client gets the
// shape it expects from a Chat Completions upstream. Tool calls map to function_call
// output items; text maps to a message item with an output_text content part.
func ChatCompletionToResponsesResponse(raw []byte, model string, plans ...*ResponsesToolBridgePlan) ([]byte, error) {
	root, err := decodeJSONMapUseNumber(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid Chat Completions response: %w", err)
	}
	var plan *ResponsesToolBridgePlan
	if len(plans) > 0 {
		plan = plans[0]
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
		wireName := stringOr(mapGet(fn, "name"), "")
		identity := ResponsesToolIdentity{Kind: ResponsesToolFunction, Name: wireName}
		if original, ok := plan.ResolveChatName(wireName); ok {
			identity = original
		}
		callID := stringOr(tcm["id"], "")
		arguments := stringOr(mapGet(fn, "arguments"), "")
		switch identity.Kind {
		case ResponsesToolCustom:
			item := map[string]interface{}{
				"type": "custom_tool_call", "id": fmt.Sprintf("ctc_%s_%d", id, i),
				"call_id": callID, "name": identity.Name, "input": customToolInput(arguments), "status": "completed",
			}
			if identity.Namespace != "" {
				item["namespace"] = identity.Namespace
			}
			output = append(output, item)
		case ResponsesToolSearch:
			item := map[string]interface{}{
				"type": "tool_search_call", "id": fmt.Sprintf("tsc_%s_%d", id, i),
				"call_id": callID, "execution": "client", "arguments": decodedJSONValue(arguments), "status": "completed",
			}
			output = append(output, item)
		default:
			item := map[string]interface{}{
				"type": "function_call", "id": fmt.Sprintf("fc_%s_%d", id, i),
				"call_id": callID, "name": identity.Name, "arguments": arguments, "status": "completed",
			}
			if identity.Namespace != "" {
				item["namespace"] = identity.Namespace
			}
			output = append(output, item)
		}
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

func customToolInput(arguments string) string {
	decoded := decodedJSONValue(arguments)
	if object, ok := decoded.(map[string]interface{}); ok {
		if input, ok := object["input"].(string); ok {
			return input
		}
	}
	return arguments
}

func decodedJSONValue(raw string) interface{} {
	value, err := decodeJSONValueUseNumber([]byte(raw))
	if err != nil {
		if strings.TrimSpace(raw) == "" {
			return map[string]interface{}{}
		}
		return raw
	}
	return value
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
