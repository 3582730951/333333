package prompt

import (
	"strings"
	"testing"
)

func TestCrossProtocolResponseConvertersRejectMalformedJSON(t *testing.T) {
	tests := []struct {
		name    string
		convert func([]byte) ([]byte, error)
	}{
		{name: "anthropic_to_chat", convert: func(raw []byte) ([]byte, error) {
			return AnthropicToChatCompletion(raw, "claude-test")
		}},
		{name: "chat_to_anthropic", convert: func(raw []byte) ([]byte, error) {
			return ChatCompletionToAnthropicResponse(raw, "chat-test")
		}},
		{name: "chat_to_responses", convert: func(raw []byte) ([]byte, error) {
			return ChatCompletionToResponsesResponse(raw, "chat-test")
		}},
		{name: "responses_to_chat", convert: func(raw []byte) ([]byte, error) {
			return ResponsesToChatCompletion(raw, "", "responses-test")
		}},
		{name: "responses_to_anthropic", convert: func(raw []byte) ([]byte, error) {
			return ResponsesToAnthropicResponse(raw, "responses-test", nil, nil)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out, err := test.convert([]byte("<html>upstream failed"))
			if err == nil {
				t.Fatalf("malformed response returned success: %q", out)
			}
			if len(out) != 0 {
				t.Fatalf("malformed upstream body was passed through: %q", out)
			}
		})
	}
}

func TestChatToAnthropicConvertersRejectInvalidToolArguments(t *testing.T) {
	request := []byte(`{
		"model":"test","messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{not-json"}}]}
		]
	}`)
	if out, err := ChatCompletionToAnthropic(request); err == nil || len(out) != 0 || !strings.Contains(err.Error(), "tool arguments") {
		t.Fatalf("request conversion out=%q err=%v", out, err)
	}

	response := []byte(`{
		"id":"chatcmpl_1","choices":[{"message":{"role":"assistant","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{not-json"}}
		]},"finish_reason":"tool_calls"}]
	}`)
	if out, err := ChatCompletionToAnthropicResponse(response, "test"); err == nil || len(out) != 0 || !strings.Contains(err.Error(), "tool arguments") {
		t.Fatalf("response conversion out=%q err=%v", out, err)
	}
}

func TestAnthropicToResponsesRejectsUnreplayableReasoning(t *testing.T) {
	for _, block := range []string{
		`{"type":"thinking","thinking":"private","signature":"native-claude-signature"}`,
		`{"type":"redacted_thinking","data":"native-claude-redacted-data"}`,
	} {
		raw := []byte(`{"model":"claude-test","messages":[{"role":"assistant","content":[` + block + `,{"type":"text","text":"answer"}]}]}`)
		if out, err := AnthropicRequestToResponses(raw); err == nil || len(out.Body) != 0 || !strings.Contains(err.Error(), "replayable Responses reasoning envelope") {
			t.Fatalf("reasoning block %s out=%q err=%v", block, out.Body, err)
		}
	}
}
