package api

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestResponsesWebSocketRequestConversionPreservesContextBytes(t *testing.T) {
	raw := []byte(`{"type":"response.create","model":"gpt-5.6-sol","instructions":"keep","previous_response_id":"resp_keep","tools":[{"schema":{"const":900719925474099312345}}],"input":[{"exact_id":900719925474099312345}]}`)
	kind, body, err := responsesWebSocketRequestToBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	if kind != "response.create" {
		t.Fatalf("kind = %q", kind)
	}
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "instructions", "previous_response_id", "tools", "input"} {
		if !bytes.Equal(before[key], after[key]) {
			t.Fatalf("context field %q changed\nbefore=%s\n after=%s", key, before[key], after[key])
		}
	}
	if _, present := after["type"]; present {
		t.Fatalf("downstream-only type remained: %s", body)
	}
	if string(after["stream"]) != "true" {
		t.Fatalf("stream default missing: %s", body)
	}
}
