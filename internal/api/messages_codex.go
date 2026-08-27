package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/supervisor"
)

// isCodexMessagesModel identifies model ids that Claude Code may send through the
// Anthropic Messages endpoint but that belong to the pool's built-in Codex account
// channel. Custom-provider membership is checked before this helper, so a custom
// provider remains authoritative even if it intentionally exposes a gpt-* id.
func isCodexMessagesModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "gpt" || strings.HasPrefix(model, "gpt-") ||
		strings.HasPrefix(model, "codex-") || strings.HasPrefix(model, "o1") ||
		strings.HasPrefix(model, "o3") || strings.HasPrefix(model, "o4")
}

// handleMessagesViaCodex bridges Claude Code directly to the built-in Codex Responses
// channel without duplicating the account scheduler, failover, isolation, usage, or
// upstream transport machinery. Avoiding a Chat Completions intermediate preserves
// encrypted reasoning, typed content, and tool-call identity across agent turns.
func (s *Server) handleMessagesViaCodex(w http.ResponseWriter, r *http.Request, raw []byte, model string) {
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		// This route is served by a GPT-family model, so o200k is the right tokenizer
		// and an exact count is available locally. The rune/4 estimate it replaces
		// undercounts CJK several-fold while overcounting tool schemas.
		writeJSON(w, http.StatusOK, map[string]interface{}{"input_tokens": countCodexInputTokens(raw)})
		return
	}

	converted, err := prompt.AnthropicRequestToResponses(raw)
	if err != nil {
		if s.writePromptCompatibilityError(w, err,
			"official_claude_or_codex_function_tools",
			"built_in_codex_responses_bridge",
			"Use client-side function tools for the Codex bridge; Anthropic-only hosted tools require an official Claude route.") {
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}

	// The 1M marker describes Anthropic model capability and is invalid once the
	// final target is Codex/Responses. Other client beta markers remain part of the
	// Claude Code session contract and must survive the protocol bridge.
	inner := withoutAnthropicContext1MBeta(r)
	urlCopy := *r.URL
	urlCopy.Path = "/v1/responses"
	urlCopy.RawPath = ""
	urlCopy.RawQuery = ""
	inner.URL = &urlCopy
	inner.RequestURI = ""
	inner.Body = io.NopCloser(bytes.NewReader(converted.Body))
	inner.ContentLength = int64(len(converted.Body))
	convertedSource := bodysource.Bytes(converted.Body)
	defer convertedSource.Close()
	inner = inner.WithContext(contextWithBodyMeta(contextWithBodySource(inner.Context(), convertedSource), bodysource.BodyMeta{}))
	inner.GetBody = convertedSource.Open
	inner.Header.Set("Content-Type", "application/json")
	inner.Header.Del("Content-Length")

	bridge, err := newCodexMessagesResponseWriter(r.Context(), w, isStreamRequest(raw), model, converted.ToolNames, converted.InheritModelTools, s.responseBodyCaptureOptions(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.handleGatewayPost(bridge, inner)
	bridge.finish()
}

type codexMessagesResponseMode int

const (
	codexMessagesResponseBuffered codexMessagesResponseMode = iota
	codexMessagesResponseSSE
)

// codexMessagesResponseWriter lets the Codex Responses handler stream into the
// Anthropic SSE converter in real time. A non-streaming Claude request still uses the
// streaming Codex backend and is aggregated into one Messages JSON response.
type codexMessagesResponseWriter struct {
	ctx               context.Context
	downstream        http.ResponseWriter
	header            http.Header
	status            int
	streamRequested   bool
	model             string
	toolNames         map[string]string
	inheritModelTools map[string]bool
	mode              codexMessagesResponseMode
	buffer            *bodysource.SpoolBuffer
	options           bodysource.CaptureOptions
	pipeWriter        *io.PipeWriter
	pipeDone          chan struct{}
}

func newCodexMessagesResponseWriter(ctx context.Context, downstream http.ResponseWriter, stream bool, model string, toolNames map[string]string, inheritModelTools map[string]bool, options bodysource.CaptureOptions) (*codexMessagesResponseWriter, error) {
	buffer, err := bodysource.NewSpoolBuffer(ctx, options)
	if err != nil {
		return nil, err
	}
	return &codexMessagesResponseWriter{
		ctx:               ctx,
		downstream:        downstream,
		header:            make(http.Header),
		streamRequested:   stream,
		model:             model,
		toolNames:         toolNames,
		inheritModelTools: inheritModelTools,
		mode:              codexMessagesResponseBuffered,
		buffer:            buffer,
		options:           options,
	}, nil
}

func (w *codexMessagesResponseWriter) Header() http.Header { return w.header }

func (w *codexMessagesResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	contentType := strings.ToLower(w.header.Get("Content-Type"))
	if w.streamRequested && status >= 200 && status < 300 && strings.Contains(contentType, "text/event-stream") {
		w.mode = codexMessagesResponseSSE
		copyBridgeHeaders(w.downstream.Header(), w.header)
		w.downstream.Header().Set("Content-Type", "text/event-stream")
		w.downstream.Header().Set("Cache-Control", "no-cache")
		w.downstream.Header().Del("Content-Length")
		w.downstream.WriteHeader(status)

		reader, writer := io.Pipe()
		w.pipeWriter = writer
		w.pipeDone = make(chan struct{})
		go func() {
			defer supervisor.Recover("messages-codex-sse")
			defer close(w.pipeDone)
			defer reader.Close()
			responsesStreamToAnthropicSSEWithOptions(w.ctx, w.downstream, reader, w.model, w.toolNames, w.inheritModelTools, streamrewrite.New(nil), w.options)
		}()
	}
}

func (w *codexMessagesResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.mode == codexMessagesResponseSSE && w.pipeWriter != nil {
		return w.pipeWriter.Write(p)
	}
	return w.buffer.Write(p)
}

func (w *codexMessagesResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	// The Anthropic converter flushes each complete event. Flushing the pipe itself is
	// unnecessary; io.Pipe writes are synchronous and immediately wake its reader.
}

func (w *codexMessagesResponseWriter) finish() {
	defer w.buffer.Close()
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.mode == codexMessagesResponseSSE {
		if w.pipeWriter != nil {
			_ = w.pipeWriter.Close()
		}
		if w.pipeDone != nil {
			<-w.pipeDone
		}
		// copyBridgeHeaders intentionally skips TrailerPrefix values so they are
		// never exposed as ordinary headers before the stream begins. Forward the
		// final values now that the Responses → Messages stream has completed.
		copyBridgeTrailers(w.downstream, w.header)
		return
	}

	copyBridgeHeaders(w.downstream.Header(), w.header)
	w.downstream.Header().Del("Content-Length")
	responsesBody, err := responseSpoolBytes(w.buffer)
	if err != nil {
		writeError(w.downstream, http.StatusBadGateway, err)
		return
	}
	if w.status < 200 || w.status >= 300 {
		w.downstream.WriteHeader(w.status)
		_, _ = w.downstream.Write(responsesBody)
		return
	}
	if strings.Contains(strings.ToLower(w.header.Get("Content-Type")), "text/event-stream") {
		responsesBody = codexSSEToResponseJSON(responsesBody)
		if len(responsesBody) == 0 {
			writeError(w.downstream, http.StatusBadGateway, io.ErrUnexpectedEOF)
			return
		}
	}
	out, err := prompt.ResponsesToAnthropicResponse(responsesBody, w.model, w.toolNames, w.inheritModelTools)
	if err != nil {
		writeError(w.downstream, http.StatusBadGateway, err)
		return
	}
	if w.streamRequested {
		w.downstream.Header().Set("Content-Type", "text/event-stream")
		w.downstream.Header().Set("Cache-Control", "no-cache")
		w.downstream.WriteHeader(w.status)
		_ = anthropicMessageJSONToSSE(w.downstream, out)
		return
	}
	w.downstream.Header().Set("Content-Type", "application/json")
	w.downstream.WriteHeader(w.status)
	_, _ = w.downstream.Write(out)
}

func copyBridgeHeaders(dst, src http.Header) {
	for key, values := range src {
		// TrailerPrefix values are internal ResponseWriter trailer storage, not
		// ordinary HTTP headers. They are forwarded after a streaming bridge closes
		// by copyBridgeTrailers so the final values are not sent as bogus headers.
		if strings.EqualFold(key, "Content-Length") || strings.HasPrefix(key, http.TrailerPrefix) {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
