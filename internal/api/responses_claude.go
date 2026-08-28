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

// responsesClaudeTolerableLosses are conversion losses that re-encode content rather
// than drop it. responsesHistoryItemAsChatMessage and responsesToolOutputChatContent
// wrap the original item in a JSON envelope that stays in the prompt, so the model
// still receives every turn — the representation changes, the context does not.
//
// LossResponsesIncludeOmitted is tolerable for a different reason: `include:
// ["reasoning.encrypted_content"]` asks the upstream to echo replayable encrypted
// reasoning, which is a Codex storage optimization with no Anthropic equivalent.
// Anthropic returns reasoning natively as thinking blocks. Codex CLI sends this
// field on every request, so rejecting it would make this route permanently dead.
//
// Deliberately absent: LossResponsesHostedToolOmitted and
// LossResponsesServerToolSearchOmitted. Those silently remove a tool the client
// declared, which is a real capability loss and must fail loudly instead.
var responsesClaudeTolerableLosses = map[string]bool{
	prompt.LossResponsesIncludeOmitted:           true,
	prompt.LossResponsesHistoryItemJSON:          true,
	prompt.LossResponsesStructuredToolOutputJSON: true,
}

func intolerableResponsesClaudeLosses(losses []string) []string {
	out := make([]string, 0, len(losses))
	for _, loss := range losses {
		if !responsesClaudeTolerableLosses[loss] {
			out = append(out, loss)
		}
	}
	return out
}

// handleResponsesViaClaude serves a Codex-protocol /v1/responses request from a
// built-in Claude account. Codex CLI speaks only Responses, so without this a Claude
// model requested through that entrypoint fell into Codex-specific routing and failed
// with a no-route 503 before any upstream call.
//
// It converts the request to Chat Completions and delegates to handleChatViaClaude,
// which already owns scheduling, failover, cloaking, cache policy, and usage. The
// response is converted back to the Responses envelope by a bridging writer, mirroring
// how messages_codex.go bridges the opposite direction.
func (s *Server) handleResponsesViaClaude(w http.ResponseWriter, r *http.Request, raw []byte, model string, pol downstreamPolicy) {
	bridge, err := prompt.ResponsesRequestToChatCompletionBridge(raw)
	if err != nil {
		if s.writePromptCompatibilityError(w, err,
			"official_codex_or_claude_function_tools",
			"built_in_claude_responses_bridge",
			"Use client-side function tools for the Claude bridge, or route this request to an official Codex account.") {
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if blocking := intolerableResponsesClaudeLosses(bridge.CompatibilityLosses); len(blocking) > 0 {
		writePoolCodeError(w, http.StatusUnprocessableEntity, "unsupported_protocol_field",
			"Responses fields cannot be converted to Anthropic Messages without losing a declared capability: "+strings.Join(blocking, ", "))
		return
	}
	setResponsesCompatibilityHeader(w, bridge.CompatibilityLosses)

	inner := r.Clone(r.Context())
	urlCopy := *r.URL
	urlCopy.Path = "/v1/chat/completions"
	urlCopy.RawPath = ""
	inner.URL = &urlCopy
	inner.RequestURI = ""
	inner.Body = io.NopCloser(bytes.NewReader(bridge.Body))
	inner.ContentLength = int64(len(bridge.Body))
	chatSource := bodysource.Bytes(bridge.Body)
	defer chatSource.Close()
	inner = inner.WithContext(contextWithBodyMeta(contextWithBodySource(inner.Context(), chatSource), bodysource.BodyMeta{}))
	inner.GetBody = chatSource.Open
	inner.Header.Set("Content-Type", "application/json")
	inner.Header.Del("Content-Length")

	writer, err := newResponsesClaudeResponseWriter(r.Context(), w, isStreamRequest(raw), model, bridge.Plan, s.responseBodyCaptureOptions(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.handleChatViaClaude(writer, inner, bridge.Body, model, pol)
	writer.finish()
}

type responsesClaudeResponseMode int

const (
	responsesClaudeBuffered responsesClaudeResponseMode = iota
	responsesClaudeSSE
)

// responsesClaudeResponseWriter converts the Chat Completions response produced by
// handleChatViaClaude into the Responses envelope Codex CLI requires. A streaming
// request is converted event-by-event so first-token latency is preserved.
type responsesClaudeResponseWriter struct {
	ctx             context.Context
	downstream      http.ResponseWriter
	header          http.Header
	status          int
	streamRequested bool
	model           string
	plan            *prompt.ResponsesToolBridgePlan
	mode            responsesClaudeResponseMode
	buffer          *bodysource.SpoolBuffer
	options         bodysource.CaptureOptions
	pipeWriter      *io.PipeWriter
	pipeDone        chan struct{}
}

func newResponsesClaudeResponseWriter(ctx context.Context, downstream http.ResponseWriter, stream bool, model string, plan *prompt.ResponsesToolBridgePlan, options bodysource.CaptureOptions) (*responsesClaudeResponseWriter, error) {
	buffer, err := bodysource.NewSpoolBuffer(ctx, options)
	if err != nil {
		return nil, err
	}
	return &responsesClaudeResponseWriter{
		ctx:             ctx,
		downstream:      downstream,
		header:          make(http.Header),
		streamRequested: stream,
		model:           model,
		plan:            plan,
		mode:            responsesClaudeBuffered,
		buffer:          buffer,
		options:         options,
	}, nil
}

func (w *responsesClaudeResponseWriter) Header() http.Header { return w.header }

func (w *responsesClaudeResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	contentType := strings.ToLower(w.header.Get("Content-Type"))
	if w.streamRequested && status >= 200 && status < 300 && strings.Contains(contentType, "text/event-stream") {
		w.mode = responsesClaudeSSE
		copyBridgeHeaders(w.downstream.Header(), w.header)
		w.downstream.Header().Set("Content-Type", "text/event-stream")
		w.downstream.Header().Set("Cache-Control", "no-cache")
		w.downstream.Header().Del("Content-Length")
		w.downstream.WriteHeader(status)
		flushWriter(w.downstream)

		reader, writer := io.Pipe()
		w.pipeWriter = writer
		w.pipeDone = make(chan struct{})
		go func() {
			defer supervisor.Recover("responses-claude-sse")
			defer close(w.pipeDone)
			defer reader.Close()
			chatStreamToResponsesSSEWithOptions(w.ctx, w.downstream, reader, w.model, streamrewrite.New(nil), w.options, w.plan)
		}()
	}
}

func (w *responsesClaudeResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.mode == responsesClaudeSSE && w.pipeWriter != nil {
		return w.pipeWriter.Write(p)
	}
	return w.buffer.Write(p)
}

func (w *responsesClaudeResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	// The Responses converter flushes each complete event it emits; io.Pipe writes are
	// synchronous and wake the reader immediately, so nothing extra is needed here.
}

func (w *responsesClaudeResponseWriter) finish() {
	defer w.buffer.Close()
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.mode == responsesClaudeSSE {
		if w.pipeWriter != nil {
			_ = w.pipeWriter.Close()
		}
		if w.pipeDone != nil {
			<-w.pipeDone
		}
		copyBridgeTrailers(w.downstream, w.header)
		return
	}

	copyBridgeHeaders(w.downstream.Header(), w.header)
	w.downstream.Header().Del("Content-Length")
	chatBody, err := responseSpoolBytes(w.buffer)
	if err != nil {
		writeError(w.downstream, http.StatusBadGateway, err)
		return
	}
	// A Codex-protocol client expects OpenAI-shaped errors, which is exactly what the
	// pool's own error writers produce. Pass a failure through unconverted.
	if w.status < 200 || w.status >= 300 {
		w.downstream.WriteHeader(w.status)
		_, _ = w.downstream.Write(chatBody)
		return
	}
	out, err := prompt.ChatCompletionToResponsesResponse(chatBody, w.model, w.plan)
	if err != nil {
		writeError(w.downstream, http.StatusBadGateway, err)
		return
	}
	w.downstream.Header().Set("Content-Type", "application/json")
	w.downstream.WriteHeader(w.status)
	_, _ = w.downstream.Write(out)
}
