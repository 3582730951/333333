package api

import (
	"bytes"
	"encoding/json"
	"testing"

	"codex-account-pool/internal/upstream"
)

func TestForceCodex429SyntheticPairIsExcludedFromDurableToolAnalysis(t *testing.T) {
	body, injected := upstream.AppendForceCodex429SyntheticPair([]byte(`{"input":[{"type":"message","role":"user","content":"real request"}]}`))
	if !injected {
		t.Fatal("expected synthetic pair")
	}
	if bodyHasClientToolResult(body) {
		t.Fatal("synthetic output was treated as a client tool result")
	}
	root, err := decodeContextJSONMap(body)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := root["input"].([]interface{})
	if hasPendingClientToolCall(input) {
		t.Fatal("synthetic pair was treated as a pending client tool call")
	}
	checkpoint, segment, _, awaitingTool, err := goalCheckpointAndSegment(body, body, []byte(`{"id":"resp-safe","output":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if awaitingTool || bytes.Contains([]byte(checkpoint), []byte("codexpool_overdraft")) || bytes.Contains([]byte(segment), []byte("codexpool_overdraft")) {
		t.Fatalf("synthetic pair leaked into durable goal state: checkpoint=%s segment=%s", checkpoint, segment)
	}
	var decoded struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(segment), &decoded); err != nil || len(decoded.Input) != 1 {
		t.Fatalf("durable segment did not retain only the real input: input=%d err=%v segment=%s", len(decoded.Input), err, segment)
	}
}
