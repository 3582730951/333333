// Package leakfilter hides pool-internal upstream signals from the downstream
// client. A relay must not expose that it is fronting a pool of accounts: the
// upstream's per-account quota state ("you've hit your usage limit", credit
// balances, reset timers), its model-switch suggestions ("Switch to another
// model now"), and the x-codex-*/openai-model telemetry headers all leak that
// the request was served by a rotating pool rather than the caller's own
// account. This package strips those headers, drops the purely-informational
// rate-limit SSE frames, and neutralizes limit/quota/overload/billing error
// bodies into a generic, account-agnostic error.
//
// It is deliberately conservative: it only acts on responses that carry a
// limit/quota/overload/billing/model-switch signature, so ordinary content and
// genuine client-side errors (400 invalid_request, 404) pass through unchanged.
package leakfilter

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"codex-account-pool/internal/streamrewrite"
)

// IsLeakHeader reports whether a (lower-cased) response header name carries
// pool-internal account/quota state that must never reach the downstream. This
// covers Codex (x-codex-*, x-ratelimit-*, openai-model) and Anthropic
// (anthropic-ratelimit-*, anthropic-organization-*) per-account telemetry.
func IsLeakHeader(lower string) bool {
	switch {
	case strings.HasPrefix(lower, "x-codex-"):
		return true
	case strings.HasPrefix(lower, "x-ratelimit-"):
		return true
	// Anthropic returns the account's live quota state on every /v1/messages
	// response as anthropic-ratelimit-{requests,tokens,unified}-{limit,remaining,reset}
	// (the same headers api/ratelimit.go consumes to drive cooldown). Forwarding
	// them downstream leaks per-account pool state: across a rotating pool the
	// "remaining" values jump around in a way a single real account never would,
	// which is itself a relay signal. Strip the whole family.
	case strings.HasPrefix(lower, "anthropic-ratelimit-"):
		return true
	// The org that owns the account must not leak to the downstream caller, who is
	// not that account's owner. (request-id / anthropic-version are benign and pass.)
	case strings.HasPrefix(lower, "anthropic-organization-"):
		return true
	case lower == "openai-model" || lower == "x-openai-model":
		return true
	case lower == "openai-processing-ms":
		return true
	}
	return false
}

// StripLeakHeaders removes every leak header from h in place.
func StripLeakHeaders(h http.Header) {
	for k := range h {
		if IsLeakHeader(strings.ToLower(k)) {
			h.Del(k)
		}
	}
}

// limitErrorSignatures are case-insensitive substrings that mark a response as
// carrying pool-internal limit/quota/overload/billing/model-switch state. They
// are matched only inside error envelopes / known limit SSE events, never inside
// ordinary content.
var limitErrorSignatures = []string{
	"usage limit",
	"usage_limit_reached",
	"usage_not_included",
	"insufficient_quota",
	"exceeded your current",
	"quota exceeded",
	"rate_limit_exceeded",
	"rate_limit_error",
	"overloaded_error",
	"server_is_overloaded",
	"slow_down",
	"switch to another model",
	"try a different model",
	"out of credits",
	"credits_depleted",
	"spend cap",
	"resets_at",
	"resets_in_seconds",
	"billing_error",
	"plan_type",
	// Phrasings the operator reported leaking ("账号额度不足" / "建议切换模型") and
	// close variants. Matched only inside error envelopes / known limit events.
	"you've reached",
	"you have reached",
	"reached your",
	"plan limit",
	"monthly limit",
	"daily limit",
	"upgrade your plan",
	"out of credit",
	"not enough credit",
	"insufficient credit",
	"please switch",
	"switch models",
	"try again later",
	"too many requests",
}

// codexFailedLeakSignatures mark a Codex "response.failed" SSE frame as carrying
// pool-internal limit / quota / overload / model-switch state — including the
// server-overload codes (server_is_overloaded / slow_down) whose user-facing
// message is "Selected model is at capacity. Please try a different model."
// (verified against other_codex codex-api/src/sse/responses.rs). A response.failed
// frame WITHOUT one of these (e.g. invalid_request_error) is a genuine client
// error and is passed through unchanged.
var codexFailedLeakSignatures = []string{
	"rate_limit_exceeded",
	"insufficient_quota",
	"usage_not_included",
	"usage_limit_reached",
	"usage limit",
	"server_is_overloaded",
	"slow_down",
	"at capacity",
	"try a different model",
	"switch to another model",
	"you've reached",
	"reached your",
	"plan limit",
	"upgrade your plan",
	"out of credit",
	"switch models",
	"please switch",
}

func containsAnyFold(haystackLower string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystackLower, n) {
			return true
		}
	}
	return false
}

// looksLikeLimitError reports whether an error body is a pool-internal
// limit/quota/overload/billing/model-switch error (vs. a genuine client error
// the caller should see, like 400 invalid_request or 404 not_found).
func looksLikeLimitError(status int, body []byte) bool {
	lower := strings.ToLower(string(body))
	if !strings.Contains(lower, "error") && status != 429 {
		return false
	}
	if status == 429 {
		return true
	}
	switch status {
	case 402, 529:
		return true
	}
	return containsAnyFold(lower, limitErrorSignatures)
}

// NeutralizeErrorBody inspects an upstream error response. When it carries
// pool-internal limit/quota/overload/billing state it returns a neutral,
// account-agnostic error body (in the caller's protocol envelope) and the
// status to surface; otherwise it returns the input unchanged with changed=false.
// provider is "claude" for the Anthropic protocol, anything else for OpenAI.
func NeutralizeErrorBody(provider string, status int, body []byte) (int, []byte, bool) {
	if !looksLikeLimitError(status, body) {
		return status, body, false
	}
	outStatus := http.StatusServiceUnavailable
	rateLimited := status == 429
	if rateLimited {
		outStatus = http.StatusTooManyRequests
	}
	msg := "The service is temporarily unavailable. Please retry shortly."
	if rateLimited {
		msg = "Rate limit reached. Please retry shortly."
	}
	var payload map[string]interface{}
	if provider == "claude" {
		etype := "overloaded_error"
		if rateLimited {
			etype = "rate_limit_error"
		}
		payload = map[string]interface{}{
			"type":  "error",
			"error": map[string]interface{}{"type": etype, "message": msg},
		}
	} else {
		etype := "server_error"
		if rateLimited {
			etype = "rate_limit_exceeded"
		}
		payload = map[string]interface{}{
			"error": map[string]interface{}{"message": msg, "type": etype},
		}
	}
	nb, err := json.Marshal(payload)
	if err != nil {
		return status, body, false
	}
	return outStatus, nb, true
}

// NeutralizeResponsesJSON inspects a NON-streaming Codex /v1/responses body that
// came back HTTP 200 but whose top-level status reports a soft failure carrying
// pool-internal limit/quota/model-switch state. It only ever reads the response
// envelope's top-level status / error / incomplete_details fields — NEVER the
// generated output/content — so ordinary assistant text that merely mentions
// "usage limit" is never touched. Returns (neutralBody, true) when it scrubs,
// otherwise (body, false). The neutral body is the generic OpenAI error envelope.
func NeutralizeResponsesJSON(body []byte) ([]byte, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body, false
	}
	status := strings.Trim(strings.ToLower(strings.TrimSpace(string(root["status"]))), `"`)
	_, hasErr := root["error"]
	// Only a failed/incomplete response or one carrying a top-level error object
	// is a candidate; a normal completed response is left byte-for-byte intact.
	if status != "failed" && status != "incomplete" && !hasErr {
		return body, false
	}
	envelope := strings.ToLower(string(root["error"]) + " " + string(root["incomplete_details"]) + " " + status)
	if !containsAnyFold(envelope, limitErrorSignatures) {
		return body, false
	}
	nb, err := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"message": "The service is temporarily unavailable. Please retry shortly.",
			"type":    "server_error",
		},
	})
	if err != nil {
		return body, false
	}
	return nb, true
}

// SSEFilter is a streaming, boundary-safe Server-Sent-Events filter. It drops
// frames that carry only pool-internal limit/quota/model-switch state and runs
// the operator sensitive-word matcher over the frames it keeps. One filter per
// stream (it is stateful); not safe for concurrent use.
type SSEFilter struct {
	provider string
	words    *streamrewrite.Matcher
	buf      []byte
}

// NewSSEFilter builds an SSE filter for the given protocol ("claude" or codex)
// that also applies the (possibly empty) sensitive-word matcher to kept frames.
func NewSSEFilter(provider string, words *streamrewrite.Matcher) *SSEFilter {
	return &SSEFilter{provider: provider, words: words}
}

// CodexFailureFrame is a terminal, account-retryable error carried inside a
// successful HTTP SSE response. The Responses HTTP transport normally emits
// response.failed, while the official Responses-over-WebSocket transport emits
// type:error with the real status and quota headers embedded in the JSON body.
type CodexFailureFrame struct {
	EventType  string
	StatusCode int
	Header     http.Header
	Body       []byte
}

// ParseRetryableCodexFailureFrame recognizes both Codex terminal error envelopes.
// It deliberately requires either an account-retryable HTTP status or a known
// limit/overload signature so a genuine client error (for example an invalid
// request or invalid cache key) is never moved to another account.
func ParseRetryableCodexFailureFrame(frame []byte) (CodexFailureFrame, bool) {
	eventType, data := parseSSEFrame(frame)
	if len(data) == 0 {
		return CodexFailureFrame{}, false
	}
	var envelope struct {
		Type       string                     `json:"type"`
		StatusCode json.RawMessage            `json:"status_code"`
		Headers    map[string]json.RawMessage `json:"headers"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return CodexFailureFrame{}, false
	}
	kind := strings.ToLower(strings.TrimSpace(eventType))
	bodyKind := strings.ToLower(strings.TrimSpace(envelope.Type))
	if kind != "response.failed" && kind != "error" {
		kind = bodyKind
	}
	if kind != "response.failed" && kind != "error" {
		return CodexFailureFrame{}, false
	}

	lower := strings.ToLower(string(data))
	status := jsonInt(envelope.StatusCode)
	statusRetryable := status == http.StatusUnauthorized || status == http.StatusForbidden ||
		status == http.StatusRequestTimeout || status == http.StatusTooManyRequests ||
		status == http.StatusServiceUnavailable || status >= 500
	signatureRetryable := containsAnyFold(lower, codexFailedLeakSignatures)
	if !statusRetryable && !signatureRetryable {
		return CodexFailureFrame{}, false
	}
	if status == 0 {
		if containsAnyFold(lower, []string{"server_is_overloaded", "slow_down", "at capacity", "try a different model"}) {
			status = http.StatusServiceUnavailable
		} else {
			status = http.StatusTooManyRequests
		}
	}

	header := http.Header{}
	for name, raw := range envelope.Headers {
		if value := jsonScalarString(raw); value != "" {
			header.Set(name, value)
		}
	}
	return CodexFailureFrame{
		EventType:  kind,
		StatusCode: status,
		Header:     header,
		Body:       append([]byte(nil), data...),
	}, true
}

func IsRetryableCodexFailureFrame(frame []byte) bool {
	_, ok := ParseRetryableCodexFailureFrame(frame)
	return ok
}

func jsonInt(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		n, _ = strconv.Atoi(strings.TrimSpace(text))
	}
	return n
}

func jsonScalarString(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	var boolean bool
	if err := json.Unmarshal(raw, &boolean); err == nil {
		return strconv.FormatBool(boolean)
	}
	return ""
}

var claudeRetryableErrorSignatures = []string{
	"overloaded_error",
	"rate_limit_error",
	"billing_error",
	"usage limit",
	"quota",
	"switch to another model",
}

func IsRetryableClaudeErrorFrame(frame []byte) bool {
	eventType, data := parseSSEFrame(frame)
	if len(data) == 0 {
		return false
	}
	if eventType != "error" && jsonStringField(data, "type") != "error" {
		return false
	}
	return containsAnyFold(strings.ToLower(string(data)), claudeRetryableErrorSignatures)
}

// sseReadBufPool recycles the 32 KiB read buffers used by SSEFilter.Copy so a
// high-concurrency stream load does not allocate a fresh buffer per response. Each
// buffer is fully overwritten by every Read before its bytes are appended to the
// frame buffer, so pooled reuse is byte-for-byte equivalent.
var sseReadBufPool = sync.Pool{New: func() interface{} { b := make([]byte, 32*1024); return &b }}

// Copy streams src to w, dropping pool-internal frames and scrubbing the rest,
// flushing after every emitted frame so streaming latency is preserved.
func (f *SSEFilter) Copy(w io.Writer, src io.Reader) error {
	flusher, _ := w.(http.Flusher)
	emit := func(p []byte) error {
		if len(p) == 0 {
			return nil
		}
		if _, err := w.Write(p); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	bufp := sseReadBufPool.Get().(*[]byte)
	defer sseReadBufPool.Put(bufp)
	rb := *bufp
	for {
		n, readErr := src.Read(rb)
		if n > 0 {
			f.buf = append(f.buf, rb[:n]...)
			for {
				idx := bytes.Index(f.buf, []byte("\n\n"))
				if idx < 0 {
					break
				}
				frame := f.buf[:idx+2]
				f.buf = f.buf[idx+2:]
				if err := emit(f.processFrame(frame)); err != nil {
					return err
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				if len(f.buf) > 0 {
					tail := f.buf
					f.buf = nil
					return emit(f.processFrame(tail))
				}
				return nil
			}
			return readErr
		}
	}
}

// processFrame returns the bytes to forward for one complete SSE frame.
//
// For the Claude protocol an upstream "error" event that carries pool-internal
// limit/quota/overload/billing/model-switch state is NEUTRALIZED in place —
// rewritten to a generic, account-agnostic Anthropic error event — rather than
// dropped. Dropping it would leave the downstream with an empty or truncated
// HTTP-200 stream, which Claude Code reports as "API returned an empty or malformed
// response (HTTP 200)" / a JSON parse failure; a well-formed error event instead
// lets the client render the error and run its normal retry/backoff. For the Codex
// protocol, purely-informational limit/metadata frames are still dropped (those
// clients tolerate their absence). Kept frames have sensitive words scrubbed.
func (f *SSEFilter) processFrame(frame []byte) []byte {
	if f.provider == "claude" {
		if nb, ok := f.neutralizeClaudeError(frame); ok {
			frame = nb
		}
	} else if f.shouldDropCodex(frame) {
		return nil
	}
	if f.words != nil && !f.words.Empty() {
		return f.words.ReplaceAll(frame)
	}
	return frame
}

// ProcessFrameForRelay applies the same frame policy as Copy to one complete
// SSE frame. It is used by the upstream-rule filter pipeline.
func (f *SSEFilter) ProcessFrameForRelay(frame []byte) []byte { return f.processFrame(frame) }

// neutralizeClaudeError rewrites a Claude SSE "error" frame that carries
// pool-internal limit/quota/overload/billing/model-switch state into a generic
// Anthropic error event, preserving SSE framing so the client still receives a
// well-formed, renderable error. Non-error frames — and genuine client errors
// (e.g. invalid_request_error) that carry no limit signature — are returned
// unchanged (ok=false), so ordinary content is never touched.
func (f *SSEFilter) neutralizeClaudeError(frame []byte) ([]byte, bool) {
	eventType, data := parseSSEFrame(frame)
	if len(data) == 0 {
		return nil, false
	}
	if eventType != "error" && jsonStringField(data, "type") != "error" {
		return nil, false
	}
	lowerData := strings.ToLower(string(data))
	if !containsAnyFold(lowerData, claudeRetryableErrorSignatures) {
		return nil, false
	}
	etype := "overloaded_error"
	msg := "The service is temporarily unavailable. Please retry shortly."
	if strings.Contains(lowerData, "rate_limit") || strings.Contains(lowerData, "too many requests") {
		etype = "rate_limit_error"
		msg = "Rate limit reached. Please retry shortly."
	}
	payload, err := json.Marshal(map[string]interface{}{
		"type":  "error",
		"error": map[string]interface{}{"type": etype, "message": msg},
	})
	if err != nil {
		return nil, false
	}
	name := eventType
	if name == "" {
		name = "error"
	}
	return []byte("event: " + name + "\ndata: " + string(payload) + "\n\n"), true
}

// shouldDropCodex reports whether a complete Codex/OpenAI SSE frame is a
// purely-informational pool-internal leak frame (rate-limit snapshot, a soft
// failure carrying limit state, a model-switch suggestion, or a verification
// recommendation) that should be dropped. Ordinary content is never dropped.
func (f *SSEFilter) shouldDropCodex(frame []byte) bool {
	eventType, data := parseSSEFrame(frame)
	if len(data) == 0 {
		return false
	}
	// This runs on every streamed token, so it must not lower the whole frame.
	// The model-switch suggestion only ever appears inside a limit error, but it
	// can ride in any frame type, so it is still checked unconditionally — via an
	// allocation-free case-insensitive scan rather than strings.ToLower(string(data)).
	if asciiContainsFold(data, "switch to another model") {
		return true
	}
	switch jsonStringField(data, "type") {
	case "codex.rate_limits":
		return true
	case "response.failed", "error":
		return IsRetryableCodexFailureFrame(frame)
	case "response.metadata":
		return asciiContainsFold(data, "openai_verification_recommendation")
	}
	if eventType == "codex.rate_limits" {
		return true
	}
	return false
}

// parseSSEFrame extracts the event type (from an "event:" line) and the joined
// data payload (from one or more "data:" lines) of a single SSE frame.
//
// The hot path — a frame with a single "data:" line — returns a sub-slice of the
// input without any bytes.Split/bytes.Join allocation; the allocating join is used
// only for genuine multi-"data:"-line frames. Lines are scanned with
// bytes.IndexByte, which is byte-for-byte equivalent to iterating bytes.Split on
// '\n' for the "event:"/"data:" lines this reads (trailing/blank lines never match
// a prefix and so never affect the result).
var (
	sseEventPrefix = []byte("event:")
	sseDataPrefix  = []byte("data:")
	sseLineFeed    = []byte("\n")
)

func parseSSEFrame(frame []byte) (eventType string, data []byte) {
	var (
		firstData []byte
		haveData  bool
		dataParts [][]byte // populated only for genuine multi-"data:" frames
	)
	for pos := 0; pos < len(frame); {
		var raw []byte
		if nl := bytes.IndexByte(frame[pos:], '\n'); nl >= 0 {
			raw = frame[pos : pos+nl]
			pos += nl + 1
		} else {
			raw = frame[pos:]
			pos = len(frame)
		}
		line := bytes.TrimRight(raw, "\r")
		switch {
		case bytes.HasPrefix(line, sseEventPrefix):
			eventType = strings.TrimSpace(string(line[len(sseEventPrefix):]))
		case bytes.HasPrefix(line, sseDataPrefix):
			part := bytes.TrimSpace(line[len(sseDataPrefix):])
			switch {
			case !haveData:
				firstData = part
				haveData = true
			case dataParts == nil:
				dataParts = [][]byte{firstData, part}
			default:
				dataParts = append(dataParts, part)
			}
		}
	}
	if dataParts != nil {
		return eventType, bytes.Join(dataParts, sseLineFeed)
	}
	return eventType, firstData
}

// asciiContainsFold reports whether b contains needle case-insensitively over
// ASCII letters, without allocating a lowered copy of b. needle must be
// lower-case. For all-ASCII input (which is all the SSE JSON these guards run
// against) this is byte-for-byte equivalent to
// strings.Contains(strings.ToLower(string(b)), needle).
func asciiContainsFold(b []byte, needle string) bool {
	n := len(needle)
	if n == 0 {
		return true
	}
	for i := 0; i+n <= len(b); i++ {
		match := true
		for j := 0; j < n; j++ {
			c := b[i+j]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// jsonStringField reads a top-level-ish JSON string field value without a full
// unmarshal (cheap, allocation-light, tolerant of streaming fragments).
func jsonStringField(body []byte, key string) string {
	needle := []byte(`"` + key + `"`)
	idx := bytes.Index(body, needle)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(needle):]
	colon := bytes.IndexByte(rest, ':')
	if colon < 0 {
		return ""
	}
	rest = bytes.TrimLeft(rest[colon+1:], " \t\r\n")
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	for end := 1; end < len(rest); end++ {
		switch rest[end] {
		case '\\':
			end++
		case '"':
			var out string
			if json.Unmarshal(rest[:end+1], &out) == nil {
				return out
			}
			return ""
		}
	}
	return ""
}
