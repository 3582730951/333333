package api

import (
	"encoding/json"
	"strings"
	"testing"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

// customCacheKeyBody builds a relay request whose stable prefix is large enough to be
// worth caching (the synthesis gate requires a reusable prefix, not a one-line prompt).
func customCacheKeyBody(t *testing.T, tail string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"model":        "relay-model",
		"instructions": strings.Repeat("stable system preamble. ", 200),
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "first turn"},
			map[string]interface{}{"role": "assistant", "content": "ack"},
			map[string]interface{}{"role": "user", "content": tail},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCustomProviderPromptCacheKeyIsSynthesizedAndStableAcrossTurns(t *testing.T) {
	chat := storage.CustomProvider{UpstreamProtocol: storage.CustomProviderProtocolChatCompletions}

	first := ensureCustomProviderPromptCacheKey(customCacheKeyBody(t, "second turn"), chat, "relay-model")
	key := routing.PromptCacheKey(first)
	if key == "" {
		t.Fatalf("relay request got no prompt_cache_key: %s", first)
	}

	// The next turn of the same conversation must reuse the key, otherwise the prefix
	// this turn just warmed is never looked up again.
	second := ensureCustomProviderPromptCacheKey(customCacheKeyBody(t, "a different third turn"), chat, "relay-model")
	if got := routing.PromptCacheKey(second); got != key {
		t.Fatalf("key changed between turns: %q then %q", key, got)
	}

	// A different model must not share one cache identity.
	other := ensureCustomProviderPromptCacheKey(customCacheKeyBody(t, "second turn"), chat, "other-model")
	if got := routing.PromptCacheKey(other); got == key {
		t.Fatalf("two models collapsed onto one cache key %q", got)
	}
}

func TestCustomProviderPromptCacheKeyNeverOverwritesClientKeyOrLeavesProtocol(t *testing.T) {
	chat := storage.CustomProvider{UpstreamProtocol: storage.CustomProviderProtocolChatCompletions}

	// A key the client chose is authoritative.
	client := ensureCustomProviderPromptCacheKey(
		[]byte(`{"model":"relay-model","prompt_cache_key":"client-owned","messages":[{"role":"user","content":"hi"}]}`), chat, "relay-model")
	if got := routing.PromptCacheKey(client); got != "client-owned" {
		t.Fatalf("client key was overwritten: %q", got)
	}

	// Anthropic-protocol relays cache via cache_control breakpoints; an unknown
	// top-level field there risks a strict relay rejecting the request outright.
	anthropic := storage.CustomProvider{UpstreamProtocol: storage.CustomProviderProtocolAnthropicMessages}
	body := customCacheKeyBody(t, "second turn")
	if got := ensureCustomProviderPromptCacheKey(body, anthropic, "relay-model"); routing.PromptCacheKey(got) != "" {
		t.Fatalf("anthropic relay body gained a prompt_cache_key: %s", got)
	}

	// A body with no reusable prefix is left exactly as it arrived.
	short := []byte(`{"model":"relay-model","messages":[{"role":"user","content":"hi"}]}`)
	if got := ensureCustomProviderPromptCacheKey(short, chat, "relay-model"); string(got) != string(short) {
		t.Fatalf("short request was rewritten: %s", got)
	}
}
