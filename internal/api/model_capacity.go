package api

import (
	"codex-account-pool/internal/leakfilter"
	"net/http"
)

// Inspect only error envelopes (or a plain HTTP error), never assistant output.
func codexModelAtCapacity(status int, body []byte) bool {
	return leakfilter.IsModelCapacityError(status, body)
}

func writeModelCapacityFailure(w http.ResponseWriter, stream bool) {
	payload := map[string]interface{}{
		"error": map[string]string{
			"type":    "server_error",
			"code":    "server_error",
			"message": leakfilter.ModelCapacityPublicErrorMessage,
		},
	}
	if !stream {
		writeJSON(w, http.StatusServiceUnavailable, payload)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = writeSSEEvent(w, "response.failed", map[string]interface{}{"response": map[string]interface{}{
		"status": "failed", "error": payload["error"],
	}})
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flushWriter(w)
}
