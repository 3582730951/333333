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
	// A streamed response is already committed as soon as its first SSE event is
	// delivered, so a whole-response rewrite cannot be applied without destroying
	// first-byte latency and native event timing. Keep the wire path transparent;
	// Memory/Monitor, when enabled, observe a tee after bytes have been forwarded.
	// Rewrite-only streamed profiles therefore add zero response-path overhead.
	if stream && !opts.MemoryEnabled && !opts.MonitorEnabled {
		return w, r, nil, false
	}
	meta := superinstruct.RequestMeta{
		UserMessage: superinstruct.ExtractUser(raw),
		Path:        r.URL.RequestURI(),
		Timestamp:   time.Now().UTC(),
	}
	meta.Category = superinstruct.Categorize(meta.UserMessage)
	start := time.Now()
	processor := s.superInstructProcessor()
	startSuperInstructObservationWorker()
	if stream || !opts.ResponseRewriteEnabled {
		ow := newSuperInstructObservingResponseWriter(w)
		r = r.WithContext(withSuperInstructResponsePipelineActive(r.Context()))
		finish := func() {
			opts.ResponseRewriteEnabled = false
			finishSuperInstructObservation(processor, ow.status, ow.body.Bytes(), ow.hijacked, meta, opts, time.Since(start))
		}
		return ow, r, finish, true
	}
	bw := newSuperInstructBufferingResponseWriter(w)
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
	body := original
	tampered := false
	if _, valid := superInstructRewriteSingleAssistantText(r, original, ""); valid {
		rewriteOnly := superinstruct.ProcessOptions{ResponseRewriteEnabled: true}
		result := s.superInstructProcessor().Process(meta, status, original, duration, rewriteOnly)
		if result.Tampered {
			body, tampered = superInstructRewriteSingleAssistantText(r, original, string(result.Body))
		}
	} else {
		opts.ResponseRewriteEnabled = false
	}
	w.writeFinal(status, body, tampered, "application/json")
	finishSuperInstructObservation(s.superInstructProcessor(), status, original, false, meta, opts, duration)
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

// superInstructObservingResponseWriter is a transparent tee. Unlike the legacy
// rewrite writer below, it forwards headers, chunks, and Flush calls immediately
// and records a copy solely for post-response Memory/Monitor processing.
type superInstructObservingResponseWriter struct {
	dst      http.ResponseWriter
	status   int
	body     bytes.Buffer
	hijacked bool
}

func newSuperInstructObservingResponseWriter(dst http.ResponseWriter) *superInstructObservingResponseWriter {
	return &superInstructObservingResponseWriter{dst: dst}
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
		captureSuperInstructObservation(&w.body, p[:n])
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
	dst         http.ResponseWriter
	header      http.Header
	status      int
	body        bytes.Buffer
	hijacked    bool
	passthrough bool
}

func newSuperInstructBufferingResponseWriter(dst http.ResponseWriter) *superInstructBufferingResponseWriter {
	return &superInstructBufferingResponseWriter{dst: dst, header: make(http.Header)}
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
	if strings.Contains(strings.ToLower(w.header.Get("Content-Type")), "text/event-stream") {
		w.startPassthrough()
		n, err := w.dst.Write(p)
		if n > 0 {
			captureSuperInstructObservation(&w.body, p[:n])
		}
		return n, err
	}
	_, _ = w.body.Write(p)
	return len(p), nil
}

func (w *superInstructBufferingResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if strings.Contains(strings.ToLower(w.header.Get("Content-Type")), "text/event-stream") {
		w.startPassthrough()
		if f, ok := w.dst.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func (w *superInstructBufferingResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{w}, src)
}

func (w *superInstructBufferingResponseWriter) startPassthrough() {
	if w == nil || w.passthrough {
		return
	}
	w.passthrough = true
	dstHeader := w.dst.Header()
	for key, values := range w.header {
		dstHeader.Del(key)
		for _, value := range values {
			dstHeader.Add(key, value)
		}
	}
	w.dst.WriteHeader(w.status)
}

func captureSuperInstructObservation(dst *bytes.Buffer, p []byte) {
	if dst == nil || len(p) == 0 || dst.Len() >= superInstructObservationLimit {
		return
	}
	remaining := superInstructObservationLimit - dst.Len()
	if len(p) > remaining {
		p = p[:remaining]
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
