package reliability

import (
	"encoding/json"
	"strings"
)

// response.go knows the two OpenAI response shapes the gateway emits — Responses API
// (output[] + output_text) and Chat Completions (choices[].message.content) — so the
// output guard can (a) read the assistant's final text out of a non-streaming upstream
// response and (b) prepend a downgrade notice to it without disturbing tool calls,
// usage, or any other field. Kept here (not in package prompt) so it is unit-tested
// alongside the guard it serves; package prompt remains the protocol translator.

// ExtractResponseText returns the assistant's text from a non-streaming Responses API
// body (output_text, else concatenated output[].content[].text). Empty on parse error.
func ExtractResponseText(body []byte) string {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	if t, ok := root["output_text"].(string); ok && t != "" {
		return t
	}
	output, ok := root["output"].([]interface{})
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, item := range output {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if content, ok := m["content"].([]interface{}); ok {
			for _, c := range content {
				if part, ok := c.(map[string]interface{}); ok {
					if txt, ok := part["text"].(string); ok {
						b.WriteString(txt)
					}
				}
			}
		}
	}
	return b.String()
}

// ExtractChatText returns choices[0].message.content from a Chat Completions body.
func ExtractChatText(body []byte) string {
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	choices, ok := root["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return ""
	}
	ch, ok := choices[0].(map[string]interface{})
	if !ok {
		return ""
	}
	msg, ok := ch["message"].(map[string]interface{})
	if !ok {
		return ""
	}
	if s, ok := msg["content"].(string); ok {
		return s
	}
	return ""
}

// PrependResponsesNotice inserts notice at the front of a Responses body's assistant
// text: it prepends to output_text and to the first message item's first text part.
// On any structural surprise it returns the body unchanged (fail-open: never corrupt a
// response to add a warning). It does not touch function_call items.
func PrependResponsesNotice(body []byte, notice string) []byte {
	if notice == "" {
		return body
	}
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	changed := false
	if t, ok := root["output_text"].(string); ok {
		root["output_text"] = notice + t
		changed = true
	}
	if output, ok := root["output"].([]interface{}); ok {
		for _, item := range output {
			m, ok := item.(map[string]interface{})
			if !ok || m["type"] != "message" {
				continue
			}
			if content, ok := m["content"].([]interface{}); ok {
				for _, c := range content {
					part, ok := c.(map[string]interface{})
					if !ok {
						continue
					}
					if txt, ok := part["text"].(string); ok {
						part["text"] = notice + txt
						changed = true
						break
					}
				}
			}
			break
		}
	}
	if !changed {
		return body
	}
	if out, err := json.Marshal(root); err == nil {
		return out
	}
	return body
}

// PrependChatNotice inserts notice at the front of choices[0].message.content in a
// Chat Completions body. Fail-open: returns the body unchanged on any surprise.
func PrependChatNotice(body []byte, notice string) []byte {
	if notice == "" {
		return body
	}
	var root map[string]interface{}
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	choices, ok := root["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return body
	}
	ch, ok := choices[0].(map[string]interface{})
	if !ok {
		return body
	}
	msg, ok := ch["message"].(map[string]interface{})
	if !ok {
		return body
	}
	switch c := msg["content"].(type) {
	case string:
		msg["content"] = notice + c
	case nil:
		msg["content"] = notice
	default:
		return body // array content (rare for chat); leave untouched
	}
	if out, err := json.Marshal(root); err == nil {
		return out
	}
	return body
}
