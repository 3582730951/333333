package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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

func TestOrphanedToolCallOutputErrorDetection(t *testing.T) {
	msg := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"No tool call found for custom tool call output with call_id call_HkoKDDLSh5YyziVSnZZjYmFK."}}`)
	if !isOrphanedToolCallOutputError(400, msg) {
		t.Fatal("should detect the orphaned tool-call-output 400")
	}
	if isOrphanedToolCallOutputError(429, msg) {
		t.Fatal("only a 400 should match")
	}
	if isOrphanedToolCallOutputError(400, []byte(`{"error":{"message":"invalid api key"}}`)) {
		t.Fatal("an unrelated 400 must not match")
	}
	if isOrphanedToolCallOutputError(400, []byte(`{"error":{"message":"tool call output has an invalid call_id"}}`)) {
		t.Fatal("a generic call_id validation error must not match")
	}
}

func TestResponsesRecoveryEligibleForTurnStateHeaderOnly(t *testing.T) {
	header := http.Header{"X-Codex-Turn-State": []string{"old-account-state"}}
	if !responsesRecoveryEligible([]byte(`{"model":"gpt","input":"next"}`), header) {
		t.Fatal("turn-state-only request must receive a recovery retry budget")
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
