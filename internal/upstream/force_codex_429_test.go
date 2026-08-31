package upstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestAppendForceCodex429SyntheticPairInjectsMatchingPair(t *testing.T) {
	body := []byte(`{"model":"gpt","store":false,"input":[{"type":"message","role":"user","content":"hello"}]}`)
	updated, ok := AppendForceCodex429SyntheticPair(body)
	if !ok {
		t.Fatal("expected injection")
	}
	var root struct {
		Input []map[string]interface{} `json:"input"`
	}
	if err := json.Unmarshal(updated, &root); err != nil {
		t.Fatal(err)
	}
	if len(root.Input) != 3 {
		t.Fatalf("expected 3 input items, got %d", len(root.Input))
	}
	call, ok := root.Input[1]["call_id"].(string)
	if !ok || root.Input[1]["type"] != "custom_tool_call" {
		t.Fatalf("second item not a custom_tool_call: %v", root.Input[1])
	}
	if !strings.HasPrefix(call, codexSyntheticAgentContextCallPrefix) {
		t.Fatalf("unexpected call_id prefix: %q", call)
	}
	if root.Input[1]["name"] != codexSyntheticAgentContextToolName {
		t.Fatalf("tool name=%v", root.Input[1]["name"])
	}
	outCallID, _ := root.Input[2]["call_id"].(string)
	if root.Input[2]["type"] != "custom_tool_call_output" || outCallID != call {
		t.Fatalf("output item mismatch: type=%v call_id=%q want %q", root.Input[2]["type"], outCallID, call)
	}
}

// The pair must be appended with byte-faithful surgery: a map re-marshal would
// round a large integer through float64 and corrupt it.
func TestAppendForceCodex429SyntheticPairPreservesOtherFields(t *testing.T) {
	body := []byte(`{"model":"gpt","store":123456789012345678901234567890,"input":[{"role":"user","content":"hi"}]}`)
	updated, ok := AppendForceCodex429SyntheticPair(body)
	if !ok {
		t.Fatal("expected injection")
	}
	if !bytes.Contains(updated, []byte(`123456789012345678901234567890`)) {
		t.Fatalf("large integer corrupted: %s", updated)
	}
	if !bytes.Contains(updated, []byte(`"role":"user"`)) {
		t.Fatalf("existing input item changed: %s", updated)
	}
}

func TestAppendForceCodex429SyntheticPairSkipsIneligible(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"tool call tail", `{"input":[{"type":"custom_tool_call","call_id":"x","name":"exec","input":"{}"}]}`},
		{"tool output tail", `{"input":[{"type":"function_call_output","call_id":"x","output":"{}"}]}`},
		{"tool role", `{"input":[{"type":"message","role":"tool","content":"out"}]}`},
		{"function role", `{"input":[{"role":"function","name":"f","arguments":"{}"}]}`},
		{"empty string input", `{"input":""}`},
		{"empty input", `{"input":[]}`},
		{"no input", `{"model":"gpt"}`},
		{"malformed body", `not-json`},
		{"already injected", `{"input":[{"role":"user","content":"hi"},{"type":"custom_tool_call","call_id":"call_codexpool_overdraft_aa","name":"exec","input":"{}"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated, ok := AppendForceCodex429SyntheticPair([]byte(tc.body))
			if ok {
				t.Fatalf("expected no injection, got body=%s", updated)
			}
		})
	}
}

func TestAppendForceCodex429SyntheticPairSkipsOversize(t *testing.T) {
	// The size guard runs before any parse, so an oversized arbitrary blob must be
	// left untouched.
	big := make([]byte, codexSyntheticAgentContextMaxBody+1)
	updated, ok := AppendForceCodex429SyntheticPair(big)
	if ok {
		t.Fatal("expected no injection for oversize body")
	}
	if len(updated) != len(big) || &updated[0] != &big[0] {
		t.Fatalf("oversize body was mutated")
	}
}

func TestAppendForceCodex429SyntheticPairAcceptsDeveloperAndAssistantTails(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"assistant message", `{"input":[{"type":"message","role":"assistant","content":"ok"}]}`},
		{"developer message", `{"input":[{"type":"message","role":"developer","content":"rules"}]}`},
		{"legacy message no type", `{"input":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := AppendForceCodex429SyntheticPair([]byte(tc.body)); !ok {
				t.Fatalf("expected injection for %s", tc.body)
			}
		})
	}
}

func TestAppendForceCodex429SyntheticPairNormalizesInputForms(t *testing.T) {
	for _, body := range []string{
		`{"input":{"type":"message","role":"user","content":"hi"}}`,
		`{"input":"hello"}`,
	} {
		updated, ok := AppendForceCodex429SyntheticPair([]byte(body))
		if !ok {
			t.Fatalf("expected injection for %s", body)
		}
		var root struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(updated, &root); err != nil || len(root.Input) != 3 {
			t.Fatalf("normalized input invalid: err=%v body=%s", err, updated)
		}
	}
}

func TestAppendForceCodex429SyntheticPairRejectsMalformedRole(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"type":"message","role":123,"content":"x"}]}`,
		`{"input":[{"type":"message","role":{"nested":true},"content":"x"}]}`,
	} {
		if _, ok := AppendForceCodex429SyntheticPair([]byte(body)); ok {
			t.Fatalf("malformed role unexpectedly injected: %s", body)
		}
	}
}

func TestForceCodex429SyntheticPrefixes(t *testing.T) {
	for _, id := range []string{"call_codexpool_overdraft_x", "call_sub2api_overdraft_x", "fc_x", "ctc_x"} {
		if !IsForceCodex429SyntheticCallID(id) {
			t.Fatalf("prefix not recognized: %s", id)
		}
	}
	if IsForceCodex429SyntheticCallID("call_other_x") {
		t.Fatal("unrelated call id recognized")
	}
}
