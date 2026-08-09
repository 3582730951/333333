package anthropicwire

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestMarshalPreservingOrderKeepsClaudeWireShape(t *testing.T) {
	original := []byte(`{"model":"claude","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"system":[{"type":"text","text":"system"}],"tools":[{"name":"Read","description":"read","input_schema":{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"file_path":{"description":"path","type":"string"}},"required":["file_path"],"additionalProperties":false}}],"metadata":{"user_id":"downstream"},"max_tokens":64,"thinking":{"type":"adaptive"},"context_management":{"edits":[]},"output_config":{"effort":"high"},"stream":true}`)
	var root map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(original))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		t.Fatal(err)
	}
	root["metadata"].(map[string]interface{})["user_id"] = "virtual"
	root["system"].([]interface{})[0].(map[string]interface{})["cache_control"] = map[string]interface{}{"ttl": "1h", "type": "ephemeral"}

	got, err := MarshalPreservingOrder(original, root)
	if err != nil {
		t.Fatal(err)
	}
	wantRoot := `{"model":"claude","messages":`
	if !bytes.HasPrefix(got, []byte(wantRoot)) {
		t.Fatalf("root order changed: %s", got)
	}
	for _, exact := range []string{
		`"role":"user","content"`,
		`"type":"text","text":"system","cache_control":{"type":"ephemeral","ttl":"1h"}`,
		`"name":"Read","description":"read","input_schema":{"$schema"`,
		`"file_path":{"description":"path","type":"string"}`,
	} {
		if !bytes.Contains(got, []byte(exact)) {
			t.Errorf("ordered fragment %q missing from %s", exact, got)
		}
	}
}

func TestMarshalPreservingOrderKeepsLargeJSONNumber(t *testing.T) {
	original := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi","exact":900719925474099312345}],"stream":true}`)
	var root map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(original))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		t.Fatal(err)
	}
	got, err := MarshalPreservingOrder(original, root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(`900719925474099312345`)) {
		t.Fatalf("large integer changed: %s", got)
	}
}

func TestMarshalPreservingOrderMatchesJSONStringEscaping(t *testing.T) {
	separators := string([]rune{'\u2028', '\u2029'})
	original := []byte(`{"model":"claude","messages":[{"role":"user","content":"<session>&` + separators + `\n\t\u0001\\\""}],"stream":true}`)
	var root map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(original))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		t.Fatal(err)
	}
	got, err := MarshalPreservingOrder(original, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, escaped := range [][]byte{[]byte(`\u003c`), []byte(`\u003e`), []byte(`\u0026`), []byte(`\u2028`), []byte(`\u2029`)} {
		if bytes.Contains(got, escaped) {
			t.Errorf("Go-only escape %q leaked into %s", escaped, got)
		}
	}
	for _, literal := range [][]byte{[]byte(`<session>&`), []byte("\u2028"), []byte("\u2029"), []byte(`\n\t\u0001\\\"`)} {
		if !bytes.Contains(got, literal) {
			t.Errorf("JSON.stringify fragment %q missing from %s", literal, got)
		}
	}
}
