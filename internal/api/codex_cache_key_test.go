package api

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestOfficialCodexBasePrefixHashPreservesToolNumbersAndOrder(t *testing.T) {
	instructions := strings.Repeat("immutable official Codex instructions ", 100)
	build := func(number string, tools string) []byte {
		return []byte(`{"instructions":` + jsonString(instructions) + `,"tools":` + tools + `,"input":[{"role":"user","content":"task"}],"future_id":` + number + `}`)
	}
	one := build("900719925474099312345", `[{"name":"first","schema":{"const":900719925474099312345}},{"name":"second"}]`)
	two := build("900719925474099312346", `[{"name":"first","schema":{"const":900719925474099312346}},{"name":"second"}]`)
	reordered := build("900719925474099312345", `[{"name":"second"},{"name":"first","schema":{"const":900719925474099312345}}]`)
	oneHash := officialCodexBasePromptCacheHash(one)
	if oneHash == "" || oneHash == officialCodexBasePromptCacheHash(two) {
		t.Fatal("byte-distinct large tool integers collapsed into one cache prefix")
	}
	if oneHash == officialCodexBasePromptCacheHash(reordered) {
		t.Fatal("tool order was omitted from the cache prefix")
	}
}

func TestOfficialCodexSubagentsDistributeStableCacheShardsWithoutChangingTasks(t *testing.T) {
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
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		task := fmt.Sprintf("child task %d", i)
		body := buildChild(fmt.Sprintf("019f4b2a-%04x-7aaa-8aaa-%012x", i, i), fmt.Sprintf("019f4b2a-%04x-7aaa-8aaa-%012x", i+32, i+32), task)
		out, changed := normalizeOfficialCodexPromptCacheKey(req, body, "gpt-5.6-luna", 4)
		if !changed || !strings.Contains(string(out), task) {
			t.Fatalf("subagent %d was not normalized losslessly", i)
		}
		seen[routing.PromptCacheKey(out)] = true
	}
	if len(seen) < 2 || len(seen) > 4 {
		t.Fatalf("16 sibling agents used %d deterministic shards, want 2..4: %#v", len(seen), seen)
	}
}

func TestOfficialCodexPromptCacheShardUsesThreadThenAnchorThenUUID(t *testing.T) {
	original := "019f4b1a-1111-7aaa-8aaa-111111111111"
	request, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("thread-id", "header-child-7")
	if got := officialCodexPromptCacheShardSeedWithRequest(request, []byte(`{"input":[{"role":"user","content":"hello"}]}`), original); got != "thread:header-child-7" {
		t.Fatalf("header thread seed = %q", got)
	}
	withThread := []byte(`{"thread_id":"child-7","input":[{"role":"user","content":"hello"}]}`)
	if got := officialCodexPromptCacheShardSeed(withThread, original); got != "thread:child-7" {
		t.Fatalf("thread seed = %q", got)
	}
	withAnchor := []byte(`{"input":[{"role":"user","content":"hello"}]}`)
	if got := officialCodexPromptCacheShardSeed(withAnchor, original); !strings.HasPrefix(got, "anchor:") {
		t.Fatalf("anchor seed = %q", got)
	}
	anchorWithVolatileTurn := []byte(`{"input":[{"role":"user","content":"hello","internal_chat_message_metadata_passthrough":{"turn_id":"volatile"}}]}`)
	if got, want := officialCodexPromptCacheShardSeed(anchorWithVolatileTurn, original), officialCodexPromptCacheShardSeed(withAnchor, original); got != want {
		t.Fatalf("volatile turn metadata changed anchor: got=%q want=%q", got, want)
	}
	if got := officialCodexPromptCacheShardSeed([]byte(`{"input":[]}`), original); got != "uuid:"+original {
		t.Fatalf("uuid seed = %q", got)
	}
	if got := officialCodexPromptCacheShardSeed([]byte(`{"input":[{"role":"developer","content":"preamble"}]}`), original); got != "uuid:"+original {
		t.Fatalf("developer-only seed = %q", got)
	}
}

func TestOfficialCodexPromptCacheOneShardRestoresLegacyKey(t *testing.T) {
	instructions := strings.Repeat("stable official prefix ", 180)
	body := []byte(`{"instructions":` + jsonString(instructions) + `,"input":[{"role":"user","content":"task"}],"prompt_cache_key":"019f4b1a-1111-7aaa-8aaa-111111111111"}`)
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("originator", "codex_exec")
	got, changed := normalizeOfficialCodexPromptCacheKey(req, body, "gpt-5.6-sol", 1)
	if !changed || strings.Contains(routing.PromptCacheKey(got), "_s") {
		t.Fatalf("legacy one-shard key = %q, changed=%v", routing.PromptCacheKey(got), changed)
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
