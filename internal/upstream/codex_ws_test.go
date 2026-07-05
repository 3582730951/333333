package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
	"github.com/gorilla/websocket"
)

func TestCodexResponsesWebSocketBridge(t *testing.T) {
	var gotPath, gotMethod string
	var gotHeaders http.Header
	var gotPayload map[string]interface{}

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotHeaders = r.Header.Clone()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		messageType, raw, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read websocket request: %v", err)
		}
		if messageType != websocket.TextMessage {
			t.Fatalf("message type = %d", messageType)
		}
		if err := json.Unmarshal(raw, &gotPayload); err != nil {
			t.Fatalf("payload json: %v\n%s", err, raw)
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_1"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`))
	}))
	defer server.Close()

	cfg := config.Default()
	cfg.UpstreamBaseURL = server.URL + "/backend-api/codex"
	client := NewClient(cfg)
	resp, err := client.Do(context.Background(), Request{
		DownstreamPath:          "/v1/responses",
		Headers:                 http.Header{"Originator": []string{"codex_exec"}, "x-client-request-id": []string{"thread-123"}},
		Body:                    []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":"hi"}],"stream":true,"prompt_cache_retention":"24h"}`),
		Account:                 storage.Account{ID: "acc-ws", UpstreamAccountID: "workspace-should-not-leak"},
		Token:                   storage.AccountToken{AccessToken: "access-ws"},
		Egress:                  storage.EgressProfile{Type: "direct", Health: "healthy"},
		CodexClientVersion:      config.DefaultClientVersion,
		CodexResponsesWebSocket: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if gotMethod != http.MethodGet || gotPath != "/v1/responses" {
		t.Fatalf("handshake = %s %s", gotMethod, gotPath)
	}
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response status/header = %d %+v", resp.StatusCode, resp.Header)
	}
	out := string(body)
	for _, want := range []string{"event: response.created", "event: response.completed", "data: [DONE]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("SSE body missing %q:\n%s", want, out)
		}
	}
	if gotHeaders.Get("OpenAI-Beta") != codexResponsesWebSocketBeta {
		t.Fatalf("OpenAI-Beta = %q", gotHeaders.Get("OpenAI-Beta"))
	}
	if v := gotHeaders.Get("version"); v != "" {
		t.Fatalf("real Codex sends no `version` header; got %q", v)
	}
	if ua := gotHeaders.Get("User-Agent"); !strings.Contains(ua, "codex_exec/"+config.DefaultClientVersion) {
		t.Fatalf("UA = %q", ua)
	}
	if gotHeaders.Get("Originator") != "codex_exec" {
		t.Fatalf("Originator = %q", gotHeaders.Get("Originator"))
	}
	if gotHeaders.Get("session-id") == "" || gotHeaders.Get("thread-id") != "thread-123" || gotHeaders.Get("x-codex-window-id") != "thread-123:0" {
		t.Fatalf("ws ids missing/mismatched: %+v", gotHeaders)
	}
	// The real WS handshake uses lowercase session-id/thread-id (never a capitalized
	// "Session_id") and DOES carry ChatGPT-Account-ID (it rides the auth headers).
	if gotHeaders.Get("Session_id") != "" {
		t.Fatalf("stale capitalized Session_id leaked into WS handshake: %+v", gotHeaders)
	}
	if gotHeaders.Get("ChatGPT-Account-ID") != "workspace-should-not-leak" {
		t.Fatalf("WS handshake must carry ChatGPT-Account-ID via auth: %+v", gotHeaders)
	}

	if gotPayload["type"] != "response.create" || gotPayload["model"] != "gpt-5.5" || gotPayload["stream"] != true {
		t.Fatalf("bad response.create payload: %+v", gotPayload)
	}
	if gotPayload["prompt_cache_retention"] != "24h" {
		t.Fatalf("WS path should preserve prompt_cache_retention, got payload: %+v", gotPayload)
	}
	metadata, _ := gotPayload["client_metadata"].(map[string]interface{})
	if metadata["x-codex-window-id"] != "thread-123:0" || metadata["x-codex-turn-metadata"] == "" {
		t.Fatalf("missing websocket client metadata: %+v", metadata)
	}
}

func TestCodexWebSocketPayloadPreservesPreviousResponseID(t *testing.T) {
	client := NewClient(config.Default())
	payload, err := buildCodexWebSocketCreatePayload([]byte(`{"model":"gpt","previous_response_id":"resp_real","session_id":"downstream-session","thread_id":"downstream-thread","input":"hi"}`), codexWebSocketIDs{
		installationID: "install-1",
		sessionID:      "session-derived",
		threadID:       "thread-derived",
		windowID:       "thread-derived:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := client.codexWSNamespacePayload(Request{Account: storage.Account{ID: "acc"}}, codexWebSocketIDs{
		sessionID: "session-derived",
		threadID:  "thread-derived",
	}, payload)
	var root map[string]interface{}
	if err := json.Unmarshal(out, &root); err != nil {
		t.Fatalf("payload json: %v\n%s", err, out)
	}
	if root["session_id"] != "session-derived" || root["thread_id"] != "thread-derived" {
		t.Fatalf("session/thread ids should still be namespaced: %+v", root)
	}
	if root["previous_response_id"] != "resp_real" {
		t.Fatalf("previous_response_id must pass through unchanged, got %+v", root)
	}
}
