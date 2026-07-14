package responsefilter

import "testing"

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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
