package api

import (
	"context"
	"io"
	"net/http"
	"strings"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/supervisor"
)

// kiroResponsesBridgeWriter adapts the existing Kiro → Chat Completions renderer
// to the Responses API. Keeping the Kiro execution path unchanged means the
// account-specific auth, EventStream decoding, cache accounting, health handling,
// and capability observations stay identical for Claude and GPT Kiro traffic.
//
// Non-streaming Kiro output is one Chat Completions JSON object and is converted
// after the request completes. Streaming output is piped through the established
// Chat-Completions → Responses SSE converter without buffering the whole reply.
type kiroResponsesBridgeWriter struct {
	ctx             context.Context
	downstream      http.ResponseWriter
	header          http.Header
	status          int
	streamRequested bool
	model           string
	plan            *prompt.ResponsesToolBridgePlan
	bridgeLosses    []string
	streaming       bool
	buffer          *bodysource.SpoolBuffer
	options         bodysource.CaptureOptions
	pipeWriter      *io.PipeWriter
	pipeDone        chan struct{}
}

func newKiroResponsesBridgeWriter(ctx context.Context, downstream http.ResponseWriter, stream bool, model string, plan *prompt.ResponsesToolBridgePlan, losses []string, options bodysource.CaptureOptions) (*kiroResponsesBridgeWriter, error) {
	buffer, err := bodysource.NewSpoolBuffer(ctx, options)
	if err != nil {
		return nil, err
	}
	return &kiroResponsesBridgeWriter{
		ctx:             ctx,
		downstream:      downstream,
		header:          make(http.Header),
		streamRequested: stream,
		model:           model,
		plan:            plan,
		bridgeLosses:    append([]string(nil), losses...),
		buffer:          buffer,
		options:         options,
	}, nil
}

func (w *kiroResponsesBridgeWriter) Header() http.Header { return w.header }

func (w *kiroResponsesBridgeWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	contentType := strings.ToLower(w.header.Get("Content-Type"))
	if !w.streamRequested || status < http.StatusOK || status >= http.StatusMultipleChoices || !strings.Contains(contentType, "text/event-stream") {
		return
	}

	// Compatibility losses may grow after Kiro finishes conversion. Declare the
	// trailer before writing the first Response event and publish the known bridge
	// losses as the initial header.
	declareResponsesCompatibilityTrailer(w)
	setResponsesCompatibilityHeader(w, w.compatibilityLosses())
	copyBridgeHeaders(w.downstream.Header(), w.header)
	w.downstream.Header().Set("Content-Type", "text/event-stream")
	w.downstream.Header().Set("Cache-Control", "no-cache")
	w.downstream.Header().Del("Content-Length")
	w.downstream.WriteHeader(status)

	reader, writer := io.Pipe()
	w.pipeWriter = writer
	w.pipeDone = make(chan struct{})
	w.streaming = true
	go func() {
		defer supervisor.Recover("kiro-responses-bridge-stream")
		defer close(w.pipeDone)
		defer reader.Close()
		chatStreamToResponsesSSEWithOptions(w.ctx, w.downstream, reader, w.model, streamrewrite.New(nil), w.options, w.plan)
	}()
}

func (w *kiroResponsesBridgeWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.streaming && w.pipeWriter != nil {
		return w.pipeWriter.Write(p)
	}
	return w.buffer.Write(p)
}

func (w *kiroResponsesBridgeWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	// io.Pipe writes synchronously wake the SSE transformer, which flushes each
	// completed Responses event to the real downstream writer.
}

func (w *kiroResponsesBridgeWriter) finish() {
	defer w.buffer.Close()
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.streaming {
		if w.pipeWriter != nil {
			_ = w.pipeWriter.Close()
		}
		if w.pipeDone != nil {
			<-w.pipeDone
		}
		setResponsesCompatibilityTrailer(w, w.compatibilityLosses())
		copyBridgeTrailers(w.downstream, w.header)
		return
	}

	copyBridgeHeaders(w.downstream.Header(), w.header)
	raw, err := responseSpoolBytes(w.buffer)
	if err != nil {
		writeError(w.downstream, http.StatusBadGateway, err)
		return
	}
	if w.status < http.StatusOK || w.status >= http.StatusMultipleChoices {
		w.downstream.WriteHeader(w.status)
		_, _ = w.downstream.Write(raw)
		return
	}
	out, err := prompt.ChatCompletionToResponsesResponse(raw, w.model, w.plan)
	if err != nil {
		writeError(w.downstream, http.StatusBadGateway, err)
		return
	}
	setResponsesCompatibilityHeader(w.downstream, w.compatibilityLosses())
	w.downstream.Header().Set("Content-Type", "application/json")
	w.downstream.Header().Del("Content-Length")
	w.downstream.WriteHeader(w.status)
	_, _ = w.downstream.Write(out)
}

func responseSpoolBytes(buffer *bodysource.SpoolBuffer) ([]byte, error) {
	if view, ok := bodysource.ByteView(buffer); ok {
		return view, nil
	}
	return bodysource.ReadAll(buffer)
}

func (w *kiroResponsesBridgeWriter) compatibilityLosses() []string {
	return mergeCompatibilityLosses(w.bridgeLosses, splitKiroCompatibilityHeader(w.header.Get("X-Pool-Kiro-Compatibility")))
}

func splitKiroCompatibilityHeader(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "none") {
		return nil
	}
	return strings.Split(value, ",")
}

// copyBridgeTrailers forwards final trailer values written to an intermediate
// response writer after its downstream headers have already been committed.
func copyBridgeTrailers(dst http.ResponseWriter, src http.Header) {
	for key, values := range src {
		if !strings.HasPrefix(key, http.TrailerPrefix) {
			continue
		}
		trailerKey := strings.TrimPrefix(key, http.TrailerPrefix)
		if trailerKey == "" {
			continue
		}
		dst.Header().Del(http.TrailerPrefix + trailerKey)
		for _, value := range values {
			dst.Header().Add(http.TrailerPrefix+trailerKey, value)
		}
	}
}
