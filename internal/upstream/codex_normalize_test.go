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
			name:             "responses lite preserves empty instructions and disables parallel tools",
			input:            `{"model":"gpt-5.6-sol","instructions":"","store":false,"parallel_tool_calls":true,"input":[{"role":"developer","content":"exact instructions"}]}`,
			baseURL:          whamBaseURL,
			wantInstructions: "",
			wantStore:        false,
			wantParallel:     false,
			wantContext:      "all_turns",
			wantOtherFields:  true,
		},
		{
			name:             "responses lite preserves omitted instructions and developer input",
			input:            `{"model":"gpt-5.6-terra","store":false,"input":[{"role":"developer","content":"exact instructions"}]}`,
			baseURL:          whamBaseURL,
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
			got := normalizeCodexResponsesBody([]byte(tt.input), tt.baseURL)
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
			if tt.wantOtherFields && (payload["model"] == nil || payload["input"] == nil) {
				t.Fatalf("other fields missing; got %s", got)
			}
		})
	}
}

func TestNormalizeCodexResponsesLitePreservesReasoningFields(t *testing.T) {
	input := []byte(`{"model":"gpt-5.6-sol","store":false,"parallel_tool_calls":false,"reasoning":{"effort":"xhigh","summary":"auto","context":"current_turn"},"input":"keep exact context"}`)
	got := normalizeCodexResponsesBody(input, whamBaseURL)
	var payload map[string]interface{}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "xhigh" || reasoning["summary"] != "auto" || reasoning["context"] != "all_turns" {
		t.Fatalf("Responses Lite reasoning was not minimally normalized: %s", got)
	}
	if payload["input"] != "keep exact context" {
		t.Fatalf("input context changed: %s", got)
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
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":"keep exact","store":true,"parallel_tool_calls":true,"reasoning":{"effort":"high"},"prompt_cache_retention":"24h","previous_response_id":"resp_keep","tools":[{"schema":{"const":900719925474099312345}}],"input":[{"value":900719925474099312345}]}`)
	normalized := normalizeCodexResponsesBody(raw, whamBaseURL)
	assertCodexContextFieldsUnchanged(t, raw, normalized)
	stripped := stripCodexResponsesPromptCacheRetention(normalized)
	assertCodexContextFieldsUnchanged(t, normalized, stripped)
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
}
