package api

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	upstreamrules "codex-account-pool/internal/upstream_error_rules"
)

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
