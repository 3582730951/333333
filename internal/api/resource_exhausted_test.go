package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResourceExhaustedResponseIsStableAndRetryable(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeResourceExhausted(recorder, "spool capacity reached")
	response := recorder.Result()
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "3" {
		t.Fatalf("status=%d retry-after=%q", response.StatusCode, response.Header.Get("Retry-After"))
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"type":"server_error"`) || !strings.Contains(body, `"code":"service_unavailable"`) {
		t.Fatalf("body=%s", body)
	}
}
