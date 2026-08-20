package upstream

import (
	"bytes"
	"encoding/json"
	"testing"

	"codex-account-pool/internal/storage"
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

func TestNormalizeCodexClassicParallelToolCallsRequireTools(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantPresent  bool
		wantParallel bool
		byteExact    bool
	}{
		{
			name: "missing tools drops orphan option",
			body: `{"model":"gpt-5.5","instructions":"keep","store":false,"parallel_tool_calls":true,"input":"hi"}`,
		},
		{
			name: "null tools drops orphan option",
			body: `{"model":"gpt-5.5","instructions":"keep","store":false,"tools":null,"parallel_tool_calls":false,"input":"hi"}`,
		},
		{
			name: "empty tools drops orphan option",
			body: `{"model":"gpt-5.5","instructions":"keep","store":false,"tools":[],"parallel_tool_calls":true,"input":"hi"}`,
		},
		{
			name:         "function tool preserves parallel true",
			body:         `{"model":"gpt-5.5","instructions":"keep","store":false,"tools":[{"type":"function","name":"Read","parameters":{"type":"object"}}],"parallel_tool_calls":true,"input":"hi"}`,
			wantPresent:  true,
			wantParallel: true,
			byteExact:    true,
		},
		{
			name:         "official tool preserves explicit false",
			body:         `{"model":"gpt-5.5","instructions":"keep","store":false,"tools":[{"type":"web_search"}],"parallel_tool_calls":false,"input":"hi"}`,
			wantPresent:  true,
			wantParallel: false,
			byteExact:    true,
		},
		{
			name:         "unknown future tools shape passes through",
			body:         `{"model":"gpt-5.5","instructions":"keep","store":false,"tools":{"future":true},"parallel_tool_calls":true,"input":"hi"}`,
			wantPresent:  true,
			wantParallel: true,
			byteExact:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(tt.body)
			got := normalizeCodexResponsesBody(raw, whamBaseURL, false)
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(got, &payload); err != nil {
				t.Fatal(err)
			}
			parallel, present := payload["parallel_tool_calls"]
			if present != tt.wantPresent {
				t.Fatalf("parallel_tool_calls present=%v, want %v: %s", present, tt.wantPresent, got)
			}
			if present {
				var value bool
				if err := json.Unmarshal(parallel, &value); err != nil || value != tt.wantParallel {
					t.Fatalf("parallel_tool_calls=%s err=%v, want %v", parallel, err, tt.wantParallel)
				}
			}
			if tt.byteExact && !bytes.Equal(got, raw) {
				t.Fatalf("valid tool request changed:\nwant %s\n got %s", raw, got)
			}
		})
	}
}

func TestStripCodexUnsupportedPromptCacheControlsIsNarrowAndExact(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","prompt_cache_key":"keep-cache-key","prompt_cache_options":{"mode":"explicit"},"future":{"n":900719925474099312345},"input":[{"role":"system","content":[{"type":"input_text","text":"base","prompt_cache_breakpoint":{"mode":"explicit"},"future_block":true}]},{"role":"user","content":[{"type":"input_text","text":"the string prompt_cache_breakpoint must stay"}]}]}`)
	got := stripCodexUnsupportedPromptCacheControls(raw)
	for _, forbidden := range []string{`"prompt_cache_options"`, `"prompt_cache_breakpoint":`} {
		if bytes.Contains(got, []byte(forbidden)) {
			t.Fatalf("unsupported cache control %s survived: %s", forbidden, got)
		}
	}
	for _, preserved := range []string{`"prompt_cache_key":"keep-cache-key"`, `900719925474099312345`, `"future_block":true`, `the string prompt_cache_breakpoint must stay`} {
		if !bytes.Contains(got, []byte(preserved)) {
			t.Fatalf("sanitizer lost %q: %s", preserved, got)
		}
	}
	if second := stripCodexUnsupportedPromptCacheControls(got); !bytes.Equal(second, got) {
		t.Fatalf("sanitizer is not idempotent: first=%s second=%s", got, second)
	}
}

func TestCodexExplicitCacheControlsRequireAPIKeyProbeAndMode(t *testing.T) {
	base := Request{Token: storage.AccountToken{OpenAIAPIKey: "sk-test"}, CodexExplicitCacheMode: "observe"}
	if codexExplicitCacheControlsAllowed(base) {
		t.Fatal("unprobed API-key transport accepted explicit cache controls")
	}
	base.CodexExplicitCacheCapable = true
	if !codexExplicitCacheControlsAllowed(base) {
		t.Fatal("probed API-key transport did not accept explicit cache controls")
	}
	base.CodexExplicitCacheMode = "off"
	if codexExplicitCacheControlsAllowed(base) {
		t.Fatal("off mode accepted explicit cache controls")
	}
	base.Token = storage.AccountToken{AccessToken: "oauth"}
	base.CodexExplicitCacheMode = "auto"
	if codexExplicitCacheControlsAllowed(base) {
		t.Fatal("ChatGPT OAuth transport accepted public cache controls")
	}
}

func TestApplyCodexGPT56AutomaticCacheBreakpointIsLosslessMetadataOnly(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":"keep top-level","previous_response_id":"resp_keep","reasoning":{"effort":"xhigh","context":"all_turns"},"tools":[{"name":"first","schema":{"const":900719925474099312345}},{"name":"second"}],"max_output_tokens":4096,"verbosity":"high","temperature":0.2,"input":[{"role":"developer","content":[{"type":"input_text","text":"stable developer prefix","future":true}]},{"role":"user","content":[{"type":"input_text","text":"keep user"}]},{"role":"assistant","content":[{"type":"output_text","text":"assistant output item"}]},{"type":"function_call","call_id":"call_keep","name":"tool","arguments":"{}"},{"type":"function_call_output","call_id":"call_keep","output":"tool result"},{"type":"reasoning","encrypted_content":"ciphertext"}]}`)
	got := applyCodexGPT56AutomaticCacheBreakpoint(raw)
	if bytes.Equal(got, raw) || !bytes.Contains(got, []byte(`"prompt_cache_options":{"mode":"implicit"}`)) || !bytes.Contains(got, []byte(`"prompt_cache_breakpoint":{"mode":"implicit"}`)) {
		t.Fatalf("automatic breakpoint missing: %s", got)
	}
	if !codexCacheOnlyMutation(raw, got) {
		t.Fatalf("automatic breakpoint changed non-cache fields: %s", got)
	}
	for _, preserved := range []string{`"previous_response_id":"resp_keep"`, `"effort":"xhigh"`, `"context":"all_turns"`, `900719925474099312345`, `"call_id":"call_keep"`, `"encrypted_content":"ciphertext"`, `"assistant output item"`, `"max_output_tokens":4096`, `"verbosity":"high"`} {
		if !bytes.Contains(got, []byte(preserved)) {
			t.Fatalf("lost %s: %s", preserved, got)
		}
	}
}

func TestApplyCodexGPT56AutomaticCacheBreakpointRequiresExistingStableBlock(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"model":"gpt-5.6-sol","input":[{"role":"user","content":[{"type":"input_text","text":"only user"}]}]}`),
		[]byte(`{"model":"gpt-5.6-sol","instructions":"top only","input":"plain"}`),
	} {
		if got := applyCodexGPT56AutomaticCacheBreakpoint(raw); !bytes.Equal(got, raw) {
			t.Fatalf("ineligible request changed: before=%s after=%s", raw, got)
		}
	}
}

func TestApplyCodexGPT56AutomaticCacheBreakpointIgnoresMentionInText(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","input":[{"role":"developer","content":[{"type":"input_text","text":"do not literally mention prompt_cache_breakpoint in metadata"}]},{"role":"user","content":[{"type":"input_text","text":"question"}]}]}`)
	got := applyCodexGPT56AutomaticCacheBreakpoint(raw)
	if bytes.Equal(got, raw) || !bytes.Contains(got, []byte(`"prompt_cache_options":{"mode":"implicit"}`)) {
		t.Fatalf("text mention suppressed automatic marker: %s", got)
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
	base, _ := items[1].(map[string]interface{})
	baseContent, _ := base["content"].([]interface{})
	if base["role"] != "developer" || len(baseContent) != 1 || baseContent[0].(map[string]interface{})["text"] != "base instructions" {
		t.Fatalf("native base developer item moved: %s", got)
	}
	developer, _ := items[2].(map[string]interface{})
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

func TestNormalizeCodexResponsesLiteOfficialGoldenFixtures(t *testing.T) {
	const firstFrame = `{"model":"gpt-5.6-sol","store":false,"stream":true,"parallel_tool_calls":false,"tool_choice":"auto","reasoning":{"effort":"xhigh","summary":"auto","context":"all_turns"},"include":["reasoning.encrypted_content"],"service_tier":"priority","prompt_cache_key":"thread-cache","text":{"verbosity":"low"},"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"request_user_input","parameters":{"type":"object"}},{"type":"local_shell","name":"shell"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"native base developer"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"<collaboration_mode>plan</collaboration_mode>"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect"}]}],"client_metadata":{"custom":"keep"}}`

	const continuation = `{"model":"gpt-5.6-sol","instructions":"dynamic developer","tools":[{"type":"function","name":"request_user_input","parameters":{"type":"object","properties":{"question":{"type":"string"}}}}],"store":false,"stream":true,"parallel_tool_calls":false,"tool_choice":"auto","reasoning":{"effort":"xhigh","summary":"auto","context":"all_turns"},"include":["reasoning.encrypted_content"],"service_tier":"priority","prompt_cache_key":"thread-cache","text":{"verbosity":"low"},"previous_response_id":"resp_previous","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"<collaboration_mode>plan</collaboration_mode>"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}],"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true","custom":"keep"}}`
	const continuationGolden = `{"model":"gpt-5.6-sol","store":false,"stream":true,"parallel_tool_calls":false,"tool_choice":"auto","reasoning":{"effort":"xhigh","summary":"auto","context":"all_turns"},"include":["reasoning.encrypted_content"],"service_tier":"priority","prompt_cache_key":"thread-cache","text":{"verbosity":"low"},"previous_response_id":"resp_previous","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"request_user_input","parameters":{"type":"object","properties":{"question":{"type":"string"}}}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"dynamic developer"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"<collaboration_mode>plan</collaboration_mode>"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}],"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true","custom":"keep"}}`

	if got := normalizeCodexResponsesBody([]byte(firstFrame), whamBaseURL, true); !bytes.Equal(got, []byte(firstFrame)) {
		t.Fatalf("official first Lite frame changed:\nwant %s\n got %s", firstFrame, got)
	}
	got := normalizeCodexResponsesBody([]byte(continuation), whamBaseURL, true)
	if !bytes.Equal(got, []byte(continuationGolden)) {
		t.Fatalf("official continuation envelope mismatch:\nwant %s\n got %s", continuationGolden, got)
	}
	if second := normalizeCodexResponsesBody(got, whamBaseURL, true); !bytes.Equal(second, got) {
		t.Fatalf("normalized continuation is not idempotent:\nfirst %s\nsecond %s", got, second)
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

func TestNormalizeCodexClassicOfficialFieldsRemainByteIdentical(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","instructions":"classic developer","store":false,"stream":true,"tools":[{"type":"function","name":"request_user_input","parameters":{"type":"object"}}],"parallel_tool_calls":true,"tool_choice":"auto","reasoning":{"effort":"xhigh","summary":"auto"},"service_tier":"priority","prompt_cache_key":"thread-cache","text":{"verbosity":"low"},"previous_response_id":"resp_previous","input":[{"role":"user","content":"continue"}],"client_metadata":{"custom":"keep"}}`)
	got := normalizeCodexResponsesBody(raw, whamBaseURL, false)
	if !bytes.Equal(got, raw) {
		t.Fatalf("classic/API-key request fields changed:\nwant %s\n got %s", raw, got)
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

func TestStripCodexResponsesMaxOutputTokensPreservesContext(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","max_output_tokens":64000,"reasoning":{"effort":"max"},"previous_response_id":"resp_keep","tools":[{"parameters":{"const":900719925474099312345}}],"input":[{"value":900719925474099312345}]}`)
	got := stripCodexResponsesMaxOutputTokens(raw)

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatal(err)
	}
	if _, present := payload["max_output_tokens"]; present {
		t.Fatalf("WHAM payload retained max_output_tokens: %s", got)
	}
	if !bytes.Equal(payload["model"], json.RawMessage(`"gpt-5.6-sol"`)) ||
		!bytes.Equal(payload["reasoning"], json.RawMessage(`{"effort":"max"}`)) ||
		!bytes.Equal(payload["previous_response_id"], json.RawMessage(`"resp_keep"`)) ||
		!bytes.Contains(got, []byte(`900719925474099312345`)) {
		t.Fatalf("max_output_tokens strip changed unrelated context: %s", got)
	}

	for _, unchanged := range [][]byte{
		[]byte(`{"model":"gpt-5.6-sol","input":"keep spacing"}`),
		[]byte(`{broken`),
	} {
		if out := stripCodexResponsesMaxOutputTokens(unchanged); !bytes.Equal(out, unchanged) {
			t.Fatalf("body without a valid output limit changed:\nwant %s\n got %s", unchanged, out)
		}
	}
}

func TestStripCodexResponsesHTTPGeneratePreservesContext(t *testing.T) {
	for _, value := range []string{"false", "true", "null"} {
		raw := []byte(`{"model":"gpt-5.6-sol","generate":` + value + `,"previous_response_id":"resp_keep","tools":[{"parameters":{"const":900719925474099312345}}],"input":[{"value":900719925474099312345}]}`)
		got := stripCodexResponsesHTTPGenerate(raw)
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(got, &payload); err != nil {
			t.Fatal(err)
		}
		if _, present := payload["generate"]; present {
			t.Fatalf("HTTP payload retained generate=%s: %s", value, got)
		}
		if !bytes.Equal(payload["previous_response_id"], json.RawMessage(`"resp_keep"`)) ||
			!bytes.Contains(got, []byte(`900719925474099312345`)) {
			t.Fatalf("generate strip changed conversation context: %s", got)
		}
	}

	for _, unchanged := range [][]byte{
		[]byte(`{"model":"gpt-5.6-sol","input":"keep spacing"}`),
		[]byte(`{broken`),
	} {
		if out := stripCodexResponsesHTTPGenerate(unchanged); !bytes.Equal(out, unchanged) {
			t.Fatalf("body without a valid generate member changed:\nwant %s\n got %s", unchanged, out)
		}
	}
}
