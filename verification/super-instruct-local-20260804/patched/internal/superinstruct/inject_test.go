package superinstruct

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestInjectSystemMatchesM1Carriers(t *testing.T) {
	raw := []byte(`{
  "instructions":"old instructions",
  "system":{"old":true},
  "system_prompt":"old prompt",
  "personality":"old personality",
  "exact":900719925474099312345,
  "messages":[
    {"role":"system","content":[{"type":"text","text":"old"}]},
    {"role":"user","content":"keep chat user"},
    {"role":"system","content":"old second"}
  ],
  "input":[
    {"role":"system","content":[{"type":"input_text","text":"old"},{"type":"opaque","value":7}]},
    {"role":"user","content":"keep responses user"}
  ]
}`)
	const instructions = "BRIDGE\n\nGROUP ADDITIONS"
	got, injected, err := InjectSystem(raw, instructions)
	if err != nil || !injected {
		t.Fatalf("InjectSystem injected=%v err=%v", injected, err)
	}
	var root map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(got))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"instructions", "system", "system_prompt", "personality"} {
		if root[field] != instructions {
			t.Fatalf("direct field %s = %#v", field, root[field])
		}
	}
	messages := root["messages"].([]interface{})
	for _, index := range []int{0, 2} {
		if messages[index].(map[string]interface{})["content"] != instructions {
			t.Fatalf("messages[%d] was not replaced: %#v", index, messages[index])
		}
	}
	if messages[1].(map[string]interface{})["content"] != "keep chat user" {
		t.Fatalf("user message changed: %#v", messages[1])
	}
	input := root["input"].([]interface{})
	parts := input[0].(map[string]interface{})["content"].([]interface{})
	for index, partValue := range parts {
		part := partValue.(map[string]interface{})
		if part["text"] != instructions {
			t.Fatalf("input system part %d was not replaced: %#v", index, part)
		}
	}
	if root["exact"].(json.Number).String() != "900719925474099312345" {
		t.Fatalf("large integer changed: %s", got)
	}
}

func TestInjectSystemInsertsMissingSystemRoles(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":"chat"}],"input":[{"role":"user","content":"responses"}]}`)
	got, injected, err := InjectSystem(raw, "BRIDGE")
	if err != nil || !injected {
		t.Fatalf("InjectSystem injected=%v err=%v", injected, err)
	}
	var root map[string]interface{}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	messages := root["messages"].([]interface{})
	if len(messages) != 2 || messages[0].(map[string]interface{})["role"] != "system" || messages[0].(map[string]interface{})["content"] != "BRIDGE" {
		t.Fatalf("missing Chat system was not inserted: %s", got)
	}
	input := root["input"].([]interface{})
	if len(input) != 2 || input[0].(map[string]interface{})["role"] != "system" {
		t.Fatalf("missing Responses system was not inserted: %s", got)
	}
}

func TestInjectSystemLeavesUnsupportedEnvelopeUntouched(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","opaque":{"keep":true}}`)
	got, injected, err := InjectSystem(raw, "BRIDGE")
	if err != nil || injected || string(got) != string(raw) {
		t.Fatalf("unsupported envelope changed: injected=%v err=%v got=%s", injected, err, got)
	}
}
