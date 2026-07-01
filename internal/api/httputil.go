// httputil.go holds the leaf HTTP/stream helper functions of package api: body-size
// limiting, SSE detection + micro-batched relay copy, downstream header copying, the
// JSON/raw/error writers, and small request-shaping helpers. Extracted verbatim from
// server.go (no behavior change) to keep the gateway file focused on request flow.
package api

import (
	"bytes"
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

func generatedID(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}
