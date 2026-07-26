package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
	"github.com/gorilla/websocket"
	"github.com/tidwall/sjson"
)

type forceCodexResponsesWebSocketKey struct{}
type codexResponsesWebSocketSessionKey struct{}

const (
	responsesWebSocketHeartbeatInterval = 30 * time.Second
	responseInProgressPayload           = `{"type":"response.in_progress"}`
)

type webSocketMessageWriter interface {
	WriteMessage(messageType int, data []byte) error
}

// responsesWebSocketConn serializes downstream writes and tracks application-level
// activity. Codex consumes WebSocket Ping/Pong frames in its connection pump, so
// only a text event resets its stream idle timeout.
type responsesWebSocketConn struct {
	dst       webSocketMessageWriter
	mu        sync.Mutex
	lastWrite time.Time
}

type responsesWebSocketState struct {
	mu                 sync.Mutex
	previousResponseID string
}

func (s *responsesWebSocketState) observe(payload []byte) {
	var event struct {
		Type     string `json:"type"`
		Response struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) == nil && event.Type == "response.completed" && strings.TrimSpace(event.Response.ID) != "" {
		s.mu.Lock()
		s.previousResponseID = strings.TrimSpace(event.Response.ID)
		s.mu.Unlock()
	}
}

func (s *responsesWebSocketState) completeAppend(body []byte) ([]byte, error) {
	if routing.JSONStringField(body, "previous_response_id") != "" {
		return body, nil
	}
	s.mu.Lock()
	previous := s.previousResponseID
	s.mu.Unlock()
	if previous == "" {
		return body, nil
	}
	return sjson.SetBytes(body, "previous_response_id", previous)
}

func newResponsesWebSocketConn(dst webSocketMessageWriter) *responsesWebSocketConn {
	return &responsesWebSocketConn{dst: dst, lastWrite: time.Now()}
}

func (c *responsesWebSocketConn) WriteMessage(messageType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.dst.WriteMessage(messageType, data); err != nil {
		return err
	}
	if messageType == websocket.TextMessage {
		c.lastWrite = time.Now()
	}
	return nil
}

func (c *responsesWebSocketConn) resetIdleClock() {
	c.mu.Lock()
	c.lastWrite = time.Now()
	c.mu.Unlock()
}

func (c *responsesWebSocketConn) writeHeartbeatIfIdle(interval time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.lastWrite) < interval {
		return nil
	}
	if err := c.dst.WriteMessage(websocket.TextMessage, []byte(responseInProgressPayload)); err != nil {
		return err
	}
	c.lastWrite = time.Now()
	return nil
}

func keepResponsesWebSocketAlive(ctx context.Context, conn *responsesWebSocketConn, interval time.Duration) error {
	if interval <= 0 {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := conn.writeHeartbeatIfIdle(interval); err != nil {
				return err
			}
		}
	}
}

func forceCodexResponsesWebSocket(ctx context.Context) bool {
	force, _ := ctx.Value(forceCodexResponsesWebSocketKey{}).(bool)
	return force
}

func codexResponsesWebSocketSession(ctx context.Context) *upstream.CodexResponsesWebSocketSession {
	session, _ := ctx.Value(codexResponsesWebSocketSessionKey{}).(*upstream.CodexResponsesWebSocketSession)
	return session
}

func isResponsesWebSocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func (s *Server) handleGatewayWebSocket(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	downstream := newResponsesWebSocketConn(conn)
	state := &responsesWebSocketState{}
	session := upstream.NewCodexResponsesWebSocketSession()
	defer session.Close()
	baseCtx := context.WithValue(r.Context(), codexResponsesWebSocketSessionKey{}, session)

	for {
		messageType, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			_ = writeWebSocketError(downstream, http.StatusBadRequest, "unexpected non-text websocket message")
			return
		}
		kind, body, err := responsesWebSocketRequestToBody(raw)
		if err != nil {
			_ = writeWebSocketError(downstream, http.StatusBadRequest, err.Error())
			return
		}
		switch kind {
		case "response.processed":
			if err := session.ForwardProcessed(raw); err != nil {
				_ = writeWebSocketError(downstream, http.StatusBadGateway, err.Error())
				return
			}
			continue
		case "response.create", "response.append":
			if kind == "response.append" {
				body, err = state.completeAppend(body)
				if err != nil {
					_ = writeWebSocketError(downstream, http.StatusBadRequest, err.Error())
					return
				}
			}
			turnBaseCtx, cancelTurn := context.WithCancel(baseCtx)
			// One downstream WebSocket can carry many inference turns. Give each
			// turn its own usage event while every retry/bridge inside that turn
			// continues to share the same idempotency key.
			turnBaseCtx = contextWithRequestID(turnBaseCtx, newRequestID())
			turnCtx := context.WithValue(turnBaseCtx, forceCodexResponsesWebSocketKey{}, true)
			req := r.Clone(turnCtx)
			req.Method = http.MethodPost
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.Header = r.Header.Clone()
			req.Header.Set("Content-Type", "application/json")
			writer := newResponsesWebSocketWriter(downstream, state.observe)
			downstream.resetIdleClock()
			heartbeatDone := make(chan error, 1)
			go func() {
				heartbeatErr := fmt.Errorf("responses websocket heartbeat stopped unexpectedly")
				defer func() {
					if heartbeatErr != nil {
						cancelTurn()
					}
					heartbeatDone <- heartbeatErr
				}()
				defer supervisor.Recover("responses-websocket-heartbeat")
				heartbeatErr = keepResponsesWebSocketAlive(turnBaseCtx, downstream, responsesWebSocketHeartbeatInterval)
			}()
			s.handleGatewayPost(writer, req)
			cancelTurn()
			heartbeatErr := <-heartbeatDone
			if heartbeatErr != nil {
				return
			}
			if err := writer.Close(); err != nil {
				return
			}
		default:
			_ = writeWebSocketError(downstream, http.StatusBadRequest, fmt.Sprintf("unsupported websocket request type %q", kind))
			return
		}
	}
}

func responsesWebSocketRequestToBody(raw []byte) (string, []byte, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return "", nil, err
	}
	var kind string
	if typeRaw, ok := root["type"]; ok {
		_ = json.Unmarshal(typeRaw, &kind)
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "", nil, fmt.Errorf("missing websocket request type")
	}
	if kind != "response.create" && kind != "response.append" {
		return kind, nil, nil
	}
	body, err := sjson.DeleteBytes(raw, "type")
	if err != nil {
		return "", nil, err
	}
	if _, ok := root["stream"]; !ok {
		body, err = sjson.SetBytes(body, "stream", true)
		if err != nil {
			return "", nil, err
		}
	}
	return kind, body, nil
}

type responsesWebSocketWriter struct {
	conn       webSocketMessageWriter
	header     http.Header
	status     int
	sseBuffer  []byte
	bodyBuffer []byte
	onEvent    func([]byte)
}

func newResponsesWebSocketWriter(conn webSocketMessageWriter, onEvent func([]byte)) *responsesWebSocketWriter {
	return &responsesWebSocketWriter{
		conn:    conn,
		header:  http.Header{},
		onEvent: onEvent,
	}
}

func (w *responsesWebSocketWriter) Header() http.Header {
	return w.header
}

func (w *responsesWebSocketWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *responsesWebSocketWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 || !isEventStream(w.header) {
		w.bodyBuffer = append(w.bodyBuffer, p...)
		return len(p), nil
	}
	w.sseBuffer = append(w.sseBuffer, p...)
	if err := w.flushSSEBlocks(false); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *responsesWebSocketWriter) Flush() {}

func (w *responsesWebSocketWriter) Close() error {
	if w.status >= 400 || (!isEventStream(w.header) && len(w.bodyBuffer) > 0) {
		return writeWebSocketError(w.conn, w.status, strings.TrimSpace(string(w.bodyBuffer)))
	}
	return w.flushSSEBlocks(true)
}

func (w *responsesWebSocketWriter) flushSSEBlocks(final bool) error {
	for {
		boundary, separatorLen := sseFrameBoundary(w.sseBuffer)
		if boundary < 0 {
			break
		}
		block := append([]byte(nil), w.sseBuffer[:boundary]...)
		w.sseBuffer = w.sseBuffer[boundary+separatorLen:]
		if err := w.writeSSEBlock(block); err != nil {
			return err
		}
	}
	if final && len(bytes.TrimSpace(w.sseBuffer)) > 0 {
		block := append([]byte(nil), w.sseBuffer...)
		w.sseBuffer = nil
		return w.writeSSEBlock(block)
	}
	return nil
}

func (w *responsesWebSocketWriter) writeSSEBlock(block []byte) error {
	payload := sseDataPayload(block)
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	if w.onEvent != nil {
		w.onEvent([]byte(payload))
	}
	return w.conn.WriteMessage(websocket.TextMessage, []byte(payload))
}

func sseDataPayload(block []byte) string {
	text := strings.ReplaceAll(string(block), "\r\n", "\n")
	var data []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return strings.Join(data, "\n")
}

func writeWebSocketError(conn webSocketMessageWriter, status int, message string) error {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	if message == "" {
		message = http.StatusText(status)
	}
	payload := map[string]interface{}{
		"type":   "error",
		"status": status,
		"error": map[string]interface{}{
			"message": message,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, raw)
}
