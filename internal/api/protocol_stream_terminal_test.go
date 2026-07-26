package api

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"codex-account-pool/internal/prompt"
	"codex-account-pool/internal/streamrewrite"
)

type protocolStreamReadError struct{}

func (protocolStreamReadError) Read([]byte) (int, error) {
	return 0, errors.New("injected upstream stream read failure")
}

func interruptedProtocolStream(raw string, readError bool) io.Reader {
	if !readError {
		return strings.NewReader(raw)
	}
	return io.MultiReader(strings.NewReader(raw), protocolStreamReadError{})
}

func TestAnthropicStreamToChatSSERequiresMessageStop(t *testing.T) {
	partial := "data: " + `{"type":"message_start","message":{"id":"msg_partial"}}` + "\n\n" +
		"data: " + `{"type":"content_block_delta","delta":{"type":"text_delta","text":"partial"}}` + "\n\n"
	for _, readError := range []bool{false, true} {
		name := "eof"
		if readError {
			name = "read_error"
		}
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			anthropicStreamToChatSSE(recorder, interruptedProtocolStream(partial, readError), "claude-x", streamrewrite.New(nil), true)
			got := recorder.Body.String()
			if !strings.Contains(got, `"code":"server_error"`) || !strings.Contains(got, publicRetryMessage) {
				t.Fatalf("truncated Anthropic stream did not emit a Chat error:\n%s", got)
			}
			if strings.Contains(got, "data: [DONE]") || strings.Contains(got, `"finish_reason":"stop"`) {
				t.Fatalf("truncated Anthropic stream was completed successfully:\n%s", got)
			}
		})
	}
}

func TestChatStreamToResponsesSSERequiresDone(t *testing.T) {
	partial := "data: " + `{"id":"chat_partial","choices":[{"delta":{"content":"partial"}}]}` + "\n\n"
	for _, readError := range []bool{false, true} {
		name := "eof"
		if readError {
			name = "read_error"
		}
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			chatStreamToResponsesSSE(recorder, interruptedProtocolStream(partial, readError), "gpt-x", streamrewrite.New(nil))
			got := recorder.Body.String()
			if !strings.Contains(got, "response.failed") || !strings.Contains(got, `"code":"server_error"`) || !strings.Contains(got, publicRetryMessage) {
				t.Fatalf("truncated Chat stream did not emit a Responses failure:\n%s", got)
			}
			if strings.Contains(got, "response.completed") {
				t.Fatalf("truncated Chat stream was completed successfully:\n%s", got)
			}
		})
	}
}

func TestChatStreamToAnthropicSSERequiresDone(t *testing.T) {
	partial := "data: " + `{"id":"chat_partial","choices":[{"delta":{"content":"partial"}}]}` + "\n\n"
	for _, readError := range []bool{false, true} {
		name := "eof"
		if readError {
			name = "read_error"
		}
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			chatStreamToAnthropicSSE(recorder, interruptedProtocolStream(partial, readError), "claude-x", streamrewrite.New(nil))
			got := recorder.Body.String()
			if !strings.Contains(got, "event: error") || !strings.Contains(got, `"code":"server_error"`) || !strings.Contains(got, publicRetryMessage) {
				t.Fatalf("truncated Chat stream did not emit an Anthropic error:\n%s", got)
			}
			if strings.Contains(got, "event: message_stop") {
				t.Fatalf("truncated Chat stream was completed successfully:\n%s", got)
			}
		})
	}
}

func TestResponsesStreamToAnthropicSSERequiresTerminalEvent(t *testing.T) {
	partial := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_partial","model":"gpt-x"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}` + "\n\n"
	for _, readError := range []bool{false, true} {
		name := "eof"
		if readError {
			name = "read_error"
		}
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			responsesStreamToAnthropicSSE(recorder, interruptedProtocolStream(partial, readError), "gpt-x", nil, nil, streamrewrite.New(nil))
			got := recorder.Body.String()
			if !strings.Contains(got, "event: error") || !strings.Contains(got, `"code":"server_error"`) || !strings.Contains(got, publicRetryMessage) {
				t.Fatalf("truncated Responses stream did not emit an Anthropic error:\n%s", got)
			}
			if strings.Contains(got, "event: message_stop") {
				t.Fatalf("truncated Responses stream was completed successfully:\n%s", got)
			}
		})
	}
}

func TestResponsesStreamToChatSSERequiresTerminalEvent(t *testing.T) {
	partial := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_partial","model":"gpt-x"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"delta":"partial"}` + "\n\n"
	for _, readError := range []bool{false, true} {
		name := "eof"
		if readError {
			name = "read_error"
		}
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			responsesStreamToChatSSE(recorder, interruptedProtocolStream(partial, readError), "gpt-x", true, streamrewrite.New(nil))
			got := recorder.Body.String()
			if !strings.Contains(got, `"code":"server_error"`) || !strings.Contains(got, publicRetryMessage) {
				t.Fatalf("truncated Responses stream did not emit a Chat error:\n%s", got)
			}
			if strings.Contains(got, "data: [DONE]") || strings.Contains(got, `"finish_reason":"stop"`) {
				t.Fatalf("truncated Responses stream was completed successfully:\n%s", got)
			}
		})
	}
}

func TestValidatedNativeSSERecognizesSplitTerminalFrames(t *testing.T) {
	tests := []struct {
		name     string
		protocol customSSEProtocol
		stream   string
		terminal string
	}{
		{
			name: "chat_crlf", protocol: customSSEChatCompletions,
			stream:   "data: {\"id\":\"chat_ok\",\"choices\":[]}\r\n\r\ndata: [DONE]\r\n\r\n",
			terminal: "data: [DONE]",
		},
		{
			name: "responses", protocol: customSSEResponses,
			stream: "event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_ok","status":"completed"}}` + "\n\n",
			terminal: "response.completed",
		},
		{
			name: "messages", protocol: customSSEAnthropicMessages,
			stream: "event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n",
			terminal: "message_stop",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			terminal, err := streamCopyRewriteValidated(
				recorder,
				iotest.OneByteReader(strings.NewReader(test.stream)),
				streamrewrite.New(nil),
				test.protocol,
			)
			if err != nil || !terminal {
				t.Fatalf("terminal=%v err=%v body=%s", terminal, err, recorder.Body.String())
			}
			got := recorder.Body.String()
			if !strings.Contains(got, test.terminal) || strings.Contains(got, publicRetryMessage) {
				t.Fatalf("valid split stream was rejected:\n%s", got)
			}
		})
	}
}

func TestAnthropicStreamToChatSSEUsageIsOptIn(t *testing.T) {
	stream := "data: " + `{"type":"message_start","message":{"id":"msg_usage","usage":{"input_tokens":11,"output_tokens":0,"cache_read_input_tokens":3}}}` + "\n\n" +
		"data: " + `{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
		"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}` + "\n\n" +
		"data: " + `{"type":"message_stop"}` + "\n\n"

	withoutUsage := httptest.NewRecorder()
	anthropicStreamToChatSSE(withoutUsage, strings.NewReader(stream), "claude-x", streamrewrite.New(nil), false)
	if got := withoutUsage.Body.String(); strings.Contains(got, `"usage":`) {
		t.Fatalf("Chat usage was emitted without stream_options.include_usage:\n%s", got)
	}

	withUsage := httptest.NewRecorder()
	anthropicStreamToChatSSE(withUsage, strings.NewReader(stream), "claude-x", streamrewrite.New(nil), true)
	got := withUsage.Body.String()
	for _, want := range []string{
		`"choices":[]`, `"prompt_tokens":11`, `"completion_tokens":4`,
		`"total_tokens":15`, `"cached_tokens":3`, "data: [DONE]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("opt-in Chat usage missing %q:\n%s", want, got)
		}
	}
}

func TestAnthropicStreamToResponsesSSEIncludesCompletedUsage(t *testing.T) {
	stream := "data: " + `{"type":"message_start","message":{"id":"msg_usage","usage":{"input_tokens":11,"output_tokens":0,"cache_read_input_tokens":3}}}` + "\n\n" +
		"data: " + `{"type":"content_block_delta","delta":{"type":"text_delta","text":"ok"}}` + "\n\n" +
		"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}` + "\n\n" +
		"data: " + `{"type":"message_stop"}` + "\n\n"
	recorder := httptest.NewRecorder()
	anthropicStreamToResponsesCustomSSE(recorder, strings.NewReader(stream), "gpt-x", streamrewrite.New(nil), prompt.NewResponsesToolBridgePlan())
	got := recorder.Body.String()
	for _, want := range []string{
		"response.completed", `"input_tokens":11`, `"output_tokens":4`,
		`"total_tokens":15`, `"cached_tokens":3`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Responses completed usage missing %q:\n%s", want, got)
		}
	}
}
