package api

import (
	"strings"
	"testing"
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
