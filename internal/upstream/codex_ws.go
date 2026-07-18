package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"github.com/gorilla/websocket"
	"github.com/tidwall/sjson"
	"golang.org/x/net/proxy"
)

const (
	codexResponsesWebSocketBeta = "responses_websockets=2026-02-06"
)

// CodexResponsesWebSocketSession owns the persistent upstream connection for one
// downstream Responses WebSocket. Requests on a Codex connection are sequential,
// so requestMu stays held until the terminal event for the current response.
type CodexResponsesWebSocketSession struct {
	requestMu sync.Mutex
	connMu    sync.Mutex
	conn      *websocket.Conn
	target    string
	accountID string
	closed    bool
}

func NewCodexResponsesWebSocketSession() *CodexResponsesWebSocketSession {
	return &CodexResponsesWebSocketSession{}
}

func (s *CodexResponsesWebSocketSession) Close() error {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	s.closed = true
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}

// ForwardProcessed relays Codex's response.processed acknowledgement on the same
// upstream connection that produced the response being acknowledged.
func (s *CodexResponsesWebSocketSession) ForwardProcessed(raw []byte) error {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.closed {
		return errors.New("codex websocket session is closed")
	}
	if s.conn == nil {
		return errors.New("codex websocket session is not connected")
	}
	return s.conn.WriteMessage(websocket.TextMessage, raw)
}

func (c *Client) doCodexResponsesWebSocket(ctx context.Context, spec Request) (*Response, error) {
	if spec.CodexWebSocketSession != nil {
		return spec.CodexWebSocketSession.do(ctx, c, spec)
	}
	target, headers, payload, err := c.prepareCodexResponsesWebSocket(spec)
	if err != nil {
		return nil, err
	}
	dialer, err := c.codexWebSocketDialerForEgress(spec.Egress)
	if err != nil {
		return nil, err
	}

	ctx, guard := newRequestGuard(ctx, c.cfg.RequestTimeout())
	conn, resp, err := dialer.DialContext(ctx, target, headers)
	if err != nil {
		if resp != nil && resp.Body != nil {
			return &Response{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: guard.Wrap(resp.Body)}, nil
		}
		guard.Fail()
		return nil, err
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	payload = stampCodexWebSocketRequestStart(payload)
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		_ = conn.Close()
		guard.Fail()
		return nil, err
	}

	pr, pw := io.Pipe()
	go func() {
		defer supervisor.Recover("codex-websocket-context")
		<-ctx.Done()
		_ = conn.Close()
	}()
	go func() {
		defer supervisor.Recover("codex-websocket-sse-pipe")
		pipeCodexWebSocketSSE(conn, pw)
	}()

	header := http.Header{}
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	return &Response{StatusCode: http.StatusOK, Header: header, Body: guard.Wrap(pr)}, nil
}

func (c *Client) prepareCodexResponsesWebSocket(spec Request) (string, http.Header, []byte, error) {
	target := ComputeCodexResponsesWebSocketURL(c.cfg.UpstreamBaseURL, spec.DownstreamPath)
	headers := http.Header{}
	c.applyCodexHeaders(headers, spec)
	metadata := spec.codexMetadata
	if metadata == nil {
		generated := c.newCodexRequestMetadata(spec)
		metadata = &generated
	}
	ids := *metadata
	applyCodexWebSocketHeaders(headers, ids)
	payload, err := buildCodexWebSocketCreatePayload(spec.Body, ids)
	if err != nil {
		return "", nil, nil, err
	}
	// Remove any downstream-original transport correlators that survived into the
	// payload body (outside client_metadata, which is already virtualized above).
	// previous_response_id is conversation state rather than a transport
	// correlator, so it must remain byte-for-byte intact to preserve context.
	payload = c.codexWSNamespacePayload(spec, ids, payload)
	return target, headers, payload, nil
}

func (s *CodexResponsesWebSocketSession) do(ctx context.Context, c *Client, spec Request) (*Response, error) {
	s.requestMu.Lock()
	target, headers, payload, err := c.prepareCodexResponsesWebSocket(spec)
	if err != nil {
		s.requestMu.Unlock()
		return nil, err
	}
	requestCtx, guard := newRequestGuard(ctx, c.cfg.RequestTimeout())
	conn, handshake, err := s.connection(requestCtx, c, spec, target, headers)
	if err != nil {
		s.requestMu.Unlock()
		if handshake != nil && handshake.Body != nil {
			return &Response{StatusCode: handshake.StatusCode, Header: handshake.Header.Clone(), Body: guard.Wrap(handshake.Body)}, nil
		}
		guard.Fail()
		return nil, err
	}
	payload = stampCodexWebSocketRequestStart(payload)
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		s.invalidate(conn)
		s.requestMu.Unlock()
		guard.Fail()
		return nil, err
	}

	pr, pw := io.Pipe()
	terminal := make(chan struct{})
	go func() {
		defer supervisor.Recover("codex-persistent-websocket-context")
		select {
		case <-requestCtx.Done():
			s.invalidate(conn)
			_ = pw.CloseWithError(requestCtx.Err())
		case <-terminal:
		}
	}()
	go func() {
		defer supervisor.Recover("codex-persistent-websocket-sse-pipe")
		kind := pipeCodexWebSocketSSEFrames(conn, pw, false)
		if kind != "response.completed" {
			s.invalidate(conn)
		}
		close(terminal)
		_ = pw.Close()
		s.requestMu.Unlock()
	}()

	header := http.Header{}
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	return &Response{StatusCode: http.StatusOK, Header: header, Body: guard.Wrap(pr)}, nil
}

func (s *CodexResponsesWebSocketSession) connection(ctx context.Context, c *Client, spec Request, target string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.closed {
		return nil, nil, errors.New("codex websocket session is closed")
	}
	if s.conn != nil && s.target == target && s.accountID == spec.Account.ID {
		return s.conn, nil, nil
	}
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	dialer, err := c.codexWebSocketDialerForEgress(spec.Egress)
	if err != nil {
		return nil, nil, err
	}
	conn, resp, err := dialer.DialContext(ctx, target, headers)
	if err != nil {
		return nil, resp, err
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	s.conn = conn
	s.target = target
	s.accountID = spec.Account.ID
	return conn, nil, nil
}

func (s *CodexResponsesWebSocketSession) invalidate(conn *websocket.Conn) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.conn != conn {
		return
	}
	_ = s.conn.Close()
	s.conn = nil
}

func (c *Client) codexWebSocketDialerForEgress(egress storage.EgressProfile) (*websocket.Dialer, error) {
	dialer := *websocket.DefaultDialer
	dialer.Proxy = http.ProxyFromEnvironment
	dialer.HandshakeTimeout = 15 * time.Second
	dialer.EnableCompression = true
	dialer.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}

	switch egress.Type {
	case "", "direct", "curl_cffi_sidecar":
		return &dialer, nil
	case "http_proxy", "https_proxy", "warp_proxy":
		if egress.Endpoint == "" {
			return nil, errors.New("proxy egress endpoint required")
		}
		proxyURL, err := url.Parse(egress.Endpoint)
		if err != nil {
			return nil, err
		}
		dialer.Proxy = http.ProxyURL(proxyURL)
		return &dialer, nil
	case "socks5h_proxy", "socks5_proxy":
		if egress.Endpoint == "" {
			return nil, errors.New("socks5 egress endpoint required")
		}
		addr, auth := socksAuthAndAddr(egress.Endpoint)
		socksDialer, err := proxy.SOCKS5("tcp", addr, auth, proxy.Direct)
		if err != nil {
			return nil, err
		}
		dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			type contextDialer interface {
				DialContext(context.Context, string, string) (net.Conn, error)
			}
			if d, ok := socksDialer.(contextDialer); ok {
				return d.DialContext(ctx, network, addr)
			}
			return socksDialer.Dial(network, addr)
		}
		return &dialer, nil
	default:
		return nil, fmt.Errorf("unsupported egress type %q", egress.Type)
	}
}

func applyCodexWebSocketHeaders(dst http.Header, ids codexWebSocketIDs) {
	// The WebSocket handshake carries neither Accept nor Content-Type (verified vs
	// codex-rs build_websocket_headers). Strip any stale capitalized "Session_id"
	// (the real client uses lowercase session-id/thread-id, set below).
	// ChatGPT-Account-ID is deliberately KEPT — real WS auth sends it.
	dst.Del("Accept")
	deleteHeaderFold(dst, "Content-Type")
	deleteHeaderFold(dst, "Session_id")

	setHeaderPreserveCase(dst, "OpenAI-Beta", codexResponsesWebSocketBeta)
	setHeaderPreserveCase(dst, "session-id", ids.sessionID)
	setHeaderPreserveCase(dst, "thread-id", ids.threadID)
	setHeaderPreserveCase(dst, "x-client-request-id", ids.threadID)
	setHeaderPreserveCase(dst, "x-codex-window-id", ids.windowID)
	// `version` and `x-codex-beta-features` are installed by applyCodexHeaders and
	// deliberately survive here; both are present in a real 0.144.5 WS capture.
	if ids.parentThreadID != "" {
		setHeaderPreserveCase(dst, "x-codex-parent-thread-id", ids.parentThreadID)
	}
	if ids.subagent != "" {
		setHeaderPreserveCase(dst, codexSubagentHeader, ids.subagent)
	}
	if ids.turnMetadata != "" {
		setHeaderPreserveCase(dst, "x-codex-turn-metadata", ids.turnMetadata)
	}
}

func buildCodexWebSocketCreatePayload(body []byte, ids codexWebSocketIDs) ([]byte, error) {
	// Do normally applies this at the shared HTTP/WS choke point. Keep the frame
	// builder defensive as well because a persistent WebSocket can switch from a
	// classic model to a Lite model without changing the connection.
	if ids.responsesLite {
		body = normalizeCodexResponsesLiteEnvelope(body)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	payload := body
	set := func(path string, value interface{}) error {
		var err error
		payload, err = sjson.SetBytes(payload, path, value)
		return err
	}
	setDefault := func(path string, value interface{}) error {
		if _, exists := fields[path]; exists {
			return nil
		}
		return set(path, value)
	}
	if err := set("type", "response.create"); err != nil {
		return nil, err
	}
	if err := set("stream", true); err != nil {
		return nil, err
	}
	// Responses Lite carries tools in the leading `additional_tools` input item and
	// omits the top-level tools member. Classic Responses keeps its historical
	// top-level tools:[] default.
	if !ids.responsesLite {
		if err := setDefault("tools", []interface{}{}); err != nil {
			return nil, err
		}
	}
	for _, field := range []struct {
		path  string
		value interface{}
	}{
		{"tool_choice", "auto"},
		{"parallel_tool_calls", false},
		{"store", false},
		{"include", []interface{}{}},
	} {
		if err := setDefault(field.path, field.value); err != nil {
			return nil, err
		}
	}
	if ids.responsesLite {
		if err := set("parallel_tool_calls", false); err != nil {
			return nil, err
		}
	}
	return applyCodexClientMetadata(payload, ids, true), nil
}

// codexWSNamespacePayload is a final defensive schema guard for WS frames. The
// downstream correlators were already consumed when canonical metadata was built;
// the live Responses backend rejects them as top-level request parameters.
func (c *Client) codexWSNamespacePayload(spec Request, ids codexWebSocketIDs, payload []byte) []byte {
	return stripCodexTopLevelTransportCorrelators(payload)
}

func pipeCodexWebSocketSSE(conn *websocket.Conn, pw *io.PipeWriter) {
	_ = pipeCodexWebSocketSSEFrames(conn, pw, true)
	_ = pw.Close()
}

func pipeCodexWebSocketSSEFrames(conn *websocket.Conn, pw *io.PipeWriter, closeConn bool) string {
	if closeConn {
		defer conn.Close()
	}
	for {
		messageType, raw, err := conn.ReadMessage()
		if err != nil {
			_ = pw.CloseWithError(err)
			return ""
		}
		if messageType != websocket.TextMessage {
			if messageType == websocket.CloseMessage {
				_ = pw.CloseWithError(errors.New("websocket closed before response.completed"))
				return ""
			}
			if messageType == websocket.BinaryMessage {
				_ = pw.CloseWithError(errors.New("unexpected binary websocket event"))
				return ""
			}
			continue
		}
		kind := jsonTypeField(raw)
		if err := writeSSEEvent(pw, kind, raw); err != nil {
			_ = pw.CloseWithError(err)
			return ""
		}
		switch kind {
		case "response.completed", "response.failed", "response.incomplete", "error":
			_, _ = pw.Write([]byte("data: [DONE]\n\n"))
			return kind
		}
	}
}

func writeSSEEvent(w io.Writer, kind string, raw []byte) error {
	if kind != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", kind); err != nil {
			return err
		}
	}
	// A WebSocket text frame may contain pretty-printed JSON with literal
	// newlines. SSE requires every physical payload line to carry its own data:
	// prefix; prefixing only the first line makes an SSE parser see just "{" and
	// later fails when the downstream WebSocket adapter reconstructs the event.
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "\n")
	return err
}

func jsonTypeField(raw []byte) string {
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &event); err != nil {
		return ""
	}
	if strings.ContainsAny(event.Type, "\r\n") {
		return ""
	}
	return event.Type
}

func codexBodyString(body []byte, key string) string {
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		return ""
	}
	value, _ := root[key].(string)
	return strings.TrimSpace(value)
}

func threadIDFromWindowID(windowID string) string {
	windowID = strings.TrimSpace(windowID)
	if windowID == "" {
		return ""
	}
	if idx := strings.LastIndex(windowID, ":"); idx > 0 {
		return windowID[:idx]
	}
	return ""
}

func deleteHeaderFold(h http.Header, key string) {
	for existing := range h {
		if strings.EqualFold(existing, key) {
			delete(h, existing)
		}
	}
}
