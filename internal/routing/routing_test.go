package routing

import (
	"net/http"
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
