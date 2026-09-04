package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func codexThreadAdminRequest(t *testing.T, h *testHarness, method, path string) (*http.Response, []byte) {
	t.Helper()
	resp := doAdminRequest(t, h, method, path, nil)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, body
}

type codexThreadRuntimeStub struct {
	mu             sync.Mutex
	thread         Thread
	statuses       chan ThreadStatusChanged
	interruptCalls []struct{ threadID, turnID string }
	readTurns      []bool
}

func newCodexThreadRuntimeStub(thread Thread) *codexThreadRuntimeStub {
	return &codexThreadRuntimeStub{thread: thread, statuses: make(chan ThreadStatusChanged, 4)}
}

func (s *codexThreadRuntimeStub) ListThreads(_ context.Context, _ string, _ ThreadListParams) (ThreadListResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ThreadListResponse{Data: []Thread{s.thread, s.thread}}, nil // intentional duplicate exercises list dedupe
}

func (s *codexThreadRuntimeStub) ReadThread(_ context.Context, _ string, threadID string, includeTurns bool) (Thread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readTurns = append(s.readTurns, includeTurns)
	if threadID != s.thread.ID {
		return Thread{}, context.Canceled
	}
	return s.thread, nil
}

func (s *codexThreadRuntimeStub) ResumeThread(_ context.Context, _ string, p ThreadResumeParams) (Thread, error) {
	return s.ReadThread(context.Background(), "", p.ThreadID, false)
}

func (s *codexThreadRuntimeStub) InterruptTurn(_ context.Context, _ string, threadID, turnID string) error {
	s.mu.Lock()
	s.interruptCalls = append(s.interruptCalls, struct{ threadID, turnID string }{threadID, turnID})
	s.mu.Unlock()
	s.statuses <- ThreadStatusChanged{ThreadID: threadID, TurnID: turnID, Status: "turnAborted", Revision: 1}
	return nil
}

func (s *codexThreadRuntimeStub) SubscribeThreadStatus(_ context.Context, _ string) (<-chan ThreadStatusChanged, error) {
	return s.statuses, nil
}

func TestCodexThreadAdminUsesOpaqueHandlesAndConfirmsExactInterrupt(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	enableAdmin(t, h)
	runtime := newCodexThreadRuntimeStub(Thread{
		ID: "thread-raw-secret", ActiveTurnID: "turn-raw-secret", Model: "gpt-5.6", ModelProvider: "openai",
		Source: "cli", Status: "active", WaitingReason: "waitingOnApproval", CWD: "/private/workspace/project-secret",
		UpdatedAt: time.Unix(1_700_000_000, 0).UTC(), DirectInput: true,
	})
	if err := h.app.RegisterCodexThreadRuntime("runtime-test", runtime, CodexRuntimeDescriptor{Label: "Test runtime", OwnerPrincipal: "admin-token"}); err != nil {
		t.Fatal(err)
	}

	resp, body := codexThreadAdminRequest(t, h, http.MethodGet, "/admin/codex-threads?runtime_id=runtime-test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "thread-raw-secret") || strings.Contains(string(body), "turn-raw-secret") || strings.Contains(string(body), "/private/workspace") {
		t.Fatalf("list leaked raw app-server locator: %s", body)
	}
	var listed struct {
		Data []codexThreadView `json:"data"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 1 {
		t.Fatalf("list did not dedupe thread IDs: %+v", listed.Data)
	}
	view := listed.Data[0]
	if view.ThreadKey == "" || view.ThreadHandle == "" || view.ActiveTurnHandle == "" || view.CWDBase != "project-secret" || view.Status != "active" {
		t.Fatalf("unsafe or incomplete list view: %+v", view)
	}
	resp, body = codexThreadAdminRequest(t, h, http.MethodGet, "/admin/codex-threads/"+view.ThreadHandle)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", resp.StatusCode, body)
	}
	runtime.mu.Lock()
	askedForTurns := append([]bool(nil), runtime.readTurns...)
	runtime.mu.Unlock()
	for _, includeTurns := range askedForTurns {
		if includeTurns {
			t.Fatal("metadata-only admin view requested transcript turns from the runtime")
		}
	}
	// Capability handles are intentionally fresh AEAD ciphertexts. The separate
	// thread key must remain stable within a runtime generation so an SSE event can
	// patch this exact browser row without exposing the raw thread locator.
	_, secondListBody := codexThreadAdminRequest(t, h, http.MethodGet, "/admin/codex-threads?runtime_id=runtime-test")
	var secondList struct {
		Data []codexThreadView `json:"data"`
	}
	if err := json.Unmarshal(secondListBody, &secondList); err != nil || len(secondList.Data) != 1 || secondList.Data[0].ThreadKey != view.ThreadKey {
		t.Fatalf("thread status key was not stable: rows=%+v err=%v", secondList.Data, err)
	}

	resp, body = codexThreadAdminRequest(t, h, http.MethodPost, "/admin/codex-threads/"+view.ThreadHandle+"/turns/"+view.ActiveTurnHandle+"/interrupt")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("interrupt status=%d body=%s", resp.StatusCode, body)
	}
	runtime.mu.Lock()
	calls := append([]struct{ threadID, turnID string }(nil), runtime.interruptCalls...)
	runtime.mu.Unlock()
	if len(calls) != 1 || calls[0].threadID != "thread-raw-secret" || calls[0].turnID != "turn-raw-secret" {
		t.Fatalf("interrupt did not target the exact active turn: %+v", calls)
	}

	// Re-registering the runtime is a new generation. The browser's old opaque
	// locator must fail closed rather than being replayed against the replacement.
	if err := h.app.RegisterCodexThreadRuntime("runtime-test", runtime, CodexRuntimeDescriptor{Label: "Test runtime", OwnerPrincipal: "admin-token"}); err != nil {
		t.Fatal(err)
	}
	resp, body = codexThreadAdminRequest(t, h, http.MethodGet, "/admin/codex-threads/"+view.ThreadHandle)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "codex_runtime_generation_stale") {
		t.Fatalf("stale handle status=%d body=%s", resp.StatusCode, body)
	}
	_, freshListBody := codexThreadAdminRequest(t, h, http.MethodGet, "/admin/codex-threads?runtime_id=runtime-test")
	var freshList struct {
		Data []codexThreadView `json:"data"`
	}
	if err := json.Unmarshal(freshListBody, &freshList); err != nil || len(freshList.Data) != 1 || freshList.Data[0].ThreadKey == view.ThreadKey {
		t.Fatalf("runtime generation did not invalidate the status key: rows=%+v err=%v", freshList.Data, err)
	}
}

func TestCodexThreadRuntimeStatusRevisionIsMonotonic(t *testing.T) {
	registry := NewCodexRuntimeRegistry([]byte("codex-thread-registry-test-secret"))
	runtime := newCodexThreadRuntimeStub(Thread{ID: "thread"})
	if err := registry.Register("runtime", runtime, CodexRuntimeDescriptor{}); err != nil {
		t.Fatal(err)
	}
	if !registry.updateStatus("runtime", ThreadStatusChanged{ThreadID: "thread", Status: "active", Revision: 2}) {
		t.Fatal("newer revision was rejected")
	}
	if registry.updateStatus("runtime", ThreadStatusChanged{ThreadID: "thread", Status: "idle", Revision: 2}) {
		t.Fatal("equal revision replaced a newer status")
	}
	entry, ok := registry.get("runtime")
	if !ok {
		t.Fatal("registered runtime disappeared")
	}
	entry.mu.RLock()
	status := entry.statuses["thread"]
	entry.mu.RUnlock()
	if status.Status != "active" || status.Revision != 2 {
		t.Fatalf("stale event was accepted: %+v", status)
	}
}
