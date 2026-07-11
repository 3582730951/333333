package kiro

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"codex-account-pool/internal/capability"
)

type Conversion struct {
	Body        []byte
	Model       string
	ToolNameMap map[string]string
	InputTokens int64
	WebSearch   *WebSearchRequest
}

// ConversionOptions controls Kiro-only compatibility defaults. Explicit
// downstream request fields always take precedence over these defaults.
type ConversionOptions struct {
	DefaultThinking bool
}

type WebSearchRequest struct{ Query, ToolUseID string }

func ConvertAnthropicRequest(raw []byte, affinity string) (Conversion, error) {
	return ConvertAnthropicRequestWithOptions(raw, affinity, ConversionOptions{})
}

func ConvertAnthropicRequestWithOptions(raw []byte, affinity string, options ConversionOptions) (Conversion, error) {
	var req map[string]interface{}
	if err := json.Unmarshal(raw, &req); err != nil {
		return Conversion{}, err
	}
	model, _ := req["model"].(string)
	canonical, ok := capability.KiroCanonicalModel(model)
	if !ok {
		return Conversion{}, fmt.Errorf("unsupported Kiro model %q", model)
	}
	messages, _ := req["messages"].([]interface{})
	if len(messages) == 0 {
		return Conversion{}, errors.New("messages required")
	}
	for len(messages) > 0 {
		m, _ := messages[len(messages)-1].(map[string]interface{})
		if stringValue(m["role"]) == "user" {
			break
		}
		messages = messages[:len(messages)-1]
	}
	if len(messages) == 0 {
		return Conversion{}, errors.New("a user message is required")
	}
	toolMap := map[string]string{}
	tools := convertTools(req["tools"], toolMap)
	webSearch := pureWebSearchRequest(req["tools"], messages, affinity)
	history := make([]interface{}, 0, len(messages)-1)
	toolUses, pairedUses := collectToolPairs(messages)
	for _, v := range messages[:len(messages)-1] {
		if m, ok := v.(map[string]interface{}); ok {
			history = append(history, convertHistory(m, canonical, toolMap, toolUses, pairedUses))
		}
	}
	// Kiro requires history to begin with a user message. Anthropic-compatible
	// clients may legally send an assistant prefill as the first historical item.
	if len(history) > 0 {
		if first, ok := history[0].(map[string]interface{}); ok && first["assistantResponseMessage"] != nil {
			history = append([]interface{}{map[string]interface{}{
				"userInputMessage": map[string]interface{}{"content": "Begin conversation", "modelId": canonical, "origin": "AI_EDITOR"},
			}}, history...)
		}
	}
	last, _ := messages[len(messages)-1].(map[string]interface{})
	content, images, results := contentParts(last["content"], toolUses)
	systemPrompt := strings.TrimSpace(contentText(req["system"]))
	if out, ok := req["output_config"].(map[string]interface{}); ok {
		if format := out["format"]; format != nil {
			if b, e := json.Marshal(format); e == nil {
				if systemPrompt != "" {
					systemPrompt += "\n"
				}
				systemPrompt += "Return output that strictly satisfies this JSON format: " + string(b)
			}
		}
	}
	// Kiro's official clients represent a system instruction as a leading
	// Human/AI history pair. Embedding it in the current user message as XML makes
	// Claude treat it as an attempted prompt injection and also pollutes the stable
	// context prefix.
	if systemPrompt != "" {
		systemHistory := []interface{}{
			map[string]interface{}{"userInputMessage": map[string]interface{}{"content": systemPrompt, "origin": "AI_EDITOR", "userInputMessageContext": map[string]interface{}{}}},
			map[string]interface{}{"assistantResponseMessage": map[string]interface{}{"content": "I will follow these instructions."}},
		}
		history = append(systemHistory, history...)
	}
	ctx := map[string]interface{}{}
	if len(tools) > 0 {
		ctx["tools"] = tools
	}
	if len(results) > 0 {
		ctx["toolResults"] = results
	}
	user := map[string]interface{}{"content": content, "modelId": canonical, "origin": "AI_EDITOR", "userInputMessageContext": ctx}
	if len(images) > 0 {
		user["images"] = images
	}
	convID := stableUUID("conversation", affinity)
	continuation := stableUUID("continuation", affinity)
	state := map[string]interface{}{"conversationId": convID, "agentContinuationId": continuation, "agentTaskType": "vibe", "chatTriggerType": "MANUAL", "currentMessage": map[string]interface{}{"userInputMessage": user}}
	if len(history) > 0 {
		state["history"] = history
	}
	out := map[string]interface{}{"conversationState": state}
	thinkingEnabled := options.DefaultThinking && supportsDefaultKiroThinking(canonical)
	if thinking, ok := req["thinking"].(map[string]interface{}); ok {
		switch strings.ToLower(strings.TrimSpace(stringValue(thinking["type"]))) {
		case "disabled", "none":
			thinkingEnabled = false
		case "enabled", "adaptive", "auto":
			thinkingEnabled = true
		}
	}
	if thinkingEnabled {
		// generateAssistantResponse accepts model-level request fields. Using the
		// native field avoids injecting <thinking_mode> text into user content.
		out["additionalModelRequestFields"] = map[string]interface{}{
			"thinking": map[string]interface{}{"type": "adaptive"},
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return Conversion{}, err
	}
	return Conversion{Body: b, Model: canonical, ToolNameMap: toolMap, InputTokens: max64(1, int64(len(raw)/4)), WebSearch: webSearch}, nil
}

func supportsDefaultKiroThinking(model string) bool {
	return strings.HasPrefix(model, "claude-opus-4.6") ||
		strings.HasPrefix(model, "claude-opus-4.7") ||
		strings.HasPrefix(model, "claude-opus-4.8") ||
		strings.HasPrefix(model, "claude-sonnet-4.6") ||
		strings.HasPrefix(model, "claude-sonnet-5")
}

func pureWebSearchRequest(toolsValue interface{}, messages []interface{}, affinity string) *WebSearchRequest {
	tools, _ := toolsValue.([]interface{})
	if len(tools) != 1 {
		return nil
	}
	tool, _ := tools[0].(map[string]interface{})
	if !strings.EqualFold(stringValue(tool["name"]), "web_search") {
		return nil
	}
	first, _ := messages[0].(map[string]interface{})
	query := strings.TrimSpace(contentText(first["content"]))
	query = strings.TrimSpace(strings.TrimPrefix(query, "Perform a web search for the query: "))
	if query == "" {
		return nil
	}
	id := strings.ReplaceAll(stableUUID("web_search", affinity+"\x00"+query), "-", "")
	return &WebSearchRequest{Query: query, ToolUseID: "srvtoolu_" + id}
}

func stableUUID(kind, key string) string {
	s := sha256.Sum256([]byte(kind + "\x00" + key))
	b := s[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func stringValue(v interface{}) string { s, _ := v.(string); return s }

func convertTools(v interface{}, names map[string]string) []interface{} {
	arr, _ := v.([]interface{})
	out := make([]interface{}, 0, len(arr))
	for _, raw := range arr {
		t, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		nameValue := stringValue(t["name"])
		if nameValue == "" && strings.HasPrefix(strings.ToLower(stringValue(t["type"])), "web_search") {
			nameValue = "web_search"
			t["description"] = "Search the web through Kiro MCP"
			t["input_schema"] = map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}}, "required": []interface{}{"query"}}
		}
		name := shortToolName(nameValue, names)
		schema := t["input_schema"]
		if schema == nil {
			schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		schema = expandRefs(schema, schema)
		desc := stringValue(t["description"])
		if utf8.RuneCountInString(desc) > 10000 {
			desc = string([]rune(desc)[:10000])
		}
		out = append(out, map[string]interface{}{"toolSpecification": map[string]interface{}{"name": name, "description": desc, "inputSchema": map[string]interface{}{"json": schema}}})
	}
	return out
}
func shortToolName(name string, names map[string]string) string {
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	prefix := []rune(name)
	if len(prefix) > 54 {
		prefix = prefix[:54]
	}
	short := string(prefix) + "_" + hex.EncodeToString(sum[:4])
	names[short] = name
	return short
}

func expandRefs(v, root interface{}) interface{} {
	m, ok := v.(map[string]interface{})
	if !ok {
		if a, ok := v.([]interface{}); ok {
			for i := range a {
				a[i] = expandRefs(a[i], root)
			}
		}
		return v
	}
	if ref, ok := m["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
		cur := root
		for _, p := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			mm, ok := cur.(map[string]interface{})
			if !ok {
				return v
			}
			cur = mm[strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")]
		}
		return expandRefs(cur, root)
	}
	out := map[string]interface{}{}
	for k, x := range m {
		if k != "$defs" && k != "definitions" {
			out[k] = expandRefs(x, root)
		}
	}
	return out
}

func convertHistory(m map[string]interface{}, model string, names map[string]string, uses, paired map[string]bool) interface{} {
	role := stringValue(m["role"])
	text, images, results := contentParts(m["content"], uses)
	if role == "assistant" {
		var tus []interface{}
		if arr, ok := m["content"].([]interface{}); ok {
			for _, x := range arr {
				b, _ := x.(map[string]interface{})
				if stringValue(b["type"]) == "tool_use" {
					id := stringValue(b["id"])
					if !paired[id] {
						continue
					}
					tus = append(tus, map[string]interface{}{"toolUseId": id, "name": shortToolName(stringValue(b["name"]), names), "input": b["input"]})
				}
			}
		}
		msg := map[string]interface{}{"content": text}
		if len(tus) > 0 {
			msg["toolUses"] = tus
		}
		return map[string]interface{}{"assistantResponseMessage": msg}
	}
	ctx := map[string]interface{}{}
	if len(results) > 0 {
		ctx["toolResults"] = results
	}
	msg := map[string]interface{}{"content": text, "modelId": model, "origin": "AI_EDITOR", "userInputMessageContext": ctx}
	if len(images) > 0 {
		msg["images"] = images
	}
	return map[string]interface{}{"userInputMessage": msg}
}

func collectToolPairs(messages []interface{}) (map[string]bool, map[string]bool) {
	uses, results := map[string]bool{}, map[string]bool{}
	for _, raw := range messages {
		m, _ := raw.(map[string]interface{})
		blocks, _ := m["content"].([]interface{})
		for _, v := range blocks {
			b, _ := v.(map[string]interface{})
			switch stringValue(b["type"]) {
			case "tool_use":
				uses[stringValue(b["id"])] = true
			case "tool_result":
				results[stringValue(b["tool_use_id"])] = true
			}
		}
	}
	paired := map[string]bool{}
	for id := range uses {
		if results[id] {
			paired[id] = true
		}
	}
	return uses, paired
}

func contentParts(v interface{}, uses map[string]bool) (string, []interface{}, []interface{}) {
	var texts []string
	var images, results []interface{}
	switch x := v.(type) {
	case string:
		texts = append(texts, x)
	case []interface{}:
		for _, raw := range x {
			b, _ := raw.(map[string]interface{})
			switch stringValue(b["type"]) {
			case "text", "thinking":
				texts = append(texts, stringValue(b["text"]), stringValue(b["thinking"]))
			case "image":
				src, _ := b["source"].(map[string]interface{})
				mt := stringValue(src["media_type"])
				format := strings.TrimPrefix(mt, "image/")
				if format == "jpg" {
					format = "jpeg"
				}
				images = append(images, map[string]interface{}{"format": format, "source": map[string]interface{}{"bytes": stringValue(src["data"])}})
			case "tool_result":
				id := stringValue(b["tool_use_id"])
				if uses == nil || uses[id] {
					status := "success"
					if e, _ := b["is_error"].(bool); e {
						status = "error"
					}
					results = append(results, map[string]interface{}{"toolUseId": id, "content": []interface{}{map[string]interface{}{"text": contentText(b["content"])}}, "status": status, "isError": status == "error"})
				}
			}
		}
	}
	var clean []string
	for _, s := range texts {
		if s != "" {
			clean = append(clean, s)
		}
	}
	return strings.Join(clean, "\n"), images, results
}
func contentText(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case []interface{}:
		var p []string
		for _, r := range x {
			if m, ok := r.(map[string]interface{}); ok {
				if s := stringValue(m["text"]); s != "" {
					p = append(p, s)
				}
			}
		}
		return strings.Join(p, "\n")
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}
func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
