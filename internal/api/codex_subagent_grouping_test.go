package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

// The header/body shape below is copied from a capture of real codex-cli 0.150.1
// running `codex review --uncommitted` against the pool. Two properties of that
// capture drive every test in this file:
//
//   - Session-Id is shared by the root thread and all of its sub-agents
//     (thread-store/src/types.rs: "Session id shared by the root thread and all of
//     its subagents"), while Thread-Id is per-thread.
//   - x-codex-parent-thread-id names the root thread, which for a freshly created
//     session is the session id itself. `codex review` drives only the sub-agent
//     thread, so that parent thread never issues a request of its own.
func codexSubagentRequest(t *testing.T, session, thread, parent, turnState string, body []byte) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Session-Id", session)
	request.Header.Set("Thread-Id", thread)
	request.Header.Set("X-Client-Request-Id", thread)
	request.Header.Set("X-Codex-Window-Id", thread+":0")
	request.Header.Set("Originator", "codex_exec")
	if parent != "" {
		request.Header.Set("X-Codex-Parent-Thread-Id", parent)
		request.Header.Set("X-Openai-Subagent", "review")
	}
	if turnState != "" {
		request.Header.Set("X-Codex-Turn-State", turnState)
	}
	turnMeta := map[string]string{"session_id": session, "thread_id": thread, "window_id": thread + ":0"}
	if parent != "" {
		turnMeta["parent_thread_id"] = parent
		turnMeta["subagent_kind"] = "review"
		turnMeta["thread_source"] = "subagent"
	}
	encoded, err := json.Marshal(turnMeta)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Codex-Turn-Metadata", string(encoded))
	return request
}

// A sub-agent whose parent thread never contacted the pool has no anchor, so it is
// bound as an upstream root while remaining a child downstream. Its own concrete
// thread is then the only alias that can name it. Leaving that case with no
// hierarchy alias made the next turn resolvable only through the turn-state
// pointer, which failed the parent lookup, recovered into a second binding, and
// left the thread and the state pointer naming two different bindings.
func TestCodexSubagentWithUnboundParentPersistsItsOwnThreadAlias(t *testing.T) {
	const session = "01a049ce-3396-7b51-85b5-cef8bd608215"
	const subThread = "01a049ce-340d-7010-8d55-e8c7b1570590"

	body := []byte(`{"model":"gpt-5.6-sol","store":false,"input":"review this diff"}`)
	request := codexSubagentRequest(t, session, subThread, session, "", body)
	identity := codexDownstreamSessionIdentity(request.Header, body)
	if identity.RootID != "" {
		t.Fatalf("a child identity must not claim a root, got %q", identity.RootID)
	}
	if identity.ThreadID != subThread || identity.ParentID != session {
		t.Fatalf("identity did not read the captured sub-agent shape: %+v", identity)
	}

	// A parentless sub-agent is bound as an upstream root: ThreadID == RootSessionID.
	rootShaped := storage.CodexSessionBinding{RootSessionID: "upstream-root", ThreadID: "upstream-root"}
	kinds := map[string]string{}
	for _, alias := range identity.aliasesForBinding(rootShaped, "resp-1", "state-1") {
		kinds[alias.Type] = alias.Value
	}
	if kinds["branch"] != subThread {
		t.Errorf("sub-agent thread was not persisted as a branch alias: %v", kinds)
	}
	if _, ok := kinds["root"]; ok {
		t.Errorf("a child must never own a root alias: %v", kinds)
	}
	if _, ok := kinds["session"]; ok {
		t.Errorf("a child must never own the shared session alias: %v", kinds)
	}
	if kinds["response"] != "resp-1" || kinds["turn_state"] != "state-1" {
		t.Errorf("state aliases lost: %v", kinds)
	}
}

// The root's own alias set must not move: it is what separates two CLIs that share
// one API key, and it is the alias a sub-agent resolves its parent through.
func TestCodexRootStillPersistsRootAndSessionAliases(t *testing.T) {
	const session = "01a049ce-3396-7b51-85b5-cef8bd608215"

	body := []byte(`{"model":"gpt-5.6-sol","store":false,"input":"hello"}`)
	request := codexSubagentRequest(t, session, session, "", "", body)
	identity := codexDownstreamSessionIdentity(request.Header, body)
	if identity.RootID != session {
		t.Fatalf("a fresh root thread must own its root alias, got %q", identity.RootID)
	}

	rootShaped := storage.CodexSessionBinding{RootSessionID: "upstream-root", ThreadID: "upstream-root"}
	kinds := map[string]string{}
	for _, alias := range identity.aliasesForBinding(rootShaped, "resp-1", "state-1") {
		kinds[alias.Type] = alias.Value
	}
	if kinds["root"] != session || kinds["session"] != session {
		t.Errorf("root alias pair changed: %v", kinds)
	}
	if _, ok := kinds["branch"]; ok {
		t.Errorf("a root must not also claim a branch alias: %v", kinds)
	}
}

func seedCodexSubagentBinding(t *testing.T, h *testHarness, namespace, treeID string, aliases []storage.CodexSessionAlias) storage.CodexSessionBinding {
	t.Helper()
	binding, err := h.store.CommitCodexSessionBinding(context.Background(), storage.CodexSessionCommit{
		Namespace: namespace,
		Binding: storage.CodexSessionBinding{
			TreeID: treeID, AccountID: "acc-subagent", EgressID: storage.DefaultDirectEgressID,
			State: "active", RootSessionID: "upstream-" + treeID, ThreadID: "upstream-" + treeID,
		},
		Aliases:   aliases,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

// Once the sub-agent owns its branch alias, both of its later turn shapes must
// resolve to that same binding even though the parent thread is still unbound: the
// stateful shape (x-codex-turn-state, which every turn after the first carries) and
// the self-contained shape (no state pointer at all).
func TestCodexSubagentResolvesWhileItsParentThreadHasNoBinding(t *testing.T) {
	const session = "01a049d0-1111-7000-8000-000000000001"
	const subThread = "01a049d0-2222-7000-8000-000000000002"
	const turnState = "gAAAAAopaque-turn-state"

	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	enableCodexSessionMappingForTest(h)
	ctx := context.Background()
	policy := downstreamPolicy{KeyHash: hashAPIKey("cap-subagent-orphan-parent")}

	namespaceRequest := codexSubagentRequest(t, session, subThread, session, "", nil)
	namespace := codexSessionNamespace(policy, namespaceRequest)
	seeded := seedCodexSubagentBinding(t, h, namespace, "tree-subagent", []storage.CodexSessionAlias{
		{Type: "branch", Value: subThread},
		{Type: "turn_state", Value: turnState},
	})

	for _, spec := range []struct {
		name  string
		state string
	}{
		{"stateful turn", turnState},
		{"self-contained turn", ""},
	} {
		body := []byte(`{"model":"gpt-5.6-sol","store":false,"input":"next review turn"}`)
		request := codexSubagentRequest(t, session, subThread, session, spec.state, body)
		mapping, err := h.app.resolveCodexSessionMapping(ctx, request, body, policy)
		if err != nil {
			t.Fatalf("%s: an unbound parent thread must not fail resolution: %v", spec.name, err)
		}
		if mapping.binding == nil || mapping.binding.ID != seeded.ID {
			t.Fatalf("%s: resolved binding=%+v, want %s", spec.name, mapping.binding, seeded.ID)
		}
	}
}

// The tolerance above is only for an ABSENT parent. A parent that exists and belongs
// to another tree is still a conflicting hierarchy claim and must stay a visible
// ambiguity rather than silently attaching this turn to the wrong tree.
func TestCodexSubagentStillRejectsAParentInAnotherTree(t *testing.T) {
	const session = "01a049d0-3333-7000-8000-000000000003"
	const subThread = "01a049d0-4444-7000-8000-000000000004"
	const turnState = "gAAAAAopaque-conflicting-state"

	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	enableCodexSessionMappingForTest(h)
	ctx := context.Background()
	policy := downstreamPolicy{KeyHash: hashAPIKey("cap-subagent-foreign-parent")}

	namespaceRequest := codexSubagentRequest(t, session, subThread, session, "", nil)
	namespace := codexSessionNamespace(policy, namespaceRequest)
	seedCodexSubagentBinding(t, h, namespace, "tree-child", []storage.CodexSessionAlias{
		{Type: "branch", Value: subThread},
		{Type: "turn_state", Value: turnState},
	})
	seedCodexSubagentBinding(t, h, namespace, "tree-foreign-parent", []storage.CodexSessionAlias{
		{Type: "root", Value: session},
	})

	body := []byte(`{"model":"gpt-5.6-sol","store":false,"input":"conflicting hierarchy"}`)
	request := codexSubagentRequest(t, session, subThread, session, turnState, body)
	if _, err := h.app.resolveCodexSessionMapping(ctx, request, body, policy); !errors.Is(err, storage.ErrCodexSessionMappingAmbiguous) {
		t.Fatalf("a parent owned by another tree must stay ambiguous, got %v", err)
	}
}

// The grouping rule the user-visible behaviour depends on: one API key, two CLI
// processes, each with its own sub-agents. A sub-agent must land in its parent's
// namespace (so it inherits the parent's tree and account) while the two CLIs must
// never share one.
func TestCodexSubagentSharesParentNamespaceWhileTwoCLIsDoNot(t *testing.T) {
	policy := downstreamPolicy{KeyHash: hashAPIKey("cap-one-key-two-clis")}
	const sessionA = "01a049d0-aaaa-7000-8000-00000000000a"
	const sessionB = "01a049d0-bbbb-7000-8000-00000000000b"

	namespaceOf := func(session, thread, parent string) string {
		return codexSessionNamespace(policy, codexSubagentRequest(t, session, thread, parent, "", nil))
	}

	rootA := namespaceOf(sessionA, sessionA, "")
	subA := namespaceOf(sessionA, "01a049d0-a5a5-7000-8000-0000000000a5", sessionA)
	compactA := namespaceOf(sessionA, "01a049d0-a6a6-7000-8000-0000000000a6", sessionA)
	rootB := namespaceOf(sessionB, sessionB, "")
	subB := namespaceOf(sessionB, "01a049d0-b5b5-7000-8000-0000000000b5", sessionB)

	if rootA != subA {
		t.Error("a sub-agent was isolated from the CLI that spawned it")
	}
	if subA != compactA {
		t.Error("two sub-agents of one CLI were isolated from each other")
	}
	if rootA == rootB {
		t.Error("two CLI processes sharing one API key collapsed into one namespace")
	}
	if subA == subB {
		t.Error("sub-agents of two different CLIs collapsed into one namespace")
	}
	for _, namespace := range []string{rootA, subA, rootB, subB} {
		if len(namespace) == 0 {
			t.Fatal("empty namespace")
		}
		for _, raw := range []string{sessionA, sessionB} {
			if bytes.Contains([]byte(namespace), []byte(raw)) {
				t.Errorf("raw session id leaked into namespace %q", namespace)
			}
		}
	}
}
