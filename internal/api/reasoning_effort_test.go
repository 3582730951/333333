package api

import (
	"encoding/json"
	"testing"
)

func TestForcedReasoningPreservesMaxAndUltra(t *testing.T) {
	for _, effort := range []string{"max", "ultra"} {
		body := applyForcedReasoningResponses([]byte(`{"model":"gpt-5.6-sol","reasoning":{"summary":"auto"},"input":"keep"}`), effort)
		var payload map[string]interface{}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		reasoning, _ := payload["reasoning"].(map[string]interface{})
		if reasoning["effort"] != effort || reasoning["summary"] != "auto" || payload["input"] != "keep" {
			t.Fatalf("effort %q was lowered or sibling context changed: %s", effort, body)
		}
	}
}

func TestForcedReasoningChangesOnlyReasoning(t *testing.T) {
	before := []byte(`{"model":"gpt-5.6-sol","instructions":"keep","reasoning":{"effort":"low","summary":"auto"},"previous_response_id":"resp_keep","tools":[{"schema":{"const":900719925474099312345}}],"input":[{"role":"user","content":"keep exact input"}]}`)
	after := applyForcedReasoningResponses(before, "high")
	assertOnlyTopLevelJSONFieldChanged(t, before, after, "reasoning")
	var payload map[string]interface{}
	if err := json.Unmarshal(after, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["reasoning"].(map[string]interface{})["effort"] != "high" {
		t.Fatalf("forced effort missing: %s", after)
	}
}
