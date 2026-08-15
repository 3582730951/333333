package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCustomMidStream429WithoutFallbackTerminatesAndLeavesServerResponsive(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				`data: {"id":"chatcmpl-midstream","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n"+
					`data: {"error":{"type":"rate_limit_error","code":429,"message":"provider quota detail"}}`+"\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-after-429","object":"chat.completion","model":"deepseek-chat","choices":[{"index":0,"message":{"role":"assistant","content":"healthy"},"finish_reason":"stop"}]}`)
	})
	setupDeepSeek(t, h, []string{"deepseek-chat"}, false)

	started := time.Now()
	first, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"deepseek-chat","stream":true,"messages":[{"role":"user","content":"trigger"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	firstBody, readErr := io.ReadAll(first.Body)
	first.Body.Close()
	if readErr != nil || time.Since(started) > 2*time.Second || first.StatusCode != http.StatusOK {
		t.Fatalf("mid-stream 429 did not terminate promptly status=%d bytes=%d err=%v elapsed=%s", first.StatusCode, len(firstBody), readErr, time.Since(started))
	}
	if !bytes.Contains(firstBody, []byte("partial")) || len(firstBody) > 64<<10 {
		t.Fatalf("mid-stream terminal looped or discarded committed content: bytes=%d body=%s", len(firstBody), firstBody)
	}

	second, err := http.Post(h.pool.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"deepseek-chat","messages":[{"role":"user","content":"health after 429"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	secondBody, readErr := io.ReadAll(second.Body)
	second.Body.Close()
	if readErr != nil || second.StatusCode != http.StatusOK || !bytes.Contains(secondBody, []byte("healthy")) {
		t.Fatalf("server was not responsive after mid-stream 429 status=%d body=%s err=%v", second.StatusCode, secondBody, readErr)
	}
}

func TestEmptyCompletedResponsesFailsOverBeforeDownstreamCommit(t *testing.T) {
	var calls atomic.Int32
	var firstAuthorization atomic.Value
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			firstAuthorization.Store(r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				"event: response.created\n"+
					`data: {"type":"response.created","response":{"id":"resp_empty"}}`+"\n\n"+
					"event: response.completed\n"+
					`data: {"type":"response.completed","response":{"id":"resp_empty","status":"completed","output":[]}}`+"\n\n")
			return
		}
		if got, _ := firstAuthorization.Load().(string); got == r.Header.Get("Authorization") {
			t.Errorf("empty completion retried the same account: %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: response.output_text.delta\n"+
				`data: {"type":"response.output_text.delta","delta":"healthy-after-empty"}`+"\n\n"+
				"event: response.completed\n"+
				`data: {"type":"response.completed","response":{"id":"resp_healthy","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"healthy-after-empty"}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`+"\n\n")
	})
	h.importAccount(t, "empty-completion-a", "acct-empty-completion-a", "access-empty-completion-a")
	h.importAccount(t, "empty-completion-b", "acct-empty-completion-b", "access-empty-completion-b")

	response, err := http.Post(h.pool.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":[{"role":"user","content":"do real work"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response status=%d err=%v body=%s", response.StatusCode, readErr, body)
	}
	if calls.Load() != 2 || !bytes.Contains(body, []byte("healthy-after-empty")) || bytes.Contains(body, []byte("resp_empty")) {
		t.Fatalf("empty completion was committed or not failed over calls=%d body=%s", calls.Load(), body)
	}
}

// Regression coverage for Sub2API #5563/#5652: some providers return HTTP 200
// and carry a statusless overload only in the first SSE error frame. That frame
// must remain pre-commit, cool the affected account, and rotate credentials.
func TestStatuslessResponsesSSEOverloadFailsOverToDifferentAccount(t *testing.T) {
	var calls atomic.Int32
	var firstAuthorization atomic.Value
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			firstAuthorization.Store(r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				"event: error\n"+
					`data: {"type":"error","error":{"type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}`+"\n\n")
			return
		}
		if got, _ := firstAuthorization.Load().(string); got == r.Header.Get("Authorization") {
			t.Errorf("statusless overload retried the same account: %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: response.output_text.delta\n"+
				`data: {"type":"response.output_text.delta","delta":"healthy-after-overload"}`+"\n\n"+
				"event: response.completed\n"+
				`data: {"type":"response.completed","response":{"id":"resp_overload_recovered","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"healthy-after-overload"}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`+"\n\n")
	})
	h.importAccount(t, "overload-a", "acct-overload-a", "access-overload-a")
	h.importAccount(t, "overload-b", "acct-overload-b", "access-overload-b")

	response, err := http.Post(h.pool.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5.6-sol","stream":true,"input":[{"role":"user","content":"continue under overload"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response status=%d err=%v body=%s", response.StatusCode, readErr, body)
	}
	if calls.Load() != 2 || !bytes.Contains(body, []byte("healthy-after-overload")) || bytes.Contains(bytes.ToLower(body), []byte("overloaded")) {
		t.Fatalf("overload was committed or not failed over calls=%d body=%s", calls.Load(), body)
	}
}

// Regression coverage for CLIProxyAPI #4886. Anthropic documents an empty
// end_turn as a model judgement: retrying the identical turn or rotating a
// credential repeats it. Add one new user continuation on the same account
// before any empty terminal is committed downstream.
func TestClaudeEmptyEndTurnContinuesOnSameAccount(t *testing.T) {
	var calls atomic.Int32
	var authorization atomic.Value
	var continuationBody atomic.Value
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := calls.Add(1)
		if call == 1 {
			authorization.Store(r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				"event: message_start\n"+
					`data: {"type":"message_start","message":{"id":"msg_empty","role":"assistant","content":[]}}`+"\n\n"+
					"event: message_delta\n"+
					`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`+"\n\n"+
					"event: message_stop\n"+
					`data: {"type":"message_stop"}`+"\n\n")
			return
		}
		continuationBody.Store(string(body))
		if got, _ := authorization.Load().(string); got != r.Header.Get("Authorization") {
			t.Errorf("empty end_turn rotated credentials: first=%q second=%q", got, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: message_start\n"+
				`data: {"type":"message_start","message":{"id":"msg_recovered","role":"assistant","content":[]}}`+"\n\n"+
				"event: content_block_start\n"+
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n"+
				"event: content_block_delta\n"+
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"recovered-after-empty"}}`+"\n\n"+
				"event: content_block_stop\n"+
				`data: {"type":"content_block_stop","index":0}`+"\n\n"+
				"event: message_delta\n"+
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`+"\n\n"+
				"event: message_stop\n"+
				`data: {"type":"message_stop"}`+"\n\n")
	})
	h.importAccount(t, "claude-empty", "", "sk-ant-oat-empty")

	response, err := http.Post(h.pool.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"do the task"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response status=%d err=%v body=%s", response.StatusCode, readErr, body)
	}
	continued, _ := continuationBody.Load().(string)
	if calls.Load() != 2 || !strings.Contains(continued, h.app.autoContinueText(context.Background())) {
		t.Fatalf("same-account continuation missing calls=%d body=%s", calls.Load(), continued)
	}
	if !bytes.Contains(body, []byte("recovered-after-empty")) || bytes.Contains(body, []byte("msg_empty")) {
		t.Fatalf("empty terminal was committed or recovery missing: %s", body)
	}
}

func TestOversizedFirstDownstreamWebSocketTurnUsesHTTPSBridge(t *testing.T) {
	var webSocketAttempts atomic.Int32
	var httpAttempts atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			webSocketAttempts.Add(1)
			http.Error(w, "oversized frame must not reach upstream websocket", http.StatusBadGateway)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %s", r.Method)
			return
		}
		httpAttempts.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read bridged body: %v", err)
			return
		}
		if len(body) <= codexResponsesWebSocketHTTPBridgeThreshold || !bytes.Contains(body, []byte("oversized-root-marker")) {
			t.Errorf("bridged body bytes=%d marker=%v", len(body), bytes.Contains(body, []byte("oversized-root-marker")))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			"event: response.completed\n"+
				`data: {"type":"response.completed","response":{"id":"resp_large_http","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"bridged"}]}],"usage":{"input_tokens":4000000,"output_tokens":1,"total_tokens":4000001}}}`+"\n\n")
	})
	h.importAccount(t, "oversized-ws-root", "acct-oversized-ws-root", "access-oversized-ws-root")

	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/v1/responses"
	conn, handshake, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if handshake != nil && handshake.Body != nil {
			raw, _ := io.ReadAll(handshake.Body)
			handshake.Body.Close()
			t.Fatalf("downstream handshake: %v status=%d body=%s", err, handshake.StatusCode, raw)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	payload := fmt.Sprintf(`{"type":"response.create","model":"gpt-5.5","stream":true,"input":[{"role":"user","content":"oversized-root-marker:%s"}]}`,
		strings.Repeat("x", codexResponsesWebSocketHTTPBridgeThreshold+1024))
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, terminal, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if webSocketAttempts.Load() != 0 || httpAttempts.Load() != 1 || !bytes.Contains(terminal, []byte("resp_large_http")) {
		t.Fatalf("ws_attempts=%d http_attempts=%d terminal=%s", webSocketAttempts.Load(), httpAttempts.Load(), terminal)
	}
}
