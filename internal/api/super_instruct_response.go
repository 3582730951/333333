package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/superinstruct"
)

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
	meta := superinstruct.RequestMeta{
		UserMessage: superinstruct.ExtractUser(raw),
		Path:        r.URL.RequestURI(),
		Timestamp:   time.Now().UTC(),
	}
	meta.Category = superinstruct.Categorize(meta.UserMessage)
	start := time.Now()
	bw := newSuperInstructBufferingResponseWriter(w)
	r = r.WithContext(withSuperInstructResponsePipelineActive(r.Context()))
	finish := func() {
		s.finishSuperInstructResponsePipeline(bw, r, raw, model, meta, opts, time.Since(start))
	}
	return bw, r, finish, true
}

func (s *Server) finishSuperInstructResponsePipeline(w *superInstructBufferingResponseWriter, r *http.Request, requestRaw []byte, model string, meta superinstruct.RequestMeta, opts superinstruct.ProcessOptions, duration time.Duration) {
	if w == nil || w.hijacked {
		return
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	processor := s.superInstructProcessor()
	result := processor.Process(meta, status, w.body.Bytes(), duration, opts)
	body := result.Body
	contentType := ""
	if result.Tampered {
		body, contentType = s.superInstructTamperWireBody(r, requestRaw, model, string(result.Body), w.header)
	}
	w.writeFinal(status, body, result.Tampered, contentType)
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

func (s *Server) superInstructTamperWireBody(r *http.Request, raw []byte, model, text string, header http.Header) ([]byte, string) {
	path := ""
	if r != nil && r.URL != nil {
		path = r.URL.Path
	}
	stream := isStreamRequest(raw)
	if !stream && strings.Contains(strings.ToLower(header.Get("Content-Type")), "text/event-stream") {
		stream = true
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = strings.TrimSpace(routing.Model(raw))
	}
	if model == "" {
		model = "unknown"
	}
	switch {
	case stream && path == "/v1/messages":
		return superInstructAnthropicSSE(text, model), "text/event-stream"
	case stream && path == "/v1/chat/completions":
		return superInstructChatSSE(text, model), "text/event-stream"
	case stream:
		return superInstructResponsesSSE(text), "text/event-stream"
	case path == "/v1/messages" || path == "/v1/messages/count_tokens" || strings.HasSuffix(path, "/v1/messages"):
		return superInstructAnthropicJSON(text, model), "application/json"
	case path == "/v1/chat/completions":
		return superInstructChatJSON(text, model), "application/json"
	default:
		return superInstructResponsesJSON(text, model), "application/json"
	}
}

type superInstructBufferingResponseWriter struct {
	dst      http.ResponseWriter
	header   http.Header
	status   int
	body     bytes.Buffer
	hijacked bool
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
	_, _ = w.body.Write(p)
	return len(p), nil
}

func (w *superInstructBufferingResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
}

func (w *superInstructBufferingResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.ReadFrom(src)
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

func superInstructResponsesJSON(text, model string) []byte {
	return superInstructMustJSON(map[string]interface{}{
		"id":         "resp_tamper",
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"model":      model,
		"output": []interface{}{map[string]interface{}{
			"id":     "msg_tamper",
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []interface{}{map[string]interface{}{
				"type": "output_text",
				"text": text,
			}},
		}},
		"output_text": text,
		"usage": map[string]int{
			"input_tokens":  0,
			"output_tokens": 0,
			"total_tokens":  0,
		},
	})
}

func superInstructChatJSON(text, model string) []byte {
	return superInstructMustJSON(map[string]interface{}{
		"id":      "chatcmpl_tamper",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []interface{}{map[string]interface{}{
			"index": 0,
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": text,
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]int{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	})
}

func superInstructAnthropicJSON(text, model string) []byte {
	return superInstructMustJSON(map[string]interface{}{
		"id":            "msg_tamper",
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []interface{}{map[string]interface{}{"type": "text", "text": text}},
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
	})
}

func superInstructResponsesSSE(text string) []byte {
	created := map[string]interface{}{
		"type": "response.created",
		"response": map[string]interface{}{
			"id":     "resp_tamper",
			"object": "response",
			"status": "in_progress",
			"output": []interface{}{},
		},
	}
	delta := map[string]interface{}{
		"type":          "response.output_text.delta",
		"item_id":       "msg_tamper",
		"output_index":  0,
		"content_index": 0,
		"delta":         text,
	}
	done := map[string]interface{}{
		"type":          "response.output_text.done",
		"item_id":       "msg_tamper",
		"output_index":  0,
		"content_index": 0,
		"text":          text,
	}
	completed := map[string]interface{}{
		"type": "response.completed",
		"response": map[string]interface{}{
			"id":     "resp_tamper",
			"object": "response",
			"status": "completed",
			"output": []interface{}{map[string]interface{}{
				"id":     "msg_tamper",
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []interface{}{map[string]interface{}{
					"type": "output_text",
					"text": text,
				}},
			}},
			"usage": map[string]int{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}
	var b bytes.Buffer
	writeSSEPayload(&b, "response.created", created)
	writeSSEPayload(&b, "response.output_text.delta", delta)
	writeSSEPayload(&b, "response.output_text.done", done)
	writeSSEPayload(&b, "response.completed", completed)
	return b.Bytes()
}

func superInstructChatSSE(text, model string) []byte {
	now := time.Now().Unix()
	first := map[string]interface{}{
		"id":      "chatcmpl_tamper",
		"object":  "chat.completion.chunk",
		"created": now,
		"model":   model,
		"choices": []interface{}{map[string]interface{}{
			"index":         0,
			"delta":         map[string]string{"role": "assistant", "content": text},
			"finish_reason": nil,
		}},
	}
	last := map[string]interface{}{
		"id":      "chatcmpl_tamper",
		"object":  "chat.completion.chunk",
		"created": now,
		"model":   model,
		"choices": []interface{}{map[string]interface{}{
			"index":         0,
			"delta":         map[string]interface{}{},
			"finish_reason": "stop",
		}},
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "data: %s\n\n", superInstructMustJSON(first))
	fmt.Fprintf(&b, "data: %s\n\n", superInstructMustJSON(last))
	b.WriteString("data: [DONE]\n\n")
	return b.Bytes()
}

func superInstructAnthropicSSE(text, model string) []byte {
	message := map[string]interface{}{
		"id":            "msg_tamper",
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       []interface{}{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
	}
	var b bytes.Buffer
	writeSSEPayload(&b, "message_start", map[string]interface{}{"type": "message_start", "message": message})
	writeSSEPayload(&b, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]string{"type": "text", "text": ""},
	})
	writeSSEPayload(&b, "content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]string{"type": "text_delta", "text": text},
	})
	writeSSEPayload(&b, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
	writeSSEPayload(&b, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": 0},
	})
	writeSSEPayload(&b, "message_stop", map[string]interface{}{"type": "message_stop"})
	return b.Bytes()
}

func writeSSEPayload(b *bytes.Buffer, event string, payload interface{}) {
	if event != "" {
		fmt.Fprintf(b, "event: %s\n", event)
	}
	fmt.Fprintf(b, "data: %s\n\n", superInstructMustJSON(payload))
}

func superInstructMustJSON(v interface{}) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return raw
}
