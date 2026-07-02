package routing

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestHasServerSideState(t *testing.T) {
	mk := func(body string, hdr map[string]string) (*http.Request, []byte) {
		req, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		return req, []byte(body)
	}
	// previous_response_id and x-codex-turn-state carry server-side state → non-movable.
	if r, b := mk(`{"previous_response_id":"resp_1","input":[]}`, nil); !HasServerSideState("/v1/responses", r, b) {
		t.Fatal("previous_response_id must be server-side state")
	}
	if r, b := mk(`{"input":[]}`, map[string]string{"x-codex-turn-state": "s"}); !HasServerSideState("/v1/responses", r, b) {
		t.Fatal("x-codex-turn-state must be server-side state")
	}
	// A strict-sticky-but-self-contained turn (function_call_output) is movable: strict
	// for cache affinity, yet NOT server-side state.
	r, b := mk(`{"prompt_cache_key":"k","input":[{"type":"function_call_output","output":"x"}]}`, nil)
	if !IsStrictSticky("/v1/responses", r, b) {
		t.Fatal("function_call_output should be strict-sticky")
	}
	if HasServerSideState("/v1/responses", r, b) {
		t.Fatal("function_call_output is self-contained — must NOT be server-side state")
	}
}

func TestAffinityPriorityParentThreadWins(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))
	req.Header.Set("x-codex-parent-thread-id", "parent-1")
	body := []byte(`{"thread_id":"thread-2","prompt_cache_key":"pc-3","model":"gpt"}`)
	key := ExtractAffinityKey(req, body)
	if key.Source != "x-codex-parent-thread-id" {
		t.Fatalf("source = %q", key.Source)
	}
	if !strings.Contains(key.Key, "parent-1") {
		t.Fatalf("key = %q", key.Key)
	}
}

func TestStrictStickyDetection(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if !IsStrictSticky("/v1/responses/compact", req, []byte(`{}`)) {
		t.Fatalf("compact should be strict")
	}
	if !IsStrictSticky("/v1/responses", req, []byte(`{"previous_response_id":"resp_1"}`)) {
		t.Fatalf("previous_response_id should be strict")
	}
	if !IsStrictSticky("/v1/responses", req, []byte(`{"input":[{"type":"compaction_trigger"}]}`)) {
		t.Fatalf("compaction_trigger should be strict")
	}
	req2, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	req2.Header.Set("x-codex-turn-state", "next")
	if !IsStrictSticky("/v1/responses", req2, []byte(`{}`)) {
		t.Fatalf("x-codex-turn-state should be strict")
	}
	req3, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if !IsStrictSticky("/v1/responses", req3, []byte(`{"input":[{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)) {
		t.Fatalf("function_call_output should be strict (tool result in responses)")
	}
	req4, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	if !IsStrictSticky("/v1/messages", req4, []byte(`{"model":"claude","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"output"}]}]}`)) {
		t.Fatalf("tool_result should be strict (anthropic tool result in messages)")
	}
	// With previous_response_id it IS strict (Codex stateful tool-result turns).
	req5, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if !IsStrictSticky("/v1/responses", req5, []byte(`{"previous_response_id":"r1","input":[{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)) {
		t.Fatalf("function_call_output WITH previous_response_id MUST be strict")
	}
}

func TestAffinityExtractionDoesNotRequireFullJSONParse(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := []byte(`{"prompt_cache_key":"pc-fast","model":"gpt","input":[` + strings.Repeat(`{"role":"user","content":"x"},`, 1000) + `]`)
	key := ExtractAffinityKey(req, body)
	if key.Source != "prompt_cache_key" || !strings.Contains(key.Key, "pc-fast") {
		t.Fatalf("key = %+v", key)
	}
	if Model(body) != "gpt" {
		t.Fatalf("model scanner failed")
	}
}

// TestRelayConversationAnchorDistinguishesConversations covers the multi-agent
// load-collapse fix: two distinct conversations from the SAME downstream identity
// (no Codex correlators, as with native Claude) must get distinct affinity so they
// can spread across accounts, while additional turns of one conversation keep the
// same affinity so routing stays sticky and the prompt cache stays warm.
func TestRelayConversationAnchorDistinguishesConversations(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	one := []byte(`{"model":"claude","messages":[{"role":"user","content":"task one"}]}`)
	two := []byte(`{"model":"claude","messages":[{"role":"user","content":"task two"}]}`)
	k1 := ExtractAffinityKey(req, one)
	k2 := ExtractAffinityKey(req, two)
	if k1.Source != "downstream_api_project_model" {
		t.Fatalf("source = %q", k1.Source)
	}
	if k1.Hash == k2.Hash {
		t.Fatal("distinct conversations collapsed onto one affinity (load collapse)")
	}
	// Later turn of conversation one: same first user message → same affinity.
	oneTurn2 := []byte(`{"model":"claude","messages":[{"role":"user","content":"task one"},{"role":"assistant","content":"ok"},{"role":"user","content":"continue"}]}`)
	k1b := ExtractAffinityKey(req, oneTurn2)
	if k1.Hash != k1b.Hash {
		t.Fatal("conversation anchor not stable across turns (would break stickiness + cache)")
	}
}

func TestLargeStablePrefixAffinityIgnoresFinalUserTurn(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer downstream-key")
	longPrefix := strings.Repeat("repo context line with stable instructions and tool inventory. ", 80)
	one := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"high"},"input":[{"role":"system","content":` + strconv.Quote(longPrefix) + `},{"role":"user","content":"summarize file A"}]}`)
	two := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"high"},"input":[{"role":"system","content":` + strconv.Quote(longPrefix) + `},{"role":"user","content":"summarize file B"}]}`)

	k1 := ExtractAffinityKey(req, one)
	k2 := ExtractAffinityKey(req, two)
	if k1.Source != "cache_prefix_hash" || k2.Source != "cache_prefix_hash" {
		t.Fatalf("source = %q/%q, want cache_prefix_hash", k1.Source, k2.Source)
	}
	if k1.Hash != k2.Hash {
		t.Fatalf("same stable prefix routed differently: %s != %s", k1.Hash, k2.Hash)
	}

	differentReasoning := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"low"},"input":[{"role":"system","content":` + strconv.Quote(longPrefix) + `},{"role":"user","content":"summarize file B"}]}`)
	k3 := ExtractAffinityKey(req, differentReasoning)
	if k1.Hash == k3.Hash {
		t.Fatal("reasoning effort must remain part of cache affinity")
	}
}

func TestLargeStablePrefixAffinityInsideFinalUserText(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer downstream-key")
	longPrefix := strings.Repeat("stable repository snapshot line for prompt cache reuse. ", 140)
	one := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"high"},"input":[{"role":"user","content":[{"type":"input_text","text":` + strconv.Quote(longPrefix+"Question: summarize file A") + `}]}]}`)
	two := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"high"},"input":[{"role":"user","content":[{"type":"input_text","text":` + strconv.Quote(longPrefix+"Question: summarize file B") + `}]}]}`)

	k1 := ExtractAffinityKey(req, one)
	k2 := ExtractAffinityKey(req, two)
	if k1.Source != "cache_prefix_hash" || k2.Source != "cache_prefix_hash" {
		t.Fatalf("source = %q/%q, want cache_prefix_hash", k1.Source, k2.Source)
	}
	if k1.Hash != k2.Hash {
		t.Fatalf("same intra-message stable prefix routed differently: %s != %s", k1.Hash, k2.Hash)
	}

	differentPrefix := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"high"},"input":[{"role":"user","content":[{"type":"input_text","text":` + strconv.Quote(strings.Repeat("different repository snapshot line. ", 180)+"Question: summarize file B") + `}]}]}`)
	k3 := ExtractAffinityKey(req, differentPrefix)
	if k1.Hash == k3.Hash {
		t.Fatal("different intra-message stable prefixes must not share affinity")
	}
}

func TestLargeStablePrefixAffinityInsideMultimodalFinalUserText(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer downstream-key")
	longPrefix := strings.Repeat("stable multimodal repository snapshot line. ", 150)
	one := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"high"},"input":[{"role":"user","content":[{"type":"input_text","text":` + strconv.Quote(longPrefix) + `},{"type":"input_image","image_url":"data:image/png;base64,AAAA"},{"type":"input_text","text":"Question: inspect image A"}]}]}`)
	two := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"high"},"input":[{"role":"user","content":[{"type":"input_text","text":` + strconv.Quote(longPrefix) + `},{"type":"input_image","image_url":"data:image/png;base64,BBBB"},{"type":"input_text","text":"Question: inspect image B"}]}]}`)

	k1 := ExtractAffinityKey(req, one)
	k2 := ExtractAffinityKey(req, two)
	if k1.Source != "cache_prefix_hash" || k2.Source != "cache_prefix_hash" {
		t.Fatalf("source = %q/%q, want cache_prefix_hash", k1.Source, k2.Source)
	}
	if k1.Hash != k2.Hash {
		t.Fatalf("same multimodal stable text prefix routed differently: %s != %s", k1.Hash, k2.Hash)
	}
}

func TestStablePromptPrefixFingerprintDiagnosticsDoNotExposePromptText(t *testing.T) {
	longPrefix := strings.Repeat("stable repository snapshot line for prompt cache reuse. ", 140)
	body := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"high"},"input":[{"role":"user","content":[{"type":"input_text","text":` + strconv.Quote(longPrefix+"Question: summarize file A") + `}]}]}`)

	fp := StablePromptPrefixFingerprint(body)
	if fp.Hash == "" || fp.Source != "text_prefix" || fp.Reason != "ok" || fp.PrefixBytes < cachePrefixAffinityMinBytes {
		t.Fatalf("fingerprint = %+v", fp)
	}
	diag, err := json.Marshal(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(diag), "stable repository snapshot") || strings.Contains(string(diag), "summarize file A") {
		t.Fatalf("diagnostics leaked prompt text: %s", diag)
	}
}

func TestSmallRequestsKeepConversationAnchorAffinity(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer downstream-key")
	one := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":"task one"}]}`)
	two := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":"task two"}]}`)

	k1 := ExtractAffinityKey(req, one)
	k2 := ExtractAffinityKey(req, two)
	if k1.Source != "downstream_api_project_model" || k2.Source != "downstream_api_project_model" {
		t.Fatalf("source = %q/%q, want downstream_api_project_model", k1.Source, k2.Source)
	}
	if k1.Hash == k2.Hash {
		t.Fatal("small independent requests should keep conversation-anchor separation")
	}
}
