package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"codex-account-pool/internal/upstream"
	"github.com/gorilla/websocket"
	"github.com/tidwall/sjson"
)

type forceCodexResponsesWebSocketKey struct{}
type codexResponsesWebSocketSessionKey struct{}

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
	session := upstream.NewCodexResponsesWebSocketSession()
	defer session.Close()
	baseCtx := context.WithValue(r.Context(), codexResponsesWebSocketSessionKey{}, session)

	for {
		messageType, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			_ = writeWebSocketError(conn, http.StatusBadRequest, "unexpected non-text websocket message")
			return
		}
		kind, body, err := responsesWebSocketRequestToBody(raw)
		if err != nil {
			_ = writeWebSocketError(conn, http.StatusBadRequest, err.Error())
			return
		}
		switch kind {
		case "response.processed":
			if err := session.ForwardProcessed(raw); err != nil {
				_ = writeWebSocketError(conn, http.StatusBadGateway, err.Error())
				return
			}
			continue
		case "response.create":
			req := r.Clone(context.WithValue(baseCtx, forceCodexResponsesWebSocketKey{}, true))
			req.Method = http.MethodPost
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.Header = r.Header.Clone()
			req.Header.Set("Content-Type", "application/json")
			writer := newResponsesWebSocketWriter(conn)
			s.handleGatewayPost(writer, req)
			if err := writer.Close(); err != nil {
				return
			}
		default:
			_ = writeWebSocketError(conn, http.StatusBadRequest, fmt.Sprintf("unsupported websocket request type %q", kind))
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
	if kind != "response.create" {
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
	conn       *websocket.Conn
	header     http.Header
	status     int
	sseBuffer  []byte
	bodyBuffer []byte
}

func newResponsesWebSocketWriter(conn *websocket.Conn) *responsesWebSocketWriter {
	return &responsesWebSocketWriter{
		conn:   conn,
		header: http.Header{},
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
		idx := bytes.Index(w.sseBuffer, []byte("\n\n"))
		sepLen := 2
		if idx < 0 {
			idx = bytes.Index(w.sseBuffer, []byte("\r\n\r\n"))
			sepLen = 4
		}
		if idx < 0 {
			break
		}
		block := append([]byte(nil), w.sseBuffer[:idx]...)
		w.sseBuffer = w.sseBuffer[idx+sepLen:]
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

func writeWebSocketError(conn *websocket.Conn, status int, message string) error {
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
