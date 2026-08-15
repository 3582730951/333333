package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	upstreamrules "codex-account-pool/internal/upstream_error_rules"
)

func TestWriteUpstreamHeadersDropsAccountScopedModelsETag(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	source := http.Header{
		"Content-Type":  []string{"text/event-stream"},
		"X-Models-Etag": []string{`W/"single-account"`},
		"Location":      []string{"https://internal-gw.example.com/v1/responses"},
	}
	destination := http.Header{}
	h.app.writeUpstreamHeaders(context.Background(), destination, source)
	if got := destination.Get("X-Models-Etag"); got != "" {
		t.Fatalf("account-scoped models ETag leaked downstream: %q", got)
	}
	if got := destination.Get("Location"); got != "" {
		t.Fatalf("upstream redirect leaked downstream: %q", got)
	}
	if got := destination.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("ordinary response header was dropped: %q", got)
	}
}

func TestWriteFilteredErrorAlwaysRedactsUpstreamTopology(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	if err := h.store.SetSetting(context.Background(), "leak_scrub", "false"); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.app.writeFilteredError(context.Background(), recorder, "codex", http.StatusBadGateway, http.Header{
		"Content-Type": {"application/json"},
		"Location":     {"https://redirect.internal.example/v1"},
	}, []byte(`{"error":{"message":"upstream https://internal-gw.example.com/v1 failed; dial tcp 10.0.0.5:18182"}}`), nil)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Location"); got != "" {
		t.Fatalf("Location leaked: %q", got)
	}
	for _, leaked := range []string{"internal-gw.example.com", "redirect.internal.example", "10.0.0.5"} {
		if strings.Contains(recorder.Body.String(), leaked) {
			t.Fatalf("topology %q leaked: %s", leaked, recorder.Body.String())
		}
	}
}

func TestProbeEarlyCodexSSEFailureDetectsFailedAfterCreated(t *testing.T) {
	stream := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_a\"}}\n\n" +
		"event: response.failed\n" +
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"You've hit your usage limit.\"}}}\n\n"
	prefix, failure, retry, err := probeEarlyCodexSSEFailure(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !retry {
		t.Fatalf("retry=false prefix=%q", prefix)
	}
	if failure.StatusCode != 429 {
		t.Fatalf("failure=%+v", failure)
	}
}

func TestProbeEarlyCodexSSEFailureRetriesEmptyCompletedResponse(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_empty"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_empty","status":"completed","output":[]}}` + "\n\n"
	prefix, failure, retry, err := probeEarlyCodexSSEFailure(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !retry || failure.StatusCode != http.StatusServiceUnavailable || failure.ErrorCode != "upstream_empty_response" || !failure.BuiltinRetryable {
		t.Fatalf("retry=%v failure=%+v prefix=%q", retry, failure, prefix)
	}
}

func TestProbeEarlyCodexSSEFailureKeepsUsageBearingEmptyOutput(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_usage"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_usage","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}}` + "\n\n"
	_, failure, retry, err := probeEarlyCodexSSEFailure(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if retry || failure.StatusCode != 0 {
		t.Fatalf("usage-bearing terminal was classified as failure: retry=%v failure=%+v", retry, failure)
	}
}

func TestProbeEarlyCodexSSEFailureDetectsWebSocketError(t *testing.T) {
	stream := "event: error\n" +
		`data: {"type":"error","error":{"type":"usage_limit_reached","message":"The usage limit has been reached"},"status_code":429,"headers":{"X-Codex-Primary-Used-Percent":"100"}}` + "\n\n" +
		"data: [DONE]\n\n"
	prefix, failure, retry, err := probeEarlyCodexSSEFailure(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !retry || failure.StatusCode != 429 || failure.Header.Get("X-Codex-Primary-Used-Percent") != "100" {
		t.Fatalf("retry=%v failure=%+v prefix=%q", retry, failure, prefix)
	}
}

func TestProbeEarlyCodexSSEFailureExposesClient400ToRules(t *testing.T) {
	stream := "event: error\n" +
		`data: {"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}` + "\n\n" +
		"data: [DONE]\n\n"
	prefix, failure, terminal, err := probeEarlyCodexSSEFailure(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !terminal || failure.StatusCode != 400 || failure.BuiltinRetryable || !strings.Contains(string(prefix), "ChatGPT account") {
		t.Fatalf("terminal=%v failure=%+v prefix=%q", terminal, failure, prefix)
	}
}

func TestCodexCreatedFrameDoesNotCommitContent(t *testing.T) {
	frame := []byte("event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_a\"}}\n\n")
	if codexSSEFrameCommitsContent(frame) {
		t.Fatalf("response.created should stay in the early retry buffer")
	}
}

func TestProbeEarlyCodexSSEFailureReleasesSafetyBufferingWithoutWaiting(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	type result struct {
		prefix []byte
		retry  bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		prefix, _, retry, err := probeEarlyCodexSSEFailure(reader)
		done <- result{prefix: prefix, retry: retry, err: err}
	}()
	frame := `data: {"type":"response.created","response":{"id":"resp_wait"},"safety_buffering":{"reasons":["user_risk"]}}` + "\n\n"
	if _, err := writer.Write([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.retry || !strings.Contains(string(got.prefix), "safety_buffering") {
			t.Fatalf("unexpected probe result: retry=%v prefix=%q", got.retry, got.prefix)
		}
	case <-time.After(time.Second):
		t.Fatal("safety_buffering remained stuck in the early retry probe")
	}
}

func TestProbeEarlyCodexSSEFailureReleasesBeforeFirstFrame(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	type result struct {
		prefix []byte
		relay  io.Reader
		err    error
	}
	done := make(chan result, 1)
	go func() {
		prefix, relay, _, _, err := probeEarlyCodexSSEFailureWithIdleRelease(reader, 10*time.Millisecond)
		done <- result{prefix: prefix, relay: relay, err: err}
	}()

	var got result
	select {
	case got = <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if len(got.prefix) != 0 || got.relay == nil {
			t.Fatalf("idle release prefix=%q relay=%v", got.prefix, got.relay)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("early SSE probe held the response while waiting for its first frame")
	}

	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_delayed"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_delayed","status":"completed"}}` + "\n\n"
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, stream)
		if closeErr := writer.Close(); err == nil {
			err = closeErr
		}
		writeDone <- err
	}()
	relayed, err := io.ReadAll(got.relay)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	if string(relayed) != stream {
		t.Fatalf("read-ahead relay changed delayed stream: got=%q want=%q", relayed, stream)
	}
}

func TestProbeEarlyClaudeSSEFailureDetectsErrorBeforeMessageStart(t *testing.T) {
	stream := "event: error\n" +
		"data: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"This account has hit its usage limit.\"}}\n\n"
	prefix, retry, err := probeEarlyClaudeSSEFailure(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !retry {
		t.Fatalf("expected retryable early Claude error, prefix=%q", prefix)
	}
}

func TestClaudeMessageStartCommitsStream(t *testing.T) {
	frame := []byte("event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_a\",\"role\":\"assistant\"}}\n\n")
	if !claudeSSEFrameCommitsContent(frame) {
		t.Fatal("message_start should commit the Claude stream so downstream sees incremental output")
	}
}

func TestClaudeContentBlockStartCommitsContent(t *testing.T) {
	frame := []byte("event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	if !claudeSSEFrameCommitsContent(frame) {
		t.Fatal("content_block_start should commit downstream-visible assistant content")
	}
}

func TestRuleSSECopyHidesSafetyBufferingWithoutBreakingLifecycle(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"},"safety_buffering":{"reasons":["user_risk"]}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hello","safety_buffering":{"reasons":["user_risk"]}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"},"safety_buffering":{"reasons":["user_risk"]}}` + "\n\n"
	rec := httptest.NewRecorder()
	rf := &responseRuleFilter{Mode: upstreamrules.DownstreamActionHideSafetyBuffering}
	if err := newRuleSSECopy(context.Background(), rec, strings.NewReader(stream), rf, false, nil, "codex"); err != nil {
		t.Fatal(err)
	}
	got := rec.Body.String()
	if strings.Contains(got, "safety_buffering") {
		t.Fatalf("safety_buffering leaked downstream: %s", got)
	}
	for _, want := range []string{"response.created", "response.output_text.delta", "hello", "response.completed", "resp_1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream lifecycle lost %q: %s", want, got)
		}
	}
}

func TestRuleSSECopyInterceptKeepsResponseCompleted(t *testing.T) {
	stream := `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","delta":"blocked notice"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}` + "\n\n"
	rec := httptest.NewRecorder()
	rf := &responseRuleFilter{Mode: upstreamrules.DownstreamActionIntercept, Keywords: []string{"blocked notice"}, CaseSensitive: true}
	if err := newRuleSSECopy(context.Background(), rec, strings.NewReader(stream), rf, false, nil, "codex"); err != nil {
		t.Fatal(err)
	}
	got := rec.Body.String()
	if strings.Contains(got, "blocked notice") {
		t.Fatalf("matching content leaked downstream: %s", got)
	}
	if !strings.Contains(got, "response.created") || !strings.Contains(got, "response.completed") {
		t.Fatalf("protocol lifecycle was interrupted: %s", got)
	}
}

func TestRuleSSECopySpoolsFramesBeyondPartialMemoryLimit(t *testing.T) {
	for _, test := range []struct {
		name   string
		suffix string
	}{
		{name: "unterminated"},
		{name: "complete", suffix: "\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := "data: " + strings.Repeat("x", streamLedgerMaxPartialFrame+1) + test.suffix
			recorder := httptest.NewRecorder()
			err := newRuleSSECopyWithHeartbeat(context.Background(), recorder, strings.NewReader(stream), nil, false, nil, "codex", 0)
			if err != nil {
				t.Fatalf("large frame error = %v", err)
			}
			if got := recorder.Body.String(); got != stream {
				t.Fatalf("large frame changed: got=%d want=%d", len(got), len(stream))
			}
		})
	}
}

func TestRuleSSECopyKeepsSafetyBufferedStreamAlive(t *testing.T) {
	reader, writer := io.Pipe()
	rec := httptest.NewRecorder()
	rf := &responseRuleFilter{Mode: upstreamrules.DownstreamActionHideSafetyBuffering}
	done := make(chan error, 1)
	go func() {
		done <- newRuleSSECopyWithHeartbeat(context.Background(), rec, reader, rf, false, nil, "codex", 10*time.Millisecond)
	}()
	go func() {
		_, _ = writer.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_wait"},"safety_buffering":{"reasons":["user_risk"]}}` + "\n\n"))
		time.Sleep(45 * time.Millisecond)
		_, _ = writer.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_wait","status":"completed"}}` + "\n\n"))
		_ = writer.Close()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat relay did not finish")
	}
	got := rec.Body.String()
	if strings.Contains(got, "safety_buffering") {
		t.Fatalf("safety_buffering leaked downstream: %s", got)
	}
	if !strings.Contains(got, "response.in_progress") {
		t.Fatalf("idle heartbeat missing: %s", got)
	}
	if !strings.Contains(got, "response.completed") {
		t.Fatalf("terminal event missing after heartbeat: %s", got)
	}
}

func TestRuleSSECopySafetyControlFramesDoNotMaskStall(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	rec := httptest.NewRecorder()
	rf := &responseRuleFilter{Mode: upstreamrules.DownstreamActionHideSafetyBuffering}
	ctx, cancel := context.WithCancel(withStreamStallRecovery(context.Background(), 45*time.Millisecond))
	defer cancel()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer writer.Close()
		frames := []string{
			`data: {"type":"response.created","response":{"id":"resp_wait"},"safety_buffering":{"reasons":["user_risk"]}}` + "\n\n",
			`data: {"type":"response.in_progress","safety_buffering":{"reasons":["user_risk"]}}` + "\n\n",
		}
		if _, err := writer.Write([]byte(frames[0])); err != nil {
			return
		}
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := writer.Write([]byte(frames[1])); err != nil {
					return
				}
			}
		}
	}()

	err := newRuleSSECopyWithHeartbeat(ctx, rec, reader, rf, false, nil, "codex", 10*time.Millisecond)
	if !errors.Is(err, errUpstreamStreamStalled) {
		t.Fatalf("control-only stream error = %v, want stall", err)
	}
	cancel()
	_ = reader.Close()
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("control-frame producer did not stop")
	}
	got := rec.Body.String()
	if strings.Contains(got, "safety_buffering") {
		t.Fatalf("safety metadata leaked downstream: %s", got)
	}
	if !strings.Contains(got, "response.in_progress") {
		t.Fatalf("downstream keepalive missing before recovery: %s", got)
	}
}

func TestSSEFrameAdvancesModelIgnoresOnlyControlTraffic(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		frame    string
		want     bool
	}{
		{name: "codex progress", provider: "codex", frame: `data: {"type":"response.in_progress"}` + "\n\n"},
		{name: "codex metadata", provider: "codex", frame: `data: {"type":"response.metadata"}` + "\n\n"},
		{name: "codex quota", provider: "codex", frame: `data: {"type":"codex.rate_limits"}` + "\n\n"},
		{name: "claude ping", provider: "claude", frame: `event: ping` + "\n" + `data: {"type":"ping"}` + "\n\n"},
		{name: "comment", provider: "codex", frame: ": keepalive\n\n"},
		{name: "text", provider: "codex", frame: `data: {"type":"response.output_text.delta","delta":"x"}` + "\n\n", want: true},
		{name: "reasoning", provider: "codex", frame: `data: {"type":"response.reasoning_summary_text.delta","delta":"x"}` + "\n\n", want: true},
		{name: "tool", provider: "codex", frame: `data: {"type":"response.output_item.added","item":{"type":"function_call"}}` + "\n\n", want: true},
		{name: "claude text", provider: "claude", frame: `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}` + "\n\n", want: true},
		{name: "unknown valid event", provider: "codex", frame: `data: {"type":"response.future_output.delta"}` + "\n\n", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sseFrameAdvancesModel([]byte(test.frame), test.provider); got != test.want {
				t.Fatalf("sseFrameAdvancesModel() = %v, want %v", got, test.want)
			}
		})
	}
}
