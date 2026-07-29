package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestCodexSourceNormalizationMatchesLegacyBytes(t *testing.T) {
	client := NewClient(config.Default())
	cases := []struct {
		name    string
		compact bool
		body    string
	}{
		{
			name: "OAuth classic",
			body: `{
  "model": "gpt-5.5",
  "stream": true,
  "instructions": "",
  "store": true,
  "reasoning": {"effort":"ultra","summary":"auto"},
  "prompt_cache_retention": "24h",
  "max_output_tokens": 123,
  "thread_id": "downstream-thread",
  "input": [{"role":"user","content":"keep","exact":900719925474099312345}],
  "client_metadata": {"custom":"keep","x-codex-turn-metadata":"{\"request_kind\":\"turn\",\"turn_started_at_unix_ms\":1234567890}"}
}`,
		},
		{
			name: "Responses Lite envelope",
			body: `{
  "model": "gpt-5.6-sol",
  "stream": true,
  "instructions": "",
  "tools": [],
  "store": true,
  "parallel_tool_calls": true,
  "reasoning": {"effort":"ultra","summary":"auto","context":"current_turn"},
  "prompt_cache_retention": "24h",
  "max_output_tokens": 123,
  "input": [{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"keep","parameters":{"const":900719925474099312345}}]},{"role":"user","content":"keep"}],
  "client_metadata": {"custom":"keep","x-codex-turn-metadata":"{\"request_kind\":\"turn\",\"turn_started_at_unix_ms\":1234567890}"}
}`,
		},
		{
			name: "Responses Lite continuation",
			body: `{
  "model": "gpt-5.6-sol",
  "stream": true,
  "tools": null,
  "store": null,
  "parallel_tool_calls": true,
  "reasoning": null,
  "input": [{"role":"user","content":"continue","exact":900719925474099312345}],
  "client_metadata": {"ws_request_header_x_openai_internal_codex_responses_lite":"true","custom":"keep","x-codex-turn-metadata":"{\"request_kind\":\"turn\",\"turn_started_at_unix_ms\":1234567890}"}
}`,
		},
		{
			name:    "Responses Lite compact",
			compact: true,
			body: `{
  "model": "gpt-5.6-sol",
  "instructions": null,
  "tools": [],
  "parallel_tool_calls": true,
  "reasoning": {"effort":"ultra"},
  "prompt_cache_retention": "24h",
  "max_output_tokens": 123,
  "input": [{"type":"additional_tools","role":"developer","tools":[]},{"role":"user","content":"compact","exact":900719925474099312345}]
}`,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(tt.body)
			source := bodysource.Bytes(raw)
			meta, err := bodysource.ScanJSON(context.Background(), source, nil)
			if err != nil {
				t.Fatal(err)
			}
			spec := Request{
				DownstreamPath: "/v1/responses",
				Body:           source,
				BodyMeta:       &meta,
				Account:        storage.Account{ID: "source-differential"},
				Token:          storage.AccountToken{AccessToken: "oauth-access", RefreshToken: "oauth-refresh"},
				CodexIdentity: &CodexIdentitySnapshot{
					InstallationID:   "installation-fixed",
					SessionID:        "session-fixed",
					ThreadID:         "thread-fixed",
					TurnID:           "turn-fixed",
					WindowGeneration: 7,
				},
			}
			if tt.compact {
				spec.DownstreamPath += "/compact"
			}
			want, err := legacyCodexNormalizedBody(client, spec, whamBaseURL, tt.compact)
			if err != nil {
				t.Fatal(err)
			}
			normalized, err := normalizeCodexSource(client, &spec, whamBaseURL, tt.compact)
			if err != nil {
				t.Fatal(err)
			}
			if !normalized {
				t.Fatal("source normalization unexpectedly fell back")
			}
			got, err := bodysource.ReadAll(spec.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("source bytes differ from legacy chain:\nwant %s\n got %s", want, got)
			}
		})
	}
}

func TestCodexSourceHTTPStripsGenerateAfterClassifyingPrewarm(t *testing.T) {
	for _, tc := range []struct {
		name      string
		websocket bool
		wantField bool
	}{
		{name: "HTTP bridge strips frame control", websocket: false, wantField: false},
		{name: "WebSocket retains frame control", websocket: true, wantField: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"model":"gpt-5.6-sol","generate":false,"stream":true,"input":[{"type":"additional_tools","role":"developer","tools":[]},{"role":"user","content":"keep","exact":900719925474099312345}]}`)
			source := bodysource.Bytes(raw)
			meta, err := bodysource.ScanJSON(context.Background(), source, nil)
			if err != nil {
				t.Fatal(err)
			}
			spec := Request{
				DownstreamPath:          "/v1/responses",
				Body:                    source,
				BodyMeta:                &meta,
				Account:                 storage.Account{ID: "source-generate"},
				Token:                   storage.AccountToken{AccessToken: "oauth-access", RefreshToken: "oauth-refresh"},
				CodexResponsesWebSocket: tc.websocket,
				CodexIdentity: &CodexIdentitySnapshot{
					InstallationID: "installation-fixed", SessionID: "session-fixed", ThreadID: "thread-fixed", WindowGeneration: 0,
				},
			}
			client := NewClient(config.Default())
			normalized, err := normalizeCodexSource(client, &spec, whamBaseURL, false)
			if err != nil || !normalized {
				t.Fatalf("normalize=%v err=%v", normalized, err)
			}
			got, err := bodysource.ReadAll(spec.Body)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err = json.Unmarshal(got, &payload); err != nil {
				t.Fatal(err)
			}
			_, present := payload["generate"]
			if present != tc.wantField {
				t.Fatalf("generate present=%v want=%v body=%s", present, tc.wantField, got)
			}
			if !bytes.Contains(got, []byte(`900719925474099312345`)) {
				t.Fatalf("large context integer changed: %s", got)
			}
			if spec.codexMetadata == nil {
				t.Fatal("Codex metadata missing")
			}
			var turn map[string]interface{}
			if err = json.Unmarshal([]byte(spec.codexMetadata.turnMetadata), &turn); err != nil {
				t.Fatal(err)
			}
			if turn["request_kind"] != "prewarm" {
				t.Fatalf("request_kind=%v metadata=%s", turn["request_kind"], spec.codexMetadata.turnMetadata)
			}
		})
	}
}

func TestCodexSourceClassicParallelToolCallsRequireTools(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tools       string
		wantPresent bool
		wantValue   bool
	}{
		{name: "tools missing"},
		{name: "tools null", tools: `,"tools":null`},
		{name: "tools empty", tools: `,"tools":[]`},
		{name: "Claude Code tool present", tools: `,"tools":[{"type":"function","name":"Bash","parameters":{"type":"object"}}]`, wantPresent: true, wantValue: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"model":"gpt-5.5","instructions":"keep","store":false,"parallel_tool_calls":true` + tc.tools + `,"input":[{"role":"user","content":"keep","exact":900719925474099312345}]}`)
			source := bodysource.Bytes(raw)
			meta, err := bodysource.ScanJSON(context.Background(), source, nil)
			if err != nil {
				t.Fatal(err)
			}
			spec := Request{
				DownstreamPath: "/v1/responses",
				Body:           source,
				BodyMeta:       &meta,
				Account:        storage.Account{ID: "source-parallel"},
				Token:          storage.AccountToken{AccessToken: "sk-api-key"},
			}
			normalized, err := normalizeCodexSource(NewClient(config.Default()), &spec, "https://api.openai.com/v1", false)
			if err != nil || !normalized {
				t.Fatalf("normalize=%v err=%v", normalized, err)
			}
			got, err := bodysource.ReadAll(spec.Body)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err = json.Unmarshal(got, &payload); err != nil {
				t.Fatal(err)
			}
			value, present := payload["parallel_tool_calls"]
			if present != tc.wantPresent {
				t.Fatalf("parallel_tool_calls present=%v, want %v: %s", present, tc.wantPresent, got)
			}
			if present && string(value) != "true" {
				t.Fatalf("parallel_tool_calls=%s, want true: %s", value, got)
			}
			if !bytes.Contains(got, []byte(`900719925474099312345`)) {
				t.Fatalf("large context integer changed: %s", got)
			}
		})
	}
}

func legacyCodexNormalizedBody(client *Client, spec Request, upstreamBaseURL string, compact bool) ([]byte, error) {
	raw, err := bodysource.ReadAll(spec.Body)
	if err != nil {
		return nil, err
	}
	responsesLite := !AccountUsesAPIKey(spec.Token) && CodexRequestUsesResponsesLite(raw)
	if compact && responsesLite {
		raw = normalizeCodexResponsesLiteCompactBody(raw)
	} else if !compact {
		raw = normalizeCodexResponsesBody(raw, upstreamBaseURL, responsesLite)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	raw = normalizeCodexReasoningEffortForWireWithFields(raw, fields)
	raw = stripCodexResponsesPromptCacheRetentionWithFields(raw, fields)
	if !AccountUsesAPIKey(spec.Token) {
		raw = stripCodexResponsesMaxOutputTokensWithFields(raw, fields)
		spec.Body, spec.BodyMeta = bodysource.Bytes(raw), nil
		metadata := client.newCodexRequestMetadataWithResponsesLite(spec, responsesLite)
		if !compact {
			raw = applyCodexClientMetadataWithFields(raw, fields, metadata, spec.CodexResponsesWebSocket)
		}
	}
	if !spec.CodexResponsesWebSocket {
		raw = stripCodexResponsesHTTPGenerateWithFields(raw, fields)
	}
	return stripCodexTopLevelTransportCorrelatorsWithFields(raw, fields), nil
}

func TestSetRequestBodyInvalidatesMetadata(t *testing.T) {
	meta := &bodysource.BodyMeta{Size: 1}
	req := Request{Body: bodysource.Bytes([]byte(`{}`)), BodyMeta: meta}
	setRequestBody(&req, []byte(`{"changed":true}`))
	if req.BodyMeta != nil {
		t.Fatal("body metadata survived replacement")
	}
}

func TestCodexSourceNormalizationPreservesSkillsPluginsAndToolMatrix(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.6-sol","stream":true,"parallel_tool_calls":true,"tool_choice":"auto","skills":[{"id":"skill_repo_review","instructions":"inspect every referenced file","future":{"n":900719925474099312345}}],"plugins":{"browser":{"enabled":true},"mcp":{"server":"workspace-skills"},"future_flag":"keep"},"tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}},{"type":"namespace","name":"calendar","tools":[{"type":"function","name":"create_event","parameters":{"type":"object"}}]},{"type":"custom","name":"apply_patch","format":{"type":"text"}},{"type":"local_shell","name":"shell"},{"type":"mcp","server_label":"workspace-skills"},{"type":"tool_search","execution":"client"},{"type":"future_tool","opaque":{"n":900719925474099312345}}],"input":[{"type":"function_call","call_id":"call_function","name":"read_file","arguments":"{}"},{"type":"function_call_output","call_id":"call_function","output":"ok"},{"type":"custom_tool_call","call_id":"call_custom","name":"apply_patch","input":"*** Begin Patch"},{"type":"custom_tool_call_output","call_id":"call_custom","output":"done"},{"type":"local_shell_call","call_id":"call_shell","action":{"type":"exec","command":"true"}},{"type":"local_shell_call_output","call_id":"call_shell","output":"done"},{"type":"mcp_tool_call","call_id":"call_mcp","server_label":"workspace-skills","name":"lookup","arguments":"{}"},{"type":"mcp_tool_call_output","call_id":"call_mcp","output":"done"},{"type":"tool_search_call","call_id":"call_search","execution":"client","arguments":{"query":"repo"}},{"type":"tool_search_output","call_id":"call_search","execution":"client","status":"completed","tools":[]},{"role":"user","content":"run the installed skill"}],"future_wire_field":{"opaque":true,"n":900719925474099312345}}`)
	source := bodysource.Bytes(raw)
	meta, err := bodysource.ScanJSON(context.Background(), source, []byte("skills-test"))
	if err != nil {
		t.Fatal(err)
	}
	if !meta.ClientToolResult || !meta.ToolContext {
		t.Fatalf("tool semantics missing from metadata: %+v", meta)
	}
	var before map[string]json.RawMessage
	if err = json.Unmarshal(raw, &before); err != nil {
		t.Fatal(err)
	}
	client := NewClient(config.Default())
	spec := Request{
		DownstreamPath: "/v1/responses", Body: source, BodyMeta: &meta,
		Account: storage.Account{ID: "skills-api-key"}, Token: storage.AccountToken{AccessToken: "sk-skills"},
	}
	normalized, err := normalizeCodexSource(client, &spec, "https://api.openai.com/v1", false)
	if err != nil {
		t.Fatal(err)
	}
	if !normalized {
		t.Fatal("skills/tool request fell back to materialized legacy normalization")
	}
	defer spec.Body.Close()
	got, err := bodysource.ReadAll(spec.Body)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]json.RawMessage
	if err = json.Unmarshal(got, &after); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"skills", "plugins", "tools", "input", "parallel_tool_calls", "tool_choice", "future_wire_field"} {
		if !bytes.Equal(before[field], after[field]) {
			t.Fatalf("%s changed across native normalization:\nbefore=%s\nafter=%s\nbody=%s", field, before[field], after[field], got)
		}
	}
	if !bytes.Contains(got, []byte("900719925474099312345")) {
		t.Fatalf("large skill/plugin/tool integer was rounded: %s", got)
	}
}
