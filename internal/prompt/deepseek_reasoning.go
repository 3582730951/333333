package prompt

import (
	"encoding/base64"
	"strings"
)

const deepSeekReasoningEnvelopePrefix = "pool-deepseek-reasoning-v1:"

// IsDeepSeekModel keeps provider-specific replay fields away from unrelated Chat
// implementations. Official and common relay model ids retain "deepseek" even when
// namespaced (for example deepseek/deepseek-v4-pro).
func IsDeepSeekModel(model string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// EncodeDeepSeekReasoningContent wraps DeepSeek's replay-required thinking text in
// an opaque value that Responses and Anthropic clients can carry between turns.
// The text remains model output; this envelope never enters a prompt by itself.
func EncodeDeepSeekReasoningContent(text string) string {
	if text == "" {
		return ""
	}
	return deepSeekReasoningEnvelopePrefix + base64.RawURLEncoding.EncodeToString([]byte(text))
}

// DecodeDeepSeekReasoningContent accepts only envelopes created above. Keeping the
// namespace narrow prevents an unrelated provider's opaque reasoning state from
// being replayed as DeepSeek chain-of-thought.
func DecodeDeepSeekReasoningContent(envelope string) (string, bool) {
	payload := strings.TrimPrefix(envelope, deepSeekReasoningEnvelopePrefix)
	if payload == envelope || payload == "" {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || len(decoded) == 0 {
		return "", false
	}
	return string(decoded), true
}

// DeepSeekReasoningItem converts Chat Completions reasoning_content to a standard
// Responses reasoning item. encrypted_content is the lossless replay carrier;
// summary is the user-visible representation expected by Codex clients.
func DeepSeekReasoningItem(id, text string) map[string]interface{} {
	if strings.TrimSpace(id) == "" {
		id = "rs_pool_deepseek"
	}
	return map[string]interface{}{
		"type":              "reasoning",
		"id":                id,
		"summary":           []interface{}{map[string]interface{}{"type": "summary_text", "text": text}},
		"encrypted_content": EncodeDeepSeekReasoningContent(text),
	}
}

func deepSeekReasoningFromResponsesItem(item map[string]interface{}) (string, bool) {
	if item == nil || stringOr(item["type"], "") != "reasoning" {
		return "", false
	}
	return DecodeDeepSeekReasoningContent(stringOr(item["encrypted_content"], ""))
}
