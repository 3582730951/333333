package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
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
type streamStallRecoveryKey struct{}

var errUpstreamStreamStalled = errors.New("upstream stream stalled without a terminal event")
var errUpstreamStreamReadPanic = errors.New("upstream stream reader failed")

const publicRetryMessage = "Please retry."

func withStreamStallRecovery(ctx context.Context, timeout time.Duration) context.Context {
	if timeout <= 0 {
		return ctx
	}
	return context.WithValue(ctx, streamStallRecoveryKey{}, timeout)
}

func streamStallRecoveryFromContext(ctx context.Context) time.Duration {
	timeout, _ := ctx.Value(streamStallRecoveryKey{}).(time.Duration)
	return timeout
}

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

type activityReadResult struct {
	data []byte
	err  error
}

// upstreamActivityReadCloser bounds each blocked upstream read independently. New
// bytes reset the stall window naturally by starting the next Read, while heartbeat
// writes keep the downstream connection alive and never count as upstream activity.
// The underlying body is closed by the caller after a timeout, which releases the
// single in-flight read goroutine.
type upstreamActivityReadCloser struct {
	io.ReadCloser
	ctx            context.Context
	stallTimeout   time.Duration
	heartbeatEvery time.Duration
	heartbeat      func() error
	pendingErr     error
}

// sseRelayReadCloser turns the frame-aware relay into an io.ReadCloser for the
// continuation stitchers. Closing it cancels the pump and, critically, closes the
// upstream body so the one blocked Read goroutine cannot survive a detected stall.
type sseRelayReadCloser struct {
	reader   *io.PipeReader
	upstream io.Closer
	cancel   context.CancelFunc
	once     sync.Once
}

func (r *sseRelayReadCloser) Read(p []byte) (int, error) { return r.reader.Read(p) }

func (r *sseRelayReadCloser) Close() error {
	var closeErr error
	r.once.Do(func() {
		r.cancel()
		if r.upstream != nil {
			closeErr = r.upstream.Close()
		}
		_ = r.reader.Close()
	})
	return closeErr
}

type pipeResponseWriter struct {
	io.Writer
	header http.Header
}

func (w *pipeResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (*pipeResponseWriter) WriteHeader(int) {}
func (*pipeResponseWriter) Flush()          {}

// newSemanticSSERelayReadCloser preserves frame boundaries while applying the
// response rule and the semantic stall detector. Unlike the generic per-Read
// wrapper, control traffic such as response.in_progress and Claude ping cannot
// keep a stuck continuation alive forever.
func newSemanticSSERelayReadCloser(ctx context.Context, body io.ReadCloser, filter *responseRuleFilter, provider string, stallTimeout, heartbeatEvery time.Duration) io.ReadCloser {
	if body == nil {
		return nil
	}
	relayCtx, cancel := context.WithCancel(withStreamStallRecovery(ctx, stallTimeout))
	reader, writer := io.Pipe()
	relay := &sseRelayReadCloser{reader: reader, upstream: body, cancel: cancel}
	go func() {
		var relayErr error
		defer func() {
			if panicValue := recover(); panicValue != nil {
				supervisor.LogPanic("semantic-sse-relay", panicValue)
				relayErr = errUpstreamStreamReadPanic
			}
			_ = writer.CloseWithError(relayErr)
		}()
		relayErr = newRuleSSECopyWithHeartbeat(relayCtx, &pipeResponseWriter{Writer: writer}, body, filter, false, nil, provider, heartbeatEvery)
	}()
	return relay
}

func newUpstreamActivityReadCloser(ctx context.Context, body io.ReadCloser, stallTimeout, heartbeatEvery time.Duration, heartbeat func() error) io.ReadCloser {
	if body == nil || (stallTimeout <= 0 && (heartbeatEvery <= 0 || heartbeat == nil)) {
		return body
	}
	return &upstreamActivityReadCloser{
		ReadCloser: body, ctx: ctx, stallTimeout: stallTimeout,
		heartbeatEvery: heartbeatEvery, heartbeat: heartbeat,
	}
}

func (r *upstreamActivityReadCloser) Read(p []byte) (int, error) {
	if r.pendingErr != nil {
		err := r.pendingErr
		r.pendingErr = nil
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	result := make(chan activityReadResult, 1)
	buf := make([]byte, len(p))
	go func() {
		defer func() {
			if panicValue := recover(); panicValue != nil {
				supervisor.LogPanic("upstream-activity-reader", panicValue)
				select {
				case result <- activityReadResult{err: errUpstreamStreamReadPanic}:
				case <-r.ctx.Done():
				}
			}
		}()
		n, err := r.ReadCloser.Read(buf)
		result <- activityReadResult{data: buf[:n], err: err}
	}()

	var stall <-chan time.Time
	var stallTimer *time.Timer
	if r.stallTimeout > 0 {
		stallTimer = time.NewTimer(r.stallTimeout)
		stall = stallTimer.C
		defer stallTimer.Stop()
	}
	var heartbeat <-chan time.Time
	var heartbeatTicker *time.Ticker
	if r.heartbeatEvery > 0 && r.heartbeat != nil {
		heartbeatTicker = time.NewTicker(r.heartbeatEvery)
		heartbeat = heartbeatTicker.C
		defer heartbeatTicker.Stop()
	}

	for {
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-stall:
			return 0, errUpstreamStreamStalled
		case <-heartbeat:
			if err := r.heartbeat(); err != nil {
				return 0, err
			}
		case got := <-result:
			copy(p, got.data)
			if len(got.data) > 0 && got.err != nil {
				r.pendingErr = got.err
				return len(got.data), nil
			}
			return len(got.data), got.err
		}
	}
}

func (w *ruleFilteringWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		boundary, separatorLen := sseFrameBoundary(w.buf)
		if boundary < 0 {
			break
		}
		frameEnd := boundary + separatorLen
		frame := append([]byte(nil), w.buf[:frameEnd]...)
		w.buf = w.buf[frameEnd:]
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
	strictCodex := codexStrictCPAFromContext(ctx)
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
		// CPA-v2 identity values are proxy-internal and must not be reflected to a
		// downstream client.  The opaque upstream turn state is the one exception:
		// the client needs it verbatim to resume, and the mapper stores only its
		// HMAC alias rather than exposing any internal session identity.
		if strictCodex {
			switch lower {
			case "session-id", "thread-id", "x-client-request-id", "x-codex-window-id", "x-codex-parent-thread-id", "x-codex-forked-from-thread-id", "x-codex-turn-metadata", "x-codex-installation-id":
				continue
			}
		}
		if stripLeak && leakfilter.IsLeakHeader(lower) && !(strictCodex && lower == "x-codex-turn-state") {
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
// scrubs sensitive words). With leak-scrub off, Codex still uses a frame-aligned
// copy so internal Responses context errors can never expose call IDs; other
// providers use the plain boundary-safe scrubbing copy. provider is "claude" for
// the Anthropic protocol, "codex" otherwise.
//
// It also feeds the stream through a usage.StreamScanner and records the token usage
// once the stream finishes. Streaming is the common case for real CLI traffic, so
// without this the usage tables (and the whole admin overview built on them) would
// only ever reflect the rare non-streaming response. Usage is recorded even when the
// copy ends in an error, so a client that disconnects mid-stream after the upstream
// already reported its counts is still billed/metered for what was produced.
func (s *Server) streamSSE(ctx context.Context, w http.ResponseWriter, body io.Reader, words *streamrewrite.Matcher, provider, accountID, routeHash string) error {
	ctx = withStreamStallRecovery(ctx, s.streamStallRecoveryInterval(ctx))
	scanner := usage.NewStreamScanner(provider)
	// Wrap the client writer so every byte forwarded to the downstream also feeds
	// the scanner — no TeeReader copy on the read side.
	sw := &scannerResponseWriter{ResponseWriter: w, scanner: scanner}
	var err error
	var rf *responseRuleFilter
	if v, ok := ctx.Value(responseRuleFilterKey{}).(*responseRuleFilter); ok {
		rf = v
	}
	// Strict CPA leaves native upstream session state and client tool payloads
	// untouched.  It does not silently bypass an administrator's explicitly
	// configured downstream response rule: those rules operate only after the
	// upstream has produced its frame.  The heartbeat reader still ensures a long
	// upstream silence produces a protocol-valid keepalive rather than being
	// mistaken for EOF and locally continued.
	if provider == "codex" && codexStrictCPAFromContext(ctx) {
		interval := s.streamKeepAliveInterval(ctx)
		if ruleInterval := responseRuleHeartbeatInterval(rf); ruleInterval > 0 {
			// hide_safety_buffering carries its own per-rule heartbeat contract.
			// Prefer it over the generic relay interval just as the non-CPA path
			// does, so an administrator sees the same behavior on either path.
			interval = ruleInterval
		}
		err = newRuleSSECopyWithHeartbeat(ctx, sw, body, rf, s.leakScrubEnabled(ctx), words, provider, interval)
	} else if rf != nil {
		err = newRuleSSECopy(ctx, sw, body, rf, s.leakScrubEnabled(ctx), words, provider)
	} else if interval := s.streamKeepAliveInterval(ctx); interval > 0 || streamStallRecoveryFromContext(ctx) > 0 {
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
		if provider == "codex" {
			// Context-loss frames carry account-local call IDs and nested proxy
			// envelopes. Keep this frame-level safety rewrite enabled independently of
			// the optional pool leak scrubber, including after content has committed.
			err = newRuleSSECopyWithHeartbeat(ctx, sw, body, nil, false, words, provider, 0)
		} else {
			err = streamCopyRewrite(sw, body, words)
		}
	} else {
		err = leakfilter.NewSSEFilter(provider, words).Copy(sw, body)
	}
	if parsed, ok := scanner.Parsed(); ok {
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

func (s *Server) streamStallRecoveryInterval(ctx context.Context) time.Duration {
	// An explicit in-process override is used by internal transports and focused
	// fault-injection tests. It is never populated from downstream headers or JSON.
	if timeout := streamStallRecoveryFromContext(ctx); timeout > 0 {
		return timeout
	}
	if !s.goalContinuityEnabled(ctx) && !s.autoContinueEnabled(ctx, nil) {
		return 0
	}
	seconds := s.settingInt(ctx, "stream_stall_recovery_seconds", s.cfg.StreamStallRecoverySeconds)
	if seconds <= 0 {
		return 0
	}
	if seconds < 60 {
		seconds = 60
	}
	if seconds > 3600 {
		seconds = 3600
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

// sseFrameAdvancesModel reports semantic upstream progress rather than raw socket
// activity. Control-plane frames can arrive forever while a generation is stuck;
// treating those bytes as progress prevents stall recovery and leaves the client
// seeing only relay heartbeats. Unknown valid events are progress by default so a
// newly introduced output/tool event cannot be timed out accidentally.
func sseFrameAdvancesModel(frame []byte, provider string) bool {
	eventType, data := sseFrameEventData(frame)
	if len(data) == 0 || strings.TrimSpace(string(data)) == "[DONE]" {
		return false
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return false
	}
	if typ := strings.TrimSpace(envelope.Type); typ != "" {
		eventType = typ
	}
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "", "ping":
		return false
	case "response.in_progress", "response.queued", "response.metadata", "codex.rate_limits":
		return !strings.EqualFold(strings.TrimSpace(provider), "codex")
	default:
		return true
	}
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
	emitFrame := func(frame []byte) (bool, error) {
		out := filterRuleSSEFrame(frame, rf)
		if len(out) == 0 {
			return false, nil
		}
		if leak {
			out = leakfilter.NewSSEFilter(provider, words).ProcessFrameForRelay(out)
		} else {
			if provider == "codex" {
				out, _ = leakfilter.NeutralizeResponsesContextErrorSSEFrame(out)
			}
			if words != nil && !words.Empty() {
				out = words.ReplaceAll(out)
			}
		}
		if len(out) == 0 {
			return false, nil
		}
		if err := emit(out); err != nil {
			return false, err
		}
		return sseFrameAdvancesModel(out, provider), nil
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

	var stall <-chan time.Time
	var stallTimer *time.Timer
	if timeout := streamStallRecoveryFromContext(ctx); timeout > 0 {
		stallTimer = time.NewTimer(timeout)
		stall = stallTimer.C
		defer stallTimer.Stop()
	}
	resetStall := func() {
		if stallTimer == nil {
			return
		}
		if !stallTimer.Stop() {
			select {
			case <-stallTimer.C:
			default:
			}
		}
		stallTimer.Reset(streamStallRecoveryFromContext(ctx))
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result, ok := <-reads:
			if !ok {
				if len(buf) > 0 {
					_, err := emitFrame(buf)
					return err
				}
				return nil
			}
			if len(result.data) > 0 {
				buf = append(buf, result.data...)
			}
			for {
				boundary, separatorLen := sseFrameBoundary(buf)
				if boundary < 0 {
					break
				}
				frameEnd := boundary + separatorLen
				frame := append([]byte(nil), buf[:frameEnd]...)
				buf = buf[frameEnd:]
				advanced, e := emitFrame(frame)
				if e != nil {
					return e
				}
				if advanced {
					resetStall()
				}
			}
			if result.err != nil {
				if result.err == io.EOF {
					if len(buf) > 0 {
						_, err := emitFrame(buf)
						return err
					}
					return nil
				}
				return result.err
			}
		case <-heartbeat:
			if _, err := emitFrame([]byte(heartbeatFrameFor(provider))); err != nil {
				return err
			}
		case <-stall:
			return errUpstreamStreamStalled
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

// earlySSECreatedIdleRelease is the small grace window after a standalone
// response.created frame. It preserves the useful early-error probe for a
// coalesced created+failed response while letting a genuine long-poll stream
// reach the heartbeat relay before its first keepalive is due.
const earlySSECreatedIdleRelease = 25 * time.Millisecond

var errEarlySSEProbeIdle = errors.New("early SSE probe idle after response.created")

type earlySSEReadResult struct {
	data []byte
	err  error
}

// earlySSEReadAhead owns the one outstanding upstream read while the initial
// SSE probe waits briefly for an error immediately following response.created.
// If that wait expires, the same reader is handed to the normal relay; it then
// consumes the pending read rather than issuing a concurrent read on resp.Body.
type earlySSEReadAhead struct {
	source io.Reader
	result chan earlySSEReadResult

	reading    bool
	pending    []byte
	pendingErr error
}

func newEarlySSEReadAhead(source io.Reader) *earlySSEReadAhead {
	return &earlySSEReadAhead{
		source: source,
		result: make(chan earlySSEReadResult, 1),
	}
}

func (r *earlySSEReadAhead) startRead() {
	if r.reading {
		return
	}
	r.reading = true
	go func() {
		defer func() {
			if panicValue := recover(); panicValue != nil {
				supervisor.LogPanic("early-sse-probe-reader", panicValue)
				r.result <- earlySSEReadResult{err: io.ErrUnexpectedEOF}
			}
		}()
		tmp := make([]byte, 4096)
		n, err := r.source.Read(tmp)
		result := earlySSEReadResult{err: err}
		if n > 0 {
			result.data = append([]byte(nil), tmp[:n]...)
		}
		r.result <- result
	}()
}

func (r *earlySSEReadAhead) accept(result earlySSEReadResult) {
	r.reading = false
	r.pending = result.data
	r.pendingErr = result.err
}

func (r *earlySSEReadAhead) consume(p []byte) (int, error, bool) {
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil, true
	}
	if r.pendingErr != nil {
		err := r.pendingErr
		r.pendingErr = nil
		return 0, err, true
	}
	return 0, nil, false
}

func (r *earlySSEReadAhead) read(p []byte, idle time.Duration) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if n, err, ok := r.consume(p); ok {
			return n, err
		}
		r.startRead()
		if idle <= 0 {
			r.accept(<-r.result)
			continue
		}
		timer := time.NewTimer(idle)
		select {
		case result := <-r.result:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			r.accept(result)
		case <-timer.C:
			return 0, errEarlySSEProbeIdle
		}
	}
}

func (r *earlySSEReadAhead) Read(p []byte) (int, error) {
	return r.read(p, 0)
}

func (r *earlySSEReadAhead) readWithIdle(p []byte, idle time.Duration) (int, error) {
	return r.read(p, idle)
}

// probeEarlyCodexSSEFailureWithIdleRelease mirrors probeEarlyCodexSSEFailure,
// but releases a live stream after a tiny post-created grace window. The returned
// reader must be used for the remaining relay because it owns an in-flight read.
func probeEarlyCodexSSEFailureWithIdleRelease(body io.Reader, idle time.Duration) ([]byte, io.Reader, leakfilter.CodexFailureFrame, bool, error) {
	reader := newEarlySSEReadAhead(body)
	var failure leakfilter.CodexFailureFrame
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	processed := 0
	frames := 0
	sawCreated := false
	for len(buf) < earlySSEMaxBytes && frames < earlySSEMaxFrames {
		committedInBatch := false
		for {
			boundary, separatorLen := sseFrameBoundary(buf[processed:])
			if boundary < 0 {
				break
			}
			end := processed + boundary + separatorLen
			frame := buf[processed:end]
			frames++
			if parsed, ok := leakfilter.ParseCodexFailureFrame(frame); ok {
				failure = parsed
				return buf, reader, failure, true, nil
			}
			if codexSSEFrameCommitsContent(frame) {
				committedInBatch = true
			}
			if bytes.Contains(bytes.ToLower(frame), []byte("response.created")) {
				sawCreated = true
			}
			processed = end
			if frames >= earlySSEMaxFrames {
				return buf, reader, failure, false, nil
			}
		}
		if committedInBatch {
			return buf, reader, failure, false, nil
		}
		readIdle := time.Duration(0)
		if sawCreated {
			readIdle = idle
		}
		n, err := reader.readWithIdle(tmp, readIdle)
		if errors.Is(err, errEarlySSEProbeIdle) {
			return buf, reader, failure, false, nil
		}
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err == io.EOF {
				if n > 0 {
					continue
				}
				return buf, reader, failure, false, nil
			}
			return buf, reader, failure, false, err
		}
	}
	return buf, reader, failure, false, nil
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
			boundary, separatorLen := sseFrameBoundary(buf[processed:])
			if boundary < 0 {
				break
			}
			end := processed + boundary + separatorLen
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
		// A response.created frame is only lifecycle metadata, not committed output.
		// Keep it in the bounded early-error probe so an immediately-following
		// response.failed can still be handled before anything is sent downstream.
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
	filteredHeader := header
	if !strings.EqualFold(provider, "claude") {
		if ns, nb, changed := leakfilter.NeutralizeResponsesContextErrorBody(status, body); changed {
			status = ns
			out = nb
			if header == nil {
				filteredHeader = http.Header{}
			} else {
				filteredHeader = header.Clone()
			}
			filteredHeader.Set("Content-Type", "application/json")
		}
	}
	if s.leakScrubEnabled(ctx) {
		if ns, nb, changed := leakfilter.NeutralizeErrorBody(provider, status, out); changed {
			status = ns
			out = nb
		}
	}
	if words != nil && !words.Empty() {
		out = words.ReplaceAll(out)
	}
	s.writeUpstreamHeaders(ctx, w.Header(), filteredHeader)
	w.WriteHeader(status)
	_, _ = w.Write(out)
}
