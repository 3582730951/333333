// httputil.go holds the leaf HTTP/stream helper functions of package api: body-size
// limiting, SSE detection + micro-batched relay copy, downstream header copying, the
// JSON/raw/error writers, and small request-shaping helpers. Extracted verbatim from
// server.go (no behavior change) to keep the gateway file focused on request flow.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
	"github.com/tidwall/sjson"
)

const upstreamErrorBodyLimit = 1 << 20
const adminJSONBodyLimit = 1 << 20

var errRequestBodyTooLarge = errors.New("request body too large")

func readLimited(r io.Reader, max int64) ([]byte, error) {
	return readLimitedWithMessage(r, max, "request body too large")
}

func requestBodyBytes(r *http.Request, max int64) ([]byte, error) {
	if source := bodySourceFromContext(r.Context()); source != nil {
		if max <= 0 {
			max = config.DefaultMaxBodyBytes
		}
		if source.Size() > max {
			return nil, errRequestBodyTooLarge
		}
		if view, ok := bodysource.ByteView(source); ok {
			return view, nil
		}
	}
	return readLimited(r.Body, max)
}

func readLimitedWithMessage(r io.Reader, max int64, limitMessage string) ([]byte, error) {
	if max <= 0 {
		max = config.DefaultMaxBodyBytes
	}
	raw, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > max {
		if limitMessage == errRequestBodyTooLarge.Error() {
			return nil, errRequestBodyTooLarge
		}
		return nil, errors.New(limitMessage)
	}
	return raw, nil
}

func readJSONRequestBody(r io.Reader, max int64) ([]byte, error) {
	return readLimitedWithMessage(r, max, "request body too large")
}

func decodeJSONRequestBody(r io.Reader, dst interface{}, max int64) error {
	raw, err := readJSONRequestBody(r, max)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra interface{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain a single JSON value")
	}
	return nil
}

// decodeJSONMapUseNumber decodes an already-bounded JSON value without turning
// integer fields into float64. It is used by internal request shapers that must
// preserve the caller's numeric wire representation while changing one field.
func decodeJSONMapUseNumber(raw []byte) (map[string]interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root map[string]interface{}
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	var extra interface{}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("JSON input must contain a single value")
		}
		return nil, err
	}
	return root, nil
}

func readUpstreamErrorBody(r io.Reader) []byte {
	body, _ := io.ReadAll(io.LimitReader(r, upstreamErrorBodyLimit))
	return body
}

func (s *Server) readUpstreamResponseBody(r io.Reader) ([]byte, error) {
	return readLimitedWithMessage(r, s.cfg.MaxBodyBytes, "upstream response body too large")
}

func isStreamRequest(raw []byte) bool {
	// Decode only the top-level "stream" flag into a tiny struct rather than the whole
	// body into a map[string]interface{}: the body can be up to MaxBodyBytes, and the
	// map form boxes every value just to read one bool. A non-bool/absent "stream", or
	// invalid JSON, yields false — identical to the previous map+type-assertion form.
	var v struct {
		Stream bool `json:"stream"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	return v.Stream
}

func chatStreamUsageRequested(raw []byte) bool {
	var v struct {
		Stream        bool `json:"stream"`
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	return v.Stream && v.StreamOptions.IncludeUsage
}

func isEventStream(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("content-type")), "text/event-stream")
}

func normalizeCodexStreamContentType(h http.Header, requestedStream bool) {
	if !requestedStream || isEventStream(h) {
		return
	}
	ct := strings.ToLower(strings.TrimSpace(h.Get("content-type")))
	if ct == "" || strings.HasPrefix(ct, "text/plain") {
		h.Set("Content-Type", "text/event-stream")
	}
}

// sseBufPool recycles the 32 KiB copy buffers used by the streaming relay paths
// (streamCopy / streamCopyRewrite) so a high-concurrency stream load does not
// allocate a fresh buffer per response. Each buffer is fully overwritten by every
// Read before it is forwarded, so pooled reuse is byte-for-byte equivalent.
var sseBufPool = sync.Pool{New: func() interface{} { b := make([]byte, 32*1024); return &b }}

const (
	sseFlushBatchSize = 4 * 1024
	sseFlushMaxDelay  = 3 * time.Millisecond
	sseTailFlushLimit = 8 * 1024
)

type adaptiveSSEBatch struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	flusher  http.Flusher
	batch    []byte
	timer    *time.Timer
	first    bool
	closed   bool
	writeErr error
}

func newAdaptiveSSEBatch(w http.ResponseWriter) *adaptiveSSEBatch {
	flusher, _ := w.(http.Flusher)
	return &adaptiveSSEBatch{w: w, flusher: flusher}
}

func (b *adaptiveSSEBatch) append(p []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.writeErr != nil {
		return b.writeErr
	}
	b.batch = append(b.batch, p...)
	complete := completeSSEPrefixLen(b.batch)
	switch {
	case complete > 0 && !b.first:
		b.first = true
		return b.flushPrefixLocked(complete)
	case complete >= sseFlushBatchSize:
		return b.flushPrefixLocked(complete)
	case complete > 0:
		b.armLocked()
	case len(b.batch) >= sseTailFlushLimit:
		return b.flushPrefixLocked(len(b.batch))
	}
	return nil
}

func (b *adaptiveSSEBatch) armLocked() {
	if b.timer != nil {
		return
	}
	b.timer = time.AfterFunc(sseFlushMaxDelay, func() {
		defer supervisor.Recover("adaptive-sse-flush")
		b.mu.Lock()
		defer b.mu.Unlock()
		b.timer = nil
		if b.closed || b.writeErr != nil {
			return
		}
		if complete := completeSSEPrefixLen(b.batch); complete > 0 {
			_ = b.flushPrefixLocked(complete)
		}
	})
}

func (b *adaptiveSSEBatch) flushPrefixLocked(n int) error {
	if n <= 0 || b.writeErr != nil {
		return b.writeErr
	}
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	if _, err := b.w.Write(b.batch[:n]); err != nil {
		b.writeErr = err
		return err
	}
	if b.flusher != nil {
		b.flusher.Flush()
	}
	copy(b.batch, b.batch[n:])
	b.batch = b.batch[:len(b.batch)-n]
	return nil
}

func (b *adaptiveSSEBatch) close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	if b.writeErr != nil {
		return b.writeErr
	}
	return b.flushPrefixLocked(len(b.batch))
}

func (b *adaptiveSSEBatch) abort() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.batch = nil
}

// streamCopy copies an upstream SSE stream to the client. When the client supports
// flushing it relays every complete SSE prefix immediately and retains only an
// unterminated tail. SSE producers in the wild use LF, CRLF, or a mixture of the
// two between adjacent lines, so looking only at the final two bytes for "\n\n"
// both misses valid frames and delays a complete frame when the same Read also
// contains the beginning of the next one.
//
// A boundary-free event is still flushed in bounded chunks so one unusually large
// data field cannot grow the accumulator without limit. The pool buffer is used for
// the upstream Read; the accumulator reuses its backing array across iterations.
func streamCopy(w http.ResponseWriter, body io.Reader) error {
	bufp := sseBufPool.Get().(*[]byte)
	defer sseBufPool.Put(bufp)
	buf := *bufp
	batch := newAdaptiveSSEBatch(w)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if ferr := batch.append(buf[:n]); ferr != nil {
				return ferr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return batch.close()
			}
			batch.abort()
			return err
		}
	}
}

// completeSSEPrefixLen returns the end of the last complete SSE frame. Each of
// the two blank-line terminators may independently use LF or CRLF, which yields
// four wire spellings. Returning the last boundary lets streamCopy flush a whole
// completed prefix with one syscall while retaining an incomplete following frame.
func completeSSEPrefixLen(p []byte) int {
	last := 0
	for _, boundary := range [][]byte{
		[]byte("\n\n"),
		[]byte("\n\r\n"),
		[]byte("\r\n\n"),
		[]byte("\r\n\r\n"),
	} {
		if index := bytes.LastIndex(p, boundary); index >= 0 {
			if end := index + len(boundary); end > last {
				last = end
			}
		}
	}
	return last
}

func copyDownstreamHeaders(dst http.Header, src http.Header) {
	for k, values := range src {
		lower := strings.ToLower(k)
		if lower == "authorization" ||
			lower == "chatgpt-account-id" ||
			lower == "x-openai-fedramp" ||
			lower == "location" ||
			strings.HasPrefix(lower, "x-pool-") ||
			strings.HasPrefix(lower, "x-sidecar-") ||
			lower == "content-length" ||
			lower == "connection" {
			continue
		}
		for _, value := range values {
			dst.Add(k, value)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, status int, headers http.Header, body []byte) {
	if status >= http.StatusBadRequest {
		setDiagnosticFailureClass(w, "upstream_error")
		writePublicServiceUnavailable(w)
		return
	}
	copyDownstreamHeaders(w.Header(), headers)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	if errors.Is(err, errRequestBodyTooLarge) {
		status = http.StatusRequestEntityTooLarge
	}
	if status < 400 || status >= 500 {
		setDiagnosticFailureClass(w, safeDiagnosticErrorClass(err))
		writePublicServiceUnavailable(w)
		return
	}
	message, code := safeClientError(status)
	if public, ok := err.(*PublicError); ok {
		if strings.TrimSpace(public.Message) != "" {
			message = public.Message
		}
		if strings.TrimSpace(public.Code) != "" {
			code = public.Code
		}
	}
	resetPublicErrorHeaders(w)
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]interface{}{
			"message":    message,
			"type":       "invalid_request_error",
			"code":       code,
			"request_id": publicRequestID(w),
		},
	})
}

func safeDiagnosticErrorClass(err error) string {
	if errors.Is(err, context.Canceled) {
		return "request_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if err == nil {
		return "internal_error"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "database is locked"),
		strings.Contains(message, "database is busy"),
		strings.Contains(message, "sqlite_busy"):
		return "database_busy"
	case strings.Contains(message, "readonly"), strings.Contains(message, "read-only"):
		return "database_readonly"
	case strings.Contains(message, "database is closed"), strings.Contains(message, "sql: database is closed"):
		return "storage_unavailable"
	default:
		return "internal_error"
	}
}

// PublicError is restricted to messages and codes that are safe to expose. Raw
// upstream or persistence errors must never be wrapped in this type.
type PublicError struct {
	Code    string
	Message string
}

func (e *PublicError) Error() string { return e.Message }

func safeClientError(status int) (string, string) {
	switch status {
	case http.StatusBadRequest:
		return "The request is invalid.", "invalid_request"
	case http.StatusUnauthorized:
		return "Invalid or missing credentials.", "invalid_api_key"
	case http.StatusForbidden:
		return "The request is not permitted.", "forbidden"
	case http.StatusNotFound:
		return "The requested resource was not found.", "not_found"
	case http.StatusMethodNotAllowed:
		return "The requested method is not supported.", "method_not_allowed"
	case http.StatusConflict:
		return "The request conflicts with the current resource state.", "conflict"
	case http.StatusGone:
		return "This feature has been removed.", "feature_removed"
	case http.StatusRequestEntityTooLarge:
		return "The request body is too large.", "request_too_large"
	case http.StatusTooManyRequests:
		return "The downstream tenant quota has been exceeded.", "rate_limit_exceeded"
	default:
		return "The request could not be accepted.", "invalid_request"
	}
}

func publicRequestID(w http.ResponseWriter) string {
	requestID := strings.TrimSpace(w.Header().Get(requestIDHeader))
	if requestID == "" {
		requestID = newRequestID()
		w.Header().Set(requestIDHeader, requestID)
	}
	return requestID
}

func resetPublicErrorHeaders(w http.ResponseWriter) {
	requestID := strings.TrimSpace(w.Header().Get(requestIDHeader))
	retryAfter := strings.TrimSpace(w.Header().Get("Retry-After"))
	cacheControl := strings.TrimSpace(w.Header().Get("Cache-Control"))
	for name := range w.Header() {
		w.Header().Del(name)
	}
	if requestID != "" {
		w.Header().Set(requestIDHeader, requestID)
	}
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	if strings.Contains(strings.ToLower(cacheControl), "no-store") {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.Header().Set("Content-Type", "application/json")
}

func writePublicServiceUnavailable(w http.ResponseWriter) {
	setDiagnosticFailureClass(w, "service_unavailable")
	requestID := publicRequestID(w)
	resetPublicErrorHeaders(w)
	w.Header().Set(requestIDHeader, requestID)
	w.Header().Set("Retry-After", "3")
	writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
		"error": map[string]interface{}{
			"type":       "server_error",
			"code":       "service_unavailable",
			"message":    "The relay service is temporarily unavailable. Please retry.",
			"request_id": requestID,
		},
	})
}

func writeResourceExhausted(w http.ResponseWriter, message string) {
	setDiagnosticFailureClass(w, "admission_capacity")
	writePublicServiceUnavailable(w)
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
}

func pathWithQuery(path, query string) string {
	if query == "" {
		return path
	}
	return path + "?" + query
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (s *Server) codexClientVersionForModel(model string) string {
	if capability.CodexRequiresCurrentClientVersion(model) {
		return s.cfg.ClientVersion
	}
	return ""
}

func (s *Server) codexResponsesWebSocketForModel(model string, isChat, isCompact bool, body []byte) bool {
	return !isChat && !isCompact && isStreamRequest(body) && capability.CodexPrefersWebSocket(model)
}

func codexRequiresNewerVersion(body []byte) bool {
	hay := strings.ToLower(string(body))
	return strings.Contains(hay, "requires a newer version of codex") ||
		strings.Contains(hay, "upgrade to the latest app or cli")
}

func forceResponsesStream(raw []byte) []byte {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw
	}
	out, err := sjson.SetBytes(raw, "stream", true)
	if err != nil {
		return raw
	}
	return out
}

func ensureResponsesPromptCacheKey(raw []byte, key string) []byte {
	key = strings.TrimSpace(key)
	if key == "" || routing.PromptCacheKey(raw) != "" {
		return raw
	}
	// Decode only the top-level field table so adding a routing hint cannot
	// round-trip input/tool payload numbers through float64.  In particular, large
	// integer IDs inside the conversation must remain byte-for-byte unchanged.
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw
	}
	if _, ok := root["prompt_cache_key"]; ok {
		return raw
	}
	out, err := sjson.SetBytes(raw, "prompt_cache_key", key)
	if err != nil {
		return raw
	}
	return out
}

// normalizeOfficialCodexPromptCacheKey replaces only Codex CLI's generated
// per-thread UUID cache key with a model + stable-prefix key. Independent CLI
// processes otherwise send identical 7–8K system/tool prefixes to different cache
// shards and lose nearly every hit. prompt_cache_key is a routing hint, not context;
// the input/instructions/tools/history bytes remain untouched, and explicit custom
// keys from other clients are preserved verbatim.
func normalizeOfficialCodexPromptCacheKey(r *http.Request, raw []byte, model string, shardCounts ...int) ([]byte, bool) {
	existing := routing.PromptCacheKey(raw)
	stable := officialCodexStablePromptCacheBase(r, raw, model)
	if stable == "" {
		return raw, false
	}
	shards := 4
	if len(shardCounts) > 0 {
		shards = shardCounts[0]
	}
	if shards < 1 || shards > 16 {
		shards = 4
	}
	if shards > 1 {
		seed := officialCodexPromptCacheShardSeedWithRequest(r, raw, existing)
		stable = fmt.Sprintf("%s_s%02d", stable, codexPromptCacheShard(seed, shards))
	}
	if stable == existing {
		return raw, false
	}
	out, err := sjson.SetBytes(raw, "prompt_cache_key", stable)
	if err != nil {
		return raw, false
	}
	return out, true
}

// officialCodexStablePromptCacheBase returns the UNSHARDED stable key that
// normalizeOfficialCodexPromptCacheKey derives for this request, or "" when the request
// is not an official Codex CLI call carrying a generated per-thread UUID.
//
// Shard sizing reads this value rather than the emitted key: it is the identity of the
// shared upstream prefix, so its aggregate request rate — not any one shard's fraction
// of it — is what decides how many shards that prefix actually needs.
func officialCodexStablePromptCacheBase(r *http.Request, raw []byte, model string) string {
	if !looksLikeUUID(routing.PromptCacheKey(raw)) || !isOfficialCodexCLIRequest(r) {
		return ""
	}
	prefixHash := officialCodexBasePromptCacheHash(raw)
	if prefixHash == "" {
		prefixHash = automaticPromptCachePrefixHash(raw)
	}
	return automaticPromptCacheKey(model, prefixHash)
}

// officialCodexPromptCacheShardSeed keeps one conversation on one shard while
// distributing sibling agents that share the large immutable base prefix. A body
// thread_id is the strongest child-session identity; otherwise the first user turn
// anchors the session, with the CLI UUID as the final deterministic fallback.
func officialCodexPromptCacheShardSeed(raw []byte, originalUUID string) string {
	return officialCodexPromptCacheShardSeedWithRequest(nil, raw, originalUUID)
}

func officialCodexPromptCacheShardSeedWithRequest(r *http.Request, raw []byte, originalUUID string) string {
	if r != nil {
		// The official CLI carries child-thread identity in headers on some
		// Responses paths, while other paths put the same value in the body.
		// Prefer the direct child id before considering a root/parent hint.
		for _, header := range []string{"thread-id", "x-codex-thread-id"} {
			if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
				return "thread:" + value
			}
		}
		if metadata := strings.TrimSpace(r.Header.Get("x-codex-turn-metadata")); metadata != "" {
			if value := routing.JSONStringField([]byte(metadata), "thread_id"); value != "" {
				return "thread:" + value
			}
		}
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) == nil {
		var threadID string
		if json.Unmarshal(root["thread_id"], &threadID) == nil && strings.TrimSpace(threadID) != "" {
			return "thread:" + threadID
		}
	}
	if anchor := officialCodexFirstUserAnchor(raw); anchor != "" {
		return "anchor:" + anchor
	}
	return "uuid:" + strings.TrimSpace(originalUUID)
}

// officialCodexFirstUserAnchor hashes only the first user turn. The generic
// routing anchor intentionally falls back to the first item when a role is
// absent; sharding must not accidentally bind a developer-only preamble to a
// session shard that was meant for a user conversation.
func officialCodexFirstUserAnchor(raw []byte) string {
	root, err := decodeJSONMapUseNumber(raw)
	if err != nil {
		return ""
	}
	sequence := root["input"]
	if sequence == nil {
		sequence = root["messages"]
	}
	if text, ok := sequence.(string); ok && strings.TrimSpace(text) != "" {
		sum := sha256.Sum256([]byte(text))
		return hex.EncodeToString(sum[:])[:16]
	}
	items, ok := sequence.([]interface{})
	if !ok {
		return ""
	}
	for _, item := range items {
		object, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := object["role"].(string)
		if !strings.EqualFold(strings.TrimSpace(role), "user") {
			continue
		}
		encoded, err := json.Marshal(stripCodexVolatileCacheMetadata(item))
		if err != nil {
			return ""
		}
		sum := sha256.Sum256(encoded)
		return hex.EncodeToString(sum[:])[:16]
	}
	return ""
}

func codexPromptCacheShard(seed string, shards int) int {
	if shards <= 1 {
		return 0
	}
	sum := sha256.Sum256([]byte(seed))
	value := uint32(sum[0])<<24 | uint32(sum[1])<<16 | uint32(sum[2])<<8 | uint32(sum[3])
	return int(value % uint32(shards))
}

// officialCodexBasePromptCacheHash fingerprints the large immutable prefix shared by
// independent Codex roots and sibling sub-agents: tool schemas, developer messages,
// instructions, and reasoning configuration before the first user item. Per-turn
// metadata is deliberately omitted from the hint because Codex regenerates those UUIDs
// on every process/thread. This function never edits the request; the upstream still
// validates the exact prompt bytes before serving a cached prefix.
func officialCodexBasePromptCacheHash(raw []byte) string {
	root, err := decodeJSONMapUseNumber(raw)
	if err != nil {
		return ""
	}
	stable := map[string]interface{}{}
	for _, key := range []string{"instructions", "tools", "tool_choice", "parallel_tool_calls", "reasoning", "text"} {
		if value, ok := root[key]; ok {
			stable[key] = stripCodexVolatileCacheMetadata(value)
		}
	}
	if input, ok := root["input"].([]interface{}); ok {
		prefix := make([]interface{}, 0, len(input))
		for _, item := range input {
			if message, ok := item.(map[string]interface{}); ok {
				if role, _ := message["role"].(string); strings.EqualFold(strings.TrimSpace(role), "user") {
					break
				}
			}
			prefix = append(prefix, stripCodexVolatileCacheMetadata(item))
		}
		if len(prefix) > 0 {
			stable["input_prefix"] = prefix
		}
	}
	encoded, err := json.Marshal(stable)
	if err != nil || len(encoded) < 2048 {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "official_codex_base:" + hex.EncodeToString(sum[:])
}

func stripCodexVolatileCacheMetadata(value interface{}) interface{} {
	switch current := value.(type) {
	case map[string]interface{}:
		clean := make(map[string]interface{}, len(current))
		for key, child := range current {
			switch key {
			case "internal_chat_message_metadata_passthrough", "client_metadata":
				continue
			}
			clean[key] = stripCodexVolatileCacheMetadata(child)
		}
		return clean
	case []interface{}:
		clean := make([]interface{}, len(current))
		for i, child := range current {
			clean[i] = stripCodexVolatileCacheMetadata(child)
		}
		return clean
	default:
		return value
	}
}

func isOfficialCodexCLIRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	ua := strings.ToLower(strings.TrimSpace(r.Header.Get("User-Agent")))
	originator := strings.ToLower(strings.TrimSpace(r.Header.Get("originator")))
	return strings.HasPrefix(ua, "codex_exec/") || strings.HasPrefix(ua, "codex_cli_rs/") ||
		originator == "codex_exec" || originator == "codex_cli_rs"
}

func looksLikeUUID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 36 {
		return false
	}
	for i := 0; i < len(value); i++ {
		switch i {
		case 8, 13, 18, 23:
			if value[i] != '-' {
				return false
			}
		default:
			c := value[i]
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

func automaticPromptCacheKey(model, prefixHash string) string {
	model = strings.TrimSpace(model)
	prefixHash = strings.TrimSpace(prefixHash)
	if prefixHash == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(model + "\x00" + prefixHash))
	return "auto_" + hex.EncodeToString(sum[:])[:24]
}

func automaticPromptCachePrefixHash(raw []byte) string {
	prefixHash := routing.StablePromptPrefixHash(raw)
	if prefixHash == "" {
		// Dead zone: the final input item is an assistant/tool turn (agentic
		// Codex), so StablePromptPrefixFingerprint refuses to synthesize a key.
		// The conversation anchor (first user turn) is still derivable and stable
		// across turns, and it is exactly the key the user-ended turns already
		// use — without this, every agentic turn silently changes shard and the
		// upstream prefix cache built by the user turns goes cold.
		if automaticPromptCacheKeySafe(raw) && hasHistoricalUserTurn(raw) {
			if anchor := routing.ConversationAnchor(raw); anchor != "" {
				return "conversation:" + anchor
			}
		}
		return ""
	}
	if !automaticPromptCacheKeySafe(raw) {
		return ""
	}
	// The official Codex client keeps one prompt_cache_key for the lifetime of a
	// thread. Mirror that behavior when a third-party client omitted the key: the
	// first user turn is stable while later assistant/tool/user items accumulate.
	// StablePromptPrefixHash above remains the eligibility gate (large reusable
	// prefix required); the anchor only prevents needless key/shard churn between
	// turns. automaticPromptCacheKey additionally isolates by model.
	if hasHistoricalUserTurn(raw) {
		if anchor := routing.ConversationAnchor(raw); anchor != "" {
			return "conversation:" + anchor
		}
	}
	return prefixHash
}

// hasHistoricalUserTurn distinguishes a growing conversation from a single
// long, still-editable user input. In the latter case the stable 4 KiB text-head
// fingerprint must remain authoritative so changing only the tail keeps one key.
func hasHistoricalUserTurn(raw []byte) bool {
	var root map[string]interface{}
	if json.Unmarshal(raw, &root) != nil {
		return false
	}
	seq := root["messages"]
	if seq == nil {
		seq = root["input"]
	}
	items, ok := seq.([]interface{})
	if !ok || len(items) < 2 {
		return false
	}
	for _, item := range items[:len(items)-1] {
		if m, ok := item.(map[string]interface{}); ok && m["role"] == "user" {
			return true
		}
	}
	return false
}

func codexSelectionAffinity(r *http.Request, raw []byte, base routing.AffinityKey, group string) routing.AffinityKey {
	return codexSelectionAffinityWithMeta(r, raw, nil, base, group)
}

func codexSelectionAffinityWithMeta(r *http.Request, raw []byte, meta *bodysource.BodyMeta, base routing.AffinityKey, group string) routing.AffinityKey {
	if isTrueConversationAffinity(base.Source) {
		return base
	}
	if meta != nil && meta.Size == int64(len(raw)) && meta.InputItemCount > 0 && meta.LastInputRole != "user" {
		// StablePromptPrefixFingerprint requires the final input item to be a user
		// turn. The streaming scanner already observed the top-level item role, so
		// avoid materializing a 128K-1M tool/assistant-ended history only to reject it.
		return base
	}
	prefixHash := automaticPromptCachePrefixHash(raw)
	if prefixHash == "" {
		return base
	}
	model := routing.Model(raw)
	return routing.AffinityFromKey(strings.Join([]string{"cache_prefix", strings.TrimSpace(group), model, prefixHash}, ":"), "cache_prefix_hash")
}

func codexRequestUsageDiagnostics(body []byte, meta *bodysource.BodyMeta, affinity routing.AffinityKey, promptCacheKeySource, retentionEffective, retentionSource string) storage.UsageDiagnostics {
	stableSource, stableReason, stableBytes := "", "", 0
	if meta != nil && meta.Size == int64(len(body)) && len(body) > 128<<10 && meta.StablePrefixHMAC != "" {
		stableSource, stableReason, stableBytes = "body_meta_hmac", "bounded_large_body", int(meta.StablePrefixBytes)
	} else {
		fp := routing.StablePromptPrefixFingerprint(body)
		stableSource, stableReason, stableBytes = fp.Source, fp.Reason, fp.PrefixBytes
	}
	breakpointCount, breakpointsJSON := codexCacheBreakpointDiagnostics(body)
	return storage.UsageDiagnostics{
		UsageProvider:         "codex",
		AffinitySource:        affinity.Source,
		PromptCacheKeyPresent: promptCacheKeyWithMeta(body, meta) != "",
		PromptCacheKeySource:  promptCacheKeySource,
		StablePrefixSource:    stableSource,
		StablePrefixReason:    stableReason,
		StablePrefixBytes:     stableBytes,
		RetentionEffective:    retentionEffective,
		RetentionSource:       retentionSource,
		CacheBreakpointCount:  breakpointCount,
		CacheBreakpointsJSON:  breakpointsJSON,
	}
}

func codexCacheBreakpointDiagnostics(body []byte) (int, string) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return 0, ""
	}
	var input []json.RawMessage
	if json.Unmarshal(root["input"], &input) != nil {
		return 0, ""
	}
	type breakpoint struct {
		Item  int    `json:"item"`
		Block int    `json:"block"`
		Mode  string `json:"mode"`
	}
	breakpoints := make([]breakpoint, 0, 2)
	for itemIndex, itemRaw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(itemRaw, &item) != nil {
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(item["content"], &blocks) != nil {
			continue
		}
		for blockIndex, blockRaw := range blocks {
			var block map[string]json.RawMessage
			if json.Unmarshal(blockRaw, &block) != nil {
				continue
			}
			var marker struct {
				Mode string `json:"mode"`
			}
			if raw, ok := block["prompt_cache_breakpoint"]; ok && json.Unmarshal(raw, &marker) == nil {
				breakpoints = append(breakpoints, breakpoint{Item: itemIndex, Block: blockIndex, Mode: marker.Mode})
			}
		}
	}
	if len(breakpoints) == 0 {
		return 0, ""
	}
	raw, _ := json.Marshal(breakpoints)
	return len(breakpoints), string(raw)
}

func codexRetentionDiagnosticsForTransport(diag storage.UsageDiagnostics, responsesWebSocket bool) storage.UsageDiagnostics {
	_ = responsesWebSocket
	if diag.RetentionEffective == "" {
		return diag
	}
	diag.RetentionEffective = ""
	diag.RetentionSource = "stripped_unsupported_transport"
	return diag
}

func claudeRequestUsageDiagnostics(body []byte, affinity routing.AffinityKey, ttl string, injected bool) storage.UsageDiagnostics {
	cacheDiag := prompt.InspectAnthropicCacheControl(body)
	breakpointsJSON := ""
	if len(cacheDiag.Breakpoints) > 0 {
		if raw, err := json.Marshal(cacheDiag.Breakpoints); err == nil {
			breakpointsJSON = string(raw)
		}
	}
	return storage.UsageDiagnostics{
		UsageProvider:                     "claude",
		AffinitySource:                    affinity.Source,
		ClaudeCacheTTL:                    ttl,
		CacheControlInjected:              injected && cacheDiag.BreakpointCount > 0,
		CacheBreakpointCount:              cacheDiag.BreakpointCount,
		CacheBreakpointsJSON:              breakpointsJSON,
		UnwrittenTailTokens:               cacheDiag.UnwrittenTailTokens,
		MaxPossibleCacheReadTokens:        cacheDiag.MaxPossibleCacheReadTokens,
		LatestUserCacheControl:            cacheDiag.LatestUserCacheControl,
		LatestUserAutoContextCacheControl: cacheDiag.LatestUserAutoContextCacheControl,
		LatestUserTailCacheControl:        cacheDiag.LatestUserTailCacheControl,
		LatestUserToolResultCacheControl:  cacheDiag.LatestUserToolResultCacheControl,
	}
}

func isTrueConversationAffinity(source string) bool {
	switch source {
	case routing.CodexRootThreadAffinitySource, "x-codex-parent-thread-id", "thread_id", "conversation_id",
		"x-codex-window-id", "previous_response_id", "x-codex-turn-state", "x-codex-turn-metadata":
		return true
	}
	return false
}

func automaticPromptCacheKeySafe(raw []byte) bool {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return false
	}
	seq := root["messages"]
	if seq == nil {
		seq = root["input"]
	}
	switch items := seq.(type) {
	case string:
		return true
	case []interface{}:
		if len(items) == 0 {
			return false
		}
		for _, item := range items {
			if !simplePromptMessage(item) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func simplePromptMessage(v interface{}) bool {
	m, ok := v.(map[string]interface{})
	if !ok {
		_, isText := v.(string)
		return isText
	}
	if t, _ := m["type"].(string); strings.TrimSpace(t) != "" {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "message":
			// Official Responses requests use type:"message" around the same
			// role/content shape accepted below.
		case "additional_tools", "agent_message", "reasoning", "function_call", "function_call_output",
			"custom_tool_call", "custom_tool_call_output", "local_shell_call", "local_shell_call_output",
			"computer_call", "computer_call_output", "web_search_call", "file_search_call",
			"tool_search_call", "tool_search_output", "image_generation_call",
			"compaction", "compaction_summary", "context_compaction",
			"mcp_call", "mcp_tool_call", "mcp_tool_call_output", "mcp_list_tools", "mcp_approval_request", "mcp_approval_response":
			// These are self-contained official Responses history items. The
			// cache key only selects a cache shard; the item bytes remain intact.
			return true
		default:
			return false
		}
	}
	role, _ := m["role"].(string)
	switch role {
	case "system", "developer", "user", "assistant":
	default:
		return false
	}
	return cacheKeySafePromptContent(m["content"])
}

func simplePromptContent(v interface{}) bool {
	switch c := v.(type) {
	case string:
		return true
	case []interface{}:
		if len(c) == 0 {
			return true
		}
		for _, part := range c {
			if !simplePromptContentPart(part) {
				return false
			}
		}
		return true
	case map[string]interface{}:
		return simplePromptContentPart(c)
	default:
		return false
	}
}

func cacheKeySafePromptContent(v interface{}) bool {
	switch c := v.(type) {
	case string:
		return true
	case []interface{}:
		if len(c) == 0 {
			return true
		}
		for _, part := range c {
			if !cacheKeySafePromptContentPart(part) {
				return false
			}
		}
		return true
	case map[string]interface{}:
		return cacheKeySafePromptContentPart(c)
	default:
		return false
	}
}

func cacheKeySafePromptContentPart(v interface{}) bool {
	switch part := v.(type) {
	case string:
		return true
	case map[string]interface{}:
		t, _ := part["type"].(string)
		switch t {
		case "input_text", "output_text", "text":
			_, ok := part["text"].(string)
			return ok
		case "input_image", "image_url", "image", "input_file", "file", "input_audio", "audio":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func simplePromptContentPart(v interface{}) bool {
	switch part := v.(type) {
	case string:
		return true
	case map[string]interface{}:
		t, _ := part["type"].(string)
		if t != "input_text" && t != "text" {
			return false
		}
		_, ok := part["text"].(string)
		return ok
	default:
		return false
	}
}

func generatedID(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}
