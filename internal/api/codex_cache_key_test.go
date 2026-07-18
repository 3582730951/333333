package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"codex-account-pool/internal/routing"
)

func assertOnlyTopLevelJSONFieldChanged(t *testing.T, before, after []byte, changed string) {
	t.Helper()
	var want, got map[string]json.RawMessage
	if err := json.Unmarshal(before, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &got); err != nil {
		t.Fatal(err)
	}
	for key, value := range want {
		if key == changed {
			continue
		}
		if !bytes.Equal(value, got[key]) {
			t.Fatalf("field %q changed while editing %q\nbefore=%s\n after=%s", key, changed, value, got[key])
		}
	}
}

func TestAutoPromptCacheKeySupportsAssistantOutputTextLosslessly(t *testing.T) {
	corpus := strings.Repeat("stable repository history for cache reuse ", 180)
	first := []byte(`{"model":"gpt-5.6-luna","reasoning":{"effort":"low"},"input":[` +
		`{"role":"developer","content":[{"type":"input_text","text":"keep all context"}]},` +
		`{"role":"user","content":[{"type":"input_text","text":` + jsonString(corpus) + `}]},` +
		`{"role":"assistant","content":[{"type":"output_text","text":"REFERENCE_ACCEPTED"}]},` +
		`{"role":"user","content":[{"type":"input_text","text":"question A"}]}]}`)
	second := []byte(strings.Replace(string(first), "question A", "question B", 1))

	firstHash := automaticPromptCachePrefixHash(first)
	secondHash := automaticPromptCachePrefixHash(second)
	if firstHash == "" || firstHash != secondHash {
		t.Fatalf("valid assistant output_text history did not produce one stable prefix: %q vs %q", firstHash, secondHash)
	}
	withKey := ensureResponsesPromptCacheKey(first, automaticPromptCacheKey("gpt-5.6-luna", firstHash))
	if routing.PromptCacheKey(withKey) == "" {
		t.Fatalf("automatic prompt_cache_key was not injected: %s", withKey)
	}
	var before, after map[string]interface{}
	if err := json.Unmarshal(first, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(withKey, &after); err != nil {
		t.Fatal(err)
	}
	delete(after, "prompt_cache_key")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("cache-key injection changed conversation context\nbefore=%v\nafter=%v", before, after)
	}
}

func TestAutomaticPromptCacheKeySafeAcceptsStableResponsesHistoryItems(t *testing.T) {
	for _, itemType := range []string{
		"agent_message",
		"tool_search_call",
		"tool_search_output",
		"image_generation_call",
		"compaction",
		"compaction_summary",
		"context_compaction",
		"mcp_tool_call_output",
	} {
		t.Run(itemType, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]interface{}{
				"input": []interface{}{map[string]interface{}{"type": itemType, "future": map[string]interface{}{"n": 1}}},
			})
			if !automaticPromptCacheKeySafe(raw) {
				t.Fatalf("stable history item %q was rejected", itemType)
			}
		})
	}
	if automaticPromptCacheKeySafe([]byte(`{"input":[{"type":"future_unknown_history_item"}]}`)) {
		t.Fatal("unknown future history item must not silently enter automatic cache-key derivation")
	}
}

func TestOfficialCodexUUIDPromptCacheKeyNormalizesAcrossIndependentCLIs(t *testing.T) {
	stableInstructions := strings.Repeat("identical official Codex system and tool prefix ", 140)
	build := func(key string) []byte {
		return []byte(`{"model":"gpt-5.6-sol","instructions":` + jsonString(stableInstructions) +
			`,"reasoning":{"effort":"low"},"input":[` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"stable environment"}]},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}],` +
			`"prompt_cache_key":` + jsonString(key) + `}`)
	}
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("User-Agent", "codex_exec/0.144.5 (Linux; x86_64) terminal (codex_exec; 0.144.5)")
	one, changedOne := normalizeOfficialCodexPromptCacheKey(req, build("019f4b1a-1111-7aaa-8aaa-111111111111"), "gpt-5.6-sol")
	two, changedTwo := normalizeOfficialCodexPromptCacheKey(req, build("019f4b1a-2222-7bbb-8bbb-222222222222"), "gpt-5.6-sol")
	if !changedOne || !changedTwo {
		t.Fatal("official Codex UUID cache keys were not normalized")
	}
	oneKey, twoKey := routing.PromptCacheKey(one), routing.PromptCacheKey(two)
	if oneKey == "" || oneKey != twoKey || !strings.HasPrefix(oneKey, "auto_") {
		t.Fatalf("normalized keys differ: %q vs %q", oneKey, twoKey)
	}

	custom := build("operator-cache-key")
	if got, changed := normalizeOfficialCodexPromptCacheKey(req, custom, "gpt-5.6-sol"); changed || !reflect.DeepEqual(got, custom) {
		t.Fatal("explicit non-UUID cache key must remain byte-identical")
	}
	thirdPartyReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	thirdPartyReq.Header.Set("User-Agent", "third-party-client/1.0")
	uuidBody := build("019f4b1a-3333-7ccc-8ccc-333333333333")
	if got, changed := normalizeOfficialCodexPromptCacheKey(thirdPartyReq, uuidBody, "gpt-5.6-sol"); changed || !reflect.DeepEqual(got, uuidBody) {
		t.Fatal("third-party UUID cache key must remain byte-identical")
	}
}

func TestPromptCacheHintChangesOnlyPromptCacheKey(t *testing.T) {
	instructions := strings.Repeat("immutable official Codex instructions and tools ", 100)
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":` + jsonString(instructions) +
		`,"tools":[{"name":"keep","schema":{"const":900719925474099312345}}],` +
		`"previous_response_id":"resp_keep","input":[{"role":"user","content":"019f4b1a-1111-7aaa-8aaa-111111111111"}],` +
		`"prompt_cache_key":"019f4b1a-1111-7aaa-8aaa-111111111111"}`)
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("originator", "codex_exec")
	normalized, changed := normalizeOfficialCodexPromptCacheKey(req, raw, "gpt-5.6-sol")
	if !changed {
		t.Fatal("official cache key was not normalized")
	}
	assertOnlyTopLevelJSONFieldChanged(t, raw, normalized, "prompt_cache_key")

	withoutKey := []byte(`{"model":"gpt-5.6-sol","instructions":"keep",` +
		`"tools":[{"schema":{"const":900719925474099312345}}],` +
		`"previous_response_id":"resp_keep","input":[{"role":"user","content":"keep exact input"}]}`)
	injected := ensureResponsesPromptCacheKey(withoutKey, "auto_test")
	assertOnlyTopLevelJSONFieldChanged(t, withoutKey, injected, "prompt_cache_key")
}

func TestOfficialCodexSubagentsShareBaseCacheHintWithoutChangingTasks(t *testing.T) {
	developerPrefix := strings.Repeat("shared child agent tools and developer rules ", 160)
	buildChild := func(cacheKey, turnID, task string) []byte {
		return []byte(`{"model":"gpt-5.6-luna","reasoning":{"effort":"xhigh","context":"all_turns"},"input":[` +
			`{"type":"message","role":"developer","content":[{"type":"input_text","text":` + jsonString(developerPrefix) + `}],` +
			`"internal_chat_message_metadata_passthrough":{"turn_id":` + jsonString(turnID) + `}},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":` + jsonString(task) + `}],` +
			`"internal_chat_message_metadata_passthrough":{"turn_id":` + jsonString(turnID) + `}}],` +
			`"prompt_cache_key":` + jsonString(cacheKey) + `}`)
	}
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("originator", "codex_exec")
	mathBody := buildChild("019f4b2a-1111-7aaa-8aaa-111111111111", "019f4b2a-aaaa-7aaa-8aaa-aaaaaaaaaaaa", "solve math")
	orderBody := buildChild("019f4b2a-2222-7bbb-8bbb-222222222222", "019f4b2a-bbbb-7bbb-8bbb-bbbbbbbbbbbb", "solve ordering")
	mathOut, mathChanged := normalizeOfficialCodexPromptCacheKey(req, mathBody, "gpt-5.6-luna")
	orderOut, orderChanged := normalizeOfficialCodexPromptCacheKey(req, orderBody, "gpt-5.6-luna")
	if !mathChanged || !orderChanged {
		t.Fatal("subagent UUID keys were not normalized")
	}
	if mathKey, orderKey := routing.PromptCacheKey(mathOut), routing.PromptCacheKey(orderOut); mathKey == "" || mathKey != orderKey {
		t.Fatalf("sibling subagents received different base cache hints: %q vs %q", mathKey, orderKey)
	}
	if !strings.Contains(string(mathOut), "solve math") || !strings.Contains(string(orderOut), "solve ordering") {
		t.Fatal("cache-hint normalization changed a child task")
	}
}

func TestAutoPromptCacheKeyStaysStableAsConversationGrows(t *testing.T) {
	corpus := strings.Repeat("stable conversation context for cache reuse ", 180)
	first := []byte(`{"model":"gpt-5.6-sol","input":[` +
		`{"type":"additional_tools","role":"developer","tools":[]},` +
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"keep all context"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":` + jsonString(corpus) + `}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"REFERENCE_ACCEPTED"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"question one"}]}]}`)
	second := []byte(`{"model":"gpt-5.6-sol","input":[` +
		`{"type":"additional_tools","role":"developer","tools":[]},` +
		`{"type":"message","role":"developer","content":[{"type":"input_text","text":"keep all context"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":` + jsonString(corpus) + `}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"REFERENCE_ACCEPTED"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"question one"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"complete result"},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer one"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"question two"}]}]}`)

	firstHash := automaticPromptCachePrefixHash(first)
	secondHash := automaticPromptCachePrefixHash(second)
	if firstHash == "" || firstHash != secondHash {
		t.Fatalf("one conversation should keep one automatic cache key: %q vs %q", firstHash, secondHash)
	}
	firstKey := automaticPromptCacheKey("gpt-5.6-sol", firstHash)
	secondKey := automaticPromptCacheKey("gpt-5.6-sol", secondHash)
	if firstKey != secondKey {
		t.Fatalf("cache key changed as history grew: %q vs %q", firstKey, secondKey)
	}
	if otherModelKey := automaticPromptCacheKey("gpt-5.6-terra", secondHash); otherModelKey == secondKey {
		t.Fatal("automatic cache keys must remain isolated by model")
	}
}

func jsonString(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
