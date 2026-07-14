package responsefilter

import (
	"encoding/json"
	"testing"
)

func TestFilterSSEFrameDropsMatchingEvent(t *testing.T) {
	got := FilterSSEFrame([]byte("data: {\"type\":\"response.output_text.delta\",\"text\":\"secret\"}\n\n"), []string{"secret"}, true)
	if got != nil {
		t.Fatalf("expected matching event to be dropped, got %q", got)
	}
}

func TestFilterSSEFrameKeepsAndPrunesTerminal(t *testing.T) {
	got := FilterSSEFrame([]byte("data: {\"type\":\"response.completed\",\"output\":[{\"text\":\"secret\"},{\"text\":\"ok\"}]}\n\n"), []string{"secret"}, true)
	if string(got) == "" || string(got) == "data: \n\n" {
		t.Fatalf("expected terminal envelope to remain: %q", got)
	}
	if string(got) == "" || contains(string(got), "secret") {
		t.Fatalf("secret leaked: %q", got)
	}
}

func TestFilterJSONPrunesMatchingLeaves(t *testing.T) {
	got, changed := FilterJSON([]byte(`{"id":"r","output":[{"text":"secret"},{"text":"ok"}]}`), []string{"secret"}, true)
	if !changed || contains(string(got), "secret") || !contains(string(got), "ok") {
		t.Fatalf("unexpected filtered body: %s", got)
	}
}

func TestStripSafetyBufferingJSONPreservesResponseEvent(t *testing.T) {
	input := []byte(`{"type":"response.completed","response":{"id":"resp_1","status":"completed"},"safety_buffering":{"use_cases":["cyber"]}}`)
	got, changed := StripSafetyBufferingJSON(input)
	if !changed {
		t.Fatal("expected safety_buffering to be removed")
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(got, &event); err != nil {
		t.Fatalf("filtered event is invalid JSON: %v: %s", err, got)
	}
	if _, ok := event["safety_buffering"]; ok {
		t.Fatalf("safety_buffering remains: %s", got)
	}
	if string(event["type"]) != `"response.completed"` || !contains(string(event["response"]), "resp_1") {
		t.Fatalf("terminal event was damaged: %s", got)
	}
}

func TestStripSafetyBufferingSSEPreservesEnvelopeAndCRLF(t *testing.T) {
	frame := []byte("event: response.created\r\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"},\"safety_buffering\":{\"reasons\":[\"user_risk\"]}}\r\n\r\n")
	got, changed := StripSafetyBufferingSSE(frame)
	if !changed {
		t.Fatal("expected SSE frame to change")
	}
	if contains(string(got), "safety_buffering") || !contains(string(got), "response.created") || !contains(string(got), "resp_1") {
		t.Fatalf("unexpected filtered frame: %q", got)
	}
	if !contains(string(got), "\r\n\r\n") {
		t.Fatalf("CRLF event boundary was not preserved: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
