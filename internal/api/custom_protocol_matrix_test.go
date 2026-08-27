package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

const protocolMatrixAnthropicResponse = `{
  "id":"msg_matrix","type":"message","role":"assistant","model":"matrix-anthropic",
  "content":[
    {"type":"text","text":"matrix ok"},
    {"type":"tool_use","id":"toolu_matrix","name":"lookup","input":{"city":"New York"}}
  ],
  "stop_reason":"tool_use",
  "usage":{"input_tokens":11,"output_tokens":4,"cache_read_input_tokens":3}
}`

const protocolMatrixAnthropicSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_matrix_stream","model":"matrix-anthropic","usage":{"input_tokens":11,"output_tokens":0,"cache_read_input_tokens":3}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"matrix "}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_matrix","name":"lookup","input":{}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"New York\"}"}}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

const protocolMatrixResponsesResponse = `{
  "id":"resp_matrix","object":"response","model":"matrix-responses","status":"completed",
  "output":[
    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"matrix ok"}]},
    {"type":"function_call","id":"fc_matrix","call_id":"call_matrix","name":"lookup","arguments":"{\"city\":\"New York\"}"}
  ],
  "usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13,"input_tokens_details":{"cached_tokens":2}}
}`

const protocolMatrixResponsesSSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_matrix_stream","model":"matrix-responses"}}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","output_index":0,"delta":"matrix "}` + "\n\n" +
	"event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_matrix","call_id":"call_matrix","name":"lookup","arguments":""}}` + "\n\n" +
	"event: response.function_call_arguments.delta\n" +
	`data: {"type":"response.function_call_arguments.delta","item_id":"fc_matrix","output_index":1,"delta":"{\"city\":\"New York\"}"}` + "\n\n" +
	"event: response.output_item.done\n" +
	`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_matrix","call_id":"call_matrix","name":"lookup","arguments":"{\"city\":\"New York\"}"}}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_matrix_stream","model":"matrix-responses","status":"completed","usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13,"input_tokens_details":{"cached_tokens":2}}}}` + "\n\n"

func setupProtocolMatrixProvider(t *testing.T, h *testHarness, id, protocol, model string) string {
	t.Helper()
	if err := h.store.UpsertCustomProvider(t.Context(), storage.CustomProvider{
		ID: id, Name: id, BaseURL: h.upstream.URL, UpstreamProtocol: protocol,
		Enabled: true, Models: []string{model},
	}); err != nil {
		t.Fatal(err)
	}
	accountID := id + "-account"
	if err := h.store.UpsertAccount(t.Context(), storage.Account{
		ID: accountID, Label: accountID, GroupName: "cyber", Provider: id, Status: "active",
	}, storage.AccountToken{OpenAIAPIKey: "sk-" + id}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(t.Context(), []storage.ModelCapability{{
		AccountID: accountID, ModelSlug: model, Source: "protocol_matrix_test",
	}}); err != nil {
		t.Fatal(err)
	}
	return accountID
}

func assertProtocolMatrixUsage(t *testing.T, h *testHarness, accountID string, prompt, completion, cached int64) {
	t.Helper()
	h.app.WaitForAsyncWrites()
	var gotPrompt, gotCompletion, gotCached int64
	err := h.store.DB().QueryRowContext(context.Background(), `
		SELECT prompt_tokens, completion_tokens, cached_tokens
		FROM usage_records WHERE account_id = ? ORDER BY id DESC LIMIT 1`, accountID).
		Scan(&gotPrompt, &gotCompletion, &gotCached)
	if err != nil {
		t.Fatalf("read protocol matrix usage: %v", err)
	}
	if gotPrompt != prompt || gotCompletion != completion || gotCached != cached {
		t.Fatalf("usage prompt=%d completion=%d cached=%d, want %d/%d/%d",
			gotPrompt, gotCompletion, gotCached, prompt, completion, cached)
	}
}

func capturedProtocolMatrixCall(t *testing.T, h *testHarness, suffix string) capturedRequest {
	t.Helper()
	for i := len(*h.captured) - 1; i >= 0; i-- {
		if strings.HasSuffix((*h.captured)[i].Path, suffix) {
			return (*h.captured)[i]
		}
	}
	t.Fatalf("no upstream %s call captured: %+v", suffix, h.requests())
	return capturedRequest{}
}

// assertProtocolMatrixBillingVersion verifies the upstream body's billing block
// carries a supported fleet-diverse cc_version (never a .NNN build suffix) and the
// expected native entrypoint.
func assertProtocolMatrixBillingVersion(t *testing.T, body, entrypoint string) {
	t.Helper()
	m := claudeProbeBillingRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("billing block missing or malformed: %s", body)
	}
	if m[2] != entrypoint {
		t.Fatalf("billing entrypoint=%s want %s: %s", m[2], entrypoint, body)
	}
	supported := config.SupportedClaudeCLIVersions()
	for _, v := range supported {
		if v == m[1] {
			return
		}
	}
	t.Fatalf("cc_version=%s not a supported Claude Code release %v", m[1], supported)
}

func TestCustomProtocolMatrixChatToAnthropicMessages(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/messages") {
					t.Fatalf("upstream path = %s, want /messages", r.URL.Path)
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, protocolMatrixAnthropicSSE)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, protocolMatrixAnthropicResponse)
			})
			accountID := setupProtocolMatrixProvider(t, h, "matrix-chat-anthropic-"+name, storage.CustomProviderProtocolAnthropicMessages, "matrix-chat-anthropic-"+name)
			body := `{
			  "model":"matrix-chat-anthropic-` + name + `","stream":` + jsonBool(stream) + `,
			  "messages":[
			    {"role":"developer","content":"Be exact."},
			    {"role":"user","content":"Find a city"},
			    {"role":"assistant","tool_calls":[{"id":"call_old","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Boston\"}"}}]},
			    {"role":"tool","tool_call_id":"call_old","content":"found"}
			  ],
			  "tools":[{"type":"function","function":{"name":"lookup","description":"lookup","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]
			}`
			resp, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
			}
			if stream {
				for _, want := range []string{`"chat.completion.chunk"`, `"tool_calls"`, `"lookup"`, `"finish_reason":"tool_calls"`, "data: [DONE]"} {
					if !strings.Contains(string(raw), want) {
						t.Fatalf("Chat SSE missing %q:\n%s", want, raw)
					}
				}
			} else {
				var root map[string]interface{}
				if err := json.Unmarshal(raw, &root); err != nil {
					t.Fatal(err)
				}
				choice := root["choices"].([]interface{})[0].(map[string]interface{})
				message := choice["message"].(map[string]interface{})
				if root["object"] != "chat.completion" || choice["finish_reason"] != "tool_calls" || len(message["tool_calls"].([]interface{})) != 1 {
					t.Fatalf("converted Chat response wrong: %s", raw)
				}
			}
			call := capturedProtocolMatrixCall(t, h, "/messages")
			assertProtocolMatrixBillingVersion(t, call.Body, "cli")
			for _, want := range []string{
				`"text":"You are a Claude agent, built on Anthropic's Claude Agent SDK."`,
				`"text":"Be exact."`, `"type":"tool_use"`, `"type":"tool_result"`, `"input_schema"`,
			} {
				if !strings.Contains(call.Body, want) {
					t.Fatalf("Anthropic request missing %q: %s", want, call.Body)
				}
			}
			if call.Auth != "Bearer sk-matrix-chat-anthropic-"+name {
				t.Fatalf("upstream auth = %q", call.Auth)
			}
			assertProtocolMatrixUsage(t, h, accountID, 11, 4, 3)
		})
	}
}

func TestCustomProtocolMatrixResponsesToAnthropicMessages(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/messages") {
					t.Fatalf("upstream path = %s, want /messages", r.URL.Path)
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, protocolMatrixAnthropicSSE)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, protocolMatrixAnthropicResponse)
			})
			model := "matrix-responses-anthropic-" + name
			accountID := setupProtocolMatrixProvider(t, h, model, storage.CustomProviderProtocolAnthropicMessages, model)
			body := `{
			  "model":"` + model + `","stream":` + jsonBool(stream) + `,"instructions":"Be exact.",
			  "input":[
			    {"role":"user","content":[{"type":"input_text","text":"Find a city"}]},
			    {"type":"function_call","call_id":"call_old","name":"lookup","arguments":"{\"city\":\"Boston\"}"},
			    {"type":"function_call_output","call_id":"call_old","output":"found"}
			  ],
			  "tools":[{"type":"function","name":"lookup","description":"lookup","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]
			}`
			resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
			}
			if stream {
				for _, want := range []string{"response.created", "response.output_item.added", `"type":"function_call"`, `"name":"lookup"`, "response.completed"} {
					if !strings.Contains(string(raw), want) {
						t.Fatalf("Responses SSE missing %q:\n%s", want, raw)
					}
				}
			} else {
				var root map[string]interface{}
				if err := json.Unmarshal(raw, &root); err != nil {
					t.Fatal(err)
				}
				output := root["output"].([]interface{})
				if root["object"] != "response" || root["status"] != "completed" || len(output) != 2 || output[1].(map[string]interface{})["type"] != "function_call" {
					t.Fatalf("converted Responses response wrong: %s", raw)
				}
			}
			call := capturedProtocolMatrixCall(t, h, "/messages")
			assertProtocolMatrixBillingVersion(t, call.Body, "cli")
			for _, want := range []string{
				`"text":"You are a Claude agent, built on Anthropic's Claude Agent SDK."`,
				`"text":"Be exact."`, `"type":"tool_use"`, `"type":"tool_result"`, `"input_schema"`,
			} {
				if !strings.Contains(call.Body, want) {
					t.Fatalf("Anthropic request missing %q: %s", want, call.Body)
				}
			}
			assertProtocolMatrixUsage(t, h, accountID, 11, 4, 3)
		})
	}
}

func TestCustomProtocolMatrixMessagesToResponses(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "sse"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/responses") {
					t.Fatalf("upstream path = %s, want /responses", r.URL.Path)
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, protocolMatrixResponsesSSE)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, protocolMatrixResponsesResponse)
			})
			model := "matrix-messages-responses-" + name
			accountID := setupProtocolMatrixProvider(t, h, model, storage.CustomProviderProtocolResponses, model)
			body := `{
			  "model":"` + model + `","max_tokens":256,"stream":` + jsonBool(stream) + `,
			  "system":"Be exact.",
			  "messages":[
			    {"role":"user","content":"Find a city"},
			    {"role":"assistant","content":[{"type":"tool_use","id":"call_old","name":"lookup","input":{"city":"Boston"}}]},
			    {"role":"user","content":[{"type":"tool_result","tool_use_id":"call_old","content":"found"}]}
			  ],
			  "tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]
			}`
			req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Anthropic-Version", "2023-06-01")
			req.Header.Set("Anthropic-Beta", anthropicContext1MBeta+",prompt-caching-2024-07-31")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
			}
			if stream {
				for _, want := range []string{"message_start", `"text":"matrix "`, `"type":"tool_use"`, `"name":"lookup"`, `"cache_read_input_tokens":2`, "message_stop"} {
					if !strings.Contains(string(raw), want) {
						t.Fatalf("Messages SSE missing %q:\n%s", want, raw)
					}
				}
			} else {
				var root map[string]interface{}
				if err := json.Unmarshal(raw, &root); err != nil {
					t.Fatal(err)
				}
				content := root["content"].([]interface{})
				usage := root["usage"].(map[string]interface{})
				if root["type"] != "message" || root["stop_reason"] != "tool_use" || len(content) != 2 || content[1].(map[string]interface{})["type"] != "tool_use" || usage["cache_read_input_tokens"] != float64(2) {
					t.Fatalf("converted Messages response wrong: %s", raw)
				}
			}
			call := capturedProtocolMatrixCall(t, h, "/responses")
			for _, want := range []string{`"instructions":"Be exact."`, `"type":"function_call"`, `"type":"function_call_output"`, `"parameters"`} {
				if !strings.Contains(call.Body, want) {
					t.Fatalf("Responses request missing %q: %s", want, call.Body)
				}
			}
			if call.Beta != "prompt-caching-2024-07-31" {
				t.Fatalf("upstream Anthropic-Beta = %q, want non-1M beta only", call.Beta)
			}
			assertProtocolMatrixUsage(t, h, accountID, 9, 4, 2)
		})
	}
}

func jsonBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
