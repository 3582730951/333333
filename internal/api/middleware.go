package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-account-pool/internal/admission"
	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/supervisor"
)

const (
	requestIDHeader = "X-Request-ID"
	requestIDKey    = "request_id"
)

type clientErrorReport struct {
	Source         string `json:"source"`
	Message        string `json:"message"`
	Stack          string `json:"stack"`
	ComponentStack string `json:"component_stack"`
	Path           string `json:"path"`
	AssetSignature string `json:"asset_signature"`
	ResourceURL    string `json:"resource_url"`
	UserAgent      string `json:"user_agent"`
	OccurredAt     string `json:"occurred_at"`
}

type responseRecorder struct {
	http.ResponseWriter
	status             int
	wrote              bool
	hijacked           bool
	bytes              int64
	firstResponseAt    time.Time
	diagnosticErrClass string
}

type diagnosticFailureClassWriter interface {
	setDiagnosticFailureClass(string)
}

func (r *responseRecorder) setDiagnosticFailureClass(class string) {
	class = strings.TrimSpace(class)
	if r == nil || class == "" || r.diagnosticErrClass != "" {
		return
	}
	r.diagnosticErrClass = class
}

func setDiagnosticFailureClass(w http.ResponseWriter, class string) {
	class = strings.TrimSpace(class)
	if w == nil || class == "" {
		return
	}
	for depth := 0; depth < 8 && w != nil; depth++ {
		if recorder, ok := w.(diagnosticFailureClassWriter); ok {
			recorder.setDiagnosticFailureClass(class)
			return
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrapper.Unwrap()
	}
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wrote {
		return
	}
	r.markFirstResponse()
	r.status = status
	r.wrote = true
	r.ResponseWriter.WriteHeader(status)
}

// markFirstResponse captures when the downstream can first observe a response.
// Keeping that point separate from the handler lifetime is essential for SSE and
// WebSocket requests: a healthy stream may stay open for minutes even though its
// headers reached the CLI immediately.
func (r *responseRecorder) markFirstResponse() {
	if r == nil || !r.firstResponseAt.IsZero() {
		return
	}
	r.firstResponseAt = time.Now()
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *responseRecorder) Flush() {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying response writer does not support hijacking")
	}
	conn, rw, err := h.Hijack()
	if err == nil {
		r.markFirstResponse()
		r.hijacked = true
	}
	return conn, rw, err
}

func (r *responseRecorder) Push(target string, opts *http.PushOptions) error {
	p, ok := r.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return p.Push(target, opts)
}

func (r *responseRecorder) ReadFrom(src io.Reader) (int64, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		r.bytes += n
		return n, err
	}
	n, err := io.Copy(r.ResponseWriter, src)
	r.bytes += n
	return n, err
}

func (r *responseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	requestID := newRequestID()
	rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
	requestBytes := r.ContentLength
	metricRoute := s.httpMetrics.begin(r.URL.Path)
	_, routePattern := s.mux.Handler(r)
	routeName := diagnosticHTTPRouteName(routePattern)
	panicRecovered := false
	// Install the containment boundary before admission, body capture, scheduling,
	// or routing. The former late boundary left middleware panics to net/http,
	// which closed the connection before the diagnostic callback could run.
	defer func() {
		if value := recover(); value != nil {
			panicRecovered = true
			committedBeforePanic := rec.wrote || rec.hijacked
			log.Printf("[PANIC] request_id=%s method=%s path=%s remote=%s panic=%v",
				requestID, r.Method, r.URL.Path, r.RemoteAddr, value)
			if !rec.wrote && !rec.hijacked {
				writePublicServiceUnavailable(rec)
			}
			panicContext := fmt.Sprintf("%s %s request_id=%s remote=%s panic=%v",
				r.Method, r.URL.Path, requestID, r.RemoteAddr, value)
			supervisor.LogPanicEvent(supervisor.Event{
				Module: "http-request", Operation: "serve_http", RequestID: requestID,
				Route: routeName, Status: rec.status, Recovered: true,
				ResponseCommitted: committedBeforePanic,
				Message:           "module panic: " + panicContext, Panic: panicContext,
			}, value)
		}
		duration := time.Since(start)
		s.httpMetrics.finish(metricRoute, rec.status, requestBytes, rec.bytes, duration)
		ttfb := duration
		if !rec.firstResponseAt.IsZero() {
			ttfb = rec.firstResponseAt.Sub(start)
			if ttfb < 0 {
				ttfb = 0
			} else if ttfb > duration {
				ttfb = duration
			}
		}
		contentType := strings.ToLower(strings.TrimSpace(rec.Header().Get("Content-Type")))
		streaming := rec.hijacked || strings.Contains(contentType, "text/event-stream")
		s.recordHTTPRequestTiming(requestID, r.Method, routeName, rec.status, requestBytes, rec.bytes, duration, ttfb, streaming)
		if rec.status >= http.StatusInternalServerError && !panicRecovered {
			errorClass := strings.TrimSpace(rec.diagnosticErrClass)
			if errorClass == "" {
				errorClass = "http_status_5xx"
			}
			severity := "error"
			if rec.status == http.StatusServiceUnavailable || isTransientHTTPFailureClass(errorClass) {
				severity = "warning"
			}
			log.Printf("[HTTP-ERROR] request_id=%s status=%d method=%s path=%s bytes=%d duration=%s",
				requestID, rec.status, r.Method, r.URL.Path, rec.bytes, duration.Round(time.Millisecond))
			supervisor.Report(supervisor.Event{
				Type: "http_error", Severity: severity, Module: "http-request",
				Operation: "serve_http", ErrorClass: errorClass,
				RequestID: requestID, Route: routeName, Status: rec.status,
				Recovered: true, ResponseCommitted: rec.wrote || rec.hijacked,
				Message: diagnosticHTTPFailureMessage(errorClass),
			})
		}
	}()
	w.Header().Set(requestIDHeader, requestID)
	setSecurityHeaders(w, r)
	if !s.authorizeRegisteredRoute(rec, r) {
		return
	}
	if s.rejectForStoragePressure(r) {
		writePublicServiceUnavailable(rec)
		return
	}
	// Admission protects model traffic, not the control plane. Reserving inference
	// headroom for every admin GET made the dashboard, diagnostics, registration
	// stats, and settings disappear precisely while model traffic was saturated.
	// Body capture has its own bounded memory/disk budget for non-inference routes.
	if s.scheduler != nil && requiresInferenceAdmission(r) {
		reserveBytes := int64(256 << 10)
		release, reserveErr := s.scheduler.Reserve(r.Context(), reserveBytes)
		if reserveErr != nil {
			if errors.Is(reserveErr, admission.ErrCapacity) {
				writeResourceExhausted(rec, reserveErr.Error())
			} else {
				writeError(rec, http.StatusServiceUnavailable, reserveErr)
			}
			return
		}
		defer release()
	}
	var requestBody bodysource.BodySource
	var bodyMeta bodysource.BodyMeta
	bodyCaptureConfig := s.cfg
	if r.URL.Path == "/api/v1/admin/accounts" || strings.HasPrefix(r.URL.Path, "/api/v1/admin/accounts/") {
		// Hub compatibility accepts credential batches, but its public, scoped key
		// surface must never inherit the gateway's 1 GiB inference-body ceiling.
		bodyCaptureConfig.MaxBodyBytes = sub2APIHubBodyLimit
		if bodyCaptureConfig.BodyMemoryThresholdBytes > sub2APIHubBodyLimit {
			bodyCaptureConfig.BodyMemoryThresholdBytes = sub2APIHubBodyLimit
		}
	}
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		if !s.cfg.BodyV2Enabled {
			var raw []byte
			raw, err = readLimited(r.Body, bodyCaptureConfig.MaxBodyBytes)
			if err == nil {
				requestBody = bodysource.Bytes(raw)
			}
		} else if requestNeedsBodyMeta(r) {
			requestBody, bodyMeta, err = captureJSONRequestBody(r.Context(), r.Body, bodyCaptureConfig, s.requestBodyBudget, s.identitySecretCached, r.ContentLength)
		} else {
			requestBody, err = captureRequestBody(r.Context(), r.Body, bodyCaptureConfig, s.requestBodyBudget, r.ContentLength)
		}
		_ = r.Body.Close()
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, bodysource.ErrBodyTooLarge) {
				status = http.StatusRequestEntityTooLarge
			} else if writeBodyStorageError(rec, err) {
				s.recordBodyStorageRejection(err)
				return
			}
			writeError(rec, status, err)
			return
		}
		defer requestBody.Close()
		body, err := requestBody.Open()
		if err != nil {
			writeError(rec, http.StatusInternalServerError, err)
			return
		}
		defer body.Close()
		r.Body = body
		r.GetBody = requestBody.Open
		r.ContentLength = requestBody.Size()
	}
	ctx := contextWithRequestID(r.Context(), requestID)
	if requestBody != nil {
		ctx = contextWithBodySource(ctx, requestBody)
	}
	if bodyMeta.Size > 0 {
		ctx = contextWithBodyMeta(ctx, bodyMeta)
	}
	ctx = contextWithUsageEventID(ctx, newRequestID())
	ctx = contextWithAccountAgentClass(ctx, requestAccountAgentClass(r))
	ctx = contextWithRuntimeSettingsCache(ctx)
	r = r.WithContext(ctx)
	s.mux.ServeHTTP(rec, r)
}

func isTransientHTTPFailureClass(class string) bool {
	switch class {
	case "admission_capacity", "database_busy", "database_readonly", "deadline_exceeded", "service_unavailable", "storage_unavailable":
		return true
	default:
		return false
	}
}

func diagnosticHTTPFailureMessage(class string) string {
	switch class {
	case "admission_capacity":
		return "Model request admission is temporarily saturated"
	case "database_busy":
		return "Control-plane storage is busy; the request may be retried"
	case "database_readonly":
		return "Control-plane storage is temporarily read-only"
	case "deadline_exceeded":
		return "HTTP request exceeded its bounded deadline"
	case "storage_unavailable":
		return "Control-plane storage is temporarily unavailable"
	case "service_unavailable":
		return "HTTP request was temporarily unavailable"
	default:
		return "HTTP request completed with an internal server error"
	}
}

func isEmergencyDiagnosticExportRequest(r *http.Request) bool {
	if r == nil || r.Method != http.MethodGet || r.URL == nil || r.URL.Path != "/admin/export/logs" {
		return false
	}
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	return mode == "rescue" || mode == "emergency"
}

func requiresInferenceAdmission(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	path := r.URL.Path
	if isInferenceRequestPath(path) || path == "/v1/alpha/search" {
		return true
	}
	if !strings.HasPrefix(path, "/public-chat/") || r.Method != http.MethodPost {
		return false
	}
	_, action, ok := parsePublicChatAPIPath(path)
	return ok && action == "messages"
}

// diagnosticHTTPRouteName turns a repository-owned ServeMux pattern into a
// stable diagnostic label. Dynamic URL components never enter the runtime ring.
func diagnosticHTTPRouteName(pattern string) string {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" || pattern == "/" {
		return "root"
	}
	wildcard := strings.HasSuffix(pattern, "/")
	name := strings.Trim(strings.ReplaceAll(pattern, "/", "."), ".")
	if wildcard {
		name += ".*"
	}
	return name
}

func (s *Server) rejectForStoragePressure(r *http.Request) bool {
	if s == nil || r == nil || !isInferenceRequestPath(r.URL.Path) {
		return false
	}
	if s.storageAdmissionBlocked.Load() {
		return true
	}
	if !s.storageLargeRequestsPaused.Load() {
		return false
	}
	// Chunked requests have no trustworthy upper bound, so treat them as large while
	// the spool filesystem is critical. Known in-memory-sized requests may continue.
	return r.ContentLength < 0 || r.ContentLength > s.cfg.BodyMemoryThresholdBytes
}

func isInferenceRequestPath(path string) bool {
	switch path {
	case "/v1/responses", "/v1/responses/compact", "/v1/chat/completions",
		"/v1/messages", "/v1/messages/count_tokens":
		return true
	default:
		return strings.HasPrefix(path, "/v1/agents/") ||
			strings.HasPrefix(path, "/v1/environments/") ||
			strings.HasPrefix(path, "/v1/sessions/")
	}
}

func writeBodyStorageError(w http.ResponseWriter, err error) bool {
	var storageErr *bodysource.BodyStorageError
	if !errors.As(err, &storageErr) {
		if errors.Is(err, bodysource.ErrDiskReserve) {
			storageErr = &bodysource.BodyStorageError{Class: bodysource.BodyStorageDiskReserve, Cause: err}
		} else if errors.Is(err, bodysource.ErrSpoolBudget) {
			storageErr = &bodysource.BodyStorageError{Class: bodysource.BodyStorageLocalCapacity, Cause: err}
		} else {
			return false
		}
	}
	writePublicServiceUnavailable(w)
	return true
}

func captureRequestBody(ctx context.Context, src io.Reader, cfg config.Config, budget *bodysource.Budget, expectedBytes ...int64) (bodysource.BodySource, error) {
	return bodysource.Capture(ctx, src, bodysource.CaptureOptions{MaxBytes: cfg.MaxBodyBytes, MemoryThreshold: cfg.BodyMemoryThresholdBytes, ExpectedBytes: positiveExpectedBytes(expectedBytes), TempDir: cfg.BodySpoolDir, Budget: budget})
}

func captureJSONRequestBody(ctx context.Context, src io.Reader, cfg config.Config, budget *bodysource.Budget, hmacKey []byte, expectedBytes ...int64) (bodysource.BodySource, bodysource.BodyMeta, error) {
	return bodysource.CaptureJSON(ctx, src, bodysource.CaptureOptions{MaxBytes: cfg.MaxBodyBytes, MemoryThreshold: cfg.BodyMemoryThresholdBytes, ExpectedBytes: positiveExpectedBytes(expectedBytes), TempDir: cfg.BodySpoolDir, Budget: budget}, hmacKey)
}

func positiveExpectedBytes(values []int64) int64 {
	if len(values) > 0 && values[0] > 0 {
		return values[0]
	}
	return 0
}

func requestNeedsBodyMeta(r *http.Request) bool {
	switch r.URL.Path {
	case "/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages", "/v1/messages/count_tokens":
		return true
	default:
		return false
	}
}

type bodySourceContextKey struct{}

func contextWithBodySource(ctx context.Context, source bodysource.BodySource) context.Context {
	return context.WithValue(ctx, bodySourceContextKey{}, source)
}

func bodySourceFromContext(ctx context.Context) bodysource.BodySource {
	source, _ := ctx.Value(bodySourceContextKey{}).(bodysource.BodySource)
	return source
}

type bodyMetaContextKey struct{}

func contextWithBodyMeta(ctx context.Context, meta bodysource.BodyMeta) context.Context {
	return context.WithValue(ctx, bodyMetaContextKey{}, meta)
}

func bodyMetaFromContext(ctx context.Context) (bodysource.BodyMeta, bool) {
	meta, ok := ctx.Value(bodyMetaContextKey{}).(bodysource.BodyMeta)
	return meta, ok
}

func setSecurityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	if strings.HasPrefix(r.URL.Path, "/console/") {
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self' ws: wss:; base-uri 'none'; frame-ancestors 'self'")
	}
}

func contextWithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey, requestID)
}

func requestIDFromContext(ctx context.Context) string {
	if value, ok := ctx.Value(requestIDKey).(string); ok {
		return value
	}
	return ""
}

// usageEventIDKey carries a SERVER-owned per-request id used as the usage_event_id
// dedup key. It is decoupled from the client-supplied X-Request-ID so a client
// cannot cause under-counting (by reusing the header across distinct requests) or
// double-counting (by omitting it) of billable usage.
const usageEventIDKey = "usage_event_id"

func contextWithUsageEventID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, usageEventIDKey, id)
}

func usageEventIDFromContext(ctx context.Context) string {
	if value, ok := ctx.Value(usageEventIDKey).(string); ok {
		return value
	}
	return ""
}

func normalizeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '.', '_', '-', ':':
			continue
		default:
			return ""
		}
	}
	return value
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return "REQ-" + strings.ToUpper(hex.EncodeToString(b[:]))
	}
	return fmt.Sprintf("REQ-%d", time.Now().UnixNano())
}

func (s *Server) handleClientError(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	remote := s.clientIP(r)
	if s.clientErrors != nil {
		allowed, logLimited := s.clientErrors.allowWithLimitLog(remote, time.Now())
		if !allowed {
			retryAfter := s.clientErrors.retryAfterSeconds()
			if logLimited {
				log.Printf("[CLIENT-ERROR-LIMITED] request_id=%q remote=%q retry_after=%d",
					requestIDFromContext(r.Context()),
					remote,
					retryAfter)
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			writeError(w, http.StatusTooManyRequests, errors.New("too many client error reports; try again later"))
			return
		}
	}
	raw, err := readLimited(r.Body, 8*1024)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	var payload clientErrorReport
	if err := json.Unmarshal(raw, &payload); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	log.Printf("[CLIENT-ERROR] request_id=%q remote=%q fingerprint=%s source=%q path=%q asset_signature=%q resource_url=%q message=%q component_stack=%q detail=%q ua=%q occurred_at=%q",
		requestIDFromContext(r.Context()),
		remote,
		clientErrorFingerprint(payload),
		clientErrorLogField(payload.Source, 80),
		clientErrorLogField(payload.Path, 180),
		clientErrorLogField(payload.AssetSignature, 600),
		clientErrorLogField(payload.ResourceURL, 1000),
		clientErrorLogField(payload.Message, 300),
		clientErrorLogField(payload.ComponentStack, 900),
		clientErrorLogField(payload.Stack, 1200),
		clientErrorLogField(payload.UserAgent, 180),
		clientErrorLogField(payload.OccurredAt, 80))
	w.WriteHeader(http.StatusNoContent)
}

func cleanLogString(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= 32 && r != 127) {
			return r
		}
		return -1
	}, value)
}

func truncateLogField(value string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(value) <= max {
		return value
	}
	var b strings.Builder
	b.Grow(max + 3)
	for _, r := range value {
		if b.Len()+len(string(r)) > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + "..."
}

func clientErrorLogField(value string, max int) string {
	return truncateLogField(singleLineLogField(cleanLogString(value)), max)
}

func singleLineLogField(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.ReplaceAll(value, "\n", `\n`)
	return strings.ReplaceAll(value, "\t", `\t`)
}

func clientErrorFingerprint(report clientErrorReport) string {
	h := sha256.New()
	for _, part := range []string{
		report.Source,
		report.Path,
		report.AssetSignature,
		report.ResourceURL,
		report.Message,
		report.ComponentStack,
	} {
		_, _ = io.WriteString(h, singleLineLogField(cleanLogString(part)))
		_, _ = h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}
