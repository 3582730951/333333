package api

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/responsefilter"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/streamrewrite"
	"codex-account-pool/internal/supervisor"
	upstreamrules "codex-account-pool/internal/upstream_error_rules"
	"codex-account-pool/internal/usage"
)

type responseRuleFilterKey struct{}
type responseRuleFilter struct {
	Keywords      []string
	CaseSensitive bool
	Mode          string
	Rule          *storage.UpstreamErrorRule
}

type ruleFilteringWriter struct {
	http.ResponseWriter
	filter   *responseRuleFilter
	provider string
	buf      []byte
}

func (w *ruleFilteringWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.Index(w.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := append([]byte(nil), w.buf[:idx+2]...)
		w.buf = w.buf[idx+2:]
		out := filterRuleSSEFrame(frame, w.filter)
		if len(out) > 0 {
			if _, err := w.ResponseWriter.Write(out); err != nil {
				return 0, err
			}
		}
	}
	return len(p), nil
}

func filterRuleSSEFrame(frame []byte, filter *responseRuleFilter) []byte {
	if filter == nil {
		return frame
	}
	if filter.Mode == upstreamrules.DownstreamActionHideSafetyBuffering {
		out, _ := responsefilter.StripSafetyBufferingSSE(frame)
		return out
	}
	return responsefilter.FilterSSEFrame(frame, filter.Keywords, filter.CaseSensitive)
}

func filterRuleJSON(body []byte, filter *responseRuleFilter) []byte {
	if filter == nil {
		return body
	}
	if filter.Mode == upstreamrules.DownstreamActionHideSafetyBuffering {
		out, _ := responsefilter.StripSafetyBufferingJSON(body)
		return out
	}
	out, _ := responsefilter.FilterJSON(body, filter.Keywords, filter.CaseSensitive)
	return out
}

func (w *ruleFilteringWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func newRuleFilteringWriter(dst http.ResponseWriter, filter *responseRuleFilter, provider string) http.ResponseWriter {
	if filter == nil {
		return dst
	}
	return &ruleFilteringWriter{ResponseWriter: dst, filter: filter, provider: provider}
}

func withResponseRuleFilter(ctx context.Context, f *responseRuleFilter) context.Context {
	return context.WithValue(ctx, responseRuleFilterKey{}, f)
}

// writeUpstreamHeaders copies upstream response headers to the downstream client
// using the same hop-by-hop / relay-internal denylist as copyDownstreamHeaders,
// and additionally drops pool-internal account/quota leak headers (x-codex-*,
// openai-model, x-ratelimit-*, anthropic-ratelimit-*, anthropic-organization-*)
// when leak-scrub is enabled. This is the single place every gateway response
// path filters headers.
func (s *Server) writeUpstreamHeaders(ctx context.Context, dst, src http.Header) {
	stripLeak := s.leakScrubEnabled(ctx)
	for k, values := range src {
		lower := strings.ToLower(k)
		if lower == "authorization" ||
			lower == "chatgpt-account-id" ||
			lower == "x-openai-fedramp" ||
			strings.HasPrefix(lower, "x-pool-") ||
			strings.HasPrefix(lower, "x-sidecar-") ||
			lower == "content-length" ||
			// The body we forward is always plaintext: the direct path requests
			// Accept-Encoding: identity, and the sidecar auto-decompresses (and
			// already drops these). Never propagate an encoding/length that would
			// describe the upstream's wire bytes rather than what we actually send.
			lower == "content-encoding" ||
			lower == "transfer-encoding" ||
			lower == "connection" {
			continue
		}
		if stripLeak && leakfilter.IsLeakHeader(lower) {
			continue
		}
		for _, value := range values {
			dst.Add(k, value)
		}
	}
}

// scannerResponseWriter wraps an http.ResponseWriter so that every byte written
// to the downstream client is simultaneously fed into a usage.StreamScanner. This
// eliminates the io.TeeReader on the read side — and its per-byte copy into the
// scanner's Write buffer — by instead feeding the scanner from the write side where
// the bytes already exist. The StreamScanner only cares about complete SSE frames
// (it parses line-by-line), so feeding it post-scrub/rewrite bytes is equivalent:
// sensitive-word scrubbing replaces in-band text (e.g. a pseudo-username embedded
// in a tool result), never the JSON usage fields; leak-filter drops rate-limit
// frames entirely — those frames carry no usage data by definition.
type scannerResponseWriter struct {
	http.ResponseWriter
	scanner *usage.StreamScanner
}

func (sw *scannerResponseWriter) Write(p []byte) (int, error) {
	sw.scanner.Write(p)
	return sw.ResponseWriter.Write(p)
}

// Flush preserves the streaming capability of the wrapped writer. Without this,
// leakfilter.SSEFilter sees only an io.Writer and buffers small SSE frames in the
// net/http server until a later write or EOF, defeating token-by-token delivery even
// though the underlying responseRecorder supports http.Flusher.
func (sw *scannerResponseWriter) Flush() {
	if flusher, ok := sw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// streamSSE forwards an upstream SSE stream to the client. When leak-scrub is on
// it routes through the SSE frame filter (drops pool-internal rate-limit frames,
// scrubs sensitive words); otherwise it uses the plain boundary-safe scrubbing
// copy. provider is "claude" for the Anthropic protocol, "codex" otherwise.
//
// It also feeds the stream through a usage.StreamScanner and records the token usage
// once the stream finishes. Streaming is the common case for real CLI traffic, so
// without this the usage tables (and the whole admin overview built on them) would
// only ever reflect the rare non-streaming response. Usage is recorded even when the
// copy ends in an error, so a client that disconnects mid-stream after the upstream
// already reported its counts is still billed/metered for what was produced.
func (s *Server) streamSSE(ctx context.Context, w http.ResponseWriter, body io.Reader, words *streamrewrite.Matcher, provider, accountID, routeHash string) error {
	scanner := usage.NewStreamScanner(provider)
	// Wrap the client writer so every byte forwarded to the downstream also feeds
	// the scanner — no TeeReader copy on the read side.
	sw := &scannerResponseWriter{ResponseWriter: w, scanner: scanner}
	var err error
	var rf *responseRuleFilter
	if v, ok := ctx.Value(responseRuleFilterKey{}).(*responseRuleFilter); ok {
		rf = v
	}
	if rf != nil {
		err = newRuleSSECopy(ctx, sw, body, rf, s.leakScrubEnabled(ctx), words, provider)
	} else if interval := s.streamKeepAliveInterval(ctx); interval > 0 {
		// General downstream keepalive: route the no-rule stream through the
		// frame-aligned heartbeat pump so a long upstream silence is bridged with a
		// provider protocol keepalive frame, preventing an intermediary or client from
		// closing a long streaming task before response.completed. With rf=nil,
		// filterRuleSSEFrame is a passthrough, so emitFrame reproduces BOTH legacy
		// branches exactly: leak-on runs the same per-frame processFrame as
		// leakfilter.Copy, and leak-off applies the same per-frame word scrub. Output
		// parity with streamCopyRewrite / leakfilter.Copy is covered by
		// TestStreamKeepAliveOutputParity.
		err = newRuleSSECopyWithHeartbeat(ctx, sw, body, nil, s.leakScrubEnabled(ctx), words, provider, interval)
	} else if !s.leakScrubEnabled(ctx) {
		err = streamCopyRewrite(sw, body, words)
	} else {
		err = leakfilter.NewSSEFilter(provider, words).Copy(sw, body)
	}
	if parsed, ok := scanner.Parsed(); ok {
		log.Printf("[STREAM-USAGE] provider=%s, account=%s, model=%s, prompt=%d, completion=%d, total=%d, cached=%d",
			provider, accountID, parsed.Model, parsed.PromptTokens, parsed.CompletionTokens, parsed.TotalTokens, parsed.CachedTokens)
		s.recordParsedUsage(ctx, accountID, routeHash, parsed)
	} else {
		log.Printf("[STREAM-USAGE] provider=%s, account=%s: NO USAGE EXTRACTED", provider, accountID)
	}
	return err
}

func newRuleSSECopy(ctx context.Context, w http.ResponseWriter, body io.Reader, rf *responseRuleFilter, leak bool, words *streamrewrite.Matcher, provider string) error {
	return newRuleSSECopyWithHeartbeat(ctx, w, body, rf, leak, words, provider, responseRuleHeartbeatInterval(rf))
}

// streamKeepAliveMaxSeconds caps the general downstream keepalive interval. Like the
// safety-buffering heartbeat, it stays well below common intermediary idle timeouts
// (nginx/cloudflare ~60-100s) and Codex's ~5-minute upstream stream idle timeout so a
// keepalive frame always lands before anything downstream decides the stream is dead.
const streamKeepAliveMaxSeconds = 60

// streamKeepAliveInterval resolves the general downstream SSE keepalive interval for
// the no-rule relay path. A non-positive value (config default 0, or an operator
// override of 0) disables it, leaving the byte-for-byte streamCopyRewrite /
// leakfilter.Copy fast paths untouched. A positive value is clamped into (0, 60s].
func (s *Server) streamKeepAliveInterval(ctx context.Context) time.Duration {
	seconds := s.settingInt(ctx, "stream_keepalive_seconds", s.cfg.StreamKeepAliveSeconds)
	if seconds <= 0 {
		return 0
	}
	if seconds > streamKeepAliveMaxSeconds {
		seconds = streamKeepAliveMaxSeconds
	}
	return time.Duration(seconds) * time.Second
}

const safetyBufferingHeartbeatFrame = "event: response.in_progress\ndata: " + responseInProgressPayload + "\n\n"

// claudePingHeartbeatFrame is the Anthropic-protocol keepalive. A real Claude
// stream emits `ping` events during long thinking, so this is the frame a Claude
// downstream expects — emitting the Codex `response.in_progress` frame to a Claude
// client (as the single hardcoded frame previously did) is a protocol mismatch.
const claudePingHeartbeatFrame = "event: ping\ndata: {\"type\":\"ping\"}\n\n"

// heartbeatFrameFor returns the provider-appropriate keepalive frame so a long
// upstream silence is bridged with a frame the DOWNSTREAM protocol understands
// (Codex response.in_progress vs Anthropic ping), keeping the connection alive
// without injecting content or affecting the eventual response.completed.
func heartbeatFrameFor(provider string) string {
	if provider == "claude" {
		return claudePingHeartbeatFrame
	}
	return safetyBufferingHeartbeatFrame
}

func responseRuleHeartbeatInterval(rf *responseRuleFilter) time.Duration {
	if rf == nil || rf.Mode != upstreamrules.DownstreamActionHideSafetyBuffering {
		return 0
	}
	seconds := int64(15)
	if rf.Rule != nil && rf.Rule.IdlePingSeconds > 0 {
		seconds = rf.Rule.IdlePingSeconds
	}
	// Stay well below Codex's default five-minute stream idle timeout even if an
	// older rule contains an unexpectedly large heartbeat value.
	if seconds > 60 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

func newRuleSSECopyWithHeartbeat(ctx context.Context, w http.ResponseWriter, body io.Reader, rf *responseRuleFilter, leak bool, words *streamrewrite.Matcher, provider string, heartbeatInterval time.Duration) error {
	// Apply the selected rule to each complete SSE event. In particular, never
	// discard a group of pending events together: Codex requires the terminal
	// response.completed event even when an earlier event is intercepted.
	buf := make([]byte, 0, 32768)
	flusher, _ := w.(http.Flusher)
	emit := func(p []byte) error {
		if len(p) == 0 {
			return nil
		}
		if _, e := w.Write(p); e != nil {
			return e
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	emitFrame := func(frame []byte) error {
		out := filterRuleSSEFrame(frame, rf)
		if len(out) == 0 {
			return nil
		}
		if leak {
			out = leakfilter.NewSSEFilter(provider, words).ProcessFrameForRelay(out)
		} else if words != nil && !words.Empty() {
			out = words.ReplaceAll(out)
		}
		return emit(out)
	}

	type readResult struct {
		data []byte
		err  error
	}
	reads := make(chan readResult, 1)
	readDone := make(chan struct{})
	defer close(readDone)
	go func() {
		defer supervisor.Recover("upstream-rule-sse-reader")
		tmp := make([]byte, 32768)
		defer close(reads)
		for {
			n, err := body.Read(tmp)
			result := readResult{err: err}
			if n > 0 {
				result.data = append([]byte(nil), tmp[:n]...)
			}
			select {
			case reads <- result:
			case <-readDone:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var heartbeat <-chan time.Time
	var heartbeatTicker *time.Ticker
	if heartbeatInterval > 0 {
		heartbeatTicker = time.NewTicker(heartbeatInterval)
		heartbeat = heartbeatTicker.C
		defer heartbeatTicker.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result, ok := <-reads:
			if !ok {
				if len(buf) > 0 {
					return emitFrame(buf)
				}
				return nil
			}
			if len(result.data) > 0 {
				buf = append(buf, result.data...)
			}
			for {
				idx := bytes.Index(buf, []byte("\n\n"))
				if idx < 0 {
					break
				}
				frame := append([]byte(nil), buf[:idx+2]...)
				buf = buf[idx+2:]
				if e := emitFrame(frame); e != nil {
					return e
				}
			}
			if result.err != nil {
				if result.err == io.EOF {
					if len(buf) > 0 {
						return emitFrame(buf)
					}
					return nil
				}
				return result.err
			}
		case <-heartbeat:
			if err := emitFrame([]byte(heartbeatFrameFor(provider))); err != nil {
				return err
			}
		}
	}
}

const (
	earlySSEMaxBytes  = 64 * 1024
	earlySSEMaxFrames = 8
)

func probeEarlyCodexSSEFailure(body io.Reader) ([]byte, leakfilter.CodexFailureFrame, bool, error) {
	var failure leakfilter.CodexFailureFrame
	prefix, terminal, err := probeEarlySSEFailure(body, func(frame []byte) bool {
		parsed, ok := leakfilter.ParseCodexFailureFrame(frame)
		if ok {
			failure = parsed
		}
		return ok
	}, codexSSEFrameCommitsContent, true)
	return prefix, failure, terminal, err
}

func probeEarlyClaudeSSEFailure(body io.Reader) ([]byte, bool, error) {
	return probeEarlySSEFailure(body, leakfilter.IsRetryableClaudeErrorFrame, claudeSSEFrameCommitsContent, false)
}

func probeEarlySSEFailure(body io.Reader, retryableFrame func([]byte) bool, contentFrame func([]byte) bool, scanBufferedAfterContent bool) ([]byte, bool, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	processed := 0
	frames := 0
	for len(buf) < earlySSEMaxBytes && frames < earlySSEMaxFrames {
		committedInBatch := false
		for {
			idx := bytes.Index(buf[processed:], []byte("\n\n"))
			if idx < 0 {
				break
			}
			end := processed + idx + 2
			frame := buf[processed:end]
			frames++
			if retryableFrame(frame) {
				return buf, true, nil
			}
			if contentFrame(frame) {
				if !scanBufferedAfterContent {
					return buf, false, nil
				}
				// Keep scanning complete frames that arrived in this same read. A
				// retryable response.failed commonly follows the first delta in the
				// same network batch; returning immediately would leak that partial
				// answer and prevent safe account failover. We still never perform a
				// subsequent blocking read after content, so TTFT remains bounded by
				// the bytes already available from upstream.
				committedInBatch = true
			}
			processed = end
			if frames >= earlySSEMaxFrames {
				return buf, false, nil
			}
		}
		if committedInBatch {
			return buf, false, nil
		}
		n, err := body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err == io.EOF {
				if n > 0 {
					continue
				}
				return buf, false, nil
			}
			return buf, false, err
		}
	}
	return buf, false, nil
}

func codexSSEFrameCommitsContent(frame []byte) bool {
	lower := strings.ToLower(string(frame))
	for _, marker := range []string{
		"response.output_text.delta",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.audio.delta",
		// Release the early retry probe before a potentially long safety check.
		// The downstream stream filter can then emit protocol heartbeats while the
		// upstream is otherwise silent. A real response.failed in the same read is
		// still detected before the prefix is committed.
		"safety_buffering",
		"response.in_progress",
		`"delta"`,
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func claudeSSEFrameCommitsContent(frame []byte) bool {
	lower := strings.ToLower(string(frame))
	for _, marker := range []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"text_delta",
		"input_json_delta",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// writeFilteredError writes an upstream error response to the downstream client,
// neutralizing pool-internal limit/quota/overload/billing bodies into a generic
// account-agnostic error when leak-scrub is on, and always filtering headers.
// provider is "claude" for the Anthropic protocol, "codex" otherwise.
func (s *Server) writeFilteredError(ctx context.Context, w http.ResponseWriter, provider string, status int, header http.Header, body []byte, words *streamrewrite.Matcher) {
	out := body
	if s.leakScrubEnabled(ctx) {
		if ns, nb, changed := leakfilter.NeutralizeErrorBody(provider, status, body); changed {
			status = ns
			out = nb
		}
	}
	if words != nil && !words.Empty() {
		out = words.ReplaceAll(out)
	}
	s.writeUpstreamHeaders(ctx, w.Header(), header)
	w.WriteHeader(status)
	_, _ = w.Write(out)
}
