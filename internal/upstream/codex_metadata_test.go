package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestCodexRequestUsesResponsesLiteRequiresNativeEnvelope(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"native Lite", `{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[]}]}`, true},
		{"Lite websocket continuation marker", `{"model":"gpt-5.6-sol","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"base"}]},{"role":"user","content":"hi"}],"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`, true},
		{"Lite websocket empty incremental continuation", `{"model":"gpt-5.6-sol","previous_response_id":"resp_warmup","input":[],"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`, true},
		{"Lite marker allows empty top-level tools", `{"model":"gpt-5.6-sol","tools":[],"input":[{"role":"user","content":"hi"}],"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`, true},
		{"Lite marker accepts gateway-added top-level tools", `{"model":"gpt-5.6-sol","tools":[{"type":"web_search"}],"input":[{"role":"user","content":"hi"}],"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`, true},
		{"false Lite marker", `{"model":"gpt-5.6-sol","input":[{"role":"user","content":"hi"}],"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"false"}}`, false},
		{"classic function tools", `{"model":"gpt-5.6-sol","tools":[{"type":"function","name":"run"}],"input":"hi"}`, false},
		{"classic hosted tool", `{"model":"gpt-5.6-sol","tools":[{"type":"web_search"}],"input":"hi"}`, false},
		{"wrong additional tools role", `{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"user","tools":[]}]}`, false},
		{"malformed additional tools", `{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":{}}]}`, false},
		{"classic model with Lite envelope", `{"model":"gpt-5.5","input":[{"type":"additional_tools","role":"developer","tools":[]}]}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CodexRequestUsesResponsesLite([]byte(tt.body)); got != tt.want {
				t.Fatalf("CodexRequestUsesResponsesLite() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAccountUsesAPIKeyRecognizesImportedShape(t *testing.T) {
	tests := []struct {
		name  string
		token storage.AccountToken
		want  bool
	}{
		{"key only", storage.AccountToken{OpenAIAPIKey: "sk-key"}, true},
		{"key mirrored to access", storage.AccountToken{AccessToken: "sk-key", OpenAIAPIKey: "sk-key"}, true},
		{"OAuth access", storage.AccountToken{AccessToken: "oauth-access"}, false},
		{"distinct OAuth access and key", storage.AccountToken{AccessToken: "oauth-access", OpenAIAPIKey: "sk-key"}, false},
		{"mirrored value with refresh evidence", storage.AccountToken{AccessToken: "same", OpenAIAPIKey: "same", RefreshToken: "refresh"}, false},
		{"mirrored value with scopes evidence", storage.AccountToken{AccessToken: "same", OpenAIAPIKey: "same", Scopes: "openid"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AccountUsesAPIKey(tt.token); got != tt.want {
				t.Fatalf("AccountUsesAPIKey() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCodexHTTPFingerprintUsesCanonicalClientMetadata(t *testing.T) {
	var gotHeader http.Header
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp","status":"completed"}`)
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	rawTurn := `{"installation_id":"real-install","session_id":"real-session","thread_id":"real-thread","turn_id":"real-turn","window_id":"real-thread:7","request_kind":"turn","thread_source":"user","sandbox":"workspace-write","workspace_label":"工作区","code_mode_tool_names":{"exec":"functions.exec","search":"tool_search"}}`
	body := []byte(`{"model":"gpt-5.6-sol","store":false,"stream":true,"parallel_tool_calls":false,"reasoning":{"effort":"ultra","context":"all_turns"},"previous_response_id":"resp_keep","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"keep","parameters":{"const":900719925474099312345}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"keep exact context"}]},{"role":"user","content":"do not truncate","exact_id":900719925474099312345}],"client_metadata":{"session_id":"real-session","thread_id":"real-thread","turn_id":"real-turn","x-codex-window-id":"real-thread:7","x-codex-turn-metadata":` + strconvJSON(rawTurn) + `,"custom-safe-key":"keep","custom_exact_id":900719925474099312345}}`)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath: "/v1/responses",
		Headers: http.Header{
			"x-client-request-id": []string{"real-thread"},
			"x-codex-window-id":   []string{"real-thread:7"},
		},
		Body:    testBody(body),
		Account: storage.Account{ID: "acc-http", UpstreamAccountID: "workspace"},
		Token:   storage.AccountToken{AccessToken: "access-http"},
		Egress:  storage.EgressProfile{Type: "direct", Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if gotHeader.Get(codexResponsesLiteHeader) != "true" {
		t.Fatalf("responses-lite header = %q", gotHeader.Get(codexResponsesLiteHeader))
	}
	if gotHeader.Get("session-id") == "" || gotHeader.Get("thread-id") == "" || gotHeader.Get("x-codex-window-id") == "" {
		t.Fatalf("canonical HTTP identity headers missing: %+v", gotHeader)
	}
	if gotHeader.Get("session-id") != gotHeader.Get("thread-id") || !looksLikeUUIDv7(gotHeader.Get("thread-id")) {
		t.Fatalf("Codex session/thread identity is not one shared UUIDv7: %+v", gotHeader)
	}
	if gotHeader.Get("x-codex-beta-features") != codexBetaFeaturesHeader || gotHeader.Get("version") != config.DefaultClientVersion {
		t.Fatalf("shipping Responses fingerprint missing: %+v", gotHeader)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("upstream body: %v\n%s", err, gotBody)
	}
	if payload["model"] != "gpt-5.6-sol" || payload["previous_response_id"] != "resp_keep" {
		t.Fatalf("model/context changed: %+v", payload)
	}
	if _, present := payload["instructions"]; present {
		t.Fatalf("Responses Lite retained top-level instructions: %s", gotBody)
	}
	if _, present := payload["tools"]; present {
		t.Fatalf("Responses Lite retained top-level tools: %s", gotBody)
	}
	items, _ := payload["input"].([]interface{})
	if len(items) != 3 || items[0].(map[string]interface{})["type"] != "additional_tools" || items[1].(map[string]interface{})["role"] != "developer" || items[2].(map[string]interface{})["role"] != "user" {
		t.Fatalf("Responses Lite input envelope malformed: %s", gotBody)
	}
	if !strings.Contains(string(gotBody), `900719925474099312345`) {
		t.Fatalf("Responses Lite context integer was rounded: %s", gotBody)
	}
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if reasoning["effort"] != "max" {
		t.Fatalf("official wire must map client-side ultra to max: %+v", reasoning)
	}
	metadata, _ := payload["client_metadata"].(map[string]interface{})
	if metadata["session_id"] != gotHeader.Get("session-id") ||
		metadata["thread_id"] != gotHeader.Get("thread-id") ||
		metadata["x-codex-window-id"] != gotHeader.Get("x-codex-window-id") ||
		metadata["turn_id"] == "" || metadata["x-codex-installation-id"] == "" {
		t.Fatalf("HTTP body/header metadata drift: body=%+v headers=%+v", metadata, gotHeader)
	}
	if metadata["custom-safe-key"] != "keep" {
		t.Fatalf("non-reserved client metadata was lost: %+v", metadata)
	}
	for _, forbidden := range []string{"real-install", "real-session", "real-thread", "real-turn"} {
		if strings.Contains(string(gotBody), forbidden) {
			t.Fatalf("downstream identity %q leaked in body: %s", forbidden, gotBody)
		}
	}
	if metadata[codexWSResponsesLiteMetadata] != nil || metadata[codexWSRequestStartMetadata] != nil {
		t.Fatalf("WS-only metadata leaked into HTTP body: %+v", metadata)
	}
	turnRaw, _ := metadata["x-codex-turn-metadata"].(string)
	var turn map[string]interface{}
	if json.Unmarshal([]byte(turnRaw), &turn) != nil || turn["request_kind"] != "turn" || turn["turn_started_at_unix_ms"] == nil || turn["sandbox"] != "workspace-write" {
		t.Fatalf("canonical turn metadata malformed: %q", turnRaw)
	}
	if turn["workspace_label"] != "工作区" || turn["code_mode_tool_names"] == nil {
		t.Fatalf("full client_metadata lost current Codex fields: %q", turnRaw)
	}
	headerTurnRaw := gotHeader.Get("x-codex-turn-metadata")
	if headerTurnRaw == "" || strings.Contains(headerTurnRaw, "code_mode_tool_names") {
		t.Fatalf("compatibility header retained unbounded Code Mode data: %q", headerTurnRaw)
	}
	if strings.Contains(headerTurnRaw, "工作区") || !strings.Contains(headerTurnRaw, `\u5de5`) {
		t.Fatalf("compatibility header must be ASCII JSON: %q", headerTurnRaw)
	}
	var headerTurn map[string]interface{}
	if err := json.Unmarshal([]byte(headerTurnRaw), &headerTurn); err != nil || headerTurn["workspace_label"] != "工作区" {
		t.Fatalf("compatibility header is not semantically equivalent: %q err=%v", headerTurnRaw, err)
	}
}

func TestCodexMappedSnapshotDoesNotDeriveUnresolvedForkRelationship(t *testing.T) {
	client := NewClient(config.Default())
	metadata := client.newCodexRequestMetadata(Request{
		DownstreamPath: "/v1/responses",
		Body:           testBody([]byte(`{"model":"gpt-5.6-sol","input":"fork","client_metadata":{"x-codex-turn-metadata":"{\"forked_from_thread_id\":\"downstream-unmapped-fork\",\"thread_id\":\"downstream-thread\"}"}}`)),
		Account:        storage.Account{ID: "acc-mapped"},
		Egress:         storage.EgressProfile{ID: "direct", Type: "direct", Health: "healthy"},
		CodexIdentity: &CodexIdentitySnapshot{
			InstallationID:   "mapped-installation",
			SessionID:        "019f0000-0000-7000-8000-000000000031",
			ThreadID:         "019f0000-0000-7000-8000-000000000031",
			TurnID:           "019f0000-0000-7000-8000-000000000032",
			WindowGeneration: 0,
		},
	})
	if !metadata.mappedIdentity {
		t.Fatal("CPA snapshot was not marked as mapping-managed")
	}
	if strings.Contains(metadata.turnMetadata, "downstream-unmapped-fork") {
		t.Fatalf("raw downstream fork leaked into mapped turn metadata: %s", metadata.turnMetadata)
	}
	var turn map[string]interface{}
	if err := json.Unmarshal([]byte(metadata.turnMetadata), &turn); err != nil {
		t.Fatal(err)
	}
	if _, present := turn["forked_from_thread_id"]; present {
		t.Fatalf("unresolved fork relationship was derived in strict CPA: %+v", turn)
	}
}

func TestCodexHTTPClassicHostedToolDoesNotOptIntoResponsesLite(t *testing.T) {
	var gotHeader http.Header
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"id":"resp","status":"completed"}`)
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	body := []byte(`{"model":"gpt-5.6-sol","instructions":"classic client","store":false,"stream":true,"tools":[{"type":"web_search"}],"input":"search"}`)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath: "/v1/responses",
		Body:           testBody(body),
		Account:        storage.Account{ID: "acc-classic-hosted", UpstreamAccountID: "workspace"},
		Token:          storage.AccountToken{AccessToken: "oauth-access", RefreshToken: "oauth-refresh"},
		Egress:         storage.EgressProfile{Type: "direct", Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotHeader.Get(codexResponsesLiteHeader) != "" {
		t.Fatalf("classic hosted-tool request received Lite header: %+v", gotHeader)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatal(err)
	}
	tools, _ := payload["tools"].([]interface{})
	if payload["instructions"] != "classic client" || len(tools) != 1 || tools[0].(map[string]interface{})["type"] != "web_search" || payload["input"] != "search" {
		t.Fatalf("classic hosted-tool body was rewritten as Lite: %s", gotBody)
	}
}

func TestCodexHTTPAPIKeyPreservesMaxOutputTokens(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"id":"resp","status":"completed"}`)
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.OpenAIAPIUpstreamBaseURL = server.URL + "/v1"
	client := NewClient(cfg)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath: "/v1/responses",
		Body:           testBody([]byte(`{"model":"gpt-5.6-sol","instructions":"API request","store":false,"stream":true,"max_output_tokens":64000,"input":"hi"}`)),
		Account:        storage.Account{ID: "acc-api-key"},
		Token:          storage.AccountToken{AccessToken: "sk-api-key", OpenAIAPIKey: "sk-api-key"},
		Egress:         storage.EgressProfile{Type: "direct", Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var payload map[string]interface{}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["max_output_tokens"] != float64(64000) {
		t.Fatalf("standard API-key Responses request lost max_output_tokens: %s", gotBody)
	}
}

func TestCodexHTTPPromptCacheControlsFollowTransportCapability(t *testing.T) {
	baseBody := `{"model":"gpt-5.6-sol","instructions":"API request","store":false,"stream":false,"prompt_cache_key":"stable","prompt_cache_options":{"mode":"explicit"},"input":[{"role":"developer","content":[{"type":"input_text","text":"stable","prompt_cache_breakpoint":{"mode":"explicit"}}]},{"role":"user","content":[{"type":"input_text","text":"question"}]}]}`
	tests := []struct {
		name       string
		token      storage.AccountToken
		mode       string
		capable    bool
		profitable bool
		wantMode   string
		autoBody   bool
	}{
		{name: "chatgpt strips", token: storage.AccountToken{AccessToken: "oauth"}, mode: "auto", capable: true, profitable: true},
		{name: "unprobed api key strips", token: storage.AccountToken{AccessToken: "sk-test", OpenAIAPIKey: "sk-test"}, mode: "observe"},
		{name: "probed api key preserves client explicit", token: storage.AccountToken{AccessToken: "sk-test", OpenAIAPIKey: "sk-test"}, mode: "observe", capable: true, wantMode: "explicit"},
		{name: "profitable auto marks implicit", token: storage.AccountToken{AccessToken: "sk-test", OpenAIAPIKey: "sk-test"}, mode: "auto", capable: true, profitable: true, wantMode: "implicit", autoBody: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				_, _ = io.WriteString(w, `{"id":"resp","status":"completed"}`)
			}))
			defer server.Close()
			cfg := config.Default()
			cfg.UpstreamBaseURL = server.URL
			cfg.OpenAIAPIUpstreamBaseURL = server.URL + "/v1"
			client := NewClient(cfg)
			requestBody := baseBody
			if tt.autoBody {
				requestBody = `{"model":"gpt-5.6-sol","instructions":"API request","store":false,"stream":false,"prompt_cache_key":"stable","input":[{"role":"developer","content":[{"type":"input_text","text":"stable"}]},{"role":"user","content":[{"type":"input_text","text":"question"}]}]}`
			}
			resp, err := client.Do(context.Background(), Request{
				DownstreamPath: "/v1/responses", Model: "gpt-5.6-sol", Body: testBody([]byte(requestBody)),
				Account: storage.Account{ID: "acc"}, Token: tt.token, Egress: storage.EgressProfile{Type: "direct", Health: "healthy"},
				CodexExplicitCacheMode: tt.mode, CodexExplicitCacheCapable: tt.capable, CodexAutomaticCacheProfitable: tt.profitable,
			})
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			var payload map[string]interface{}
			if err := json.Unmarshal(gotBody, &payload); err != nil {
				t.Fatal(err)
			}
			options, _ := payload["prompt_cache_options"].(map[string]interface{})
			if tt.wantMode == "" {
				if options != nil || bytes.Contains(gotBody, []byte(`"prompt_cache_breakpoint"`)) {
					t.Fatalf("unsupported controls survived: %s", gotBody)
				}
			} else if options["mode"] != tt.wantMode || !bytes.Contains(gotBody, []byte(`"prompt_cache_breakpoint":{"mode":"`+tt.wantMode+`"}`)) {
				t.Fatalf("cache mode %q not forwarded: %s", tt.wantMode, gotBody)
			}
		})
	}
}

func TestCodexPrewarmOmitsTurnIDAndStartTimestamp(t *testing.T) {
	client := NewClient(config.Default())
	metadata := client.newCodexRequestMetadata(Request{
		DownstreamPath: "/v1/responses",
		Headers:        http.Header{"x-client-request-id": []string{"019f4a5e-85d5-7ff2-b047-d392ac030c3d"}},
		Body:           testBody([]byte(`{"model":"gpt-5.6-sol","generate":false,"input":[{"type":"additional_tools","role":"developer","tools":[]}]}`)),
		Account:        storage.Account{ID: "acc-prewarm"},
	})
	if metadata.sessionID != metadata.threadID || !looksLikeUUIDv7(metadata.threadID) {
		t.Fatalf("prewarm session/thread IDs malformed: %+v", metadata)
	}
	if metadata.threadID[:13] != "019f4a5e-85d5" {
		t.Fatalf("downstream UUIDv7 timestamp was not preserved: %q", metadata.threadID)
	}
	if metadata.turnID != "" {
		t.Fatalf("prewarm turn ID = %q, want empty", metadata.turnID)
	}
	var turn map[string]interface{}
	if err := json.Unmarshal([]byte(metadata.turnMetadata), &turn); err != nil {
		t.Fatal(err)
	}
	if turn["request_kind"] != "prewarm" {
		t.Fatalf("prewarm turn metadata mismatch: %+v", turn)
	}
	if _, present := turn["turn_id"]; present {
		t.Fatalf("prewarm must omit turn_id: %+v", turn)
	}
	if _, present := turn["turn_started_at_unix_ms"]; present {
		t.Fatalf("prewarm must omit turn_started_at_unix_ms: %+v", turn)
	}
	original := []byte(`{"model":"gpt-5.6-sol","generate":false,"previous_response_id":"resp_keep","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"keep","parameters":{"const":900719925474099312345}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"keep prewarm"}]},{"role":"user","content":"hi","exact_id":900719925474099312345}]}`)
	body := applyCodexClientMetadata(original, metadata, true)
	assertCodexContextFieldsUnchanged(t, original, body)
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	clientMetadata, _ := payload["client_metadata"].(map[string]interface{})
	if _, present := clientMetadata["turn_id"]; present {
		t.Fatalf("prewarm client_metadata must omit turn_id: %+v", clientMetadata)
	}
	payloadBody, err := buildCodexWebSocketCreatePayload(body, metadata)
	if err != nil {
		t.Fatal(err)
	}
	var payloadRoot map[string]interface{}
	if err := json.Unmarshal(payloadBody, &payloadRoot); err != nil {
		t.Fatal(err)
	}
	if _, present := payloadRoot["instructions"]; present {
		t.Fatalf("prewarm Lite payload retained top-level instructions: %s", payloadBody)
	}
	if _, present := payloadRoot["tools"]; present {
		t.Fatalf("prewarm Lite payload retained top-level tools: %s", payloadBody)
	}
	items, _ := payloadRoot["input"].([]interface{})
	if len(items) != 3 || items[0].(map[string]interface{})["type"] != "additional_tools" {
		t.Fatalf("prewarm Lite input envelope malformed: %s", payloadBody)
	}
	if !strings.Contains(string(payloadBody), `900719925474099312345`) {
		t.Fatalf("prewarm Lite context integer was rounded: %s", payloadBody)
	}
	stamped := stampCodexWebSocketRequestStart(payloadBody)
	assertCodexContextFieldsUnchanged(t, payloadBody, stamped)
	var stampedRoot map[string]interface{}
	if err := json.Unmarshal(stamped, &stampedRoot); err != nil {
		t.Fatal(err)
	}
	stampedMetadata, _ := stampedRoot["client_metadata"].(map[string]interface{})
	if _, present := stampedMetadata["turn_id"]; present {
		t.Fatalf("prewarm WS payload must omit turn_id: %+v", stampedMetadata)
	}
	if stampedMetadata[codexWSRequestStartMetadata] == "" {
		t.Fatalf("prewarm WS payload missing request-start metadata: %+v", stampedMetadata)
	}
}

func TestCodexHTTPBridgeStripsGenerateAfterClassifyingPrewarm(t *testing.T) {
	var gotHeader http.Header
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http_prewarm\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	original := []byte(`{"model":"gpt-5.6-sol","generate":false,"input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"keep prewarm context"}]}],"stream":true}`)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath: "/v1/responses",
		Body:           testBody(original),
		Account:        storage.Account{ID: "acc-http-prewarm"},
		Token:          storage.AccountToken{AccessToken: "access-http-prewarm", RefreshToken: "refresh-http-prewarm"},
		Egress:         storage.EgressProfile{Type: "direct", Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatal(err)
	}
	if _, present := payload["generate"]; present {
		t.Fatalf("HTTPS bridge leaked WebSocket-only generate: %s", gotBody)
	}
	if !bytes.Contains(payload["input"], []byte("keep prewarm context")) {
		t.Fatalf("HTTPS bridge lost prewarm input: %s", gotBody)
	}
	var turn map[string]interface{}
	if err := json.Unmarshal([]byte(gotHeader.Get("X-Codex-Turn-Metadata")), &turn); err != nil {
		t.Fatalf("turn metadata decode: %v header=%q", err, gotHeader.Get("X-Codex-Turn-Metadata"))
	}
	if turn["request_kind"] != "prewarm" {
		t.Fatalf("generate:false classification was lost before HTTP strip: %+v", turn)
	}
}

func TestStripCodexTopLevelTransportCorrelatorsPreservesContext(t *testing.T) {
	original := []byte(`{"model":"gpt-5.6-sol","thread_id":"thread-downstream","session_id":"session-downstream","conversation_id":"conversation-downstream","client_version":"0.148.0","instructions":"keep","previous_response_id":"resp_keep","tools":[{"schema":{"const":900719925474099312345}}],"input":[{"exact_id":900719925474099312345}]}`)
	got := stripCodexTopLevelTransportCorrelators(original)
	assertCodexContextFieldsUnchanged(t, original, got)
	var root map[string]json.RawMessage
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"thread_id", "session_id", "conversation_id", "client_version"} {
		if _, present := root[key]; present {
			t.Fatalf("unsupported top-level field %q survived: %s", key, got)
		}
	}
}

func TestCodexCompactFingerprintKeepsMetadataOutOfBody(t *testing.T) {
	var gotHeader http.Header
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"output":[]}`)
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	original := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[]}],"parallel_tool_calls":false,"reasoning":{"context":"all_turns"}}`)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath: "/v1/responses/compact",
		Body:           testBody(original),
		Account:        storage.Account{ID: "acc-compact"},
		Token:          storage.AccountToken{AccessToken: "access-compact"},
		Egress:         storage.EgressProfile{Type: "direct", Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if string(gotBody) != string(original) {
		t.Fatalf("compact body gained normal-turn fields:\nwant %s\n got %s", original, gotBody)
	}
	if gotHeader.Get(codexInstallationIDMetadataKey) == "" || gotHeader.Get("session-id") == "" || gotHeader.Get("x-codex-turn-metadata") == "" {
		t.Fatalf("compact compatibility headers missing: %+v", gotHeader)
	}
	if gotHeader.Get(codexResponsesLiteHeader) != "true" {
		t.Fatalf("compact responses-lite header = %q", gotHeader.Get(codexResponsesLiteHeader))
	}
}

func TestCodexClassicCompactDoesNotOptIntoResponsesLite(t *testing.T) {
	var gotHeader http.Header
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"output":[]}`)
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	original := []byte(`{"model":"gpt-5.6-sol","input":[],"instructions":"classic compact","tools":[{"type":"web_search"}]}`)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath: "/v1/responses/compact",
		Body:           testBody(original),
		Account:        storage.Account{ID: "acc-classic-compact"},
		Token:          storage.AccountToken{AccessToken: "access-classic-compact"},
		Egress:         storage.EgressProfile{Type: "direct", Health: "healthy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !bytes.Equal(gotBody, original) {
		t.Fatalf("classic compact body changed:\nwant %s\n got %s", original, gotBody)
	}
	if gotHeader.Get(codexResponsesLiteHeader) != "" {
		t.Fatalf("classic compact unexpectedly opted into Responses Lite: %+v", gotHeader)
	}
}

func strconvJSON(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func looksLikeUUIDv7(value string) bool {
	return len(value) == 36 && value[8] == '-' && value[13] == '-' && value[14] == '7' && value[18] == '-' &&
		(value[19] == '8' || value[19] == '9' || value[19] == 'a' || value[19] == 'b') && value[23] == '-'
}
