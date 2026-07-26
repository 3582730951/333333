package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestCustomProtocolMalformed2xxReturnsBadGateway(t *testing.T) {
	protocols := []string{
		storage.CustomProviderProtocolChatCompletions,
		storage.CustomProviderProtocolResponses,
		storage.CustomProviderProtocolAnthropicMessages,
	}
	entrypoints := []struct {
		name string
		path string
		body func(string) string
	}{
		{name: "chat", path: "/v1/chat/completions", body: func(model string) string {
			return `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`
		}},
		{name: "responses", path: "/v1/responses", body: func(model string) string {
			return `{"model":"` + model + `","input":"hello"}`
		}},
		{name: "messages", path: "/v1/messages", body: func(model string) string {
			return `{"model":"` + model + `","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`
		}},
	}

	for _, protocol := range protocols {
		for _, entrypoint := range entrypoints {
			t.Run(entrypoint.name+"_via_"+protocol, func(t *testing.T) {
				h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/html")
					_, _ = io.WriteString(w, "<html>upstream failed")
				})
				model := "malformed-" + entrypoint.name + "-" + strings.ReplaceAll(protocol, "_", "-")
				setupProtocolMatrixProvider(t, h, model, protocol, model)

				req, err := http.NewRequest(http.MethodPost, h.pool.URL+entrypoint.path, strings.NewReader(entrypoint.body(model)))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/json")
				if entrypoint.name == "messages" {
					req.Header.Set("Anthropic-Version", "2023-06-01")
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(raw), `"code":"invalid_upstream_response"`) {
					t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
				}
			})
		}
	}
}

func TestCustomProtocolValidationRejectsLossyNestedFieldsBeforeUpstream(t *testing.T) {
	tests := []struct {
		name      string
		protocol  string
		path      string
		body      func(string) string
		anthropic bool
	}{
		{
			name: "invalid_chat_tool_arguments", protocol: storage.CustomProviderProtocolAnthropicMessages,
			path: "/v1/chat/completions",
			body: func(model string) string {
				return `{"model":"` + model + `","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{not-json"}}]}]}`
			},
		},
		{
			name: "native_thinking_signature", protocol: storage.CustomProviderProtocolResponses,
			path: "/v1/messages", anthropic: true,
			body: func(model string) string {
				return `{"model":"` + model + `","max_tokens":128,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"native-signature"},{"type":"text","text":"answer"}]}]}`
			},
		},
		{
			name: "native_redacted_thinking", protocol: storage.CustomProviderProtocolResponses,
			path: "/v1/messages", anthropic: true,
			body: func(model string) string {
				return `{"model":"` + model + `","max_tokens":128,"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"native-redacted"},{"type":"text","text":"answer"}]}]}`
			},
		},
		{
			name: "unknown_anthropic_block", protocol: storage.CustomProviderProtocolResponses,
			path: "/v1/messages", anthropic: true,
			body: func(model string) string {
				return `{"model":"` + model + `","max_tokens":128,"messages":[{"role":"user","content":[{"type":"future_block","value":"must not disappear"}]}]}`
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstreamCalls atomic.Int64
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			})
			model := "validation-" + strings.ReplaceAll(test.name, "_", "-")
			setupProtocolMatrixProvider(t, h, model, test.protocol, model)

			req, err := http.NewRequest(http.MethodPost, h.pool.URL+test.path, strings.NewReader(test.body(model)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if test.anthropic {
				req.Header.Set("Anthropic-Version", "2023-06-01")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(string(raw), `"code":"protocol_conversion_failed"`) {
				t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
			}
			if calls := upstreamCalls.Load(); calls != 0 {
				t.Fatalf("validation happened after %d upstream calls", calls)
			}
		})
	}
}

func TestCustomNativeSSERequiresProtocolTerminal(t *testing.T) {
	tests := []struct {
		name            string
		protocol        string
		path            string
		requestBody     func(string) string
		upstreamBody    string
		wantFailure     string
		forbiddenFinish string
		anthropic       bool
	}{
		{
			name: "chat", protocol: storage.CustomProviderProtocolChatCompletions, path: "/v1/chat/completions",
			requestBody: func(model string) string {
				return `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hello"}]}`
			},
			upstreamBody:    `data: {"id":"chat_partial","object":"chat.completion.chunk","choices":[{"delta":{"content":"partial"}}]}` + "\n\n",
			wantFailure:     `"code":"server_error"`,
			forbiddenFinish: "data: [DONE]",
		},
		{
			name: "responses", protocol: storage.CustomProviderProtocolResponses, path: "/v1/responses",
			requestBody: func(model string) string {
				return `{"model":"` + model + `","stream":true,"input":"hello"}`
			},
			upstreamBody: "event: response.created\n" +
				`data: {"type":"response.created","response":{"id":"resp_partial"}}` + "\n\n",
			wantFailure:     "event: response.failed",
			forbiddenFinish: "event: response.completed",
		},
		{
			name: "messages", protocol: storage.CustomProviderProtocolAnthropicMessages, path: "/v1/messages", anthropic: true,
			requestBody: func(model string) string {
				return `{"model":"` + model + `","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
			},
			upstreamBody: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_partial","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n",
			wantFailure:     "event: error",
			forbiddenFinish: "event: message_stop",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.upstreamBody)
			})
			model := "native-terminal-" + test.name
			setupProtocolMatrixProvider(t, h, model, test.protocol, model)

			req, err := http.NewRequest(http.MethodPost, h.pool.URL+test.path, strings.NewReader(test.requestBody(model)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			if test.anthropic {
				req.Header.Set("Anthropic-Version", "2023-06-01")
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), test.wantFailure) {
				t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
			}
			if strings.Contains(string(raw), test.forbiddenFinish) {
				t.Fatalf("truncated %s stream completed successfully: %s", test.protocol, raw)
			}
		})
	}
}

func TestCustomNativeSSEPrivacyHidesSafetyQuotaAndVendorErrors(t *testing.T) {
	tests := []struct {
		name      string
		protocol  customSSEProtocol
		stream    string
		want      string
		wantRetry bool
		forbidden []string
	}{
		{
			name:     "responses_completed",
			protocol: customSSEResponses,
			stream: "event: response.created\n" +
				`data: {"type":"response.created","response":{"id":"resp-private"},"safety_buffering":{"reason":"policy check"}}` + "\n\n" +
				"event: codex.rate_limits\n" +
				`data: {"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":97,"remaining_percent":3,"plan_type":"pro","reset_at":"tomorrow"}}}` + "\n\n" +
				"event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp-private","status":"completed","output":[]},"safety_buffering":{"reason":"done"}}` + "\n\n",
			want: "response.completed",
			forbidden: []string{
				"safety_buffering", "codex.rate_limits", "used_percent", "remaining_percent", "plan_type", "reset_at", "policy check",
			},
		},
		{
			name:     "responses_error",
			protocol: customSSEResponses,
			stream: "event: response.failed\n" +
				`data: {"type":"response.failed","response":{"id":"resp-vendor-error","status":"failed","error":{"code":"vendor_overloaded","message":"Acme service temporarily unavailable; account has 2% quota remaining and resets tomorrow"}}}` + "\n\n",
			want:      `"code":"server_error"`,
			wantRetry: true,
			forbidden: []string{"acme", "temporarily unavailable", "2%", "quota", "remaining", "resets tomorrow", "vendor_overloaded"},
		},
		{
			name:     "messages_error",
			protocol: customSSEAnthropicMessages,
			stream: "event: error\n" +
				`data: {"type":"error","error":{"type":"overloaded_error","message":"Vendor safety review active; Team plan has 4% remaining"}}` + "\n\n",
			want:      `"type":"api_error"`,
			wantRetry: true,
			forbidden: []string{"vendor", "safety review", "team plan", "4%", "remaining", "overloaded_error"},
		},
		{
			name:      "chat_error",
			protocol:  customSSEChatCompletions,
			stream:    `data: {"error":{"type":"provider_error","code":"account_blocked","message":"Service temporarily unavailable after safety check; quota 1%"}}` + "\n\n",
			want:      `"code":"server_error"`,
			wantRetry: true,
			forbidden: []string{"provider_error", "account_blocked", "temporarily unavailable", "safety check", "quota", "1%"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			terminal, err := streamCopyRewriteValidated(recorder, strings.NewReader(test.stream), nil, test.protocol, true)
			if err != nil {
				t.Fatal(err)
			}
			got := recorder.Body.String()
			if !terminal || !strings.Contains(got, test.want) {
				t.Fatalf("terminal=%v output=%s", terminal, got)
			}
			if test.wantRetry != strings.Contains(got, publicRetryMessage) {
				t.Fatalf("retry message mismatch: %s", got)
			}
			lower := strings.ToLower(got)
			for _, forbidden := range test.forbidden {
				if strings.Contains(lower, strings.ToLower(forbidden)) {
					t.Fatalf("private upstream detail %q leaked: %s", forbidden, got)
				}
			}
		})
	}
}
