package upstream

import (
	"bytes"
	"encoding/json"
	"testing"
)

const whamBaseURL = "https://chatgpt.com/backend-api/codex"
const azureBaseURL = "https://my-resource.openai.azure.com/openai"

func TestNormalizeCodexResponsesBody(t *testing.T) {
	tests := []struct {
		name             string
		input            string
		baseURL          string
		responsesLite    bool
		wantInstructions string
		wantStore        interface{} // bool, or nil when output should be unchanged/malformed
		wantParallel     interface{} // bool for Responses Lite, nil otherwise
		wantContext      string      // "all_turns" for Responses Lite, empty otherwise
		wantOtherFields  bool
		instructionsGone bool
	}{
		{
			name:             "absent instructions backfilled, store forced false (WHAM)",
			input:            `{"model":"gpt-5-codex","store":false,"input":[{"role":"user","content":"hi"}]}`,
			baseURL:          whamBaseURL,
			wantInstructions: "You are a coding agent.",
			wantStore:        false,
			wantOtherFields:  true,
		},
		{
			name:             "store true corrected to false on WHAM",
			input:            `{"model":"gpt-5-codex","instructions":"x","store":true,"input":[{"role":"user","content":"hi"}]}`,
			baseURL:          whamBaseURL,
			wantInstructions: "x",
			wantStore:        false,
			wantOtherFields:  true,
		},
		{
			name:             "store absent backfilled false on WHAM",
			input:            `{"model":"gpt-5-codex","instructions":"x","input":[{"role":"user","content":"hi"}]}`,
			baseURL:          whamBaseURL,
			wantInstructions: "x",
			wantStore:        false,
			wantOtherFields:  true,
		},
		{
			name:             "store forced true on Azure endpoint",
			input:            `{"model":"gpt-5-codex","instructions":"x","store":false,"input":[{"role":"user","content":"hi"}]}`,
			baseURL:          azureBaseURL,
			wantInstructions: "x",
			wantStore:        true,
			wantOtherFields:  true,
		},
		{
			name:             "empty instructions + store true both corrected",
			input:            `{"model":"gpt-5-codex","instructions":"","store":true,"input":[{"role":"user","content":"hi"}]}`,
			baseURL:          whamBaseURL,
			wantInstructions: "You are a coding agent.",
			wantStore:        false,
			wantOtherFields:  true,
		},
		{
			name:             "responses lite disables parallel tools",
			input:            `{"model":"gpt-5.6-sol","store":false,"parallel_tool_calls":true,"input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"exact instructions"}]}]}`,
			baseURL:          whamBaseURL,
			responsesLite:    true,
			wantInstructions: "",
			wantStore:        false,
			wantParallel:     false,
			wantContext:      "all_turns",
			wantOtherFields:  true,
		},
		{
			name:             "responses lite preserves native developer input",
			input:            `{"model":"gpt-5.6-terra","store":false,"input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"exact instructions"}]}]}`,
			baseURL:          whamBaseURL,
			responsesLite:    true,
			wantInstructions: "",
			wantStore:        false,
			wantParallel:     false,
			wantContext:      "all_turns",
			wantOtherFields:  true,
			instructionsGone: true,
		},
		{
			name:             "malformed JSON passes through",
			input:            `{broken`,
			baseURL:          whamBaseURL,
			wantInstructions: "",
			wantStore:        nil,
			wantOtherFields:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeCodexResponsesBody([]byte(tt.input), tt.baseURL, tt.responsesLite)
			if tt.name == "malformed JSON passes through" {
				if string(got) != tt.input {
					t.Fatalf("malformed input should pass through; got %s", got)
				}
				return
			}

			var payload map[string]interface{}
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatalf("output is not valid JSON: %v\n%s", err, got)
			}
			if instr, _ := payload["instructions"].(string); instr != tt.wantInstructions {
				t.Fatalf("instructions = %q, want %q\nfull output: %s", instr, tt.wantInstructions, got)
			}
			if _, present := payload["instructions"]; tt.instructionsGone && present {
				t.Fatalf("Responses Lite must preserve omitted instructions; got %s", got)
			}
			if store, ok := payload["store"].(bool); !ok || store != tt.wantStore.(bool) {
				t.Fatalf("store = %v (ok=%v), want %v\nfull output: %s", payload["store"], ok, tt.wantStore, got)
			}
			if tt.wantParallel != nil {
				if parallel, ok := payload["parallel_tool_calls"].(bool); !ok || parallel != tt.wantParallel.(bool) {
					t.Fatalf("parallel_tool_calls = %v (ok=%v), want %v\nfull output: %s", payload["parallel_tool_calls"], ok, tt.wantParallel, got)
				}
			}
			if tt.wantContext != "" {
				reasoning, _ := payload["reasoning"].(map[string]interface{})
				if reasoning["context"] != tt.wantContext {
					t.Fatalf("reasoning.context = %v, want %q\nfull output: %s", reasoning["context"], tt.wantContext, got)
				}
			}
			if tt.responsesLite {
				if _, present := payload["tools"]; present {
					t.Fatalf("Responses Lite retained top-level tools: %s", got)
				}
				items, _ := payload["input"].([]interface{})
				if len(items) == 0 || items[0].(map[string]interface{})["type"] != "additional_tools" {
					t.Fatalf("Responses Lite input does not start with additional_tools: %s", got)
				}
			}
			if tt.wantOtherFields && (payload["model"] == nil || payload["input"] == nil) {
				t.Fatalf("other fields missing; got %s", got)
			}
		})
	}
}

func TestNormalizeCodexResponsesLitePreservesReasoningFields(t *testing.T) {
	input := []byte(`{"model":"gpt-5.6-sol","store":false,"parallel_tool_calls":false,"reasoning":{"effort":"xhigh","summary":"auto","context":"current_turn"},"input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"user","content":[{"type":"input_text","text":"keep exact context"}]}]}`)
	got := normalizeCodexResponsesBody(input, whamBaseURL, true)
	var payload map[string]interface{}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "xhigh" || reasoning["summary"] != "auto" || reasoning["context"] != "all_turns" {
		t.Fatalf("Responses Lite reasoning was not minimally normalized: %s", got)
	}
	items, _ := payload["input"].([]interface{})
	if len(items) != 2 || items[0].(map[string]interface{})["type"] != "additional_tools" || items[1].(map[string]interface{})["role"] != "user" {
		t.Fatalf("native Lite input envelope changed: %s", got)
	}
}

func TestNormalizeCodexResponsesLiteMergesGatewayToolsAndInstructions(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":"gateway instructions","store":false,"parallel_tool_calls":false,"reasoning":{"context":"all_turns"},"tools":[{"type":"custom","name":"gateway_tool","format":{"type":"text"}}],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"const":900719925474099312345}}}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"base instructions"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	got := normalizeCodexResponsesBody(raw, whamBaseURL, true)

	var payload map[string]interface{}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	if _, present := payload["tools"]; present {
		t.Fatalf("Lite payload retained top-level tools: %s", got)
	}
	if _, present := payload["instructions"]; present {
		t.Fatalf("Lite payload retained top-level instructions: %s", got)
	}
	items, _ := payload["input"].([]interface{})
	if len(items) != 4 {
		t.Fatalf("Lite input item count = %d, want 4: %s", len(items), got)
	}
	additional, _ := items[0].(map[string]interface{})
	tools, _ := additional["tools"].([]interface{})
	if additional["type"] != "additional_tools" || additional["role"] != "developer" || len(tools) != 2 {
		t.Fatalf("invalid additional_tools prefix: %s", got)
	}
	developer, _ := items[1].(map[string]interface{})
	content, _ := developer["content"].([]interface{})
	if developer["type"] != "message" || developer["role"] != "developer" || len(content) != 1 || content[0].(map[string]interface{})["text"] != "gateway instructions" {
		t.Fatalf("invalid developer instructions item: %s", got)
	}
	if !bytes.Contains(got, []byte(`900719925474099312345`)) {
		t.Fatalf("large integer context was rounded: %s", got)
	}
}

func TestNormalizeCodexResponsesLiteAlreadyNormalizedIsByteIdentical(t *testing.T) {
	raw := []byte(`{ "model":"gpt-5.6-sol", "store":false, "parallel_tool_calls":false, "reasoning":{"context":"all_turns"}, "input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"keep","parameters":{"const":900719925474099312345}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"keep"}]},{"role":"user","content":"hi"}] }`)
	got := normalizeCodexResponsesBody(raw, whamBaseURL, true)
	if !bytes.Equal(got, raw) {
		t.Fatalf("already-normalized Lite body changed:\nwant %s\n got %s", raw, got)
	}
}

func TestNormalizeCodexResponsesLiteCompactAddsOnlyLiteFields(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":"gateway compact instructions","tools":[{"type":"custom","name":"gateway_tool","format":{"type":"text"}}],"parallel_tool_calls":true,"reasoning":{"effort":"high"},"input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"user","content":[{"type":"input_text","text":"compact"}]}]}`)
	got := normalizeCodexResponsesLiteCompactBody(raw)
	var payload map[string]interface{}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["parallel_tool_calls"] != false {
		t.Fatalf("compact Lite parallel_tool_calls was not disabled: %s", got)
	}
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "high" || reasoning["context"] != "all_turns" {
		t.Fatalf("compact Lite reasoning was not minimally normalized: %s", got)
	}
	if _, present := payload["store"]; present {
		t.Fatalf("compact Lite gained normal-turn store field: %s", got)
	}
	if _, present := payload["client_metadata"]; present {
		t.Fatalf("compact Lite gained normal-turn client_metadata: %s", got)
	}
	if _, present := payload["instructions"]; present {
		t.Fatalf("compact Lite retained top-level instructions: %s", got)
	}
	if _, present := payload["tools"]; present {
		t.Fatalf("compact Lite retained top-level tools: %s", got)
	}
	items, _ := payload["input"].([]interface{})
	additional, _ := items[0].(map[string]interface{})
	tools, _ := additional["tools"].([]interface{})
	if len(items) != 3 || len(tools) != 1 || tools[0].(map[string]interface{})["name"] != "gateway_tool" {
		t.Fatalf("compact Lite envelope was not merged correctly: %s", got)
	}
}

func TestNormalizeCodexResponsesBodyDoesNotApplyLiteEnvelopeWithoutLiteTransport(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":"classic","store":false,"tools":[{"type":"web_search"}],"input":"hi"}`)
	got := normalizeCodexResponsesBody(raw, whamBaseURL, false)
	if !bytes.Equal(got, raw) {
		t.Fatalf("API-key/classic request was rewritten as Lite:\nwant %s\n got %s", raw, got)
	}
}

func TestNormalizeCodexReasoningEffortForWire(t *testing.T) {
	ultra := []byte(`{"model":"gpt-5.6-sol","instructions":"keep","reasoning":{"effort":"ultra","summary":"auto","context":"all_turns"},"previous_response_id":"resp_keep","tools":[{"schema":{"const":900719925474099312345}}],"input":[{"value":900719925474099312345}]}`)
	got := normalizeCodexReasoningEffortForWire(ultra)
	var payload map[string]interface{}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "max" || reasoning["summary"] != "auto" || reasoning["context"] != "all_turns" {
		t.Fatalf("ultra wire mapping changed unrelated fields: %s", got)
	}
	assertCodexContextFieldsUnchanged(t, ultra, got)

	for _, unchanged := range [][]byte{
		[]byte(`{"reasoning":{"effort":"high"},"input":"keep spacing"}`),
		[]byte(`{broken`),
	} {
		if out := normalizeCodexReasoningEffortForWire(unchanged); string(out) != string(unchanged) {
			t.Fatalf("non-ultra body changed:\nwant %s\n got %s", unchanged, out)
		}
	}
}

func assertCodexContextFieldsUnchanged(t *testing.T, before, after []byte) {
	t.Helper()
	var want, got map[string]json.RawMessage
	if err := json.Unmarshal(before, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"model", "instructions", "input", "tools", "previous_response_id"} {
		if !bytes.Equal(want[key], got[key]) {
			t.Fatalf("context field %q changed\nbefore=%s\n after=%s", key, want[key], got[key])
		}
	}
}

func TestCodexResponseNormalizationAndRetentionStripPreserveContext(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":"gateway instructions","store":true,"parallel_tool_calls":true,"reasoning":{"effort":"high"},"prompt_cache_retention":"24h","previous_response_id":"resp_keep","tools":[{"type":"custom","name":"gateway_tool","format":{"type":"text"}}],"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"keep","parameters":{"const":900719925474099312345}}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"keep exact"}],"value":900719925474099312345}]}`)
	normalized := normalizeCodexResponsesBody(raw, whamBaseURL, true)
	stripped := stripCodexResponsesPromptCacheRetention(normalized)
	var payload map[string]interface{}
	if err := json.Unmarshal(stripped, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["store"] != false || payload["parallel_tool_calls"] != false {
		t.Fatalf("required wire normalization missing: %s", stripped)
	}
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if reasoning["context"] != "all_turns" || payload["prompt_cache_retention"] != nil {
		t.Fatalf("reasoning/retention normalization missing: %s", stripped)
	}
	if _, present := payload["tools"]; present {
		t.Fatalf("Lite normalization retained top-level tools: %s", stripped)
	}
	if _, present := payload["instructions"]; present {
		t.Fatalf("Lite normalization retained top-level instructions: %s", stripped)
	}
	if payload["previous_response_id"] != "resp_keep" || !bytes.Contains(stripped, []byte(`900719925474099312345`)) {
		t.Fatalf("Lite normalization lost context: %s", stripped)
	}
}
