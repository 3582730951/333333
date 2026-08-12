package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/identity"
)

func TestInferDownstreamOS(t *testing.T) {
	cases := [][2]string{
		{`{"input":"see /Users/bob/x"}`, "Mac OS"},
		{`{"input":"see /home/bob/x"}`, "Linux"},
		{`{"system":"Platform: win32"}`, "Windows"},
		{`{"input":"nothing here"}`, ""},
	}
	for _, c := range cases {
		if got := inferDownstreamOS([]byte(c[0])); got != c[1] {
			t.Errorf("inferDownstreamOS(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestIsolateCodexConversationOnlyChangesCorrelationFields(t *testing.T) {
	id := identity.For(nil, "acc-isolation-exact")
	correlator := "same-value-must-stay-in-context"
	body := []byte(`{"model":"gpt","instructions":"` + correlator + `","prompt_cache_key":"` + correlator + `","conversation_id":"` + correlator + `","thread_id":"thread-real","session_id":"session-real","previous_response_id":"resp_keep","tools":[{"schema":{"const":900719925474099312345}}],"input":[{"role":"user","content":"` + correlator + `"}]}`)
	header := http.Header{
		"Conversation_id":   []string{"conv-real"},
		"X-Codex-Window-Id": []string{"window-real:1"},
		"X-Unrelated":       []string{"keep"},
	}
	after := isolateCodexConversation(header, body, id)
	var beforeFields, afterFields map[string]json.RawMessage
	if err := json.Unmarshal(body, &beforeFields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &afterFields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "instructions", "previous_response_id", "tools", "input"} {
		if !bytes.Equal(beforeFields[key], afterFields[key]) {
			t.Fatalf("context field %q changed\nbefore=%s\n after=%s", key, beforeFields[key], afterFields[key])
		}
	}
	for _, key := range []string{"prompt_cache_key", "conversation_id", "thread_id", "session_id"} {
		if bytes.Equal(beforeFields[key], afterFields[key]) {
			t.Fatalf("correlation field %q was not isolated", key)
		}
	}
	if header.Get("X-Unrelated") != "keep" {
		t.Fatalf("unrelated header changed: %+v", header)
	}
}

func TestIsolateCodexConversationNamespacesPerAccount(t *testing.T) {
	idA := identity.For(nil, "acc-A")
	idB := identity.For(nil, "acc-B")
	body := []byte(`{"model":"gpt","prompt_cache_key":"pck-123","input":"hi"}`)

	hA := http.Header{}
	hA.Set("Conversation_id", "conv-1")
	hA.Set("X-Codex-Window-Id", "win-1")
	bA := isolateCodexConversation(hA, append([]byte(nil), body...), idA)

	hB := http.Header{}
	hB.Set("Conversation_id", "conv-1")
	hB.Set("X-Codex-Window-Id", "win-1")
	bB := isolateCodexConversation(hB, append([]byte(nil), body...), idB)

	// Same conversation served by two accounts → different upstream identifiers,
	// so the upstream cannot correlate them (the cross-account 401 bug).
	if hA.Get("Conversation_id") == "conv-1" {
		t.Fatal("conversation_id not namespaced")
	}
	if hA.Get("Conversation_id") == hB.Get("Conversation_id") {
		t.Fatal("two accounts share a namespaced conversation_id (contamination)")
	}
	if hA.Get("X-Codex-Window-Id") == hB.Get("X-Codex-Window-Id") {
		t.Fatal("two accounts share a namespaced window id")
	}
	if string(bA) == string(bB) {
		t.Fatal("prompt_cache_key not namespaced per account")
	}
	if strings.Contains(string(bA), "pck-123") {
		t.Fatal("raw prompt_cache_key leaked upstream")
	}

	// Stable for the same account → conversation continuity & prompt cache survive.
	h2 := http.Header{}
	h2.Set("Conversation_id", "conv-1")
	isolateCodexConversation(h2, append([]byte(nil), body...), idA)
	if h2.Get("Conversation_id") != hA.Get("Conversation_id") {
		t.Fatal("namespacing not stable for the same account")
	}
}

func TestUsageLimitCooldown(t *testing.T) {
	if usageLimitCooldown(200, []byte("ok")) != 0 {
		t.Fatal("200 must not bench the account")
	}
	if usageLimitCooldown(429, []byte("slow down")) != 60 {
		t.Fatal("generic 429 → 60s")
	}
	if usageLimitCooldown(429, []byte(`{"error":"You exceeded your current quota"}`)) != 1800 {
		t.Fatal("quota signal → 1800s")
	}
	if usageLimitCooldown(402, []byte(`{"error":"balance exhausted"}`)) != 1800 {
		t.Fatal("payment required → 1800s")
	}
	if usageLimitCooldown(401, []byte(`{"error":{"type":"usage_limit_reached"}}`)) != 1800 {
		t.Fatal("usage_limit body → 1800s even on 401")
	}
}

func TestBenchOnLimitSurvivesCanceledRequestContext(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	accountID := h.importAccount(t, "limited-canceled", "upstream-canceled", "access-canceled")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.app.benchOnLimit(ctx, accountID, http.StatusTooManyRequests, http.Header{"Retry-After": {"120"}}, []byte(`{"error":"usage limit"}`))

	binding, err := h.store.GetEgressBinding(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CooldownUntil <= time.Now().Unix() || !binding.RecheckPending {
		t.Fatalf("canceled request did not persist cooldown and recheck: %+v", binding)
	}
}
