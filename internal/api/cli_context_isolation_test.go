package api

import (
	"net/http"
	"testing"
)

func scopeFor(t *testing.T, keyHash string, headers map[string]string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	scope := downstreamClientScope(keyHash, req)
	if scope == "" {
		t.Fatalf("no scope derived from %v", headers)
	}
	return scope
}

// Different CLI instances on one API key stay isolated.
func TestDifferentCLIsOnOneAPIKeyStayIsolated(t *testing.T) {
	const key = "one-api-key"
	if scopeFor(t, key, map[string]string{"Thread-Id": "thread_iso_a_root"}) ==
		scopeFor(t, key, map[string]string{"Thread-Id": "thread_iso_b_root"}) {
		t.Error("two Codex CLI instances sharing an API key collapsed into one context")
	}
}

// One converged gateway device serving several CLI processes must still separate
// them: the device id is only a last-resort fallback.
func TestConvergedDeviceDoesNotMergeDistinctCLIContexts(t *testing.T) {
	const key = "converged-key"
	const device = "same-gateway-device"

	claudeA := scopeFor(t, key, map[string]string{
		"X-Pool-Client-ID": device, "X-Claude-Code-Session-Id": "claude_ctx_a"})
	claudeB := scopeFor(t, key, map[string]string{
		"X-Pool-Client-ID": device, "X-Claude-Code-Session-Id": "claude_ctx_b"})
	codex := scopeFor(t, key, map[string]string{
		"X-Pool-Client-ID": device, "Thread-Id": "thread_converged_root"})

	if claudeA == claudeB {
		t.Error("two Claude CLI contexts on one converged device collapsed")
	}
	for name, other := range map[string]string{"claudeA": claudeA, "claudeB": claudeB} {
		if codex == other {
			t.Errorf("a Codex context collided with %s on the same device", name)
		}
	}
}

func TestCLIContextScopeIsStableAcrossTurns(t *testing.T) {
	const key = "stable-key"
	if scopeFor(t, key, map[string]string{"Thread-Id": "thread_stable_root"}) !=
		scopeFor(t, key, map[string]string{"Thread-Id": "thread_stable_root"}) {
		t.Error("the same CLI context produced two scopes on consecutive turns")
	}
}

func TestDistinctAPIKeysNeverShareContext(t *testing.T) {
	headers := map[string]string{"Thread-Id": "thread_same_everywhere"}
	if scopeFor(t, "key-one", headers) == scopeFor(t, "key-two", headers) {
		t.Error("two API keys shared one context scope")
	}
}

// Native session identity separates two contexts that present the same visible
// thread id. Pinned because an attempt to fix sub-agent grouping by raising the
// thread root above Session-Id broke this and 18 other tests.
func TestNativeSessionIdentityStillSeparatesContextsSharingAThread(t *testing.T) {
	const key = "shared-thread-key"
	const thread = "thread_shared_between_contexts"
	if scopeFor(t, key, map[string]string{"Thread-Id": thread, "Session-Id": "native_ctx_a"}) ==
		scopeFor(t, key, map[string]string{"Thread-Id": thread, "Session-Id": "native_ctx_b"}) {
		t.Error("two native Codex sessions sharing one visible thread id collapsed")
	}
}

// A Codex sub-agent (review / compact / memory_consolidation / collab_spawn) gets its
// own thread id but REUSES its parent's session id — thread-store/src/types.rs
// documents session_id as "shared by the root thread and all of its subagents", and a
// capture of codex-cli 0.150.1 running `codex review` confirms it: Session-Id equals
// the root thread's id while Thread-Id is the sub-agent's own.
//
// Session-Id is therefore the CLI-context boundary, and grouping a sub-agent with its
// parent needs no special case here — it falls out of preferring the session over the
// thread. Do not try to "fix" grouping by reordering these headers: raising the thread
// root above Session-Id was tried and broke 19 tests, because two independent CLI
// contexts legitimately present the same visible thread id (see the test above).
//
// What actually had to be fixed lives in the CPA layer, not in this scope function:
// see TestCodexSubagentWithUnboundParentPersistsItsOwnThreadAlias and
// TestCodexSubagentResolvesWhileItsParentThreadHasNoBinding.
func TestCodexSubagentSharesTheContextOfTheCLIThatSpawnedIt(t *testing.T) {
	const key = "subagent-grouping-key"
	const rootThread = "thread_group_root"

	parent := scopeFor(t, key, map[string]string{
		"Thread-Id": rootThread, "Session-Id": rootThread})
	sub := scopeFor(t, key, map[string]string{
		"Thread-Id":                "thread_group_sub",
		"Session-Id":               rootThread,
		"X-Codex-Parent-Thread-Id": rootThread,
		"X-Openai-Subagent":        "review",
	})
	sibling := scopeFor(t, key, map[string]string{
		"Thread-Id":                "thread_group_sibling",
		"Session-Id":               rootThread,
		"X-Codex-Parent-Thread-Id": rootThread,
		"X-Openai-Subagent":        "compact",
	})
	if sub != parent {
		t.Error("a sub-agent was isolated from the CLI that spawned it")
	}
	if sibling != parent {
		t.Error("a second sub-agent of the same CLI was isolated from it")
	}

	// A sub-agent of a different CLI on the same key stays out.
	other := scopeFor(t, key, map[string]string{
		"Thread-Id":                "thread_other_sub",
		"Session-Id":               "thread_other_root",
		"X-Codex-Parent-Thread-Id": "thread_other_root",
		"X-Openai-Subagent":        "review",
	})
	if other == parent {
		t.Error("a sub-agent of another CLI collapsed into this CLI's context")
	}
}
