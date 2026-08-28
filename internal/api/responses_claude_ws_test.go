package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func dialClaudeBridgeWS(t *testing.T, poolURL string) *websocket.Conn {
	t.Helper()
	wsURL := strings.Replace(poolURL, "http://", "ws://", 1) + "/v1/responses"
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.Dial(wsURL, http.Header{})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial failed: %v (status=%d)", err, status)
	}
	return conn
}

func readClaudeBridgeWSFrames(t *testing.T, conn *websocket.Conn, limit int) string {
	t.Helper()
	var frames []string
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	for i := 0; i < limit; i++ {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			break
		}
		frames = append(frames, string(raw))
		if strings.Contains(string(raw), "response.completed") ||
			strings.Contains(string(raw), "response.failed") ||
			strings.Contains(string(raw), `"type":"error"`) {
			break
		}
	}
	return strings.Join(frames, "\n")
}

// GPT over the WebSocket transport must be untouched by the Claude relay branch.
// handleGatewayWebSocketTurn re-enters handleGatewayPost for every turn, so that
// dispatch decision is on this path too — a mistake there would break Codex CLI's
// WebSocket transport entirely, which the HTTP SSE tests would not catch.
func TestGPTOverWebSocketTransportUnaffectedByClaudeRelay(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		serveCodexResponsesFixture(t, w, r,
			"event: response.created\n"+
				`data: {"type":"response.created","response":{"id":"resp_ws_gpt","model":"gpt-5.6-sol"}}`+"\n\n"+
				"event: response.output_text.delta\n"+
				`data: {"type":"response.output_text.delta","delta":"ws gpt ok"}`+"\n\n"+
				"event: response.completed\n"+
				`data: {"type":"response.completed","response":{"id":"resp_ws_gpt","model":"gpt-5.6-sol","status":"completed","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`+"\n\n")
	})
	h.importAccount(t, "ws-gpt-guard", "upstream-ws-gpt-guard", "access-ws-gpt-guard")

	conn := dialClaudeBridgeWS(t, h.pool.URL)
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","stream":true,"store":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ws probe"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	joined := readClaudeBridgeWSFrames(t, conn, 24)
	if strings.TrimSpace(joined) == "" {
		t.Fatal("GPT over WebSocket produced no frames: the dispatch change broke the Codex WS path")
	}
	if !strings.Contains(joined, "ws gpt ok") {
		t.Fatalf("GPT/WS lost the model text:\n%s", joined)
	}
	if !strings.Contains(joined, "response.completed") {
		t.Fatalf("GPT/WS never reached a terminal event:\n%s", joined)
	}
	// The Claude bridge must not have been involved: it would rewrite ids to msg_*.
	if strings.Contains(joined, "msg_resp_") {
		t.Fatalf("a GPT request was routed through the Claude bridge:\n%s", joined)
	}
}

// Codex CLI can use a WebSocket transport instead of HTTP SSE, and
// handleGatewayWebSocketTurn re-enters handleGatewayPost for every turn. A Claude model
// on that transport therefore composes responsesClaudeResponseWriter over
// responsesWebSocketWriter: the bridge emits Responses SSE bytes, and the WebSocket
// writer reframes each complete event as one message. This guards that composition —
// without it, a change to either writer could silently drop Claude support on the
// WebSocket transport while the HTTP SSE tests kept passing.
func TestResponsesViaClaudeOverWebSocketTransport(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_ws_bridge","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"usage":{"input_tokens":4,"output_tokens":0}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ws bridge ok"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		} {
			_, _ = w.Write([]byte(frame + "\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
	seedClaudeResponsesAccount(t, h, "responses-claude-ws")

	conn := dialClaudeBridgeWS(t, h.pool.URL)
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"claude-opus-4-8","stream":true,"store":false,"include":["reasoning.encrypted_content"],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"ws probe"}]}]}`)); err != nil {
		t.Fatal(err)
	}
	joined := readClaudeBridgeWSFrames(t, conn, 24)
	if strings.TrimSpace(joined) == "" {
		t.Fatal("Claude over the WebSocket transport produced no frames")
	}
	// Each WebSocket message must be one Responses event, not raw Anthropic SSE.
	for _, want := range []string{
		"response.created", "response.output_text.delta", "response.completed",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("WebSocket stream missing %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "ws bridge ok") {
		t.Fatalf("model text did not survive the WebSocket bridge:\n%s", joined)
	}
	for _, forbidden := range []string{"content_block_delta", "message_stop", "event: "} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("Anthropic/SSE artifact %q leaked onto the WebSocket transport:\n%s", forbidden, joined)
		}
	}
}
