package upstream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"codex-account-pool/internal/config"

	"github.com/tidwall/gjson"
)

func TestRefreshAntigravityTokenMatchesNativeOAuthProfile(t *testing.T) {
	cfg := &config.Config{
		AntigravityOAuthClientID:     "client-id",
		AntigravityOAuthClientSecret: "client-secret",
		AntigravityOAuthTokenURL:     "https://oauth2.googleapis.com/token",
	}
	result, err := refreshAntigravityToken(context.Background(), "refresh-token", cfg,
		func(_ context.Context, target string, headers http.Header, body []byte) (*Response, error) {
			if target != cfg.AntigravityOAuthTokenURL {
				t.Fatalf("target = %q", target)
			}
			if got := headers.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
				t.Fatalf("content type = %q", got)
			}
			if got := headers.Get("User-Agent"); got != "Go-http-client/2.0" {
				t.Fatalf("user agent = %q", got)
			}
			form, parseErr := url.ParseQuery(string(body))
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			want := map[string]string{
				"client_id": "client-id", "client_secret": "client-secret",
				"grant_type": "refresh_token", "refresh_token": "refresh-token",
			}
			for key, value := range want {
				if got := form.Get(key); got != value {
					t.Fatalf("form %s = %q, want %q", key, got, value)
				}
			}
			return &Response{
				StatusCode: http.StatusOK,
				Body: io.NopCloser(strings.NewReader(
					`{"access_token":"new-access","refresh_token":"new-refresh","expires_in":1234,"token_type":"Bearer"}`)),
			}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if result.AccessToken != "new-access" || result.RefreshToken != "new-refresh" || result.ExpiresIn != 1234 {
		t.Fatalf("token response = %+v", result)
	}
}

func TestRefreshAntigravityTokenRejectsMissingAccessToken(t *testing.T) {
	cfg := &config.Config{AntigravityOAuthTokenURL: "https://oauth2.googleapis.com/token"}
	_, err := refreshAntigravityToken(context.Background(), "refresh-token", cfg,
		func(context.Context, string, http.Header, []byte) (*Response, error) {
			return &Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"expires_in":3600}`))}, nil
		})
	if err == nil || !strings.Contains(err.Error(), "missing access_token") {
		t.Fatalf("missing access token was accepted: %v", err)
	}
}

func TestAnthropicToAntigravityPreservesStructuredMessages(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-4-8","max_tokens":4096,"stop_sequences":["END"],
		"thinking":{"type":"enabled","budget_tokens":1024},
		"system":[{"type":"text","text":"system one"},{"type":"text","text":"system two"}],
		"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],
		"tool_choice":{"type":"tool","name":"read_file"},
		"messages":[
			{"role":"user","content":[{"type":"text","text":"open it"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]},
			{"role":"assistant","content":[{"type":"thinking","thinking":"inspect","signature":"EgE="},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"/tmp/a"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"ok"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"anBlZw=="}}]}]}
		]
	}`)

	out, err := anthropicToAntigravity(body, "claude-opus-4-8", "project-1")
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		"model":                                  "claude-opus-4-8",
		"project":                                "project-1",
		"request.systemInstruction.parts.0.text": "system one",
		"request.systemInstruction.parts.1.text": "system two",
		"request.contents.0.parts.1.inlineData.mimeType":                          "image/png",
		"request.contents.1.parts.0.thoughtSignature":                             "RWdFPQ==",
		"request.contents.1.parts.1.functionCall.id":                              "toolu_1",
		"request.contents.2.parts.0.functionResponse.name":                        "read_file",
		"request.contents.2.parts.0.functionResponse.parts.0.inlineData.mimeType": "image/jpeg",
		"request.tools.0.functionDeclarations.0.parameters.type":                  "object",
		"request.toolConfig.functionCallingConfig.mode":                           "VALIDATED",
		"request.toolConfig.functionCallingConfig.allowedFunctionNames.0":         "read_file",
		"request.generationConfig.stopSequences.0":                                "END",
	}
	for path, want := range checks {
		if got := gjson.GetBytes(out, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; body=%s", path, got, want, out)
		}
	}
	if got := gjson.GetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget").Int(); got != 1024 {
		t.Fatalf("thinking budget = %d, want 1024", got)
	}
	if session := gjson.GetBytes(out, "request.sessionId").String(); session == "" || !strings.HasPrefix(session, "-") {
		t.Fatalf("generated session id = %q", session)
	}
}

func TestAnthropicToAntigravityDerivesUnknownToolResultName(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"read_file-123-456","content":"result"}]}]}`)
	out, err := anthropicToAntigravity(body, "claude-opus-4-8", "project")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "request.contents.0.parts.0.functionResponse.name").String(); got != "read_file" {
		t.Fatalf("derived function response name = %q, want read_file; body=%s", got, out)
	}
}

func TestAnthropicToAntigravityDropsInvalidClaudeThinkingSignature(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"sig-1"},{"type":"text","text":"visible"}]}]}`)
	out, err := anthropicToAntigravity(body, "claude-opus-4-8", "project")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "request.contents.0.parts.#").Int(); got != 1 {
		t.Fatalf("part count = %d, want only visible text; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "request.contents.0.parts.0.text").String(); got != "visible" {
		t.Fatalf("visible text = %q; body=%s", got, out)
	}
}

func TestAnthropicToAntigravityGeminiToolCallUsesMissingSignatureSentinel(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"read_file","input":{"path":"/tmp/a"}}]}]}`)
	out, err := anthropicToAntigravity(body, "gemini-3.1-pro", "project")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "request.contents.0.parts.0.thoughtSignature").String(); got != "skip_thought_signature_validator" {
		t.Fatalf("tool signature = %q, want missing-signature sentinel; body=%s", got, out)
	}
}

func TestAnthropicToAntigravityValidatedToolSchemasHaveRequiredPlaceholders(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"name":"empty","input_schema":{"type":"object","properties":{}}},
			{"name":"optional","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}
		],
		"messages":[{"role":"user","content":"run"}]
	}`)
	out, err := anthropicToAntigravity(body, "claude-opus-4-8", "project")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(out, "request.toolConfig.functionCallingConfig.mode").String(); got != "VALIDATED" {
		t.Fatalf("tool mode = %q, want VALIDATED; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "request.tools.0.functionDeclarations.0.parameters.required.0").String(); got != "reason" {
		t.Fatalf("empty schema required = %q, want reason; body=%s", got, out)
	}
	if got := gjson.GetBytes(out, "request.tools.0.functionDeclarations.1.parameters.required.0").String(); got != "_" {
		t.Fatalf("optional schema required = %q, want _; body=%s", got, out)
	}
}

func TestAntigravityStreamWrapperThinkingToolAndTerminal(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":"plan","thought":true,"thoughtSignature":"sig"}]}}]}}`,
		`data: {"response":{"candidates":[{"content":{"parts":[{"text":"answer"},{"functionCall":{"id":"call-1","name":"read_file","args":{"path":"/tmp/a"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,"thoughtsTokenCount":1,"cachedContentTokenCount":4}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	in, output, cached, stop, err := AntigravityStreamToAnthropic(context.Background(), strings.NewReader(stream), &out, "claude-opus-4-8", "msg_test")
	if err != nil {
		t.Fatal(err)
	}
	if in != 10 || output != 4 || cached != 4 || stop != "end_turn" {
		t.Fatalf("usage/stop = %d/%d/%d/%q", in, output, cached, stop)
	}
	for _, want := range []string{`"type":"thinking"`, `"type":"signature_delta"`, `"text":"answer"`, `"id":"call-1"`, `event: message_stop`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("stream missing %q: %s", want, out.String())
		}
	}
}

func TestAntigravityTruncatedStreamStillTerminatesDownstream(t *testing.T) {
	stream := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}}\n"
	var out bytes.Buffer
	_, _, _, _, err := AntigravityStreamToAnthropic(context.Background(), strings.NewReader(stream), &out, "claude-opus-4-8", "msg_test")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want unexpected EOF", err)
	}
	if !strings.Contains(out.String(), "partial") || !strings.Contains(out.String(), "event: message_stop") {
		t.Fatalf("truncated stream was not closed cleanly: %s", out.String())
	}
}

func TestProbeAntigravitySSERejectsEmbeddedError(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("data: {\"error\":{\"code\":429,\"message\":\"quota\"}}\n\n"))
	if prefix, err := ProbeAntigravitySSE(reader); err == nil || len(prefix) != 0 {
		t.Fatalf("prefix=%q err=%v, want hidden upstream error", prefix, err)
	}
}

func TestFetchAntigravityModelsUsesAccountCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:fetchAvailableModels" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer access" {
			t.Fatalf("authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != antigravityDefaultUA {
			t.Fatalf("user agent = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		if got := gjson.GetBytes(body, "project").String(); got != "project-1" {
			t.Fatalf("project = %q", got)
		}
		_, _ = io.WriteString(w, `{"models":{"claude-sonnet-5-1":{"displayName":"Claude Sonnet 5.1","maxTokens":1000000,"maxOutputTokens":64000},"gemini-3.1-pro":{"displayName":"Gemini 3.1 Pro","maxTokens":1048576},"chat_20706":{"displayName":"internal"}}}`)
	}))
	defer server.Close()

	models, err := FetchAntigravityModels(context.Background(), "access", "project-1", server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "claude-sonnet-5-1" || models[1].ID != "gemini-3.1-pro" {
		t.Fatalf("models = %+v", models)
	}
}

func TestFetchAntigravityModelsRejectsCapabilityOnlyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"webSearchModelIds":["gemini-3.1-flash-lite"]}`)
	}))
	defer server.Close()

	models, err := FetchAntigravityModels(context.Background(), "access", "project-1", server.URL, "")
	if err == nil || len(models) != 0 || !strings.Contains(err.Error(), "capability hints") {
		t.Fatalf("models=%+v err=%v, want non-authoritative capability-hint error", models, err)
	}
}
