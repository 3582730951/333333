package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// This mirrors DeepSeek's official cache harness shape: a stable, meaningful
// system/tool prefix, a reasoning tool-call turn, then an append-only tool result.
// The custom Responses bridge must reconstruct that prefix before converting to
// stateless Chat Completions; otherwise both context and provider cache are lost.
func TestCustomDeepSeekResponsesContinuationReplaysStablePrefixAndReasoning(t *testing.T) {
	const (
		model     = "deepseek-v4-flash"
		response1 = "chatcmpl-deepseek-cache-first"
		callID    = "call_deepseek_cache_fixture"
		reasoning = "I must preserve this reasoning value for the tool continuation."
	)
	var attempts atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		attempt := attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			_, _ = io.WriteString(w, `{"id":"`+response1+`","object":"chat.completion","model":"`+model+`",`+
				`"choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":"`+reasoning+`",`+
				`"tool_calls":[{"id":"`+callID+`","type":"function","function":{"name":"lookup","arguments":"{\"key\":\"stable\"}"}}]},"finish_reason":"tool_calls"}],`+
				`"usage":{"prompt_tokens":180,"completion_tokens":40,"total_tokens":220,"prompt_cache_hit_tokens":0,"prompt_cache_miss_tokens":180}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-deepseek-cache-second","object":"chat.completion","model":"`+model+`",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"continued"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":230,"completion_tokens":4,"total_tokens":234,"prompt_cache_hit_tokens":210,"prompt_cache_miss_tokens":20}}`)
	})
	setupDeepSeek(t, h, []string{model}, false)

	instructions := strings.Repeat("Stable system policy for cache continuity. ", 32)
	tools := []map[string]any{{
		"type": "function", "name": "lookup", "description": "Read a stable fixture value without changing it.",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}, "required": []string{"key"}},
	}}
	first := map[string]any{
		"model": model, "instructions": instructions, "reasoning": map[string]any{"effort": "xhigh"},
		"tools": tools, "input": []map[string]any{{"role": "user", "content": "Find the stable fixture and use the tool."}},
	}
	firstRaw, _ := json.Marshal(first)
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", bytes.NewReader(firstRaw))
	if err != nil {
		t.Fatal(err)
	}
	firstResponse, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(firstResponse, []byte(response1)) {
		t.Fatalf("first response status=%d body=%s", resp.StatusCode, firstResponse)
	}

	second := map[string]any{
		"model": model, "previous_response_id": response1, "instructions": instructions, "reasoning": map[string]any{"effort": "xhigh"},
		"tools": tools, "input": []map[string]any{{"type": "function_call_output", "call_id": callID, "output": "stable-value"}},
	}
	secondRaw, _ := json.Marshal(second)
	secondReq, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", bytes.NewReader(secondRaw))
	secondReq.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(secondReq)
	if err != nil {
		t.Fatal(err)
	}
	secondResponse, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(secondResponse, []byte("continued")) {
		t.Fatalf("second response status=%d body=%s", resp.StatusCode, secondResponse)
	}
	if got := resp.Header.Get("X-MiCliProxy-Context-Status"); !strings.HasPrefix(got, "rebuilt") {
		t.Fatalf("context status=%q, want rebuilt", got)
	}

	requests := h.requests()
	if len(requests) != 2 {
		t.Fatalf("upstream requests=%d, want 2", len(requests))
	}
	var cold, warm map[string]any
	if err := json.Unmarshal([]byte(requests[0].Body), &cold); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(requests[1].Body), &warm); err != nil {
		t.Fatal(err)
	}
	coldMessages, _ := cold["messages"].([]any)
	warmMessages, _ := warm["messages"].([]any)
	if len(coldMessages) < 2 || len(warmMessages) < 4 {
		t.Fatalf("cold/warm messages lost history: cold=%s warm=%s", requests[0].Body, requests[1].Body)
	}
	for i := 0; i < 2; i++ {
		coldJSON, _ := json.Marshal(coldMessages[i])
		warmJSON, _ := json.Marshal(warmMessages[i])
		if !bytes.Equal(coldJSON, warmJSON) {
			t.Fatalf("stable prefix message %d changed:\ncold=%s\nwarm=%s", i, coldJSON, warmJSON)
		}
	}
	if !strings.Contains(requests[1].Body, `"reasoning_content":"`+reasoning+`"`) ||
		!strings.Contains(requests[1].Body, `"tool_call_id":"`+callID+`"`) ||
		!strings.Contains(requests[1].Body, `"content":"stable-value"`) {
		t.Fatalf("warm request lost reasoning/tool continuation: %s", requests[1].Body)
	}
	if warm["reasoning_effort"] != "xhigh" {
		t.Fatalf("reasoning effort changed: %#v", warm["reasoning_effort"])
	}
	coldTools, _ := json.Marshal(cold["tools"])
	warmTools, _ := json.Marshal(warm["tools"])
	if !bytes.Equal(coldTools, warmTools) {
		t.Fatalf("stable tools changed:\ncold=%s\nwarm=%s", coldTools, warmTools)
	}
}

func TestCustomResponsesUnknownPreviousIDFailsBeforeUpstream(t *testing.T) {
	var attempts atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	setupDeepSeek(t, h, []string{"deepseek-v4-flash"}, false)
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(
		`{"model":"deepseek-v4-flash","previous_response_id":"resp_unknown","input":"continue"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("goal")) {
		t.Fatalf("unknown continuation status=%d body=%s", resp.StatusCode, body)
	}
	if attempts.Load() != 0 {
		t.Fatalf("unknown continuation reached upstream %d time(s)", attempts.Load())
	}
}
