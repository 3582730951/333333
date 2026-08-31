// Package refusaldetect contains the deliberately small, deterministic detector
// used for the optional Codex session-rollover feature.  It is independent from
// response filtering: a refusal decision never rewrites a response and this
// package has no network, storage, or model dependencies.
package refusaldetect

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	Version = "gpt-refusal-v1"

	KindNone              = "none"
	KindHighConfidence    = "high_confidence_refusal"
	KindAmbiguous         = "ambiguous"
	KindUnsupported       = "unsupported"
	ReasonProtocolSafety  = "protocol_safety_buffering"
	ReasonEnglishDirect   = "english_direct_refusal"
	ReasonChineseDirect   = "chinese_direct_refusal"
	ReasonToolMixed       = "terminal_contains_tool_call"
	ReasonNonTerminal     = "non_completed_terminal"
	ReasonMalformed       = "malformed_terminal"
	ReasonInvalidUTF8     = "invalid_utf8"
	ReasonTooLong         = "input_too_long"
	ReasonNoAssistantText = "no_assistant_output_text"
)

// MaxTextBytes is a hard bound.  The detector never truncates an input before
// deciding; oversized input is ambiguous and therefore cannot trigger rollover.
const MaxTextBytes = 512 * 1024

type Decision struct {
	Kind    string `json:"kind"`
	Reason  string `json:"reason"`
	Version string `json:"version"`
}

func decision(kind, reason string) Decision {
	return Decision{Kind: kind, Reason: reason, Version: Version}
}

// Detect examines one final assistant output_text value.  Callers must provide
// only terminal text; user/developer/reasoning/tool content is intentionally not
// accepted by this API.
func Detect(text string) Decision {
	if !utf8.ValidString(text) {
		return decision(KindAmbiguous, ReasonInvalidUTF8)
	}
	if len([]byte(text)) > MaxTextBytes {
		return decision(KindAmbiguous, ReasonTooLong)
	}
	text = normalize(text)
	if strings.TrimSpace(text) == "" {
		return decision(KindNone, ReasonNoAssistantText)
	}
	if hasExcludedContext(text) {
		return decision(KindAmbiguous, "excluded_context")
	}
	// A refusal mixed with a fresh tool call is not a terminal refusal.  The
	// terminal extractor also checks this, but keeping the guard here makes the
	// text-only entry point safe for callers that already merged output items.
	if englishRefusal(text) {
		return decision(KindHighConfidence, ReasonEnglishDirect)
	}
	if chineseRefusal(text) {
		return decision(KindHighConfidence, ReasonChineseDirect)
	}
	return decision(KindNone, "no_high_confidence_rule")
}

// DetectOutputText is a descriptive alias used by stream integrations.
func DetectOutputText(text string) Decision { return Detect(text) }

// DetectResponseCompleted inspects a raw response.completed event/response
// object.  It only reads the terminal response object and assistant output_text
// items.  Presence (not value) of safety_buffering wins over text detection.
func DetectResponseCompleted(raw []byte) Decision {
	if len(raw) == 0 {
		return decision(KindAmbiguous, ReasonMalformed)
	}
	if !utf8.Valid(raw) {
		return decision(KindAmbiguous, ReasonInvalidUTF8)
	}
	if len(raw) > MaxTextBytes*2 {
		return decision(KindAmbiguous, ReasonTooLong)
	}
	var root map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if dec.Decode(&root) != nil || root == nil {
		return decision(KindAmbiguous, ReasonMalformed)
	}
	if typ, _ := root["type"].(string); typ != "" && typ != "response.completed" {
		// Some callers pass the response object without an event type.  A known
		// non-completed event must never be treated as a refusal.
		if strings.HasPrefix(typ, "response.") && typ != "response.completed" {
			return decision(KindAmbiguous, ReasonNonTerminal)
		}
	}
	response := root
	if nested, ok := root["response"].(map[string]interface{}); ok {
		response = nested
	}
	if status, _ := response["status"].(string); status != "" && !strings.EqualFold(status, "completed") {
		return decision(KindAmbiguous, ReasonNonTerminal)
	}
	if _, present := response["safety_buffering"]; present {
		return decision(KindHighConfidence, ReasonProtocolSafety)
	}
	// A terminal carrying any new call cannot be classified from its text.  Only
	// inspect output items, never arbitrary structured fields or tool payloads.
	items, hasItems := response["output"].([]interface{})
	var texts []string
	tool := false
	for _, value := range items {
		item, ok := value.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := item["type"].(string)
		if strings.Contains(strings.ToLower(strings.TrimSpace(typ)), "tool") ||
			strings.EqualFold(strings.TrimSpace(typ), "function_call") {
			tool = true
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(typ), "message") && typ != "" {
			continue
		}
		role, _ := item["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "assistant") {
			continue
		}
		content, _ := item["content"].([]interface{})
		for _, partValue := range content {
			part, ok := partValue.(map[string]interface{})
			if !ok {
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(stringValue(part["type"])), "output_text") {
				continue
			}
			if text, ok := part["text"].(string); ok {
				texts = append(texts, text)
			}
		}
	}
	if tool {
		return decision(KindAmbiguous, ReasonToolMixed)
	}
	if !hasItems {
		// A canonical response may expose the final text at this field.  It is
		// accepted only when no output array exists, so structured output fields
		// cannot accidentally enter the detector.
		if text, ok := response["output_text"].(string); ok {
			return Detect(text)
		}
		return decision(KindNone, ReasonNoAssistantText)
	}
	return Detect(strings.Join(texts, "\n"))
}

func stringValue(value interface{}) string {
	s, _ := value.(string)
	return s
}

func normalize(value string) string {
	value = norm.NFKC.String(value)
	// NFKC handles full-width ASCII but deliberately leaves typographic
	// apostrophes alone. Treat those forms identically so a direct refusal is not
	// missed merely because a client rendered “can’t” instead of "can't".
	value = strings.NewReplacer("’", "'", "‘", "'", "＇", "'").Replace(value)
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	var b strings.Builder
	lastSpace := false
	for _, r := range value {
		if r == '\n' {
			b.WriteRune('\n')
			lastSpace = false
			continue
		}
		if unicode.IsSpace(r) {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(unicode.ToLower(r))
		lastSpace = false
	}
	return strings.TrimSpace(b.String())
}

func hasExcludedContext(text string) bool {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, ">") || strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
		return true
	}
	// Translation/quotation/meta discussion is deliberately conservative.  A
	// refusal embedded after a paragraph of normal answer text is not high
	// confidence because it may be explaining a quoted sentence.
	for _, marker := range []string{"translation:", "translated:", "引用:", "翻译：", "例句:", "example:"} {
		if strings.Contains(trimmed, marker) {
			return true
		}
	}
	return false
}

func englishRefusal(text string) bool {
	first, remainder := firstRefusalParagraph(text, stripEnglishApology)
	if remainder != "" {
		// A direct refusal followed by a substantive continuation is not a high
		// confidence terminal refusal: it may be a normal answer that offers a
		// safe alternative. Conservatively leave it unclassified.
		return false
	}
	// Explicit negation of inability is a normal answer, not a refusal.
	if strings.Contains(first, "not unable") || strings.Contains(first, "not incapable") || strings.Contains(first, "isn't that i can't") {
		return false
	}
	for _, marker := range []string{" but ", " however ", " instead ", " here's ", " here is "} {
		if strings.Contains(first, marker) {
			return false
		}
	}
	pronoun := strings.HasPrefix(first, "i can't ") || strings.HasPrefix(first, "i cannot ") ||
		strings.HasPrefix(first, "i can not ") || strings.HasPrefix(first, "i won't ") ||
		strings.HasPrefix(first, "i will not ") || strings.HasPrefix(first, "i'm unable to ") ||
		strings.HasPrefix(first, "i am unable to ") || strings.HasPrefix(first, "i'm not able to ") ||
		strings.HasPrefix(first, "i am not able to ")
	if !pronoun {
		return false
	}
	for _, verb := range []string{"help", "assist", "provide", "complete", "fulfill", "comply", "create", "generate", "write", "give", "perform", "carry out"} {
		if strings.Contains(first, verb) {
			return true
		}
	}
	return false
}

func chineseRefusal(text string) bool {
	first, remainder := firstRefusalParagraph(text, stripChineseApology)
	if remainder != "" {
		return false
	}
	// Chinese clients often use full-width/ideographic spaces. They are not
	// semantically meaningful inside the compact sentence forms below.
	first = strings.ReplaceAll(first, " ", "")
	if strings.Contains(first, "并不是不能") || strings.Contains(first, "不是不能") || strings.Contains(first, "并非不能") {
		return false
	}
	for _, marker := range []string{"但是", "不过", "但我", "但可以", "但能"} {
		if strings.Contains(first, marker) {
			return false
		}
	}
	pronoun := strings.HasPrefix(first, "我不能") || strings.HasPrefix(first, "我无法") ||
		strings.HasPrefix(first, "我不可以") || strings.HasPrefix(first, "我不会") ||
		strings.HasPrefix(first, "我拒绝")
	if !pronoun {
		return false
	}
	for _, verb := range []string{"帮助", "协助", "完成", "提供", "执行", "回答", "满足", "生成", "编写"} {
		if strings.Contains(first, verb) {
			return true
		}
	}
	return false
}

func firstRefusalParagraph(text string, stripApology func(string) string) (string, string) {
	first, remainder := splitFirstParagraph(text)
	first = stripApology(first)
	if first != "" {
		return first, remainder
	}
	// A short apology by itself followed by a direct refusal on the next line is
	// a common terminal format. Only this narrow two-line form is joined; any
	// additional material remains a conservative non-match.
	second, afterSecond := splitFirstParagraph(remainder)
	return stripApology(second), afterSecond
}

func splitFirstParagraph(text string) (string, string) {
	text = strings.TrimSpace(text)
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return strings.TrimSpace(text[:index]), strings.TrimSpace(text[index+1:])
	}
	return text, ""
}

func stripEnglishApology(value string) string {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"sorry,", "sorry.", "i'm sorry,", "i am sorry,"} {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(value, prefix))
		}
	}
	return value
}

func stripChineseApology(value string) string {
	value = strings.TrimSpace(value)
	for _, prefix := range []string{"抱歉，", "抱歉,", "很抱歉，", "很抱歉,", "对不起，", "对不起,"} {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(value, prefix))
		}
	}
	return value
}
