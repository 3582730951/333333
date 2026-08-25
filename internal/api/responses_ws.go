package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"codex-account-pool/internal/upstream"
	"github.com/gorilla/websocket"
	"github.com/tidwall/sjson"
)

type forceCodexResponsesWebSocketKey struct{}
type codexResponsesWebSocketSessionKey struct{}

// codexResponsesWebSocketPrewarmKey marks a downstream response.create/append
// frame that carries generate:false — OpenAI's prewarm signal ("write the
// prefix cache, do not generate"). Such a frame must stay on the upstream
// WebSocket (the HTTP/SSE path strips `generate` at the choke point and would
// silently turn the prewarm into a full, billable inference) and must not
// persist any turn/session state (no binding commit, no goal checkpoint).
type codexResponsesWebSocketPrewarmKey struct{}

func codexPrewarmFrameFromContext(ctx context.Context) bool {
	prewarm, _ := ctx.Value(codexResponsesWebSocketPrewarmKey{}).(bool)
	return prewarm
}

// codexResponsesGenerateFalse reports whether a scanned response.create/append
// frame is a prewarm signal (generate == false). The scanner keeps `generate`
// in BodyMeta.Scalars because the field is tracked for Responses WebSocket
// frames; the HTTP Responses schema does not define it, so an HTTP request can
// never set this bit.
func codexResponsesGenerateFalse(meta bodysource.BodyMeta) bool {
	raw, ok := meta.Scalars["generate"]
	if !ok {
		return false
	}
	// json.Unmarshal treats null as a no-op (zero value), so it must be rejected
	// explicitly: only a literal false is a prewarm signal.
	if strings.EqualFold(strings.TrimSpace(string(raw)), "null") {
		return false
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return !value
}
type codexResponsesWebSocketHTTPSRecoveryTurnKey struct{}

const (
	responsesWebSocketHeartbeatInterval = 30 * time.Second
	responseInProgressPayload           = `{"type":"response.in_progress"}`
	// The Codex upstream can reject one oversized WebSocket message with close
	// code 1009 even though the same Responses request is valid over HTTP/SSE.
	// Keep the first turn below the production-safe bridge threshold used by the
	// compatible gateways we regression-test against. Later stateful appends stay
	// on their existing transport because a connection-scoped response id is not
	// portable without durable replay.
	codexResponsesWebSocketHTTPBridgeThreshold = 15 << 20
)

type webSocketMessageWriter interface {
	WriteMessage(messageType int, data []byte) error
}

type webSocketNextWriter interface {
	NextWriter(messageType int) (io.WriteCloser, error)
}

// responsesWebSocketConn serializes downstream writes and tracks application-level
// activity. Codex consumes WebSocket Ping/Pong frames in its connection pump, so
// only a text event resets its stream idle timeout.
type responsesWebSocketConn struct {
	dst       webSocketMessageWriter
	mu        sync.Mutex
	lastWrite time.Time
}

// responsesWebSocketDrainRegistry rotates only the WebSocket sessions that
// existed when a deployment drain was requested. An idle session is closed
// immediately; a session processing a model turn is marked and closed only after
// that turn has emitted its terminal frame. Registrations created later are not
// poisoned, which makes a failed/aborted deployment safe to resume without a
// process restart.
type responsesWebSocketDrainRegistry struct {
	mu          sync.Mutex
	connections map[*responsesWebSocketDrainConnection]struct{}
}

type responsesWebSocketDrainConnection struct {
	registry   *responsesWebSocketDrainRegistry
	raw        io.Closer
	downstream webSocketMessageWriter
	active     bool
	drain      bool
	closeOnce  sync.Once
}

func (r *responsesWebSocketDrainRegistry) register(raw io.Closer, downstream webSocketMessageWriter) *responsesWebSocketDrainConnection {
	connection := &responsesWebSocketDrainConnection{registry: r, raw: raw, downstream: downstream}
	r.mu.Lock()
	if r.connections == nil {
		r.connections = make(map[*responsesWebSocketDrainConnection]struct{})
	}
	r.connections[connection] = struct{}{}
	r.mu.Unlock()
	return connection
}

func (c *responsesWebSocketDrainConnection) unregister() {
	if c == nil || c.registry == nil {
		return
	}
	c.registry.mu.Lock()
	delete(c.registry.connections, c)
	c.registry.mu.Unlock()
}

func (c *responsesWebSocketDrainConnection) beginWork() bool {
	if c == nil || c.registry == nil {
		return false
	}
	c.registry.mu.Lock()
	defer c.registry.mu.Unlock()
	if c.drain {
		return false
	}
	c.active = true
	return true
}

// endWork returns true when the completed unit of work was the last operation
// this pre-drain session may perform. The close frame is serialized with normal
// response writes by responsesWebSocketConn.
func (c *responsesWebSocketDrainConnection) endWork() bool {
	if c == nil || c.registry == nil {
		return false
	}
	c.registry.mu.Lock()
	c.active = false
	shouldClose := c.drain
	c.registry.mu.Unlock()
	if shouldClose {
		c.closeForRestart()
	}
	return shouldClose
}

func (c *responsesWebSocketDrainConnection) closeForRestart() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		if c.downstream != nil {
			_ = c.downstream.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(
				websocket.CloseServiceRestart, "pool worker upgrade; reconnect to continue",
			))
		}
		if c.raw != nil {
			_ = c.raw.Close()
		}
	})
}

// drainExisting marks a stable snapshot. Active turns are never interrupted;
// idle readers are closed outside the registry lock so a network write cannot
// block connection registration or turn completion.
func (r *responsesWebSocketDrainRegistry) drainExisting() (total, idle int) {
	r.mu.Lock()
	toClose := make([]*responsesWebSocketDrainConnection, 0, len(r.connections))
	for connection := range r.connections {
		total++
		connection.drain = true
		if !connection.active {
			idle++
			toClose = append(toClose, connection)
		}
	}
	r.mu.Unlock()
	for _, connection := range toClose {
		connection.closeForRestart()
	}
	return total, idle
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

func (s *responsesWebSocketState) observeMeta(meta bodysource.BodyMeta) {
	if meta.Type != "response.completed" || strings.TrimSpace(meta.ResponseID) == "" {
		return
	}
	s.mu.Lock()
	s.previousResponseID = strings.TrimSpace(meta.ResponseID)
	s.mu.Unlock()
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

func (s *responsesWebSocketState) completeAppendSource(ctx context.Context, source bodysource.BodySource, meta bodysource.BodyMeta, hmacKey []byte) (bodysource.BodySource, bodysource.BodyMeta, error) {
	if meta.PreviousResponseID != "" {
		return source, meta, nil
	}
	s.mu.Lock()
	previous := s.previousResponseID
	s.mu.Unlock()
	if previous == "" {
		return source, meta, nil
	}
	value, err := json.Marshal(previous)
	if err != nil {
		return nil, bodysource.BodyMeta{}, err
	}
	patched, err := bodysource.PatchTopLevel(source, meta, []bodysource.JSONFieldPatch{{Name: "previous_response_id", Value: value}})
	if err != nil {
		return source, meta, err
	}
	patchedMeta, err := bodysource.ScanJSON(ctx, patched, hmacKey)
	if err != nil {
		_ = patched.Close()
		return patched, bodysource.BodyMeta{}, err
	}
	return patched, patchedMeta, nil
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

func (c *responsesWebSocketConn) WriteSourceMessage(messageType int, source bodysource.BodySource) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if source == nil {
		return errors.New("nil websocket message source")
	}
	if dst, ok := c.dst.(webSocketNextWriter); ok {
		reader, err := source.Open()
		if err != nil {
			return err
		}
		defer reader.Close()
		writer, err := dst.NextWriter(messageType)
		if err != nil {
			return err
		}
		_, copyErr := io.CopyBuffer(writer, reader, make([]byte, bodysource.DefaultChunkSize))
		if err = errors.Join(copyErr, writer.Close()); err != nil {
			return err
		}
	} else {
		if source.Size() > 1<<20 {
			return errors.New("websocket writer does not support streaming messages")
		}
		raw, err := bodysource.ReadAll(source)
		if err != nil {
			return err
		}
		if err = c.dst.WriteMessage(messageType, raw); err != nil {
			return err
		}
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

func codexResponsesWebSocketNeedsHTTPSRecovery(ctx context.Context) bool {
	recovery, _ := ctx.Value(codexResponsesWebSocketHTTPSRecoveryTurnKey{}).(bool)
	return forceCodexResponsesWebSocket(ctx) && recovery
}

func isResponsesWebSocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// DrainEstablishedWebSockets asks sessions that predate a worker upgrade to
// reconnect. Idle sessions close now; active model turns close after their normal
// terminal frame. The returned counts are safe to expose in deployment logs.
func (s *Server) DrainEstablishedWebSockets() (total, idle int) {
	if s == nil {
		return 0, 0
	}
	return s.responsesWSDrainer.drainExisting()
}

func (s *Server) handleGatewayWebSocket(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{HandshakeTimeout: 15 * time.Second, EnableCompression: true}
	responseHeader := http.Header{}
	responseHeader.Set(requestIDHeader, requestIDFromContext(r.Context()))
	conn, err := upgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		log.Printf("[RESPONSES-WS] downstream upgrade failed request_id=%s: %v", requestIDFromContext(r.Context()), err)
		_ = s.store.InsertAuditLog(context.WithoutCancel(r.Context()), storage.AuditLogRow{
			Action: "codex_downstream_websocket_upgrade", State: "failed", Reason: "handshake_failed", Detail: "path=/v1/responses",
		})
		return
	}
	_ = s.store.InsertAuditLog(context.WithoutCancel(r.Context()), storage.AuditLogRow{
		Action: "codex_downstream_websocket_upgrade", State: "connected", Reason: "switching_protocols", Detail: "path=/v1/responses compression=negotiated_if_requested",
	})
	defer conn.Close()
	downstream := newResponsesWebSocketConn(conn)
	drainConnection := s.responsesWSDrainer.register(conn, downstream)
	defer drainConnection.unregister()
	state := &responsesWebSocketState{}
	session := upstream.NewCodexResponsesWebSocketSession()
	defer session.Close()
	baseCtx := context.WithValue(r.Context(), codexResponsesWebSocketSessionKey{}, session)

	for {
		messageType, message, err := conn.NextReader()
		if err != nil {
			return
		}
		if messageType != websocket.TextMessage {
			_ = writeWebSocketError(downstream, http.StatusBadRequest, "unexpected non-text websocket message")
			return
		}
		if !drainConnection.beginWork() {
			drainConnection.closeForRestart()
			return
		}
		source, meta, err := captureJSONRequestBody(baseCtx, message, s.cfg, s.requestBodyBudget, s.identitySecretCached)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, bodysource.ErrBodyTooLarge) {
				status = http.StatusRequestEntityTooLarge
			} else if errors.Is(err, bodysource.ErrSpoolBudget) || errors.Is(err, bodysource.ErrDiskReserve) {
				status = http.StatusServiceUnavailable
			}
			_ = writeWebSocketError(downstream, status, err.Error())
			return
		}
		kind, source, meta, err := responsesWebSocketRequestToSource(baseCtx, source, meta, s.identitySecretCached)
		if err != nil {
			_ = source.Close()
			_ = writeWebSocketError(downstream, http.StatusBadRequest, err.Error())
			return
		}
		switch kind {
		case "response.processed":
			err = session.ForwardProcessedSource(source)
			_ = source.Close()
			if err != nil {
				_ = writeWebSocketError(downstream, http.StatusBadGateway, err.Error())
				return
			}
			if drainConnection.endWork() {
				return
			}
			continue
		case "response.create", "response.append":
			if kind == "response.append" {
				source, meta, err = state.completeAppendSource(baseCtx, source, meta, s.identitySecretCached)
				if err != nil {
					_ = source.Close()
					_ = writeWebSocketError(downstream, http.StatusBadRequest, err.Error())
					return
				}
			}
			body, openErr := source.Open()
			if openErr != nil {
				_ = source.Close()
				_ = writeWebSocketError(downstream, http.StatusInternalServerError, openErr.Error())
				return
			}
			prewarm := codexResponsesGenerateFalse(meta)
			turnBaseCtx, cancelTurn := context.WithCancel(baseCtx)
			// One downstream WebSocket can carry many inference turns. Give each
			// turn its own request and usage events while every retry/bridge inside
			// that turn continues to share the same idempotency keys.
			turnBaseCtx = contextWithRequestID(turnBaseCtx, newRequestID())
			turnBaseCtx = contextWithUsageEventID(turnBaseCtx, newRequestID())
			turnBaseCtx = contextWithBodySource(turnBaseCtx, source)
			turnBaseCtx = contextWithBodyMeta(turnBaseCtx, meta)
			if prewarm {
				turnBaseCtx = context.WithValue(turnBaseCtx, codexResponsesWebSocketPrewarmKey{}, true)
			}
			// Snapshot only unresolved cross-transport context at turn admission.
			// Merely choosing HTTPS from the first turn is not a recovery and must keep
			// native HTTP previous_response_id continuation enabled.
			httpsRecoveryAtStart := session.HTTPSFallbackNeedsRecovery()
			turnCtx := context.WithValue(turnBaseCtx, forceCodexResponsesWebSocketKey{}, true)
			turnCtx = context.WithValue(turnCtx, codexResponsesWebSocketHTTPSRecoveryTurnKey{}, httpsRecoveryAtStart)
			req := r.Clone(turnCtx)
			req.Method = http.MethodPost
			req.Body = body
			req.GetBody = source.Open
			req.ContentLength = source.Size()
			req.Header = r.Header.Clone()
			req.Header.Set("Content-Type", "application/json")
			writer := newResponsesWebSocketWriter(baseCtx, downstream, func(event bodysource.BodyMeta) {
				state.observeMeta(event)
				if event.Type == "response.completed" && strings.TrimSpace(event.ResponseID) != "" {
					session.CompleteHTTPSRecovery()
				}
			}, bodysource.CaptureOptions{
				MaxBytes: s.cfg.MaxBodyBytes, MemoryThreshold: s.cfg.BodyMemoryThresholdBytes, TempDir: s.cfg.BodySpoolDir,
				Budget: s.responseBodyBudget, DiskReserver: s.bodyDiskReserver, TempFileNamePrefix: "codex-pool-ws-response-*",
			}, s.identitySecretCached)
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
			turnPanicked := s.handleGatewayWebSocketTurn(writer, req)
			var writerErr error
			if turnPanicked {
				writer.closeBuffers()
				writer.closed = true
				writerErr = writeWebSocketError(downstream, http.StatusInternalServerError, "internal server error")
			} else {
				writerErr = writer.Close()
			}
			cancelTurn()
			heartbeatErr := <-heartbeatDone
			_ = body.Close()
			_ = source.Close()
			if turnPanicked {
				return
			}
			if heartbeatErr != nil {
				return
			}
			if writerErr != nil {
				return
			}
			if drainConnection.endWork() {
				return
			}
		default:
			_ = source.Close()
			_ = writeWebSocketError(downstream, http.StatusBadRequest, fmt.Sprintf("unsupported websocket request type %q", kind))
			return
		}
	}
}

// handleGatewayWebSocketTurn keeps a panic in one post-upgrade turn from unwinding
// through ServeHTTP as a misleading "GET /v1/responses" failure. Normal transport
// faults are returned as protocol errors before reaching this boundary; this is the
// final containment layer for an unforeseen handler bug and records the per-turn id.
func (s *Server) handleGatewayWebSocketTurn(w *responsesWebSocketWriter, r *http.Request) (panicked bool) {
	defer func() {
		if value := recover(); value != nil {
			panicked = true
			requestID := requestIDFromContext(r.Context())
			log.Printf("[PANIC] responses websocket turn request_id=%s panic=%v", requestID, value)
			panicContext := fmt.Sprintf("request_id=%s panic=%v", requestID, value)
			supervisor.LogPanicEvent(supervisor.Event{
				Module: "responses-websocket-turn", Operation: "serve_turn",
				RequestID: requestID, Route: "v1.responses.websocket",
				Status: http.StatusInternalServerError, Recovered: true, ResponseCommitted: true,
				Message: "module panic: " + panicContext, Panic: panicContext,
			}, value)
		}
	}()
	s.handleGatewayPost(w, r)
	return false
}

func responsesWebSocketRequestToSource(ctx context.Context, source bodysource.BodySource, meta bodysource.BodyMeta, hmacKey []byte) (string, bodysource.BodySource, bodysource.BodyMeta, error) {
	kind := strings.TrimSpace(meta.Type)
	if kind == "" {
		return "", source, meta, fmt.Errorf("missing websocket request type")
	}
	if kind != "response.create" && kind != "response.append" {
		return kind, source, meta, nil
	}
	fields := []bodysource.JSONFieldPatch{{Name: "type", Delete: true}}
	if !meta.StreamPresent {
		fields = append(fields, bodysource.JSONFieldPatch{Name: "stream", Value: []byte("true")})
	}
	patched, err := bodysource.PatchTopLevel(source, meta, fields)
	if err != nil {
		return "", source, meta, err
	}
	patchedMeta, err := bodysource.ScanJSON(ctx, patched, hmacKey)
	if err != nil {
		_ = patched.Close()
		return "", patched, bodysource.BodyMeta{}, err
	}
	return kind, patched, patchedMeta, nil
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
	ctx        context.Context
	conn       webSocketMessageWriter
	header     http.Header
	status     int
	frame      *bodysource.SpoolBuffer
	body       *bodysource.SpoolBuffer
	lineNonCR  bool
	options    bodysource.CaptureOptions
	hmacKey    []byte
	onEvent    func(bodysource.BodyMeta)
	closed     bool
	pendingErr error
}

func newResponsesWebSocketWriter(ctx context.Context, conn webSocketMessageWriter, onEvent func(bodysource.BodyMeta), options bodysource.CaptureOptions, hmacKey []byte) *responsesWebSocketWriter {
	if ctx == nil {
		ctx = context.Background()
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = 1 << 30
	}
	if options.MemoryThreshold <= 0 {
		options.MemoryThreshold = 8 << 20
	}
	return &responsesWebSocketWriter{
		ctx: ctx, conn: conn, header: http.Header{}, onEvent: onEvent, options: options, hmacKey: hmacKey,
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
	if w.closed {
		return 0, bodysource.ErrClosed
	}
	if w.pendingErr != nil {
		return 0, w.pendingErr
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 || !isEventStream(w.header) {
		if w.body == nil {
			w.body, w.pendingErr = w.newBuffer("codex-pool-ws-error-*")
			if w.pendingErr != nil {
				return 0, w.pendingErr
			}
		}
		return w.body.Write(p)
	}
	start := 0
	for index, value := range p {
		if value != '\n' {
			if value != '\r' {
				w.lineNonCR = true
			}
			continue
		}
		boundary := !w.lineNonCR
		w.lineNonCR = false
		if !boundary {
			continue
		}
		if err := w.writeFrameBytes(p[start : index+1]); err != nil {
			w.pendingErr = err
			return start, err
		}
		start = index + 1
		if err := w.flushFrame(); err != nil {
			w.pendingErr = err
			return start, err
		}
	}
	if err := w.writeFrameBytes(p[start:]); err != nil {
		w.pendingErr = err
		return start, err
	}
	return len(p), nil
}

func (w *responsesWebSocketWriter) Flush() {}

func (w *responsesWebSocketWriter) Close() error {
	if w.closed {
		return w.pendingErr
	}
	w.closed = true
	if w.pendingErr != nil {
		w.closeBuffers()
		return w.pendingErr
	}
	if w.status >= 400 || !isEventStream(w.header) {
		err := writeWebSocketErrorSource(w.conn, w.status, w.body)
		w.closeBuffers()
		return err
	}
	if w.frame != nil && w.frame.Size() > 0 {
		if err := w.flushFrame(); err != nil {
			w.closeBuffers()
			return err
		}
	}
	w.closeBuffers()
	return nil
}

func (w *responsesWebSocketWriter) newBuffer(prefix string) (*bodysource.SpoolBuffer, error) {
	options := w.options
	options.TempFileNamePrefix = prefix
	return bodysource.NewSpoolBuffer(w.ctx, options)
}

func (w *responsesWebSocketWriter) writeFrameBytes(payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	if w.frame == nil {
		var err error
		w.frame, err = w.newBuffer("codex-pool-ws-sse-frame-*")
		if err != nil {
			return err
		}
	}
	_, err := w.frame.Write(payload)
	return err
}

func (w *responsesWebSocketWriter) flushFrame() error {
	frame := w.frame
	w.frame = nil
	if frame == nil || frame.Size() == 0 {
		if frame != nil {
			_ = frame.Close()
		}
		return nil
	}
	defer frame.Close()
	payload, hasData, err := extractSSEDataSource(w.ctx, frame, w.options)
	if err != nil {
		return err
	}
	if payload == nil {
		return nil
	}
	defer payload.Close()
	if !hasData || payload.Size() == 0 {
		return nil
	}
	if payload.Size() <= 64 {
		raw, readErr := bodysource.ReadAll(payload)
		if readErr != nil {
			return readErr
		}
		if strings.TrimSpace(string(raw)) == "[DONE]" {
			return nil
		}
	}
	meta, scanErr := bodysource.ScanJSON(w.ctx, payload, w.hmacKey)
	if w.onEvent != nil {
		if scanErr == nil {
			w.onEvent(meta)
		}
	}
	return writeWebSocketSourceMessage(w.conn, websocket.TextMessage, payload)
}

func (w *responsesWebSocketWriter) closeBuffers() {
	if w.frame != nil {
		_ = w.frame.Close()
		w.frame = nil
	}
	if w.body != nil {
		_ = w.body.Close()
		w.body = nil
	}
}

func extractSSEDataSource(ctx context.Context, frame bodysource.BodySource, options bodysource.CaptureOptions) (*bodysource.SpoolBuffer, bool, error) {
	reader, err := frame.Open()
	if err != nil {
		return nil, false, err
	}
	defer reader.Close()
	options.TempFileNamePrefix = "codex-pool-ws-sse-data-*"
	payload, err := bodysource.NewSpoolBuffer(ctx, options)
	if err != nil {
		return nil, false, err
	}
	buffered := bufio.NewReaderSize(reader, bodysource.DefaultChunkSize)
	hasData := false
	for {
		fragment, readErr := buffered.ReadSlice('\n')
		firstFragment := true
		active := false
		for {
			part := fragment
			if firstFragment {
				active = len(part) >= len("data:") && bytes.Equal(part[:len("data:")], []byte("data:"))
				if active {
					if hasData {
						if _, err = payload.Write([]byte{'\n'}); err != nil {
							_ = payload.Close()
							return nil, false, err
						}
					}
					hasData = true
					part = part[len("data:"):]
					if len(part) > 0 && part[0] == ' ' {
						part = part[1:]
					}
				}
				firstFragment = false
			}
			if active {
				if readErr != bufio.ErrBufferFull {
					part = bytes.TrimSuffix(part, []byte{'\n'})
					part = bytes.TrimSuffix(part, []byte{'\r'})
				}
				if len(part) > 0 {
					if _, err = payload.Write(part); err != nil {
						_ = payload.Close()
						return nil, false, err
					}
				}
			}
			if readErr != bufio.ErrBufferFull {
				break
			}
			fragment, readErr = buffered.ReadSlice('\n')
		}
		if readErr == io.EOF {
			return payload, hasData, nil
		}
		if readErr != nil {
			_ = payload.Close()
			return nil, false, readErr
		}
	}
}

func writeWebSocketSourceMessage(conn webSocketMessageWriter, messageType int, source bodysource.BodySource) error {
	if streaming, ok := conn.(interface {
		WriteSourceMessage(int, bodysource.BodySource) error
	}); ok {
		return streaming.WriteSourceMessage(messageType, source)
	}
	if source.Size() > 1<<20 {
		return errors.New("websocket writer does not support streaming messages")
	}
	raw, err := bodysource.ReadAll(source)
	if err != nil {
		return err
	}
	return conn.WriteMessage(messageType, raw)
}

func writeWebSocketErrorSource(conn webSocketMessageWriter, status int, source bodysource.BodySource) error {
	return writeWebSocketError(conn, http.StatusServiceUnavailable, "")
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
	requestID := newRequestID()
	if status < 400 || status >= 500 {
		status = http.StatusServiceUnavailable
		message = "The relay service is temporarily unavailable. Please retry."
	} else {
		message, _ = safeClientError(status)
	}
	payload := map[string]interface{}{
		"type":   "error",
		"status": status,
		"error": map[string]interface{}{
			"type":       map[bool]string{true: "server_error", false: "invalid_request_error"}[status == http.StatusServiceUnavailable],
			"code":       map[bool]string{true: "service_unavailable", false: "invalid_request"}[status == http.StatusServiceUnavailable],
			"message":    message,
			"request_id": requestID,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, raw)
}
