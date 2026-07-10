package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseAndValidateJSONLAcceptsStrictMultiAgentRun(t *testing.T) {
	got, err := parseAndValidateJSONL(strings.NewReader(validJSONL("child-a", "child-b", childAAnswer, childBAnswer, finalAnswer, "")))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Children) != 2 || got.Children[0].Label != "A" || got.Children[1].Label != "B" {
		t.Fatalf("children = %+v", got.Children)
	}
	if got.Children[0].Actual != childAAnswer || got.Children[1].Actual != childBAnswer {
		t.Fatalf("child results = %+v", got.Children)
	}
	if got.Final != finalAnswer {
		t.Fatalf("final = %q", got.Final)
	}
	if got.Usage.Input != 1000 || got.Usage.Cached != 800 || got.Usage.Output != 30 || got.Usage.Reasoning != 20 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestParseAndValidateJSONLRejectsInvalidRuns(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "duplicate child id",
			raw:  validJSONL("same-child", "same-child", childAAnswer, childBAnswer, finalAnswer, ""),
			want: "not distinct",
		},
		{
			name: "wrong child result",
			raw:  validJSONL("child-a", "child-b", "72", childBAnswer, finalAnswer, ""),
			want: "child A result",
		},
		{
			name: "wrong final",
			raw:  validJSONL("child-a", "child-b", childAAnswer, childBAnswer, "MULTI_AGENT_BAD", ""),
			want: "final answer",
		},
		{
			name: "stream error",
			raw:  validJSONL("child-a", "child-b", childAAnswer, childBAnswer, finalAnswer, `{"type":"error","message":"transport failed"}`),
			want: "stream error",
		},
		{
			name: "malformed JSONL",
			raw:  "not-json\n",
			want: "not valid JSON",
		},
		{
			name: "wait before second spawn",
			raw: strings.Join([]string{
				spawnEvent("child-a", childAMarker),
				waitEvent("child-a", childAAnswer),
				spawnEvent("child-b", childBMarker),
				waitEvent("child-b", childBAnswer),
				agentMessageEvent(finalAnswer),
				usageEvent(),
			}, "\n") + "\n",
			want: "before both children",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAndValidateJSONL(strings.NewReader(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestRedactSensitiveRemovesCredentialShapes(t *testing.T) {
	secret := "at-M_xXfuENYvZzJhxUqKIPZFi"
	jwt := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.payload.signature"
	raw := `Authorization: Bearer ` + secret + ` {"access_token":"` + secret + `","id_token":"` + jwt + `","email":"person@example.com"}`
	got := redactSensitive(raw)
	for _, forbidden := range []string{secret, jwt, "person@example.com"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("redacted output still contains %q: %s", forbidden, got)
		}
	}
}

func validJSONL(childA, childB, answerA, answerB, final, extra string) string {
	events := []string{
		`{"type":"thread.started","thread_id":"root"}`,
		`{"type":"turn.started"}`,
		spawnEvent(childA, childAMarker),
		spawnEvent(childB, childBMarker),
		waitEvent(childA, answerA),
		waitEvent(childB, answerB),
		agentMessageEvent(final),
		usageEvent(),
	}
	if extra != "" {
		events = append(events, extra)
	}
	return strings.Join(events, "\n") + "\n"
}

func spawnEvent(childID, marker string) string {
	return fmt.Sprintf(`{"type":"item.completed","item":{"id":"spawn-%s","type":"collab_tool_call","tool":"spawn_agent","sender_thread_id":"root","receiver_thread_ids":[%q],"prompt":%q,"agents_states":{},"status":"completed"}}`, childID, childID, marker+": task")
}

func waitEvent(childID, answer string) string {
	return fmt.Sprintf(`{"type":"item.completed","item":{"id":"wait-%s","type":"collab_tool_call","tool":"wait","sender_thread_id":"root","receiver_thread_ids":[%q],"prompt":null,"agents_states":{%q:{"status":"completed","message":%q}},"status":"completed"}}`, childID, childID, childID, answer)
}

func agentMessageEvent(text string) string {
	return fmt.Sprintf(`{"type":"item.completed","item":{"id":"message","type":"agent_message","text":%q}}`, text)
}

func usageEvent() string {
	return `{"type":"turn.completed","usage":{"input_tokens":1000,"cached_input_tokens":800,"output_tokens":30,"reasoning_output_tokens":20}}`
}
