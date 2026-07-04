// httputil.go holds the leaf HTTP/stream helper functions of package api: body-size
// limiting, SSE detection + micro-batched relay copy, downstream header copying, the
// JSON/raw/error writers, and small request-shaping helpers. Extracted verbatim from
// server.go (no behavior change) to keep the gateway file focused on request flow.
package api

import (
	"bytes"
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

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

const upstreamErrorBodyLimit = 1 << 20
const adminJSONBodyLimit = 1 << 20

func readLimited(r io.Reader, max int64) ([]byte, error) {
	return readLimitedWithMessage(r, max, "request body too large")
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

const sseFlushBatchSize = 1024 // flush at most every ~1 KB accumulated to reduce syscalls

// streamCopy copies an upstream SSE stream to the client. When the client supports
// flushing it micro-batches writes: small chunks are accumulated until they reach
// sseFlushBatchSize or a complete SSE event boundary (\n\n), whichever comes first,
// so the typical few-dozen-byte SSE frame does not trigger a syscall per chunk.
// The pool buffer is used for the upstream Read; a stack-local accumulator avoids
// putting a second pooled buffer on the hot path.
func streamCopy(w http.ResponseWriter, body io.Reader) error {
	flusher, _ := w.(http.Flusher)
	bufp := sseBufPool.Get().(*[]byte)
	defer sseBufPool.Put(bufp)
	buf := *bufp
	// Micro-batch accumulator: stack-allocated slice that reuses its backing array
	// across iterations, resetting to zero-length without re-allocation.
	var batch []byte
	flushBatch := func() error {
		if len(batch) == 0 {
			return nil
		}
		if _, err := w.Write(batch); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		batch = batch[:0]
		return nil
	}
	for {
		n, err := body.Read(buf)
		if n > 0 {
			batch = append(batch, buf[:n]...)
			// Flush when the batch is large enough OR when we've accumulated a
			// complete SSE event (ends with \n\n). The SSE event boundary check
			// ensures timely token delivery even at low throughput (the downstream
			// CLI sees tokens as soon as the upstream finishes emitting them).
			if len(batch) >= sseFlushBatchSize || (len(batch) >= 2 && batch[len(batch)-2] == '\n' && batch[len(batch)-1] == '\n') {
				if ferr := flushBatch(); ferr != nil {
					return ferr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return flushBatch()
			}
			return err
		}
	}
}

func copyDownstreamHeaders(dst http.Header, src http.Header) {
	for k, values := range src {
		lower := strings.ToLower(k)
		if lower == "authorization" ||
			lower == "chatgpt-account-id" ||
			lower == "x-openai-fedramp" ||
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
	copyDownstreamHeaders(w.Header(), headers)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	errorBody := map[string]interface{}{
		"message": err.Error(),
		"type":    "codex_pool_error",
	}
	if requestID := strings.TrimSpace(w.Header().Get(requestIDHeader)); requestID != "" {
		errorBody["request_id"] = requestID
	}
	writeJSON(w, status, map[string]interface{}{
		"error": errorBody,
	})
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
	return !isChat && !isCompact && isStreamRequest(body) && capability.CodexRequiresCurrentClientVersion(model)
}

func codexRequiresNewerVersion(body []byte) bool {
	hay := strings.ToLower(string(body))
	return strings.Contains(hay, "requires a newer version of codex") ||
		strings.Contains(hay, "upgrade to the latest app or cli")
}

func forceResponsesStream(raw []byte) []byte {
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw
	}
	root["stream"] = true
	out, err := json.Marshal(root)
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
	var root map[string]interface{}
	if err := json.Unmarshal(raw, &root); err != nil {
		return raw
	}
	if _, ok := root["prompt_cache_key"]; ok {
		return raw
	}
	root["prompt_cache_key"] = key
	out, err := json.Marshal(root)
	if err != nil {
		return raw
	}
	return out
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
	if prefixHash == "" || !automaticPromptCacheKeySafe(raw) {
		return ""
	}
	return prefixHash
}

func codexSelectionAffinity(r *http.Request, raw []byte, base routing.AffinityKey, group string) routing.AffinityKey {
	if isTrueConversationAffinity(base.Source) {
		return base
	}
	prefixHash := automaticPromptCachePrefixHash(raw)
	if prefixHash == "" {
		return base
	}
	model := routing.Model(raw)
	return routing.AffinityFromKey(strings.Join([]string{"cache_prefix", strings.TrimSpace(group), model, prefixHash}, ":"), "cache_prefix_hash")
}

func codexRequestUsageDiagnostics(body []byte, affinity routing.AffinityKey, promptCacheKeySource, retentionEffective, retentionSource string) storage.UsageDiagnostics {
	fp := routing.StablePromptPrefixFingerprint(body)
	return storage.UsageDiagnostics{
		UsageProvider:         "codex",
		AffinitySource:        affinity.Source,
		PromptCacheKeyPresent: routing.PromptCacheKey(body) != "",
		PromptCacheKeySource:  promptCacheKeySource,
		StablePrefixSource:    fp.Source,
		StablePrefixReason:    fp.Reason,
		StablePrefixBytes:     fp.PrefixBytes,
		RetentionEffective:    retentionEffective,
		RetentionSource:       retentionSource,
	}
}

func codexRetentionDiagnosticsForTransport(diag storage.UsageDiagnostics, responsesWebSocket bool) storage.UsageDiagnostics {
	if responsesWebSocket || diag.RetentionEffective == "" {
		return diag
	}
	diag.RetentionEffective = ""
	diag.RetentionSource = "stripped_http_transport"
	return diag
}

func claudeRequestUsageDiagnostics(body []byte, affinity routing.AffinityKey, ttl string, injected bool) storage.UsageDiagnostics {
	cacheDiag := prompt.InspectAnthropicCacheControl(body)
	return storage.UsageDiagnostics{
		UsageProvider:          "claude",
		AffinitySource:         affinity.Source,
		ClaudeCacheTTL:         ttl,
		CacheControlInjected:   injected && cacheDiag.BreakpointCount > 0,
		CacheBreakpointCount:   cacheDiag.BreakpointCount,
		LatestUserCacheControl: cacheDiag.LatestUserCacheControl,
	}
}

func isTrueConversationAffinity(source string) bool {
	switch source {
	case "x-codex-parent-thread-id", "thread_id", "conversation_id",
		"x-codex-window-id", "prompt_cache_key", "x-codex-turn-metadata":
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
		return false
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
		case "input_text", "text":
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
