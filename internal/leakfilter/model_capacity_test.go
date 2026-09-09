package leakfilter

import (
	"net/http"
	"strings"
	"testing"
)

func TestModelCapacityErrorsUseClientRetryMessage(t *testing.T) {
	body := []byte(`{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity. Please try a different model."}}`)
	status, out, changed := NeutralizeErrorBody("codex", http.StatusServiceUnavailable, body)
	if !changed || status != http.StatusServiceUnavailable || !strings.Contains(string(out), `"message":"upstream failed"`) || !strings.Contains(string(out), `"type":"server_error"`) {
		t.Fatalf("capacity body was not rewritten for client retry: status=%d body=%s", status, out)
	}
	frame := "event: response.failed\ndata: " + string(body) + "\n\n"
	outFrame, ok := NeutralizeCodexRetryableFailureSSEFrame([]byte(frame))
	if !ok || !strings.Contains(string(outFrame), `"message":"upstream failed"`) {
		t.Fatalf("capacity SSE frame was not rewritten: %s", outFrame)
	}
}

func TestModelCapacityPhraseInAssistantTextIsUntouched(t *testing.T) {
	body := []byte(`{"output_text":"Selected model is at capacity. Please try a different model."}`)
	if IsModelCapacityError(http.StatusOK, body) {
		t.Fatal("assistant text was classified as a capacity error")
	}
	if out, changed := NeutralizeResponsesJSON(body); changed || string(out) != string(body) {
		t.Fatalf("assistant text changed: changed=%v body=%s", changed, out)
	}
}
