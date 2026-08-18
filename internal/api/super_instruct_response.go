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
	// body exactly as the upstream pipeline does. "Complete" is defined per
	// protocol by a terminal frame (response.completed/[DONE]/message_stop) and is
	// capped by a front window so a long in-flight agentic turn can never wedge the
	// client: the stream degrades to raw passthrough once the window is exhausted.
	// The byte cap and idle deadline below remain as backstops for streams that
	// never emit a terminal frame and outgrow the window's safety margins.
	superInstructStreamBufferLimit     = 4 << 20
	superInstructStreamIdleTimeout     = 60 * time.Second
	superInstructKeepaliveInterval     = 500 * time.Millisecond
	superInstructStreamFrontWindowTimeout = 20 * time.Second
	superInstructStreamFrontWindowBytes   = 256 << 10
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
		// Rewrite-enabled stream: buffer the complete SSE, run M3 on the finished
		// body, and on a refusal match swap in a protocol-correct SSE replacement.
		// A complete packet is bounded by its protocol terminal frame AND a front
		// window, so a long agentic turn (no terminal frame for minutes) degrades to
		// live passthrough instead of hanging the client. Keepalive comment frames
		// keep the client alive while the packet is held.
		bw.bufferStreams = true
		bw.bufferLimit = superInstructStreamBufferLimit
		bw.bufferIdleTimeout = superInstructStreamIdleTimeout
		bw.keepaliveInterval = superInstructKeepaliveInterval
		bw.lastWrite = time.Now()
		bw.scanner = newSuperInstructStreamScan(r.URL.Path)
		bw.frontWindowTimeout = superInstructStreamFrontWindowTimeout
		bw.frontWindowBytes = superInstructStreamFrontWindowBytes
		profile, _ := superInstructPolicyForModel(requestUserGroupPolicy(r.Context()), model)
		if profile.StreamRewriteFrontWindowSeconds > 0 {
			bw.frontWindowTimeout = time.Duration(profile.StreamRewriteFrontWindowSeconds) * time.Second
		}
		if profile.StreamRewriteFrontWindowBytes > 0 {
			bw.frontWindowBytes = int(profile.StreamRewriteFrontWindowBytes)
		}
		// The finalizer settles the held packet the moment a terminal frame is
		// buffered (or at handler return for a stream that ends without one). It
		// must always write the final body through the writer.
		bw.finalizer = func(writer *superInstructBufferingResponseWriter, original []byte, status int) {
			s.finishSuperInstructStreamPipeline(writer, r, status, original, meta, opts, time.Since(start))
		}
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
	if w.released {
		// The stream already committed at its protocol terminal frame. The
		// finalizer ran M3 and wrote the final body; there is nothing left to do.
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

// superInstructStreamFinalizer settles a fully buffered SSE packet. It runs M3 on
// the held body and must always write the final body through the writer, which
// marks itself released before invoking it.
type superInstructStreamFinalizer func(w *superInstructBufferingResponseWriter, original []byte, status int)

// superInstructStreamScan finds the protocol terminal frame that marks a complete
// SSE packet. It consumes complete events (delimited by a blank line) and only
// inspects the top-level "type" of each data frame, so assistant text that merely
// contains the terminal strings can never trigger a false early release.
type superInstructStreamScan struct {
	path    string
	pending []byte
	done    bool
}

func newSuperInstructStreamScan(path string) *superInstructStreamScan {
	return &superInstructStreamScan{path: path}
}

func (s *superInstructStreamScan) feed(p []byte) {
	if s == nil || s.done || len(p) == 0 {
		return
	}
	// Normalize CRLF so both line styles delimit events identically.
	p = bytes.ReplaceAll(p, []byte("\r\n"), []byte("\n"))
	s.pending = append(s.pending, p...)
	for !s.done {
		index := bytes.Index(s.pending, []byte("\n\n"))
		if index < 0 {
			return
		}
		block := s.pending[:index]
		s.pending = s.pending[index+2:]
		if superInstructStreamEventTerminal(block) {
			s.done = true
			return
		}
	}
}

// superInstructStreamEventTerminal reports whether a complete SSE event carries the
// protocol terminal frame for any of the served stream protocols. The Responses
// and Anthropic terminals are matched by the data frame's top-level "type"; chat
// completions terminate on the bare [DONE] marker. Frames whose data does not
// decode as JSON (or carries another top-level type) are never terminal, so
// assistant text that merely quotes the marker strings cannot end a packet early.
func superInstructStreamEventTerminal(block []byte) bool {
	for _, line := range bytes.Split(block, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimPrefix(line, []byte("data:"))
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			return true
		}
		var event struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &event) == nil {
			switch event.Type {
			case "response.completed", "response.failed", "response.incomplete", "message_stop":
				return true
			}
		}
	}
	return false
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
	bufferLimit       int
	bufferIdleTimeout time.Duration
	lastWrite         time.Time
	// Complete-packet boundary for held SSE: the protocol terminal frame found by
	// the scanner, bounded by a front window so a long in-flight agentic turn
	// degrades to passthrough instead of hanging the client.
	scanner            *superInstructStreamScan
	frontWindowTimeout time.Duration
	frontWindowBytes   int
	windowStarted      bool
	windowDeadline     time.Time
	released           bool
	finalizer          superInstructStreamFinalizer
	// Keepalive comment frames keep a buffering SSE client from timing out while
	// the gateway holds the body for M3. The goroutine is started lazily on the
	// first buffered write and stopped before any final write or passthrough.
	keepaliveInterval time.Duration
	keepaliveStop     chan struct{}
	keepaliveOnce     sync.Once
	// writeMu serializes every downstream write so the keepalive goroutine can
	// never splice a comment frame into a final body or corrupt a WebSocket
	// writer's shared frame state.
	writeMu sync.Mutex
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
	if w.released {
		// A complete packet already committed at its protocol terminal frame. Any
		// bytes the upstream sends after that frame are not part of the packet and
		// must not reach the client.
		return len(p), nil
	}
	if w.passthrough {
		return w.writeToDst(p)
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
		w.startStreamWindow()
		w.lastWrite = time.Now()
		_, _ = w.body.Write(p)
		// A terminal frame ends the packet the moment it is buffered: M3 runs and
		// the final body is committed without waiting for upstream EOF.
		if w.scanner != nil {
			w.scanner.feed(p)
			if w.scanner.done {
				w.releaseStream()
				return len(p), nil
			}
		}
		if w.superInstructStreamOverflow(0) || w.superInstructStreamWindowExpired(0) {
			w.startPassthrough()
			return len(p), nil
		}
		return len(p), nil
	}
	_, _ = w.body.Write(p)
	return len(p), nil
}

func (w *superInstructBufferingResponseWriter) Flush() {
	if w.released {
		// The complete packet already committed at its terminal frame; a flush is
		// a no-op rather than an attempt to re-emit buffered or final bytes.
		return
	}
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
		// until finish or a terminal frame decides the final body.
		w.ensureKeepalive()
	}
}

// startStreamWindow anchors the front-window deadline at the first buffered write,
// so a rewrite-enabled stream that never reaches a terminal frame within the
// window degrades to live passthrough instead of being held for the whole turn.
func (w *superInstructBufferingResponseWriter) startStreamWindow() {
	if w.windowStarted {
		return
	}
	w.windowStarted = true
	if w.frontWindowTimeout > 0 {
		w.windowDeadline = time.Now().Add(w.frontWindowTimeout)
	}
}

// superInstructStreamWindowExpired reports whether the held SSE has outgrown the
// complete-packet front window (bytes or wall-clock). The writer then hands the
// stream to raw passthrough; M3 disengages for that stream.
func (w *superInstructBufferingResponseWriter) superInstructStreamWindowExpired(pending int) bool {
	if w.frontWindowBytes > 0 && w.body.Len()+pending >= w.frontWindowBytes {
		return true
	}
	return w.frontWindowTimeout > 0 && !w.windowDeadline.IsZero() && time.Now().After(w.windowDeadline)
}

// releaseStream commits the held packet immediately: M3 runs on the complete body
// and the final response is written without waiting for upstream EOF. Subsequent
// upstream writes are dropped as not-part-of-the-packet.
func (w *superInstructBufferingResponseWriter) releaseStream() {
	if w == nil || w.released || w.passthrough {
		return
	}
	w.released = true
	w.stopKeepalive()
	if w.finalizer != nil {
		w.finalizer(w, w.body.Bytes(), w.status)
		return
	}
	w.writeFinal(w.status, w.body.Bytes(), false, "")
}

func (w *superInstructBufferingResponseWriter) writeToDst(p []byte) (int, error) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	return w.dst.Write(p)
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
					w.emitKeepalive(status, headers, stop)
				}
			}
		}()
	})
}

func (w *superInstructBufferingResponseWriter) emitKeepalive(status int, headers http.Header, stop <-chan struct{}) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	// The stop channel is checked under the same lock as the write, so a comment
	// frame can never be spliced after writeFinal has committed the final body.
	select {
	case <-stop:
		return
	default:
	}
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
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if w.passthrough {
		return
	}
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
		// The held bytes stay for finish()'s bounded Memory/Monitor observation;
		// nothing re-reads them for output after passthrough.
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
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if w.hijacked {
		return
	}
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
