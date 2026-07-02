package api

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestClaudeMessagesStreamSurvivesDelayedTail is the full end-to-end guard for the
// user's bug, exercised through the REAL relay handler (handleMessages → streamSSE →
// leakfilter → upstream guard), not just the upstream client in isolation. The
// upstream flushes the SSE head, then streams the terminating message_stop frame only
// AFTER a delay — i.e. after the relay has already returned from doClaude. The premature
// `defer cancel()` bug cancelled the request context at that point, so the downstream
// received the head but never the delayed tail: a truncated SSE that Claude Code reports
// as "API Error: Failed to parse JSON" (missing message_stop) or, when nothing buffered,
// "empty or malformed response (HTTP 200)". The relayed stream must arrive complete.
func TestClaudeMessagesStreamSurvivesDelayedTail(t *testing.T) {
	const head = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\"}}\n\n"
	const delta = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	const tail = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, head)
		if fl != nil {
			fl.Flush()
		}
		// The rest is produced only after the relay has returned from doClaude and is
		// streaming the body — exactly when the old defer-cancel fired.
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, delta)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(150 * time.Millisecond)
		_, _ = io.WriteString(w, tail)
		if fl != nil {
			fl.Flush()
		}
	})
	h.importAccount(t, "claude-a", "", "sk-ant-oat-test")

	reqBody := `{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading relayed SSE failed (premature cancel truncated the stream?): %v", err)
	}
	for _, want := range []string{"message_start", "content_block_delta", "message_stop"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("relayed SSE truncated — missing %q (Failed-to-parse-JSON repro):\n%q", want, got)
		}
	}
}
