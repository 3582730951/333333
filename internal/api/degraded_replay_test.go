package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/leakfilter"
)

func TestDegradedReplayNeutralizesOrphanedToolOutput(t *testing.T) {
	// A tool-output turn that relied on previous_response_id (the call lived in upstream
	// server-side state). Degrading strips previous_response_id, which would orphan the
	// output — it must instead become a user message that preserves the result text.
	body := []byte(`{"model":"gpt","previous_response_id":"resp_x","input":[{"type":"custom_tool_call_output","call_id":"call_1","output":"the result text"}]}`)
	var root map[string]interface{}
	if err := json.Unmarshal(degradedResponsesReplay(body), &root); err != nil {
		t.Fatal(err)
	}
	if _, has := root["previous_response_id"]; has {
		t.Fatal("previous_response_id must be stripped")
	}
	input := root["input"].([]interface{})
	if len(input) != 1 {
		t.Fatalf("input len=%d want 1", len(input))
	}
	item := input[0].(map[string]interface{})
	if item["type"] == "custom_tool_call_output" {
		t.Fatal("orphaned tool output was not neutralized")
	}
	if item["role"] != "user" {
		t.Fatalf("orphaned output not converted to a user message: %v", item)
	}
	content := item["content"].([]interface{})[0].(map[string]interface{})
	if !strings.Contains(content["text"].(string), "the result text") {
		t.Fatalf("tool result text lost: %v", content)
	}
}

func TestDegradedReplayKeepsPairedToolCallOutput(t *testing.T) {
	// When the matching call is present in the same input, the output is valid and must
	// be left untouched.
	body := []byte(`{"input":[{"type":"custom_tool_call","call_id":"call_2","name":"foo","input":"{}"},{"type":"custom_tool_call_output","call_id":"call_2","output":"ok"}]}`)
	var root map[string]interface{}
	if err := json.Unmarshal(degradedResponsesReplay(body), &root); err != nil {
		t.Fatal(err)
	}
	input := root["input"].([]interface{})
	if len(input) != 2 || input[1].(map[string]interface{})["type"] != "custom_tool_call_output" {
		t.Fatalf("a paired tool output must be left intact: %v", input)
	}
}

func TestContextErrorReplayNeutralizesCompletedPairedToolExchange(t *testing.T) {
	body := []byte(`{"model":"gpt","input":[
	  {"type":"custom_tool_call","call_id":"call_done","name":"apply_patch","input":"{}"},
	  {"type":"custom_tool_call_output","call_id":"call_done","status":"completed","output":{"text":"preserve paired result","n":900719925474099312345}},
	  {"type":"custom_tool_call","call_id":"call_pending","name":"next_tool","input":"{}"}
	]}`)
	degraded := degradedResponsesReplayForContextError(body, leakfilter.ResponsesContextErrorOrphanedToolOutput)
	text := string(degraded)
	root, err := decodeContextJSONMap(degraded)
	if err != nil {
		t.Fatal(err)
	}
	pending := false
	for _, raw := range root["input"].([]interface{}) {
		item, _ := raw.(map[string]interface{})
		if streamString(item["call_id"]) == "call_done" && (isToolCallItemType(streamString(item["type"])) || isToolOutputItemType(streamString(item["type"]))) {
			t.Fatalf("completed rejected tool exchange remained executable: %v", item)
		}
		if streamString(item["call_id"]) == "call_pending" && isToolCallItemType(streamString(item["type"])) {
			pending = true
		}
	}
	if !pending {
		t.Fatalf("pending tool call was removed: %s", degraded)
	}
	for _, want := range []string{"preserve paired result", "900719925474099312345", "call_done", "call_pending", "next_tool"} {
		if !strings.Contains(text, want) {
			t.Fatalf("context replay lost %q: %s", want, degraded)
		}
	}
}

func TestNeutralizeOrphanedToolOutputsUsesStablePairingRules(t *testing.T) {
	var root map[string]interface{}
	raw := []byte(`{"input":[
	  {"type":"function_call","call_id":"f"},
	  {"type":"function_call_output","call_id":"f","output":"ok"},
	  {"type":"local_shell_call","call_id":"shell"},
	  {"type":"function_call_output","call_id":"shell","output":"ok"},
	  {"type":"custom_tool_call","call_id":"custom"},
	  {"type":"custom_tool_call_output","call_id":"custom","output":"ok"},
	  {"type":"tool_search_call","call_id":"search","execution":"client","arguments":{}},
	  {"type":"tool_search_output","call_id":"search","execution":"client","status":"completed","tools":[]},
	  {"type":"function_call","call_id":"mcp"},
	  {"type":"mcp_tool_call_output","call_id":"mcp","output":"ok"},
	  {"type":"mcp_tool_call","call_id":"managed-mcp"},
	  {"type":"mcp_tool_call_output","call_id":"managed-mcp","output":"orphan"},
	  {"type":"tool_search_output","execution":"server","status":"completed","tools":[]},
	  {"type":"function_call","call_id":"wrong-kind"},
	  {"type":"custom_tool_call_output","call_id":"wrong-kind","output":"orphan"},
	  {"type":"tool_search_output","execution":"client","status":"completed","tools":[]},
	  {"type":"future_call_output","call_id":"future","output":"untouched"}
	]}`)
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	input := root["input"].([]interface{})
	fixed, converted := neutralizeOrphanedToolOutputs(input)
	if converted != 3 {
		t.Fatalf("converted = %d, want 3: %v", converted, fixed)
	}
	for _, index := range []int{1, 3, 5, 7, 9, 10, 12, 16} {
		item := fixed[index].(map[string]interface{})
		if item["role"] == "user" {
			t.Fatalf("valid/stable item at %d was degraded: %v", index, item)
		}
	}
	for _, index := range []int{11, 14, 15} {
		if fixed[index].(map[string]interface{})["role"] != "user" {
			t.Fatalf("orphan at %d was not degraded: %v", index, fixed[index])
		}
	}
}

func TestDegradedReplayPreservesStructuredToolOutputAndLargeIntegers(t *testing.T) {
	body := []byte(`{
	  "model":"gpt","previous_response_id":"resp_x","turn_state":{"opaque":true},
	  "input":[{"type":"function_call_output","call_id":"missing","status":"failed","output":[
	    {"type":"input_image","image_url":"https://example.test/image.png"},
	    {"type":"input_text","text":"failed","code":900719925474099312345}
	  ],"encrypted_payload":"opaque","future":{"n":900719925474099312345}}]
	}`)
	degraded := degradedResponsesReplay(body)
	text := string(degraded)
	for _, want := range []string{"900719925474099312345", "https://example.test/image.png", "encrypted_payload", `\"status\":\"failed\"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("degraded structured output lost %q: %s", want, degraded)
		}
	}
	if strings.Contains(text, "previous_response_id") || strings.Contains(text, "turn_state") || strings.Contains(text, "function_call_output") && !strings.Contains(text, `\"type\":\"function_call_output\"`) {
		t.Fatalf("state was not stripped or structured envelope malformed: %s", degraded)
	}
}

func TestResponsesContextErrorDetection(t *testing.T) {
	msg := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"No tool call found for custom tool call output with call_id call_HkoKDDLSh5YyziVSnZZjYmFK."}}`)
	if got := responsesContextError(400, msg); got != leakfilter.ResponsesContextErrorOrphanedToolOutput {
		t.Fatal("should detect the orphaned tool-call-output 400")
	}
	previous := []byte(`{"error":{"type":"previous_response_not_found","message":"Previous response resp_missing was not found."}}`)
	if got := responsesContextError(400, previous); got != leakfilter.ResponsesContextErrorPreviousResponseNotFound {
		t.Fatalf("previous-response error kind = %q", got)
	}
	if got := responsesContextError(429, msg); got != leakfilter.ResponsesContextErrorNone {
		t.Fatal("only a 400 should match")
	}
	if got := responsesContextError(400, []byte(`{"error":{"message":"invalid api key"}}`)); got != leakfilter.ResponsesContextErrorNone {
		t.Fatal("an unrelated 400 must not match")
	}
	if got := responsesContextError(400, []byte(`{"error":{"message":"tool call output has an invalid call_id"}}`)); got != leakfilter.ResponsesContextErrorNone {
		t.Fatal("a generic call_id validation error must not match")
	}
}

func TestResponsesRecoveryEligibleForTurnStateHeaderOnly(t *testing.T) {
	header := http.Header{"X-Codex-Turn-State": []string{"old-account-state"}}
	if !responsesRecoveryEligible([]byte(`{"model":"gpt","input":"next"}`), header) {
		t.Fatal("turn-state-only request must receive a recovery retry budget")
	}
}

func TestStatelessHTTPSFallbackRecoveryNeutralizesOriginalOrphan(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	body := []byte(`{"model":"gpt","previous_response_id":"resp_lost_ws","input":[{"type":"custom_tool_call_output","call_id":"call_finished","output":"preserve this result"}]}`)
	retry, mode, recovered := h.app.recoverResponsesContext(
		context.Background(),
		body,
		http.Header{"X-Codex-Turn-State": []string{"lost-socket-state"}},
		leakfilter.ResponsesContextErrorPreviousResponseNotFound,
	)
	if !recovered || mode != "degraded" {
		t.Fatalf("fallback recovery mode=%q recovered=%v", mode, recovered)
	}
	if responsesHasUnpairedToolOutput(retry.Raw, leakfilter.ResponsesContextErrorNone) {
		t.Fatalf("recovered HTTPS payload still has an orphaned tool output: %s", retry.Raw)
	}
	text := string(retry.Raw)
	if !strings.Contains(text, "preserve this result") ||
		strings.Contains(text, "previous_response_id") ||
		strings.Contains(text, "custom_tool_call_output") {
		t.Fatalf("fallback recovery lost result or retained server state: %s", retry.Raw)
	}
}

func TestResponsesNeedsDegradeGate(t *testing.T) {
	if !responsesNeedsDegrade([]byte(`{"previous_response_id":"r","input":[]}`)) {
		t.Fatal("a body with previous_response_id needs degrade")
	}
	if !responsesNeedsDegrade([]byte(`{"input":[{"type":"function_call_output","call_id":"x","output":"y"}]}`)) {
		t.Fatal("a body with an orphaned tool output needs degrade")
	}
	if responsesNeedsDegrade([]byte(`{"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)) {
		t.Fatal("a clean stateless body must not need degrade (would loop)")
	}
}
