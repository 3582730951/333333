package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// maxBridgeErrorCapture bounds the error-shape buffer. Pool error bodies are small
// and fixed; the cap only guards against an unexpected large writer.
const maxBridgeErrorCapture = 64 << 10

// anthropicErrorRecorder captures a pool error response so its shape can be converted
// for a Claude client. It deliberately reuses the existing writeError/writeJSON policy
// (status normalization, safe text, request id, diagnostic class) instead of restating
// it, so the bridge cannot drift from the pool's public error contract.
type anthropicErrorRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newAnthropicErrorRecorder() *anthropicErrorRecorder {
	return &anthropicErrorRecorder{header: make(http.Header)}
}

func (r *anthropicErrorRecorder) Header() http.Header { return r.header }

func (r *anthropicErrorRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
}

func (r *anthropicErrorRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	if remaining := maxBridgeErrorCapture - r.body.Len(); remaining > 0 {
		if len(p) > remaining {
			r.body.Write(p[:remaining])
		} else {
			r.body.Write(p)
		}
	}
	return len(p), nil
}

// writeAnthropicBridgeError runs the pool's normal error writer, then forwards the
// result to the downstream Claude client as an Anthropic envelope.
func writeAnthropicBridgeError(downstream http.ResponseWriter, status int, err error) {
	recorder := newAnthropicErrorRecorder()
	writeError(recorder, status, err)

	dst := downstream.Header()
	for key, values := range recorder.header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	dst.Set("Content-Type", "application/json")
	dst.Del("Content-Length")

	finalStatus := recorder.status
	if finalStatus == 0 {
		finalStatus = status
	}
	downstream.WriteHeader(finalStatus)
	_, _ = downstream.Write(responsesErrorToAnthropicEnvelope(finalStatus, recorder.body.Bytes()))
}

// anthropicErrorTypeForStatus maps an HTTP status onto Anthropic's documented
// error-type vocabulary. Claude Code switches on this value, so an unknown string
// degrades its message to a generic failure.
func anthropicErrorTypeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

// knownAnthropicErrorTypes is the set a Claude client understands. An upstream type
// is preserved only when it is already one of these; otherwise the status-derived
// type is authoritative.
var knownAnthropicErrorTypes = map[string]bool{
	"invalid_request_error": true,
	"authentication_error":  true,
	"permission_error":      true,
	"not_found_error":       true,
	"request_too_large":     true,
	"rate_limit_error":      true,
	"api_error":             true,
	"overloaded_error":      true,
	"billing_error":         true,
}

// responsesErrorToAnthropicEnvelope rewrites an already leak-filtered Codex/Responses
// error body into Anthropic's error envelope. It is shape-only: the message text is
// carried across untouched, and no upstream detail is newly exposed.
//
// A body that is already an Anthropic envelope is returned unchanged, so paths that
// emit one before reaching the bridge are not double-wrapped.
func responsesErrorToAnthropicEnvelope(status int, body []byte) []byte {
	fallbackType := anthropicErrorTypeForStatus(status)
	message, code := "", ""

	var root map[string]interface{}
	if json.Unmarshal(body, &root) == nil && root != nil {
		if topType, _ := root["type"].(string); topType == "error" {
			if _, hasError := root["error"]; hasError {
				return body
			}
		}
		errorType := ""
		switch nested := root["error"].(type) {
		case map[string]interface{}:
			message, _ = nested["message"].(string)
			errorType, _ = nested["type"].(string)
			code, _ = nested["code"].(string)
		case string:
			message = nested
		}
		if message == "" {
			// Some gateways and proxies report only a bare detail/message field.
			if detail, ok := root["detail"].(string); ok {
				message = detail
			} else if direct, ok := root["message"].(string); ok {
				message = direct
			}
		}
		if knownAnthropicErrorTypes[strings.TrimSpace(errorType)] {
			fallbackType = strings.TrimSpace(errorType)
		}
	} else if text := strings.TrimSpace(string(body)); text != "" && !strings.HasPrefix(text, "{") {
		// A non-JSON upstream failure (HTML error page, plain text). Only keep it when
		// it is short enough to be a real message rather than a whole document.
		if len(text) <= 480 {
			message = text
		}
	}

	if strings.TrimSpace(message) == "" {
		message, _ = safeClientError(status)
	}
	errorObject := map[string]interface{}{"type": fallbackType, "message": message}
	if strings.TrimSpace(code) != "" {
		errorObject["code"] = code
	}
	out, err := json.Marshal(map[string]interface{}{"type": "error", "error": errorObject})
	if err != nil {
		return body
	}
	return out
}
