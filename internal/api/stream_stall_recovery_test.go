package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const faultInjectionStallTimeout = 35 * time.Millisecond

type stalledUpstreamCapture struct {
	body    string
	session string
	thread  string
	turn    string
}

func serveWithStallRecovery(t *testing.T, h *testHarness, path, body string, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	base := withStreamStallRecovery(req.Context(), faultInjectionStallTimeout)
	ctx, cancel := context.WithTimeout(base, 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	recorder := httptest.NewRecorder()
	h.app.ServeHTTP(recorder, req)
	return recorder
}

func waitForUpstreamCancellation(t *testing.T, cancelled <-chan bool, name string) {
	t.Helper()
	select {
	case ok := <-cancelled:
		if !ok {
			t.Fatalf("%s was not cancelled after the relay declared it stalled", name)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s cancellation", name)
	}
}

func blockUntilUpstreamCancelled(r *http.Request, result chan<- bool) {
	select {
	case <-r.Context().Done():
		result <- true
	case <-time.After(time.Second):
		result <- false
	}
}

func TestCodexStreamStallContinuesOnceWithMappedIdentity(t *testing.T) {
	const downstreamSession = "downstream-session-must-not-reach-upstream"
	firstCancelled := make(chan bool, 1)
	var mu sync.Mutex
	var captures []stalledUpstreamCapture

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		captures = append(captures, stalledUpstreamCapture{
			body:    string(raw),
			session: r.Header.Get("Session-Id"),
			thread:  r.Header.Get("Thread-Id"),
			turn:    turnIDFromHeader(r.Header.Get("X-Codex-Turn-Metadata")),
		})
		call := len(captures)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			w.Header().Set("X-Codex-Turn-State", "opaque-stall-state")
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stall-source\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			blockUntilUpstreamCancelled(r, firstCancelled)
			return
		}
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stall-final\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"continued successfully\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-stall-final\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"completed\",\"output\":[]}}\n\n"+
			"data: [DONE]\n\n")
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "stall-codex", "upstream-stall-codex", "access-stall-codex")

	headers := http.Header{}
	headers.Set("Session-Id", downstreamSession)
	headers.Set("Thread-Id", downstreamSession)
	recorder := serveWithStallRecovery(t, h, "/v1/responses",
		`{"model":"gpt","stream":true,"session_id":"`+downstreamSession+`","thread_id":"`+downstreamSession+`","input":"start a long goal"}`,
		headers,
	)
	waitForUpstreamCancellation(t, firstCancelled, "initial Codex stream")

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "response.completed") || !strings.Contains(body, "continued successfully") || strings.Contains(body, "response.failed") {
		t.Fatalf("stall recovery status=%d body=%s", recorder.Code, body)
	}

	mu.Lock()
	got := append([]stalledUpstreamCapture(nil), captures...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("upstream calls=%d, want initial request plus one continuation", len(got))
	}
	for index, capture := range got {
		requireMappedUUIDv7(t, capture.session)
		requireMappedUUIDv7(t, capture.turn)
		if capture.thread != capture.session || capture.session == downstreamSession || strings.Contains(capture.body, downstreamSession) {
			t.Fatalf("downstream identity leaked on upstream call %d: %+v", index+1, capture)
		}
	}
	if got[0].session != got[1].session || got[0].turn == got[1].turn {
		t.Fatalf("continuation identity drift: first=%+v continuation=%+v", got[0], got[1])
	}
	var continuation map[string]interface{}
	if err := json.Unmarshal([]byte(got[1].body), &continuation); err != nil {
		t.Fatalf("decode continuation body: %v (%s)", err, got[1].body)
	}
	if continuation["previous_response_id"] != "resp-stall-source" || !strings.Contains(got[1].body, h.app.autoContinueText(context.Background())) || strings.Contains(got[1].body, "start a long goal") {
		t.Fatalf("continuation did not use only native upstream state: %s", got[1].body)
	}
}

func TestCodexContinuationStallEmitsOneGenericTerminal(t *testing.T) {
	const downstreamSession = "downstream-double-stall-session"
	initialCancelled := make(chan bool, 1)
	continuationCancelled := make(chan bool, 1)
	var calls atomic.Int32
	var mu sync.Mutex
	var captures []stalledUpstreamCapture

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		captures = append(captures, stalledUpstreamCapture{
			body:    string(raw),
			session: r.Header.Get("Session-Id"),
			thread:  r.Header.Get("Thread-Id"),
			turn:    turnIDFromHeader(r.Header.Get("X-Codex-Turn-Metadata")),
		})
		mu.Unlock()
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-double-stall\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			blockUntilUpstreamCancelled(r, initialCancelled)
			return
		}
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-double-stall-continuation\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"partial continuation\"}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		blockUntilUpstreamCancelled(r, continuationCancelled)
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "double-stall-codex", "upstream-double-stall", "access-double-stall")

	headers := http.Header{}
	headers.Set("Session-Id", downstreamSession)
	headers.Set("Thread-Id", downstreamSession)
	recorder := serveWithStallRecovery(t, h, "/v1/responses", `{"model":"gpt","stream":true,"input":"work"}`, headers)
	waitForUpstreamCancellation(t, initialCancelled, "initial Codex stream")
	waitForUpstreamCancellation(t, continuationCancelled, "Codex continuation stream")

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || calls.Load() != 2 || strings.Count(body, "event: response.failed\n") != 1 || !strings.Contains(body, `"code":"server_error"`) || !strings.Contains(body, publicRetryMessage) || strings.Contains(body, "response.completed") {
		t.Fatalf("double-stall result status=%d calls=%d body=%s", recorder.Code, calls.Load(), body)
	}
	if strings.Contains(body, downstreamSession) || strings.Contains(body, h.app.autoContinueText(context.Background())) {
		t.Fatalf("private session or continuation instruction leaked downstream: %s", body)
	}

	mu.Lock()
	got := append([]stalledUpstreamCapture(nil), captures...)
	mu.Unlock()
	if len(got) != 2 || got[0].session == "" || got[0].session != got[1].session || got[0].turn == got[1].turn {
		t.Fatalf("unexpected continuation identity: %+v", got)
	}
	for _, capture := range got {
		if strings.Contains(capture.body, downstreamSession) || capture.session == downstreamSession || capture.thread != capture.session {
			t.Fatalf("downstream identity leaked upstream: %+v", capture)
		}
	}
}

func TestCodexStallWithoutRecoverableStateDoesNotContinue(t *testing.T) {
	tests := []struct {
		name   string
		frames string
	}{
		{
			name:   "missing_response_id",
			frames: "event: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n",
		},
		{
			name: "pending_client_tool_call",
			frames: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-pending-stall\",\"object\":\"response\",\"model\":\"gpt\",\"status\":\"in_progress\"}}\n\n" +
				"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"custom_tool_call\",\"call_id\":\"call-pending-stall\",\"name\":\"apply_patch\",\"input\":\"{}\"}}\n\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cancelled := make(chan bool, 1)
			var calls atomic.Int32
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.frames)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				blockUntilUpstreamCancelled(r, cancelled)
			})
			enableCodexSessionMappingForTest(h)
			h.importAccount(t, "uncontinuable-"+test.name, "upstream-uncontinuable", "access-uncontinuable")

			recorder := serveWithStallRecovery(t, h, "/v1/responses", `{"model":"gpt","stream":true,"input":"work"}`, nil)
			waitForUpstreamCancellation(t, cancelled, "uncontinuable Codex stream")
			body := recorder.Body.String()
			if recorder.Code != http.StatusOK || calls.Load() != 1 || strings.Count(body, "event: response.failed\n") != 1 || !strings.Contains(body, publicRetryMessage) || strings.Contains(body, "response.completed") {
				t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls.Load(), body)
			}
		})
	}
}

func TestCodexStatelessStallIgnoresSafetyFramesAndContinuesOnSameIdentity(t *testing.T) {
	const downstreamSession = "downstream-stateless-session-must-not-reach-upstream"
	initialCancelled := make(chan bool, 1)
	var calls atomic.Int32
	var mu sync.Mutex
	var captures []stalledUpstreamCapture
	var authHeaders []string

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		captures = append(captures, stalledUpstreamCapture{
			body: string(raw), session: r.Header.Get("Session-Id"),
			thread: r.Header.Get("Thread-Id"), turn: turnIDFromHeader(r.Header.Get("X-Codex-Turn-Metadata")),
		})
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		mu.Unlock()
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stateless-stall\",\"model\":\"gpt\"},\"safety_buffering\":{\"reasons\":[\"user_risk\"]}}\n\n")
			flusher, _ := w.(http.Flusher)
			flusher.Flush()
			ticker := time.NewTicker(8 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-r.Context().Done():
					initialCancelled <- true
					return
				case <-ticker.C:
					_, _ = io.WriteString(w, "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"safety_buffering\":{\"reasons\":[\"user_risk\"]}}\n\n")
					flusher.Flush()
				}
			}
		}
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stateless-final\",\"model\":\"gpt\"}}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"continued statelessly\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-stateless-final\",\"model\":\"gpt\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
	})
	h.importAccount(t, "stateless-stall-a", "upstream-stateless-a", "access-stateless-a")
	h.importAccount(t, "stateless-stall-b", "upstream-stateless-b", "access-stateless-b")

	headers := http.Header{}
	headers.Set("Session-Id", downstreamSession)
	headers.Set("Thread-Id", downstreamSession)
	recorder := serveWithStallRecovery(t, h, "/v1/responses", `{"model":"gpt","stream":true,"session_id":"`+downstreamSession+`","thread_id":"`+downstreamSession+`","input":"long goal"}`, headers)
	waitForUpstreamCancellation(t, initialCancelled, "stateless Codex stream")

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || calls.Load() != 2 || !strings.Contains(body, "response.completed") || !strings.Contains(body, "continued statelessly") || strings.Contains(body, "response.failed") || strings.Contains(strings.ToLower(body), "safety") {
		t.Fatalf("stateless recovery status=%d calls=%d body=%s", recorder.Code, calls.Load(), body)
	}
	mu.Lock()
	got := append([]stalledUpstreamCapture(nil), captures...)
	gotAuth := append([]string(nil), authHeaders...)
	mu.Unlock()
	if len(got) != 2 || len(gotAuth) != 2 || gotAuth[0] == "" || gotAuth[0] != gotAuth[1] {
		t.Fatalf("continuation changed account: captures=%+v auth=%v", got, gotAuth)
	}
	if got[0].session == "" || got[0].session != got[1].session || got[0].thread != got[0].session || got[1].thread != got[1].session {
		t.Fatalf("stateless continuation identity drift: %+v", got)
	}
	for _, capture := range got {
		if capture.session == downstreamSession || strings.Contains(capture.body, downstreamSession) {
			t.Fatalf("downstream session leaked upstream: %+v", capture)
		}
	}
	if !strings.Contains(got[1].body, h.app.autoContinueText(context.Background())) || strings.Contains(got[1].body, "previous_response_id") {
		t.Fatalf("stateless continuation body is not self-contained: %s", got[1].body)
	}
}

func TestCodexStatelessContinuationSecondStallEmitsOneGenericTerminal(t *testing.T) {
	initialCancelled := make(chan bool, 1)
	continuationCancelled := make(chan bool, 1)
	var calls atomic.Int32
	var authMu sync.Mutex
	var authHeaders []string

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		authMu.Lock()
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		authMu.Unlock()
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stateless-double\",\"model\":\"gpt\"}}\n\n")
			w.(http.Flusher).Flush()
			blockUntilUpstreamCancelled(r, initialCancelled)
			return
		}
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stateless-double-cont\",\"model\":\"gpt\"}}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"private partial\"}\n\n")
		w.(http.Flusher).Flush()
		blockUntilUpstreamCancelled(r, continuationCancelled)
	})
	h.importAccount(t, "stateless-double-a", "upstream-stateless-double-a", "access-stateless-double-a")
	h.importAccount(t, "stateless-double-b", "upstream-stateless-double-b", "access-stateless-double-b")

	recorder := serveWithStallRecovery(t, h, "/v1/responses", `{"model":"gpt","stream":true,"input":"work"}`, nil)
	waitForUpstreamCancellation(t, initialCancelled, "initial stateless Codex stream")
	waitForUpstreamCancellation(t, continuationCancelled, "stateless Codex continuation")
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || calls.Load() != 2 || strings.Count(body, "event: response.failed\n") != 1 || !strings.Contains(body, publicRetryMessage) || strings.Contains(body, "response.completed") {
		t.Fatalf("stateless double-stall status=%d calls=%d body=%s", recorder.Code, calls.Load(), body)
	}
	for _, forbidden := range []string{"upstream stream stalled", "account", "quota", "usage limit", "safety"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("stateless failure leaked %q: %s", forbidden, body)
		}
	}
	authMu.Lock()
	defer authMu.Unlock()
	if len(authHeaders) != 2 || authHeaders[0] == "" || authHeaders[0] != authHeaders[1] {
		t.Fatalf("continuation changed account: %v", authHeaders)
	}
}

func TestCodexStatelessTruncatedToolCallIsNotReissued(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-tool-truncated\",\"model\":\"gpt\"}}\n\n"+
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call-must-not-replay\",\"name\":\"apply_patch\",\"arguments\":\"{}\"}}\n\n")
	})
	h.importAccount(t, "stateless-tool", "upstream-stateless-tool", "access-stateless-tool")

	recorder := serveWithStallRecovery(t, h, "/v1/responses", `{"model":"gpt","stream":true,"input":"use a tool"}`, nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || calls.Load() != 1 || strings.Count(body, "event: response.failed\n") != 1 || !strings.Contains(body, publicRetryMessage) {
		t.Fatalf("truncated tool result status=%d calls=%d body=%s", recorder.Code, calls.Load(), body)
	}
}

func TestCodexStatelessContinuationErrorIsNeverExposed(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-before-private-error\",\"model\":\"gpt\"}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"service temporarily unavailable for account secret-account-id; quota remaining 2%; safety check"}}`)
	})
	h.importAccount(t, "stateless-private-error", "upstream-stateless-private-error", "access-stateless-private-error")

	recorder := serveWithStallRecovery(t, h, "/v1/responses", `{"model":"gpt","stream":true,"input":"work"}`, nil)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || calls.Load() != 2 || strings.Count(body, "event: response.failed\n") != 1 || !strings.Contains(body, publicRetryMessage) {
		t.Fatalf("continuation error result status=%d calls=%d body=%s", recorder.Code, calls.Load(), body)
	}
	for _, forbidden := range []string{"temporarily unavailable", "secret-account-id", "quota", "2%", "safety"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("private upstream detail %q leaked: %s", forbidden, body)
		}
	}
}

func TestClaudeStreamStallContinuesAndOffsetsBlocks(t *testing.T) {
	const downstreamSession = "downstream-claude-stall-session"
	initialCancelled := make(chan bool, 1)
	var calls atomic.Int32
	var mu sync.Mutex
	var bodies []string
	var sessions []string

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		sessions = append(sessions, r.Header.Get("X-Claude-Code-Session-Id"))
		mu.Unlock()
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-stall-source\",\"role\":\"assistant\"}}\n\n"+
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"first half\"}}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			blockUntilUpstreamCancelled(r, initialCancelled)
			return
		}
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-stall-final\",\"role\":\"assistant\"}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" second half\"}}\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	h.importAccount(t, "stall-claude", "", "sk-ant-oat-stall")

	headers := http.Header{}
	headers.Set("Anthropic-Version", "2023-06-01")
	headers.Set("X-Claude-Code-Session-Id", downstreamSession)
	recorder := serveWithStallRecovery(t, h, "/v1/messages", `{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"long task"}]}`, headers)
	waitForUpstreamCancellation(t, initialCancelled, "initial Claude stream")

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || calls.Load() != 2 || strings.Count(body, "event: message_start\n") != 1 || strings.Count(body, "event: message_stop\n") != 1 || !strings.Contains(body, `"index":1`) || !strings.Contains(body, "first half") || !strings.Contains(body, "second half") || strings.Contains(body, "event: error") {
		t.Fatalf("Claude stall recovery status=%d calls=%d body=%s", recorder.Code, calls.Load(), body)
	}

	mu.Lock()
	gotBodies := append([]string(nil), bodies...)
	gotSessions := append([]string(nil), sessions...)
	mu.Unlock()
	if len(gotBodies) != 2 || len(gotSessions) != 2 || gotSessions[0] == "" || gotSessions[0] != gotSessions[1] || gotSessions[0] == downstreamSession {
		t.Fatalf("Claude continuation identity drift: sessions=%v bodies=%v", gotSessions, gotBodies)
	}
	if strings.Contains(gotBodies[0], downstreamSession) || strings.Contains(gotBodies[1], downstreamSession) || !strings.Contains(gotBodies[1], "first half") || !strings.Contains(gotBodies[1], h.app.autoContinueText(context.Background())) {
		t.Fatalf("Claude continuation context mismatch: %s", gotBodies[1])
	}
}

func TestClaudeContinuationStallClosesOffsetBlockOnce(t *testing.T) {
	initialCancelled := make(chan bool, 1)
	continuationCancelled := make(chan bool, 1)
	var calls atomic.Int32

	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if call == 1 {
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-double-stall\",\"role\":\"assistant\"}}\n\n"+
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"first\"}}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			blockUntilUpstreamCancelled(r, initialCancelled)
			return
		}
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-double-stall-cont\",\"role\":\"assistant\"}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"second\"}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		blockUntilUpstreamCancelled(r, continuationCancelled)
	})
	h.importAccount(t, "double-stall-claude", "", "sk-ant-oat-double-stall")

	headers := http.Header{}
	headers.Set("Anthropic-Version", "2023-06-01")
	recorder := serveWithStallRecovery(t, h, "/v1/messages", `{"model":"claude-x","stream":true,"messages":[{"role":"user","content":"long task"}]}`, headers)
	waitForUpstreamCancellation(t, initialCancelled, "initial Claude stream")
	waitForUpstreamCancellation(t, continuationCancelled, "Claude continuation stream")

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || calls.Load() != 2 || strings.Count(body, "event: error\n") != 1 || strings.Count(body, "event: message_stop\n") != 1 || strings.Count(body, `"type":"content_block_stop"`) != 2 || strings.Count(body, `"index":0`) < 3 || strings.Count(body, `"index":1`) < 3 || !strings.Contains(body, publicRetryMessage) {
		t.Fatalf("Claude double-stall status=%d calls=%d body=%s", recorder.Code, calls.Load(), body)
	}
	for _, forbidden := range []string{"upstream stream stalled", "usage limit", "safety", "account"} {
		if strings.Contains(strings.ToLower(body), forbidden) {
			t.Fatalf("Claude failure leaked %q: %s", forbidden, body)
		}
	}
}
