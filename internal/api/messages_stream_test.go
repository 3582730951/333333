package api

import (
	"bufio"
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

func TestClaudeMessagesStreamFlushesFirstFrameBeforeTail(t *testing.T) {
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
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, delta)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(100 * time.Millisecond)
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

	firstLine := make(chan string, 1)
	readErr := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		if err != nil {
			readErr <- err
			return
		}
		firstLine <- line
	}()

	select {
	case line := <-firstLine:
		if !strings.Contains(line, "message_start") {
			t.Fatalf("first streamed line = %q", line)
		}
	case err := <-readErr:
		t.Fatalf("reading first streamed line failed: %v", err)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("first Claude SSE frame was buffered until the delayed tail instead of being streamed")
	}
}

func TestCodexResponsesStreamFlushesFirstFrameBeforeTail(t *testing.T) {
	const head = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"first\"}\n\n"
	const tail = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\ndata: [DONE]\n\n"
	releaseTail := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseTail)
		}
	}()

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, head)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		<-releaseTail
		_, _ = io.WriteString(w, tail)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	})
	h.importAccount(t, "codex-a", "upstream-a", "access-a")

	type result struct {
		resp *http.Response
		err  error
	}
	response := make(chan result, 1)
	go func() {
		resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"input":[{"role":"user","content":"hi"}]}`))
		response <- result{resp: resp, err: err}
	}()

	var resp *http.Response
	select {
	case got := <-response:
		if got.err != nil {
			t.Fatal(got.err)
		}
		resp = got.resp
	case <-time.After(500 * time.Millisecond):
		close(releaseTail)
		released = true
		got := <-response
		if got.resp != nil {
			got.resp.Body.Close()
		}
		t.Fatal("Codex response headers were held until the complete SSE tail instead of being streamed after the bounded prefix")
	}
	defer resp.Body.Close()

	firstLine := make(chan string, 1)
	readErr := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(resp.Body).ReadString('\n')
		if err != nil {
			readErr <- err
			return
		}
		firstLine <- line
	}()
	select {
	case line := <-firstLine:
		if !strings.Contains(line, "response.output_text.delta") {
			t.Fatalf("first streamed line = %q", line)
		}
	case err := <-readErr:
		t.Fatalf("reading first Codex SSE line failed: %v", err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first Codex SSE frame was not flushed while the upstream tail was pending")
	}
	close(releaseTail)
	released = true
}

func TestCodexResponsesStreamFlushesHeadersBeforeFirstEvent(t *testing.T) {
	releaseFirstEvent := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseFirstEvent)
		}
	}()

	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-releaseFirstEvent
		_, _ = io.WriteString(w, "event: response.created\n"+
			`data: {"type":"response.created","response":{"id":"resp_delayed","model":"gpt"}}`+"\n\n"+
			"event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp_delayed","status":"completed","model":"gpt","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n"+
			"data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	})
	h.importAccount(t, "codex-delayed", "upstream-delayed", "access-delayed")

	type result struct {
		resp *http.Response
		err  error
	}
	response := make(chan result, 1)
	go func() {
		resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"input":"wait for first event"}`))
		response <- result{resp: resp, err: err}
	}()

	var resp *http.Response
	select {
	case got := <-response:
		if got.err != nil {
			t.Fatal(got.err)
		}
		resp = got.resp
	case <-time.After(500 * time.Millisecond):
		close(releaseFirstEvent)
		released = true
		got := <-response
		if got.resp != nil {
			got.resp.Body.Close()
		}
		t.Fatal("Codex response headers were held while the upstream waited for its first SSE event")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	close(releaseFirstEvent)
	released = true
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
}

func TestCodexResponsesStreamIgnoresLegacyFullCaptureLimit(t *testing.T) {
	largeDelta := strings.Repeat("streaming-content-", 128)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n"+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\""+largeDelta+"\"}\n\n"+
			"event: response.completed\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"+
			"data: [DONE]\n\n")
	})
	// Under the removed full-stream capture path this successful response exceeded
	// the tiny cap and became a false 503. Bounded prefix probing must ignore it.
	h.app.cfg.StreamFailoverHoldMemoryBytes = 700
	h.app.cfg.StreamFailoverHoldDiskBytes = 0
	h.importAccount(t, "codex-a", "upstream-a", "access-a")

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"input":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), largeDelta) {
		t.Fatalf("successful oversized stream became status=%d body=%q", resp.StatusCode, body)
	}
}
