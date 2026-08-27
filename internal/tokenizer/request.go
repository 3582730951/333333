package tokenizer

import (
	"encoding/json"
	"strings"
)

// Framing overheads. A request costs more than the concatenation of its text: each
// message carries role/delimiter tokens, and each tool is presented to the model as a
// JSON schema. These constants are the standard OpenAI chat framing (3 tokens per
// message plus a 3-token reply priming) and are applied on top of exact text counts, so
// the residual error is the framing approximation alone rather than the tokenizer's.
const (
	tokensPerMessage = 3
	tokensPerReply   = 3
)

// CountRequestTokens returns the exact input-token count for an OpenAI-shaped request
// body (Responses or Chat Completions). It tokenizes only what the model actually
// receives — instructions/system text, message content, and tool schemas — and adds
// chat framing, instead of hashing the raw JSON envelope.
//
// ok is false when the body is not an object or the encoder is unavailable; callers
// keep their previous estimate in that case.
func CountRequestTokens(raw []byte) (int64, bool) {
	if !Available() {
		return 0, false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return 0, false
	}

	var total int64
	messages := 0

	// Responses puts the system prompt in `instructions`; Chat/Messages use `system`.
	for _, key := range []string{"instructions", "system"} {
		if field, ok := root[key]; ok {
			text, items := decodeTextField(field)
			if n, counted := CountText(text); counted {
				total += n
			}
			messages += items
		}
	}

	// The conversation lives in `input` (Responses) or `messages` (Chat/Messages).
	sequence, hasSequence := root["input"]
	if !hasSequence {
		sequence, hasSequence = root["messages"]
	}
	if hasSequence {
		text, items := decodeTextField(sequence)
		if n, counted := CountText(text); counted {
			total += n
		}
		messages += items
	}

	// Tool schemas are part of the prompt the model reads. Their JSON *is* the payload
	// here, so counting the serialized schema is correct rather than an envelope error.
	if tools, ok := root["tools"]; ok {
		if n, counted := CountText(compactJSONText(tools)); counted {
			total += n
		}
	}

	if messages == 0 {
		messages = 1
	}
	return total + int64(messages)*tokensPerMessage + tokensPerReply, true
}

// decodeTextField collects every model-visible string inside a system/message/input
// field and reports how many discrete message items it saw (for framing). It walks the
// shape generically so Responses items, Chat parts and Anthropic content blocks are all
// handled without a per-protocol branch.
func decodeTextField(raw json.RawMessage) (string, int) {
	var value interface{}
	if json.Unmarshal(raw, &value) != nil {
		return "", 0
	}
	var b strings.Builder
	items := 0
	switch typed := value.(type) {
	case string:
		return typed, 1
	case []interface{}:
		for _, item := range typed {
			items++
			appendVisibleText(&b, item)
		}
	default:
		appendVisibleText(&b, value)
		items = 1
	}
	return b.String(), items
}

// contentKeys carry model-visible prose. A container under one of these is RECURSED
// into, so the block wrappers around the text ({"type":"input_text",...}) are not
// counted as if they were content.
var contentKeys = map[string]bool{
	"text":         true,
	"content":      true,
	"output":       true,
	"instructions": true,
	"summary":      true,
	"refusal":      true,
	"thinking":     true,
	"name":         true,
	"description":  true,
}

// schemaKeys carry structure that the model genuinely reads as JSON — a tool's
// parameter schema, or a tool call's argument object. For these the SERIALIZED form is
// the honest payload, so they are marshaled rather than recursed.
var schemaKeys = map[string]bool{
	"arguments":    true,
	"input":        true,
	"input_schema": true,
	"parameters":   true,
}

func appendVisibleText(b *strings.Builder, value interface{}) {
	switch typed := value.(type) {
	case string:
		writeSpaced(b, typed)
	case []interface{}:
		for _, item := range typed {
			appendVisibleText(b, item)
		}
	case map[string]interface{}:
		for key, item := range typed {
			lower := strings.ToLower(key)
			switch {
			case contentKeys[lower]:
				appendVisibleText(b, item)
			case schemaKeys[lower]:
				if text, ok := item.(string); ok {
					writeSpaced(b, text)
				} else if encoded, err := json.Marshal(item); err == nil {
					writeSpaced(b, string(encoded))
				}
			}
		}
	}
}

func writeSpaced(b *strings.Builder, s string) {
	if s == "" {
		return
	}
	if b.Len() > 0 {
		b.WriteByte('\n')
	}
	b.WriteString(s)
}

func compactJSONText(raw json.RawMessage) string {
	var value interface{}
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return string(raw)
	}
	return string(encoded)
}
