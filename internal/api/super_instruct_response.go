package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/superinstruct"
	"codex-account-pool/internal/supervisor"
	"github.com/tidwall/sjson"
)

const (
	superInstructObservationLimit = 1 << 20
	superInstructObservationDepth = 64

	// Rewrite-enabled streams buffer the complete SSE so M3 runs on the finished
	// body exactly as the upstream pipeline does. A byte cap and an idle-time
	// deadline fall back to raw passthrough so a pathological stream can never
	// wedge the client or the gateway.
	superInstructStreamBufferLimit   = 4 << 20
	superInstructStreamIdleTimeout   = 60 * time.Second
	superInstructKeepaliveInterval   = 500 * time.Millisecond
)

type superInstructObservation struct {
	processor *superinstruct.Processor
	meta      superinstruct.RequestMeta
	status    int
	body      []byte
	duration  time.Duration
	opts      superinstruct.ProcessOptions
}

var (
	superInstructObservations    = make(chan superInstructObservation, superInstructObservationDepth)
	superInstructObservationOnce sync.Once
)

func startSuperInstructObservationWorker() {
	superInstructObservationOnce.Do(func() {
		go func() {
			defer supervisor.Recover("super-instruct-observation-worker")
			for observation := range superInstructObservations {
				processSuperInstructObservation(observation)
			}
		}()
	})
}

func processSuperInstructObservation(observation superInstructObservation) {
	defer supervisor.Recover("super-instruct-observation")
	if observation.processor != nil {
		observation.processor.Process(observation.meta, observation.status, observation.body, observation.duration, observation.opts)
	}
}

func enqueueSuperInstructObservation(queue chan<- superInstructObservation, observation superInstructObservation) bool {
	select {
	case queue <- observation:
		return true
	default:
		return false
	}
}

type superInstructResponsePipelineActiveKey struct{}

func withSuperInstructResponsePipelineActive(ctx context.Context) context.Context {
	return context.WithValue(ctx, superInstructResponsePipelineActiveKey{}, true)
}

func superInstructResponsePipelineActive(ctx context.Context) bool {
	active, _ := ctx.Value(superInstructResponsePipelineActiveKey{}).(bool)
	return active
}

func superInstructResponseFeatures(group storage.Group, model string) superinstruct.ProcessOptions {
	profile, _ := superInstructPolicyForModel(group, model)
	return superinstruct.ProcessOptions{
		ResponseRewriteEnabled: profile.ResponseRewriteEnabled,
		MemoryEnabled:          profile.MemoryEnabled,
		MonitorEnabled:         profile.MonitorEnabled,
	}
}

func (s *Server) maybeSuperInstructResponsePipeline(w http.ResponseWriter, r *http.Request, raw []byte, model string) (http.ResponseWriter, *http.Request, func(), bool) {
	if s == nil || r == nil || superInstructResponsePipelineActive(r.Context()) {
		return w, r, nil, false
	}
	opts := superInstructResponseFeatures(requestUserGroupPolicy(r.Context()), model)
	if !opts.Enabled() {
		return w, r, nil, false
	}
	stream := isStreamRequest(raw)
	meta := superinstruct.RequestMeta{
		UserMessage: superinstruct.ExtractUserSource(raw),
		Path:        r.URL.RequestURI(),
		Timestamp:   time.Now().UTC(),
	}
	meta.Category = superinstruct.Categorize(meta.UserMessage)
	start := time.Now()
	processor := s.superInstructProcessor()
	startSuperInstructObservationWorker()
	if !opts.ResponseRewriteEnabled {
		// Memory/Monitor-only responses stay byte-transparent: a bounded tee
		// records a copy for post-response observation while bytes are forwarded.
		ow := newSuperInstructObservingResponseWriter(w)
		r = r.WithContext(withSuperInstructResponsePipelineActive(r.Context()))
		finish := func() {
			finishSuperInstructObservation(processor, ow.status, ow.body.Bytes(), ow.hijacked, meta, opts, time.Since(start))
		}
		return ow, r, finish, true
	}
	bw := newSuperInstructBufferingResponseWriter(w)
	if stream {
		// Rewrite-enabled stream: upstream-aligned full buffering. The completed
		// SSE is parsed, M3 runs, and on a refusal match the whole body is swapped
		// for a protocol-correct SSE replacement. Keepalive comment frames keep
		// the client alive while the buffer is held.
		bw.bufferStreams = true
		bw.bufferLimit = superInstructStreamBufferLimit
		bw.bufferIdleTimeout = superInstructStreamIdleTimeout
		bw.keepaliveInterval = superInstructKeepaliveInterval
		bw.lastWrite = time.Now()
	}
	r = r.WithContext(withSuperInstructResponsePipelineActive(r.Context()))
	finish := func() {
		s.finishSuperInstructResponsePipeline(bw, r, raw, model, meta, opts, time.Since(start))
	}
	return bw, r, finish, true
}

func finishSuperInstructObservation(processor *superinstruct.Processor, status int, body []byte, hijacked bool, meta superinstruct.RequestMeta, opts superinstruct.ProcessOptions, duration time.Duration) {
	if processor == nil || hijacked || (!opts.MemoryEnabled && !opts.MonitorEnabled) {
		return
	}
	if status == 0 {
		status = http.StatusOK
	}
	observation := superInstructObservation{
		processor: processor,
		meta:      meta,
		status:    status,
		body:      append([]byte(nil), body[:min(len(body), superInstructObservationLimit)]...),
		duration:  duration,
		opts:      opts,
	}
	// Observation must never extend response completion. If the bounded worker is
	// congested, this response is deliberately omitted from Memory/Monitor.
	_ = enqueueSuperInstructObservation(superInstructObservations, observation)
}

func (s *Server) finishSuperInstructResponsePipeline(w *superInstructBufferingResponseWriter, r *http.Request, requestRaw []byte, model string, meta superinstruct.RequestMeta, opts superinstruct.ProcessOptions, duration time.Duration) {
	if w == nil || w.hijacked {
		return
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	if w.passthrough {
		opts.ResponseRewriteEnabled = false
		finishSuperInstructObservation(s.superInstructProcessor(), status, w.body.Bytes(), false, meta, opts, duration)
		return
	}
	original := w.body.Bytes()
	if isStreamRequest(requestRaw) {
		s.finishSuperInstructStreamPipeline(w, r, status, original, meta, opts, duration)
		return
	}
	body := original
	tampered := false
	// M3 is allowed only for one unambiguous assistant-text field. Tool and
	// reasoning envelopes remain byte-identical, and a successful rewrite is
	// placed back into the original protocol envelope.
	if opts.ResponseRewriteEnabled && !superInstructResponseHasStructuredOutput(original) {
		if _, valid := superInstructRewriteSingleAssistantText(r, original, ""); valid {
			result := s.superInstructProcessor().Process(meta, status, original, duration, opts)
			if result.Tampered {
				body, tampered = superInstructRewriteSingleAssistantText(r, original, string(result.Body))
			}
			w.writeFinal(status, body, tampered, "application/json")
			return
		}
	}
	opts.ResponseRewriteEnabled = false
	if opts.MemoryEnabled || opts.MonitorEnabled {
		s.superInstructProcessor().Process(meta, status, original, duration, opts)
	}
	w.writeFinal(status, original, false, "")
}

// finishSuperInstructStreamPipeline runs M3 on a fully buffered SSE body. On a
// refusal match the entire stream is replaced with a protocol-correct SSE
// sequence for the request path (upstream alignment: no partial-stream rewrite,
// streaming latency is deliberately traded for a guaranteed tamper). Structured
// or malformed streams stay byte-for-byte identical.
func (s *Server) finishSuperInstructStreamPipeline(w *superInstructBufferingResponseWriter, r *http.Request, status int, original []byte, meta superinstruct.RequestMeta, opts superinstruct.ProcessOptions, duration time.Duration) {
	if opts.ResponseRewriteEnabled && !superInstructResponseHasStructuredOutput(original) {
		result := s.superInstructProcessor().Process(meta, status, original, duration, opts)
		if result.Tampered {
			replacement := wrapSuperInstructStreamTamper(r, string(result.Body))
			w.writeFinal(status, replacement, true, "text/event-stream")
			return
		}
		// Not a refusal: Process already recorded Memory/Monitor. Pass the
		// completed stream through byte-for-byte.
		w.writeFinal(status, original, false, "")
		return
	}
	opts.ResponseRewriteEnabled = false
	if opts.MemoryEnabled || opts.MonitorEnabled {
		s.superInstructProcessor().Process(meta, status, original, duration, opts)
	}
	w.writeFinal(status, original, false, "")
}

// wrapSuperInstructStreamTamper emits a protocol-correct SSE replacement for the
// request path. The Responses form mirrors the upstream wrap_tamper_as_sse
// exactly; chat completions and Anthropic messages get their own minimal valid
// streams so non-Responses clients are not fed a foreign envelope.
func wrapSuperInstructStreamTamper(r *http.Request, text string) []byte {
	path := ""
	if r != nil && r.URL != nil {
		path = r.URL.Path
	}
	switch {
	case path == "/v1/chat/completions":
		return wrapSuperInstructChatSSE(text)
	case path == "/v1/messages" || strings.HasSuffix(path, "/v1/messages"):
		return wrapSuperInstructAnthropicSSE(text)
	default:
		return wrapSuperInstructTamperSSE(text)
	}
}

func wrapSuperInstructChatSSE(text string) []byte {
	chunk := map[string]interface{}{
		"id":      "chatcmpl_tamper",
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   "super-instruct",
		"choices": []interface{}{map[string]interface{}{
			"index":         0,
			"delta":         map[string]interface{}{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
	}
	raw, _ := json.Marshal(chunk)
	return []byte("data: " + string(raw) + "\n\ndata: [DONE]\n\n")
}

func wrapSuperInstructAnthropicSSE(text string) []byte {
	messageStart := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            "msg_tamper",
			"type":          "message",
			"role":          "assistant",
			"model":         "super-instruct",
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
		},
	}
	blockStart := map[string]interface{}{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	}
	blockDelta := map[string]interface{}{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	}
	blockStop := map[string]interface{}{"type": "content_block_stop", "index": 0}
	messageDelta := map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]interface{}{"output_tokens": 0},
	}
	messageStop := map[string]interface{}{"type": "message_stop"}
	var out strings.Builder
	for _, event := range []struct {
		name  string
		value interface{}
	}{
		{"message_start", messageStart},
		{"content_block_start", blockStart},
		{"content_block_delta", blockDelta},
		{"content_block_stop", blockStop},
		{"message_delta", messageDelta},
		{"message_stop", messageStop},
	} {
		raw, _ := json.Marshal(event.value)
		out.WriteString("event: " + event.name + "\ndata: " + string(raw) + "\n\n")
	}
	return []byte(out.String())
}

func superInstructResponseHasStructuredOutput(raw []byte) bool {
	parse := func(data []byte) (valid, structured bool) {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value interface{}
		if decoder.Decode(&value) != nil {
			return false, false
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return false, false
		}
		return true, superInstructStructuredValue(value, 0)
	}
	if valid, structured := parse(raw); valid && structured {
		return true
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), superInstructObservationLimit)
	var eventData bytes.Buffer
	flushEvent := func() bool {
		defer eventData.Reset()
		data := bytes.TrimSpace(eventData.Bytes())
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			return false
		}
		valid, structured := parse(data)
		return !valid || structured
	}
	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte{'\r'})
		if len(line) == 0 {
			if flushEvent() {
				return true
			}
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimPrefix(line, []byte("data:"))
		if len(data) > 0 && data[0] == ' ' {
			data = data[1:]
		}
		if eventData.Len()+len(data)+1 > superInstructObservationLimit {
			return true
		}
		if eventData.Len() > 0 {
			eventData.WriteByte('\n')
		}
		_, _ = eventData.Write(data)
	}
	// An oversized or malformed SSE frame is not safe rewrite input. Preserve it
	// byte-for-byte rather than risking loss of a structured event we could not inspect.
	return scanner.Err() != nil || flushEvent()
}

func superInstructStructuredValue(value interface{}, depth int) bool {
	if depth > 12 || value == nil {
		return false
	}
	switch value := value.(type) {
	case []interface{}:
		for _, item := range value {
			if superInstructStructuredValue(item, depth+1) {
				return true
			}
		}
	case map[string]interface{}:
		kind := strings.ToLower(strings.TrimSpace(stringValue(value["type"])))
		if strings.HasSuffix(kind, "_call") || strings.HasSuffix(kind, "_call_output") {
			return true
		}
		for _, marker := range []string{
			"tool_use", "tool_call", "function_call", "reasoning", "thinking", "input_json",
			"local_shell_call", "computer_call", "web_search_call", "file_search_call",
			"tool_search", "image_generation_call", "code_interpreter_call", "mcp_", "compaction",
		} {
			if strings.Contains(kind, marker) {
				return true
			}
		}
		for _, key := range []string{"tool_calls", "function_call", "reasoning", "reasoning_content", "thinking", "encrypted_content"} {
			if field, exists := value[key]; exists && field != nil {
				return true
			}
		}
		for _, key := range []string{"output", "choices", "message", "delta", "content", "content_block", "item", "response"} {
			if superInstructStructuredValue(value[key], depth+1) {
				return true
			}
		}
	}
	return false
}

func (s *Server) superInstructProcessor() *superinstruct.Processor {
	if s.superMemory == nil {
		s.superMemory = superinstruct.NewMemoryKernel("")
	}
	if s.superMonitor == nil {
		s.superMonitor = superinstruct.NewMonitorPanel()
	}
	return superinstruct.NewProcessor(s.superMemory, s.superMonitor)
}

func superInstructRewriteSingleAssistantText(r *http.Request, raw []byte, replacement string) ([]byte, bool) {
	path := ""
	if r != nil && r.URL != nil {
		path = r.URL.Path
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root map[string]interface{}
	if decoder.Decode(&root) != nil || root == nil {
		return raw, false
	}
	// A second JSON value or trailing non-whitespace bytes make the envelope
	// ambiguous.  Only a single complete object is eligible for in-place text
	// replacement.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return raw, false
	}
	valid := false
	switch {
	case path == "/v1/chat/completions":
		valid = rewriteSuperInstructChatText(root, replacement)
	case path == "/v1/messages" || strings.HasSuffix(path, "/v1/messages"):
		valid = rewriteSuperInstructAnthropicText(root, replacement)
	default:
		valid = rewriteSuperInstructResponsesText(root, replacement)
	}
	if !valid {
		return raw, false
	}
	if replacement == "" {
		return raw, true
	}
	out := raw
	var err error
	switch {
	case path == "/v1/chat/completions":
		out, err = sjson.SetBytes(out, "choices.0.message.content", replacement)
	case path == "/v1/messages" || strings.HasSuffix(path, "/v1/messages"):
		out, err = sjson.SetBytes(out, "content.0.text", replacement)
	default:
		out, err = sjson.SetBytes(out, "output.0.content.0.text", replacement)
		if err == nil {
			if _, exists := root["output_text"]; exists {
				out, err = sjson.SetBytes(out, "output_text", replacement)
			}
		}
	}
	if err != nil {
		return raw, false
	}
	return out, true
}

func rewriteSuperInstructResponsesText(root map[string]interface{}, replacement string) bool {
	output, ok := root["output"].([]interface{})
	if !ok || len(output) != 1 {
		return false
	}
	message, ok := output[0].(map[string]interface{})
	if !ok || message["type"] != "message" || message["role"] != "assistant" {
		return false
	}
	content, ok := message["content"].([]interface{})
	if !ok || len(content) != 1 {
		return false
	}
	block, ok := content[0].(map[string]interface{})
	text, textOK := block["text"].(string)
	if !ok || !textOK || block["type"] != "output_text" {
		return false
	}
	if top, exists := root["output_text"]; exists {
		topText, topOK := top.(string)
		if !topOK || topText != text {
			return false
		}
		if replacement != "" {
			root["output_text"] = replacement
		}
	}
	if replacement != "" {
		block["text"] = replacement
	}
	return true
}

func rewriteSuperInstructChatText(root map[string]interface{}, replacement string) bool {
	choices, ok := root["choices"].([]interface{})
	if !ok || len(choices) != 1 {
		return false
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return false
	}
	message, ok := choice["message"].(map[string]interface{})
	if !ok || message["role"] != "assistant" {
		return false
	}
	for _, key := range []string{"tool_calls", "function_call", "reasoning", "reasoning_content"} {
		if value, exists := message[key]; exists && value != nil {
			return false
		}
	}
	if _, ok := message["content"].(string); !ok {
		return false
	}
	if replacement != "" {
		message["content"] = replacement
	}
	return true
}

func rewriteSuperInstructAnthropicText(root map[string]interface{}, replacement string) bool {
	if root["type"] != "message" || root["role"] != "assistant" {
		return false
	}
	content, ok := root["content"].([]interface{})
	if !ok || len(content) != 1 {
		return false
	}
	block, ok := content[0].(map[string]interface{})
	if !ok || block["type"] != "text" {
		return false
	}
	if _, ok := block["text"].(string); !ok {
		return false
	}
	if replacement != "" {
		block["text"] = replacement
	}
	return true
}

func wrapSuperInstructTamperSSE(text string) []byte {
	created := map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id": "resp_tamper", "object": "response", "status": "in_progress", "output": []interface{}{},
		},
	}
	delta := map[string]interface{}{
		"type": "response.output_text.delta", "item_id": "msg_tamper",
		"output_index": 0, "content_index": 0, "delta": text,
	}
	done := map[string]interface{}{
		"type": "response.output_text.done", "item_id": "msg_tamper",
		"output_index": 0, "content_index": 0, "text": text,
	}
	completed := map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id": "resp_tamper", "object": "response", "status": "completed",
			"output": []interface{}{map[string]interface{}{
				"id": "msg_tamper", "type": "message", "role": "assistant", "status": "completed",
				"content": []interface{}{map[string]interface{}{"type": "output_text", "text": text}},
			}},
			"usage": map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
		},
	}
	var out strings.Builder
	for _, event := range []struct {
		name  string
		value interface{}
	}{
		{"response.created", created},
		{"response.output_text.delta", delta},
		{"response.output_text.done", done},
		{"response.completed", completed},
	} {
		raw, _ := json.Marshal(event.value)
		out.WriteString("event: " + event.name + "\ndata: " + string(raw) + "\n\n")
	}
	return []byte(out.String())
}

// superInstructObservingResponseWriter is a transparent tee. Unlike the legacy
// rewrite writer below, it forwards headers, chunks, and Flush calls immediately
// and records a copy solely for post-response Memory/Monitor processing.
type superInstructObservingResponseWriter struct {
	dst              http.ResponseWriter
	status           int
	body             bytes.Buffer
	hijacked         bool
	observationLimit int
}

func newSuperInstructObservingResponseWriter(dst http.ResponseWriter) *superInstructObservingResponseWriter {
	return &superInstructObservingResponseWriter{dst: dst, observationLimit: superInstructObservationLimit}
}

func (w *superInstructObservingResponseWriter) Header() http.Header {
	return w.dst.Header()
}

func (w *superInstructObservingResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.dst.WriteHeader(status)
}

func (w *superInstructObservingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.dst.Write(p)
	if n > 0 {
		captureSuperInstructObservation(&w.body, p[:n], w.observationLimit)
	}
	return n, err
}

func (w *superInstructObservingResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *superInstructObservingResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	// Hide ReadFrom from io.Copy so every forwarded byte passes through Write and
	// is captured exactly once while retaining normal backpressure.
	return io.Copy(struct{ io.Writer }{w}, src)
}

func (w *superInstructObservingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.dst.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying response writer does not support hijacking")
	}
	w.hijacked = true
	return h.Hijack()
}

func (w *superInstructObservingResponseWriter) Push(target string, opts *http.PushOptions) error {
	p, ok := w.dst.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}

func (w *superInstructObservingResponseWriter) Unwrap() http.ResponseWriter {
	return w.dst
}

type superInstructBufferingResponseWriter struct {
	dst              http.ResponseWriter
	header           http.Header
	status           int
	body             bytes.Buffer
	hijacked         bool
	passthrough      bool
	bufferStreams    bool
	observationLimit int
	// Stream-buffering safety valves. Only rewrite-enabled streams set these.
	bufferLimit      int
	bufferIdleTimeout time.Duration
	lastWrite        time.Time
	// Keepalive comment frames keep a buffering SSE client from timing out while
	// the gateway holds the body for M3. The goroutine is started lazily on the
	// first buffered write and stopped before any final write or passthrough.
	keepaliveInterval time.Duration
	keepaliveStop     chan struct{}
	keepaliveOnce     sync.Once
}

func newSuperInstructBufferingResponseWriter(dst http.ResponseWriter) *superInstructBufferingResponseWriter {
	return &superInstructBufferingResponseWriter{dst: dst, header: make(http.Header), observationLimit: superInstructObservationLimit}
}

func (w *superInstructBufferingResponseWriter) Header() http.Header {
	return w.header
}

func (w *superInstructBufferingResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
}

func (w *superInstructBufferingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.passthrough {
		return w.dst.Write(p)
	}
	streaming := isEventStream(w.header)
	if streaming && !w.bufferStreams {
		w.startPassthrough()
		n, err := w.dst.Write(p)
		if n > 0 {
			captureSuperInstructObservation(&w.body, p[:n], w.observationLimit)
		}
		return n, err
	}
	if streaming && w.bufferStreams {
		w.ensureKeepalive()
		if w.superInstructStreamOverflow(len(p)) {
			w.startPassthrough()
			n, err := w.dst.Write(p)
			if n > 0 {
				captureSuperInstructObservation(&w.body, p[:n], w.observationLimit)
			}
			return n, err
		}
		w.lastWrite = time.Now()
		_, _ = w.body.Write(p)
		return len(p), nil
	}
	_, _ = w.body.Write(p)
	return len(p), nil
}

func (w *superInstructBufferingResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	streaming := isEventStream(w.header)
	if streaming && !w.bufferStreams {
		w.startPassthrough()
		if f, ok := w.dst.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	if streaming && w.bufferStreams {
		// Keepalive covers the buffering window; nothing is flushed to the client
		// until finish decides the final body.
		w.ensureKeepalive()
	}
}

func (w *superInstructBufferingResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{w}, src)
}

// superInstructStreamOverflow reports whether holding the SSE for rewrite would
// outgrow safe bounds. Either the byte cap (counting the chunk about to be
// buffered) or a long silent stall converts the stream to raw passthrough so the
// gateway never buffers without bound.
func (w *superInstructBufferingResponseWriter) superInstructStreamOverflow(pending int) bool {
	if w.bufferLimit > 0 && w.body.Len()+pending >= w.bufferLimit {
		return true
	}
	return w.bufferIdleTimeout > 0 && !w.lastWrite.IsZero() && time.Since(w.lastWrite) > w.bufferIdleTimeout
}

// ensureKeepalive starts a comment-frame heartbeat while a rewrite-enabled SSE is
// held. The goroutine writes only to dst and a header snapshot captured at start,
// so it never races the handler's buffered body.
func (w *superInstructBufferingResponseWriter) ensureKeepalive() {
	if !w.bufferStreams || !isEventStream(w.header) || w.passthrough {
		return
	}
	w.keepaliveOnce.Do(func() {
		interval := w.keepaliveInterval
		if interval <= 0 {
			return
		}
		status := w.status
		if status == 0 {
			status = http.StatusOK
		}
		headers := make(http.Header)
		for key, values := range w.header {
			headers[key] = append([]string(nil), values...)
		}
		stop := make(chan struct{})
		w.keepaliveStop = stop
		go func() {
			defer supervisor.Recover("super-instruct-stream-keepalive")
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					w.emitKeepalive(status, headers)
				}
			}
		}()
	})
}

func (w *superInstructBufferingResponseWriter) emitKeepalive(status int, headers http.Header) {
	dstHeader := w.dst.Header()
	for key, values := range headers {
		dstHeader.Del(key)
		for _, value := range values {
			dstHeader.Add(key, value)
		}
	}
	w.dst.WriteHeader(status)
	_, _ = w.dst.Write([]byte(": keepalive\n\n"))
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *superInstructBufferingResponseWriter) stopKeepalive() {
	if w.keepaliveStop != nil {
		close(w.keepaliveStop)
		w.keepaliveStop = nil
	}
}

func (w *superInstructBufferingResponseWriter) startPassthrough() {
	if w == nil || w.passthrough {
		return
	}
	w.stopKeepalive()
	w.passthrough = true
	if w.status == 0 {
		w.status = http.StatusOK
	}
	dstHeader := w.dst.Header()
	for key, values := range w.header {
		dstHeader.Del(key)
		for _, value := range values {
			dstHeader.Add(key, value)
		}
	}
	w.dst.WriteHeader(w.status)
	if w.body.Len() > 0 {
		_, _ = w.dst.Write(w.body.Bytes())
		w.body.Reset()
	}
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
}

func captureSuperInstructObservation(dst *bytes.Buffer, p []byte, limit int) {
	if dst == nil || len(p) == 0 || (limit > 0 && dst.Len() >= limit) {
		return
	}
	if limit > 0 {
		remaining := limit - dst.Len()
		if len(p) > remaining {
			p = p[:remaining]
		}
	}
	_, _ = dst.Write(p)
}

func (w *superInstructBufferingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.dst.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying response writer does not support hijacking")
	}
	w.hijacked = true
	return h.Hijack()
}

func (w *superInstructBufferingResponseWriter) Push(target string, opts *http.PushOptions) error {
	p, ok := w.dst.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}

func (w *superInstructBufferingResponseWriter) Unwrap() http.ResponseWriter {
	return w.dst
}

func (w *superInstructBufferingResponseWriter) writeFinal(status int, body []byte, tampered bool, contentType string) {
	if w == nil || w.dst == nil || w.hijacked {
		return
	}
	w.stopKeepalive()
	dstHeader := w.dst.Header()
	for key, values := range w.header {
		dstHeader.Del(key)
		for _, value := range values {
			dstHeader.Add(key, value)
		}
	}
	if tampered {
		dstHeader.Del("Content-Length")
		dstHeader.Del("Content-Encoding")
		dstHeader.Del("Transfer-Encoding")
		if contentType != "" {
			dstHeader.Set("Content-Type", contentType)
		}
		if strings.Contains(contentType, "text/event-stream") {
			dstHeader.Set("Cache-Control", "no-cache")
		}
	}
	if status == 0 {
		status = http.StatusOK
	}
	w.dst.WriteHeader(status)
	if status == http.StatusNoContent || status == http.StatusNotModified {
		return
	}
	_, _ = w.dst.Write(body)
	if f, ok := w.dst.(http.Flusher); ok {
		f.Flush()
	}
}
