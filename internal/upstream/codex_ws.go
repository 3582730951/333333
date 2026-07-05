package upstream

import (
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
	"time"

	"codex-account-pool/internal/identity"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

const (
	codexResponsesWebSocketBeta         = "responses_websockets=2026-02-06"
	codexResponsesWebSocketBetaFeatures = "terminal_resize_reflow"
)

type codexWebSocketIDs struct {
	installationID string
	sessionID      string
	threadID       string
	windowID       string
	parentThreadID string
	turnMetadata   string
}

func (c *Client) doCodexResponsesWebSocket(ctx context.Context, spec Request) (*Response, error) {
	target := ComputeCodexResponsesWebSocketURL(c.cfg.UpstreamBaseURL, spec.DownstreamPath)
	headers := http.Header{}
	c.applyCodexHeaders(headers, spec)
	ids := c.codexWebSocketIDs(spec)
	applyCodexWebSocketHeaders(headers, ids)
	payload, err := buildCodexWebSocketCreatePayload(spec.Body, ids)
	if err != nil {
		return nil, err
	}
	// Namespace any downstream-original session/thread identifiers that survived
	// into the payload body (outside client_metadata, which is already virtualized
	// above). The downstream's real session_id/thread_id/previous_response_id are
	// account-correlatable signals; replace them with the per-account derived
	// equivalents so the upstream cannot link one account's session to another's.
	payload = c.codexWSNamespacePayload(spec, ids, payload)
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

func (c *Client) codexWebSocketIDs(spec Request) codexWebSocketIDs {
	id := identity.ForOS(c.identitySecret, spec.Account.ID, spec.OSHint)
	windowID := firstNonEmpty(
		getHeaderFold(spec.Headers, "x-codex-window-id"),
	)
	threadID := firstNonEmpty(
		getHeaderFold(spec.Headers, "thread-id"),
		getHeaderFold(spec.Headers, "x-client-request-id"),
		threadIDFromWindowID(windowID),
	)
	if threadID == "" {
		if seed := firstNonEmpty(
			codexBodyString(spec.Body, "prompt_cache_key"),
			codexBodyString(spec.Body, "previous_response_id"),
			codexRunCorrelator(spec.Headers),
		); seed != "" {
			threadID = identity.DerivedUUID(id.MachineID, "thread:"+seed)
		}
	}
	if threadID == "" {
		threadID = identity.DerivedUUID(id.MachineID, fmt.Sprintf("thread:%s:%d", spec.Account.ID, time.Now().UnixNano()))
	}
	if windowID == "" {
		windowID = threadID + ":0"
	}
	sessionID := firstNonEmpty(
		getHeaderFold(spec.Headers, "session-id"),
		getHeaderFold(spec.Headers, "Session_id"),
		threadID,
	)
	ids := codexWebSocketIDs{
		installationID: id.MachineID,
		sessionID:      sessionID,
		threadID:       threadID,
		windowID:       windowID,
		parentThreadID: strings.TrimSpace(getHeaderFold(spec.Headers, "x-codex-parent-thread-id")),
	}
	ids.turnMetadata = codexWebSocketTurnMetadata(spec.Headers, ids)
	return ids
}

func applyCodexWebSocketHeaders(dst http.Header, ids codexWebSocketIDs) {
	// The WebSocket handshake carries no Accept header (verified vs codex-rs
	// build_websocket_headers). Strip any stale capitalized "Session_id" (the real
	// client uses lowercase session-id/thread-id, set below). ChatGPT-Account-ID is
	// deliberately KEPT — the real WS sends it via the auth headers.
	dst.Del("Accept")
	deleteHeaderFold(dst, "Session_id")

	setHeaderPreserveCase(dst, "OpenAI-Beta", codexResponsesWebSocketBeta)
	setHeaderPreserveCase(dst, "session-id", ids.sessionID)
	setHeaderPreserveCase(dst, "thread-id", ids.threadID)
	setHeaderPreserveCase(dst, "x-client-request-id", ids.threadID)
	setHeaderPreserveCase(dst, "x-codex-window-id", ids.windowID)
	// NOTE: no `version` request header. The canonical codex-rs client sets only
	// `originator` + `User-Agent` as default headers (login/src/auth/default_client.rs
	// default_headers(), shared by the HTTP and WS transports); it never sends a
	// `version` header on any path. A capture once showed `version` on the WS upgrade,
	// but the source is authoritative and Session 18 deliberately removed the spurious
	// `version` injection as a relay tell — re-adding it would reintroduce that tell.
	setIfEmptyPreserveCase(dst, "x-codex-beta-features", codexResponsesWebSocketBetaFeatures)
	if ids.parentThreadID != "" {
		setHeaderPreserveCase(dst, "x-codex-parent-thread-id", ids.parentThreadID)
	}
	if ids.turnMetadata != "" {
		setHeaderPreserveCase(dst, "x-codex-turn-metadata", ids.turnMetadata)
	}
}

func buildCodexWebSocketCreatePayload(body []byte, ids codexWebSocketIDs) ([]byte, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	payload["type"] = "response.create"
	payload["stream"] = true
	setJSONDefault(payload, "instructions", "")
	setJSONDefault(payload, "tools", []interface{}{})
	setJSONDefault(payload, "tool_choice", "auto")
	setJSONDefault(payload, "parallel_tool_calls", false)
	setJSONDefault(payload, "store", false)
	setJSONDefault(payload, "include", []interface{}{})

	metadata, _ := payload["client_metadata"].(map[string]interface{})
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	metadata["x-codex-installation-id"] = ids.installationID
	metadata["x-codex-window-id"] = ids.windowID
	if ids.parentThreadID != "" {
		metadata["x-codex-parent-thread-id"] = ids.parentThreadID
	}
	if ids.turnMetadata != "" {
		metadata["x-codex-turn-metadata"] = ids.turnMetadata
	}
	payload["client_metadata"] = metadata

	return json.Marshal(payload)
}

// codexWSNamespacePayload replaces any downstream-original session_id / thread_id
// that survived into the WS create payload body with the per-account derived
// equivalents. This is the WS-frame analogue of the HTTP-header conversation
// isolation: the upstream must not see a session id that originated on a
// different account. The replacement is byte-level (json re-marshal), so it does
// not disturb WS frame boundaries. client_metadata is already virtualized by
// buildCodexWebSocketCreatePayload, so we only touch top-level string fields that
// carry a downstream session/thread id.
func (c *Client) codexWSNamespacePayload(spec Request, ids codexWebSocketIDs, payload []byte) []byte {
	var root map[string]interface{}
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload // leave untouched on parse failure (do not break the request)
	}
	changed := false
	// session_id / thread_id at the top level: replace with the derived per-account
	// value (same one used in client_metadata and the handshake headers), keeping
	// everything coherent.
	for _, key := range []string{"session_id", "thread_id"} {
		if v, ok := root[key].(string); ok && v != "" {
			var repl string
			if key == "session_id" {
				repl = ids.sessionID
			} else {
				repl = ids.threadID
			}
			if repl != "" && repl != v {
				root[key] = repl
				changed = true
			}
		}
	}
	if !changed {
		return payload
	}
	if out, err := json.Marshal(root); err == nil {
		return out
	}
	return payload
}

func pipeCodexWebSocketSSE(conn *websocket.Conn, pw *io.PipeWriter) {
	defer conn.Close()
	defer pw.Close()
	for {
		messageType, raw, err := conn.ReadMessage()
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if messageType != websocket.TextMessage {
			if messageType == websocket.CloseMessage {
				_ = pw.CloseWithError(errors.New("websocket closed before response.completed"))
				return
			}
			if messageType == websocket.BinaryMessage {
				_ = pw.CloseWithError(errors.New("unexpected binary websocket event"))
				return
			}
			continue
		}
		kind := jsonTypeField(raw)
		if err := writeSSEEvent(pw, kind, raw); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if kind == "response.completed" {
			_, _ = pw.Write([]byte("data: [DONE]\n\n"))
			return
		}
	}
}

func writeSSEEvent(w io.Writer, kind string, raw []byte) error {
	if kind != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", kind); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", raw)
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

func codexWebSocketTurnMetadata(headers http.Header, ids codexWebSocketIDs) string {
	if v := strings.TrimSpace(getHeaderFold(headers, "x-codex-turn-metadata")); v != "" {
		return v
	}
	meta := map[string]interface{}{
		"session_id":    ids.sessionID,
		"thread_id":     ids.threadID,
		"thread_source": "user",
		"turn_id":       "",
		"request_kind":  "turn",
		"window_id":     ids.windowID,
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return ""
	}
	return string(raw)
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

func setJSONDefault(payload map[string]interface{}, key string, value interface{}) {
	if _, ok := payload[key]; !ok {
		payload[key] = value
	}
}

func deleteHeaderFold(h http.Header, key string) {
	for existing := range h {
		if strings.EqualFold(existing, key) {
			delete(h, existing)
		}
	}
}
