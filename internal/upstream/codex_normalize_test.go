package upstream

import (
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
		wantOtherFields  bool
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
			if store, ok := payload["store"].(bool); !ok || store != tt.wantStore.(bool) {
				t.Fatalf("store = %v (ok=%v), want %v\nfull output: %s", payload["store"], ok, tt.wantStore, got)
			}
			if tt.wantOtherFields && (payload["model"] == nil || payload["input"] == nil) {
				t.Fatalf("other fields missing; got %s", got)
			}
		})
	}
}
