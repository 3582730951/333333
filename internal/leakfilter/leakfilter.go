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
	"regexp"
	"strconv"
	"strings"
	"sync"

	"codex-account-pool/internal/responsefilter"
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
	"overloaded_error",
	"service_unavailable_error",
	"server_is_overloaded",
	"slow_down",
	"currently overloaded",
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
	const msg = responsesPublicRetryMessage
	var payload map[string]interface{}
	if provider == "claude" {
		payload = map[string]interface{}{
			"type":  "error",
			"error": map[string]interface{}{"type": "api_error", "message": msg},
		}
	} else {
		payload = map[string]interface{}{
			"error": map[string]interface{}{"message": msg, "type": "server_error"},
		}
	}
	nb, err := json.Marshal(payload)
	if err != nil {
		return status, body, false
	}
	return http.StatusServiceUnavailable, nb, true
}

const responsesPublicRetryMessage = "Please retry."

func neutralResponsesFailureSSEFrame() []byte {
	payload, err := json.Marshal(map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"status": "failed",
			"error": map[string]interface{}{
				"code":    "server_error",
				"message": responsesPublicRetryMessage,
			},
		},
	})
	if err != nil {
		return nil
	}
	return []byte("event: response.failed\ndata: " + string(payload) + "\n\n")
}

// NeutralizeResponsesContextErrorBody replaces an internal Responses context-loss
// error with an account-agnostic server error. Unlike quota leak scrubbing, this is a
// protocol-safety invariant: call IDs and serialized upstream wrappers must never be
// exposed even when leak scrubbing is disabled or the one recovery attempt fails.
func NeutralizeResponsesContextErrorBody(status int, body []byte) (int, []byte, bool) {
	if DetectResponsesContextError(status, body) == ResponsesContextErrorNone {
		return status, body, false
	}
	payload := map[string]interface{}{
		"error": map[string]interface{}{
			"message": responsesPublicRetryMessage,
			"type":    "server_error",
		},
	}
	nb, err := json.Marshal(payload)
	if err != nil {
		return status, body, false
	}
	return http.StatusServiceUnavailable, nb, true
}

// NeutralizeResponsesContextErrorSSEFrame is the streaming counterpart of
// NeutralizeResponsesContextErrorBody. It preserves a valid terminal Responses event
// after downstream content has committed, where transparent retry is no longer safe.
func NeutralizeResponsesContextErrorSSEFrame(frame []byte) ([]byte, bool) {
	failure, ok := ParseCodexFailureFrame(frame)
	if !ok || failure.ContextError == ResponsesContextErrorNone {
		return frame, false
	}
	neutral := neutralResponsesFailureSSEFrame()
	if len(neutral) == 0 {
		return frame, false
	}
	return neutral, true
}

// NeutralizeCodexRetryableFailureSSEFrame removes pool-local quota, overload,
// authentication, and risk-control details while preserving a terminal event that
// the Responses clients recognize. Early failures are retried before reaching this
// function; this path is for failures received after downstream output committed.
func NeutralizeCodexRetryableFailureSSEFrame(frame []byte) ([]byte, bool) {
	if _, ok := ParseRetryableCodexFailureFrame(frame); !ok {
		return frame, false
	}
	neutral := neutralResponsesFailureSSEFrame()
	if len(neutral) == 0 {
		return frame, false
	}
	return neutral, true
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
			"message": responsesPublicRetryMessage,
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
	provider            string
	words               *streamrewrite.Matcher
	buf                 []byte
	codexSawLifecycle   bool
	codexSemanticOutput bool
}

// NewSSEFilter builds an SSE filter for the given protocol ("claude" or codex)
// that also applies the (possibly empty) sensitive-word matcher to kept frames.
func NewSSEFilter(provider string, words *streamrewrite.Matcher) *SSEFilter {
	return &SSEFilter{provider: provider, words: words}
}

// CodexFailureFrame is a terminal error carried inside a successful HTTP SSE
// response that can be handled before downstream content is committed. The
// Responses HTTP transport normally emits response.failed, while WebSocket
// transports may emit error or response.error with the real status and quota
// headers embedded in the JSON body.
type CodexFailureFrame struct {
	EventType string
	// ErrorCode is the structured upstream error code. It is intentionally kept
	// separate from Body so callers can classify a terminal SSE response without
	// logging or exporting the upstream message.
	ErrorCode        string
	StatusCode       int
	ContextError     ResponsesContextErrorKind
	RequestError     ResponsesRequestErrorKind
	BuiltinRetryable bool
	Header           http.Header
	Body             []byte
}

// ResponsesRequestErrorKind identifies request-scoped failures that must remain
// visible to the Codex client but must not be treated as account-local context loss,
// quota pressure, or an account health failure.
type ResponsesRequestErrorKind string

const (
	ResponsesRequestErrorNone                  ResponsesRequestErrorKind = ""
	ResponsesRequestErrorContextLengthExceeded ResponsesRequestErrorKind = "context_length_exceeded"
)

// ResponsesContextErrorKind identifies the precise upstream 400s that mean a
// Responses request lost account-local context. This includes a transport rejecting
// previous_response_id after a WebSocket-to-HTTPS fallback: the pointer is no longer
// usable on that transport even though the response itself may still exist on the
// retired connection. These errors are recoverable by rebuilding or degrading the
// request and must not affect account health.
type ResponsesContextErrorKind string

const (
	ResponsesContextErrorNone                     ResponsesContextErrorKind = ""
	ResponsesContextErrorOrphanedToolOutput       ResponsesContextErrorKind = "orphaned_tool_output"
	ResponsesContextErrorEncryptedFunctionOutput  ResponsesContextErrorKind = "encrypted_function_output"
	ResponsesContextErrorPreviousResponseNotFound ResponsesContextErrorKind = "previous_response_not_found"
)

// ParseCodexFailureFrame recognizes both Codex terminal error envelopes, including
// ordinary client 400s. Callers can inspect BuiltinRetryable; operator rule matching
// needs to see every early terminal frame even when the built-in policy would pass it
// downstream unchanged.
func ParseCodexFailureFrame(frame []byte) (CodexFailureFrame, bool) {
	eventType, data := parseSSEFrame(frame)
	if len(data) == 0 {
		return CodexFailureFrame{}, false
	}
	var envelope struct {
		Type       string                     `json:"type"`
		Status     json.RawMessage            `json:"status"`
		StatusCode json.RawMessage            `json:"status_code"`
		Error      json.RawMessage            `json:"error"`
		Response   json.RawMessage            `json:"response"`
		Headers    map[string]json.RawMessage `json:"headers"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return CodexFailureFrame{}, false
	}
	kind := strings.ToLower(strings.TrimSpace(eventType))
	bodyKind := strings.ToLower(strings.TrimSpace(envelope.Type))
	if kind != "response.failed" && kind != "response.error" && kind != "error" {
		kind = bodyKind
	}
	if kind != "response.failed" && kind != "response.error" && kind != "error" {
		return CodexFailureFrame{}, false
	}

	lower := strings.ToLower(string(data))
	status := jsonInt(envelope.StatusCode)
	if status == 0 {
		status = jsonInt(envelope.Status)
	}
	if status < 100 || status > 599 {
		status = 0
	}
	topLevelCode := structuredErrorCode(envelope.Error)
	nestedCode := structuredResponseErrorCode(envelope.Response)
	requestCode := topLevelCode
	if kind == "response.failed" {
		requestCode = nestedCode
		if requestCode == "" {
			requestCode = topLevelCode
		}
	} else if requestCode == "" {
		requestCode = nestedCode
	}
	requestError := ResponsesRequestErrorNone
	if requestCode == string(ResponsesRequestErrorContextLengthExceeded) {
		requestError = ResponsesRequestErrorContextLengthExceeded
		if status == 0 {
			status = http.StatusBadRequest
		}
	}
	// backend-api currently emits cyber_policy as a statusless response.failed
	// event inside an otherwise successful HTTP 200 SSE response. It is a
	// deterministic request-scoped 400, not a successful completion and not an
	// account-local retry signal.
	if requestCode == "cyber_policy" && status == 0 {
		status = http.StatusBadRequest
	}
	contextError := ResponsesContextErrorNone
	if requestError == ResponsesRequestErrorNone && requestCode != "cyber_policy" {
		contextError = DetectResponsesContextError(status, data)
		if status == 0 {
			// Some Codex SSE transports omit the redundant HTTP status from an
			// event:error/response.failed body. Promote only the exact encrypted
			// function-output signature; ordinary statusless client errors and the
			// broader context classifiers must remain pass-through.
			if detected := DetectResponsesContextError(http.StatusBadRequest, data); detected == ResponsesContextErrorEncryptedFunctionOutput {
				status = http.StatusBadRequest
				contextError = detected
			}
		}
	}
	statusRetryable := status == http.StatusUnauthorized || status == http.StatusForbidden ||
		status == http.StatusRequestTimeout || status == http.StatusTooManyRequests ||
		status == http.StatusServiceUnavailable || status >= 500
	signatureRetryable := containsAnyFold(lower, codexFailedLeakSignatures)
	builtinRetryable := statusRetryable || signatureRetryable || contextError != ResponsesContextErrorNone
	if requestError != ResponsesRequestErrorNone || requestCode == "cyber_policy" {
		// Request size is deterministic for this payload. Rotating or cooling down an
		// account cannot repair it. The same is true for the structured cyber_policy
		// request rejection, whose message must not accidentally opt into retry via a
		// generic overload phrase.
		builtinRetryable = false
	}
	if status == 0 && builtinRetryable {
		if containsAnyFold(lower, []string{"overloaded_error", "service_unavailable_error", "server_is_overloaded", "slow_down", "currently overloaded", "at capacity", "try a different model"}) {
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
		EventType:        kind,
		ErrorCode:        requestCode,
		StatusCode:       status,
		ContextError:     contextError,
		RequestError:     requestError,
		BuiltinRetryable: builtinRetryable,
		Header:           header,
		Body:             append([]byte(nil), data...),
	}, true
}

var (
	upstreamAbsoluteURLPattern = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"'<>\\]+`)
	upstreamIPPortPattern      = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}:\d{1,5}\b`)
	upstreamHostPortPattern    = regexp.MustCompile(`(?i)\b(?:localhost|(?:[a-z0-9-]+\.)+[a-z0-9-]+):\d{1,5}\b`)
	upstreamDialTargetPattern  = regexp.MustCompile(`(?i)(\bdial\s+(?:tcp|udp)\s+)(?:\[[0-9a-f:]+\]|[a-z0-9._-]+):\d{1,5}`)
)

// RedactUpstreamTopology removes transport addresses from an upstream error
// envelope without otherwise rewriting its protocol shape. This is an invariant,
// independent of optional quota/leak scrubbing: a normal downstream API key must
// never reveal a custom provider's internal URL, redirect target, or dial address.
func RedactUpstreamTopology(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	out := upstreamAbsoluteURLPattern.ReplaceAll(body, []byte("<upstream>"))
	out = upstreamIPPortPattern.ReplaceAll(out, []byte("<upstream>"))
	out = upstreamHostPortPattern.ReplaceAll(out, []byte("<upstream>"))
	out = upstreamDialTargetPattern.ReplaceAll(out, []byte("$1<upstream>"))
	if bytes.Equal(out, body) {
		return body, false
	}
	return out, true
}

func structuredResponseErrorCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var response struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return ""
	}
	return structuredErrorCode(response.Error)
}

func structuredErrorCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var object struct {
		Code json.RawMessage `json:"code"`
	}
	if err := json.Unmarshal(raw, &object); err != nil || len(object.Code) == 0 {
		return ""
	}
	var code string
	if err := json.Unmarshal(object.Code, &code); err != nil {
		return ""
	}
	return strings.TrimSpace(code)
}

// ParseRetryableCodexFailureFrame applies the built-in retry policy to a parsed
// terminal frame. Ordinary client errors remain visible to ParseCodexFailureFrame for
// operator rules but are not retried or filtered by default.
func ParseRetryableCodexFailureFrame(frame []byte) (CodexFailureFrame, bool) {
	failure, ok := ParseCodexFailureFrame(frame)
	return failure, ok && failure.BuiltinRetryable
}

// DetectResponsesContextError recognizes only the Responses 400s that indicate
// missing account-local context. Matching structured fields/messages keeps unrelated
// invalid_request_error responses out of the transparent recovery path.
func DetectResponsesContextError(status int, body []byte) ResponsesContextErrorKind {
	if status != http.StatusBadRequest {
		return ResponsesContextErrorNone
	}
	const maxStructuredErrorBytes = 64 << 10
	if len(body) == 0 || len(body) > maxStructuredErrorBytes {
		return ResponsesContextErrorNone
	}
	remaining := maxStructuredErrorBytes
	return responsesContextErrorInStructuredError(body, 0, &remaining)
}

// IsOrphanedToolCallOutputError is kept for callers that only care about the original
// missing-tool-call subtype. New recovery code should use DetectResponsesContextError.
func IsOrphanedToolCallOutputError(status int, body []byte) bool {
	return DetectResponsesContextError(status, body) == ResponsesContextErrorOrphanedToolOutput
}

// responsesContextErrorInStructuredError follows only the error-message wrapping used
// by the Responses transports. Some gateways serialize the real OpenAI error object
// into error.message, and a second relay can do that once more. Limiting both depth and
// the cumulative decoded bytes prevents a client-controlled 400 from turning recovery
// detection into an unbounded recursive JSON parser.
func responsesContextErrorInStructuredError(raw []byte, depth int, remaining *int) ResponsesContextErrorKind {
	if depth > 2 || remaining == nil || len(raw) == 0 || len(raw) > *remaining {
		return ResponsesContextErrorNone
	}
	*remaining -= len(raw)

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root map[string]interface{}
	if err := decoder.Decode(&root); err != nil {
		return ResponsesContextErrorNone
	}
	var trailing interface{}
	if decoder.Decode(&trailing) != io.EOF {
		return ResponsesContextErrorNone
	}

	if hasPreviousResponseNotFoundType(root) {
		return ResponsesContextErrorPreviousResponseNotFound
	}

	messages := structuredErrorMessages(root)
	for _, message := range messages {
		if isUnsupportedPreviousResponseIDMessage(message) {
			// Treat the exact schema rejection as unavailable continuation state.
			// The durable recovery path will remove the connection-scoped pointer
			// before retrying; unrelated unsupported parameters remain ordinary 400s.
			return ResponsesContextErrorPreviousResponseNotFound
		}
		if isMissingPairedToolOutputMessage(message) {
			return ResponsesContextErrorOrphanedToolOutput
		}
		if isEncryptedFunctionOutputMessage(message) {
			return ResponsesContextErrorEncryptedFunctionOutput
		}
	}
	if depth == 2 {
		return ResponsesContextErrorNone
	}
	for _, message := range messages {
		nested := strings.TrimSpace(message)
		if len(nested) < 2 || nested[0] != '{' || nested[len(nested)-1] != '}' {
			continue
		}
		if kind := responsesContextErrorInStructuredError([]byte(nested), depth+1, remaining); kind != ResponsesContextErrorNone {
			return kind
		}
	}
	return ResponsesContextErrorNone
}

func hasPreviousResponseNotFoundType(root map[string]interface{}) bool {
	if root == nil {
		return false
	}
	matches := func(detail map[string]interface{}) bool {
		if detail == nil {
			return false
		}
		for _, field := range []string{"type", "code"} {
			value, _ := detail[field].(string)
			if strings.TrimSpace(value) == string(ResponsesContextErrorPreviousResponseNotFound) {
				return true
			}
		}
		return false
	}
	if matches(root) {
		return true
	}
	if detail, ok := root["error"].(map[string]interface{}); ok && matches(detail) {
		return true
	}
	if response, ok := root["response"].(map[string]interface{}); ok {
		if detail, ok := response["error"].(map[string]interface{}); ok && matches(detail) {
			return true
		}
	}
	return false
}

func structuredErrorMessages(root map[string]interface{}) []string {
	if root == nil {
		return nil
	}
	messages := make([]string, 0, 3)
	appendMessage := func(value interface{}) {
		if message, ok := value.(string); ok && strings.TrimSpace(message) != "" {
			messages = append(messages, message)
		}
	}
	appendMessage(root["message"])
	appendMessage(root["detail"])
	if detail, ok := root["error"].(map[string]interface{}); ok {
		appendMessage(detail["message"])
		appendMessage(detail["detail"])
	}
	if response, ok := root["response"].(map[string]interface{}); ok {
		if detail, ok := response["error"].(map[string]interface{}); ok {
			appendMessage(detail["message"])
			appendMessage(detail["detail"])
		}
	}
	return messages
}

// IsAuthoritativeCodexUsageLimit recognizes only the machine-readable terminal
// envelopes that stable Codex maps to CodexErrorInfo::UsageLimitExceeded. A local
// quota snapshot, a generic 429/rate_limit_exceeded, overload, or error-message
// wording is deliberately insufficient: Goal turns keep their bound account until
// one of these exact upstream fields is observed.
func IsAuthoritativeCodexUsageLimit(status int, body []byte) bool {
	if status != http.StatusTooManyRequests || len(body) == 0 || len(body) > 64<<10 {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]interface{}
	if decoder.Decode(&root) != nil {
		return false
	}
	var trailing interface{}
	if decoder.Decode(&trailing) != io.EOF {
		return false
	}
	exactString := func(object map[string]interface{}, field string, accepted ...string) bool {
		value, ok := object[field].(string)
		if !ok {
			return false
		}
		for _, candidate := range accepted {
			if value == candidate {
				return true
			}
		}
		return false
	}

	// HTTP Responses errors and WebSocket event:error frames both carry the fixed
	// terminal code in error.type. The outer type is absent for HTTP and "error"
	// for WebSocket; any other outer event shape is handled below as SSE.
	outerType, _ := root["type"].(string)
	if outerType == "" || outerType == "error" {
		if detail, _ := root["error"].(map[string]interface{}); exactString(detail, "type", "usage_limit_reached", "usage_not_included") {
			return true
		}
	}

	// A Responses SSE terminal instead carries the fixed code at
	// response.failed.response.error.code. Requiring the exact outer event type and
	// field path prevents a message string or look-alike nested JSON from becoming an
	// account-switch trigger.
	if outerType == "response.failed" {
		if response, _ := root["response"].(map[string]interface{}); response != nil {
			if detail, _ := response["error"].(map[string]interface{}); exactString(detail, "code", "insufficient_quota", "usage_not_included") {
				return true
			}
		}
	}
	return false
}

func isUnsupportedPreviousResponseIDMessage(message string) bool {
	// Fields collapses the newline emitted by one observed backend wrapper:
	// "Unsupported parameter:\nprevious_response_id".
	normalized := strings.ToLower(strings.Join(strings.Fields(message), " "))
	const prefix = "unsupported parameter:"
	if !strings.HasPrefix(normalized, prefix) {
		return false
	}
	parameter := strings.TrimSpace(strings.TrimPrefix(normalized, prefix))
	parameter = strings.Trim(parameter, " \t\r\n\"'`.")
	return parameter == "previous_response_id"
}

func isMissingPairedToolOutputMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if !strings.HasPrefix(lower, "no tool call found for ") {
		return false
	}
	for _, outputKind := range []string{
		"function call output",
		"custom tool call output",
		"tool search output",
		"mcp tool call output",
	} {
		if strings.HasPrefix(lower, "no tool call found for "+outputKind+" with call_id") {
			return true
		}
	}
	return false
}

func isEncryptedFunctionOutputMessage(message string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(message), " "))
	return normalized == "encrypted function output content could not be decrypted or decoded." ||
		normalized == "encrypted function output content could not be decrypted or decoded"
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
				boundary, separatorLen := sseFrameBoundary(f.buf)
				if boundary < 0 {
					break
				}
				frameEnd := boundary + separatorLen
				frame := f.buf[:frameEnd]
				f.buf = f.buf[frameEnd:]
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
// protocol, context-loss errors are rewritten to a generic terminal event while
// purely-informational limit/metadata frames are still dropped (those clients
// tolerate their absence). Kept frames have sensitive words scrubbed.
func (f *SSEFilter) processFrame(frame []byte) []byte {
	if f.provider == "claude" {
		if nb, ok := f.neutralizeClaudeError(frame); ok {
			frame = nb
		}
	} else {
		f.ObserveFrameForRelay(frame)
		// An upstream can occasionally emit response.created followed immediately
		// by response.completed with no output and no usage. That is a silent
		// upstream refusal, not a successful assistant turn. Preserve legitimate
		// no-text turns when a tool/reasoning item was already emitted or when the
		// terminal carries usage evidence, but turn the truly empty terminal into a
		// protocol-visible retryable failure so clients never checkpoint an empty
		// response as valid context.
		if f.codexSawLifecycle && !f.codexSemanticOutput && IsEmptyCodexCompletionFrame(frame) {
			frame = neutralResponsesFailureSSEFrame()
		}
		// safety_buffering is an upstream-only UI control signal. When pool leak
		// scrubbing is enabled it must never reach any Responses client, regardless
		// of whether an administrator also configured a dedicated heartbeat rule.
		frame, _ = responsefilter.StripSafetyBufferingSSE(frame)
		if nb, ok := NeutralizeResponsesContextErrorSSEFrame(frame); ok {
			frame = nb
		} else if nb, ok := NeutralizeCodexRetryableFailureSSEFrame(frame); ok {
			frame = nb
		} else if f.shouldDropCodex(frame) {
			return nil
		}
	}
	if f.words != nil && !f.words.Empty() {
		return f.words.ReplaceAll(frame)
	}
	return frame
}

// ObserveFrameForRelay advances only stream-semantic state. Rule pipelines call
// this before an operator filter can intentionally remove an output delta; the
// following response.completed must still remain a valid terminal rather than be
// mistaken for an upstream-empty response.
func (f *SSEFilter) ObserveFrameForRelay(frame []byte) {
	if f == nil || f.provider == "claude" {
		return
	}
	if codexSSEFrameStartsResponse(frame) {
		f.codexSawLifecycle = true
	}
	if codexSSEFrameHasSemanticOutput(frame) {
		f.codexSemanticOutput = true
	}
}

func codexSSEFrameStartsResponse(frame []byte) bool {
	eventType, data := parseSSEFrame(frame)
	if len(data) == 0 {
		return false
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return false
	}
	kind := strings.TrimSpace(envelope.Type)
	if kind == "" {
		kind = strings.TrimSpace(eventType)
	}
	return kind == "response.created"
}

// IsEmptyCodexCompletionFrame reports the narrow malformed-success shape seen in
// production: response.completed with neither semantic output nor usage evidence.
// It intentionally does not consider earlier frames; the stateful SSEFilter and
// the early probe separately require that no prior content was observed.
func IsEmptyCodexCompletionFrame(frame []byte) bool {
	eventType, data := parseSSEFrame(frame)
	if len(data) == 0 {
		return false
	}
	var envelope struct {
		Type     string          `json:"type"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return false
	}
	kind := strings.TrimSpace(envelope.Type)
	if kind == "" {
		kind = strings.TrimSpace(eventType)
	}
	if kind != "response.completed" || len(bytes.TrimSpace(envelope.Response)) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Response), []byte("null")) {
		return false
	}
	var response struct {
		Output     json.RawMessage `json:"output"`
		OutputText string          `json:"output_text"`
		Usage      json.RawMessage `json:"usage"`
		Error      json.RawMessage `json:"error"`
	}
	if json.Unmarshal(envelope.Response, &response) != nil {
		return false
	}
	if strings.TrimSpace(response.OutputText) != "" || jsonContainerHasValues(response.Output) || jsonContainerHasValues(response.Usage) || jsonContainerHasValues(response.Error) {
		return false
	}
	return true
}

func codexSSEFrameHasSemanticOutput(frame []byte) bool {
	eventType, data := parseSSEFrame(frame)
	if len(data) == 0 {
		return false
	}
	var envelope struct {
		Type     string          `json:"type"`
		Delta    json.RawMessage `json:"delta"`
		Item     json.RawMessage `json:"item"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return false
	}
	kind := strings.TrimSpace(envelope.Type)
	if kind == "" {
		kind = strings.TrimSpace(eventType)
	}
	switch {
	case kind == "response.output_item.added", kind == "response.output_item.done":
		return jsonContainerHasValues(envelope.Item)
	case strings.HasPrefix(kind, "response.output_text."),
		strings.HasPrefix(kind, "response.function_call_arguments."),
		strings.HasPrefix(kind, "response.custom_tool_call_input."),
		strings.HasPrefix(kind, "response.reasoning."),
		strings.HasPrefix(kind, "response.audio."),
		strings.HasPrefix(kind, "response.image_generation_call."):
		return jsonContainerHasValues(envelope.Delta) || len(bytes.TrimSpace(data)) > 0
	case kind == "response.completed" && len(envelope.Response) > 0:
		var response struct {
			Output     json.RawMessage `json:"output"`
			OutputText string          `json:"output_text"`
		}
		return json.Unmarshal(envelope.Response, &response) == nil &&
			(strings.TrimSpace(response.OutputText) != "" || jsonContainerHasValues(response.Output))
	}
	return false
}

func jsonContainerHasValues(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) || bytes.Equal(trimmed, []byte("[]")) || bytes.Equal(trimmed, []byte(`""`)) {
		return false
	}
	return true
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
	payload, err := json.Marshal(map[string]interface{}{
		"type":  "error",
		"error": map[string]interface{}{"type": "api_error", "message": responsesPublicRetryMessage},
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

// shouldDropCodex reports whether a complete Codex/OpenAI SSE frame is purely
// informational pool state. Terminal failures are rewritten by processFrame and
// must never be dropped, otherwise clients remain stuck waiting for completion.
func (f *SSEFilter) shouldDropCodex(frame []byte) bool {
	eventType, data := parseSSEFrame(frame)
	if len(data) == 0 {
		return false
	}
	switch jsonStringField(data, "type") {
	case "codex.rate_limits":
		return true
	case "response.metadata":
		return asciiContainsFold(data, "openai_verification_recommendation")
	}
	if eventType == "codex.rate_limits" {
		return true
	}
	return false
}

// sseFrameBoundary accepts all legal/common blank-line forms. Intermediaries may
// normalize individual newlines, so mixed pairs must be handled as well.
func sseFrameBoundary(raw []byte) (int, int) {
	boundary, separatorLen := -1, 0
	for _, separator := range [][]byte{
		[]byte("\r\n\r\n"),
		[]byte("\n\n"),
		[]byte("\r\n\n"),
		[]byte("\n\r\n"),
	} {
		if index := bytes.Index(raw, separator); index >= 0 &&
			(boundary < 0 || index < boundary || (index == boundary && len(separator) > separatorLen)) {
			boundary, separatorLen = index, len(separator)
		}
	}
	return boundary, separatorLen
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
