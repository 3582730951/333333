package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/bodysource"
	"codex-account-pool/internal/leakfilter"
)

func TestCodexModelAtCapacityOnlyMatchesErrorEnvelopes(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"server overload code", 503, `{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity. Please try a different model."}}`, true},
		{"capacity code in response", 200, `{"response":{"error":{"code":"model_capacity_exceeded","message":"busy"}}}`, true},
		{"plain capacity error", 503, `{"message":"Selected model is at capacity. Please try a different model."}`, true},
		{"assistant text", 200, `{"output_text":"Selected model is at capacity. Please try a different model."}`, false},
		{"other error", 503, `{"error":{"code":"context_length_exceeded","message":"Selected model is at capacity because context is too large"}}`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := leakfilter.IsModelCapacityError(test.status, []byte(test.body)); got != test.want {
				t.Fatalf("codexModelAtCapacity(%d, %s)=%v, want %v", test.status, test.body, got, test.want)
			}
		})
	}
}

func TestCodexModelCapacityRetriesSameLeaseBeforeFailover(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity. Please try a different model."}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"response-capacity-recovered","status":"completed","model":"gpt-5.6-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"recovered after capacity"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	})
	h.importAccount(t, "capacity-account", "capacity-upstream", "capacity-token")
	response, err := http.Post(h.pool.URL+"/v1/responses", "application/json", bytes.NewBufferString(`{"model":"gpt-5.6-sol","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "recovered after capacity") {
		t.Fatalf("capacity retry status=%d calls=%d body=%s err=%v", response.StatusCode, calls.Load(), body, readErr)
	}
	if calls.Load() != 2 {
		t.Fatalf("capacity response made %d upstream calls, want exactly 2", calls.Load())
	}
}

func TestCodexModelCapacitySSEErrorRetriesBeforeCommit(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "event: response.failed\n"+
				`data: {"type":"response.failed","response":{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity. Please try a different model."}}}`+"\n\n")
			return
		}
		_, _ = io.WriteString(w, "event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","delta":"recovered from busy model"}`+"\n\n"+
			"event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"response-sse-recovered","status":"completed","model":"gpt-5.6-sol","output":[],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`+"\n\n")
	})
	h.importAccount(t, "capacity-sse-account", "capacity-sse-upstream", "capacity-sse-token")
	response, err := http.Post(h.pool.URL+"/v1/responses", "application/json", bytes.NewBufferString(`{"model":"gpt-5.6-sol","stream":true,"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "recovered from busy model") {
		t.Fatalf("capacity SSE retry status=%d calls=%d body=%s err=%v", response.StatusCode, calls.Load(), body, readErr)
	}
	if calls.Load() != 2 {
		t.Fatalf("capacity SSE response made %d upstream calls, want exactly 2", calls.Load())
	}
}

func TestCodexModelCapacityFinalFailureUsesClientRetryEnvelope(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeModelCapacityFailure(recorder, stream)
			body := recorder.Body.String()
			if !strings.Contains(body, `"message":"upstream failed"`) || !strings.Contains(body, `"server_error"`) {
				t.Fatalf("capacity failure envelope=%s", body)
			}
			if stream && recorder.Code != http.StatusOK {
				t.Fatalf("stream capacity terminal status=%d", recorder.Code)
			}
			if !stream && recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("json capacity terminal status=%d", recorder.Code)
			}
		})
	}
}

func TestCodexModelCapacityPreservesMappedEpoch(t *testing.T) {
	body := []byte(`{"error":{"code":"server_is_overloaded","message":"Selected model is at capacity. Please try a different model."}}`)
	for _, status := range []int{200, 429, 500, 503} {
		if codexMappedSessionRiskError(status, body) || codexMappedSessionRotationRequired(status, nil, body, false, true) {
			t.Fatalf("temporary model capacity retired a mapped session for status %d", status)
		}
	}
}

func TestCodexModelCapacityRewriteWithoutOptionalLeakScrubbing(t *testing.T) {
	const frame = "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Selected model is at capacity. Please try a different model.\"}}}\n\n"
	recorder := httptest.NewRecorder()
	if err := newRuleSSECopyWithHeartbeat(context.Background(), recorder, strings.NewReader(frame), nil, false, nil, "codex", 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorder.Body.String(), `"message":"upstream failed"`) {
		t.Fatalf("capacity message escaped the mandatory rewrite: %s", recorder.Body.String())
	}
	var continued bytes.Buffer
	writer := newScrubbingFrameWriter(&continued, false, nil, "codex")
	if _, err := writer.Write([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(continued.String(), `"message":"upstream failed"`) {
		t.Fatalf("continued stream lost capacity rewrite: %s", continued.String())
	}
}

func TestCodexModelCapacityWebSocketEnvelopeSurvivesBridge(t *testing.T) {
	for _, stream := range []bool{false, true} {
		conn := &recordingWebSocketWriter{}
		writer := newResponsesWebSocketWriter(context.Background(), conn, nil, bodysource.CaptureOptions{}, nil)
		writeModelCapacityFailure(writer, stream)
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		body := string(conn.lastMessage())
		if !strings.Contains(body, `"message":"upstream failed"`) || !strings.Contains(body, `"code":"server_error"`) {
			t.Fatalf("stream=%v WebSocket lost client-retry envelope: %s", stream, body)
		}
	}
}
