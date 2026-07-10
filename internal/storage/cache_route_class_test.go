package storage

import "testing"

func TestCanonicalCodexRootThreadIsTrueConversationRoute(t *testing.T) {
	if got := RouteClassForAffinitySource("codex-root-thread-id"); got != "true_conversation" {
		t.Fatalf("canonical Codex root route class = %q, want true_conversation", got)
	}
}
