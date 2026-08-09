package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
	"codex-account-pool/internal/supervisor"
)

func TestIncrementalGoalRequestStoresOnlyDurableHistorySuffix(t *testing.T) {
	durable := []byte(`{"model":"claude","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"}]}`)
	current := []byte(`{"model":"claude","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"done"}]}]}`)
	trimmed := incrementalGoalRequest(current, durable)
	var root map[string]interface{}
	if err := json.Unmarshal(trimmed, &root); err != nil {
		t.Fatal(err)
	}
	messages, _ := root["messages"].([]interface{})
	if len(messages) != 1 || !strings.Contains(string(trimmed), "tool_result") || strings.Contains(string(trimmed), `"content":"first"`) || strings.Contains(string(trimmed), `"content":"answer"`) {
		t.Fatalf("incremental history was not trimmed safely: %s", trimmed)
	}

	mismatch := []byte(`{"model":"claude","messages":[{"role":"user","content":"changed"},{"role":"user","content":"new"}]}`)
	if got := incrementalGoalRequest(mismatch, durable); string(got) != string(mismatch) {
		t.Fatalf("mismatched history must be preserved: %s", got)
	}

	// A full Claude snapshot contains both sides of the conversation. When it no
	// longer has the durable prefix, it is an authoritative compact/edit boundary,
	// not a delta to append after the old context.
	compacted := []byte(`{"model":"claude","messages":[{"role":"user","content":"summary of the retained task"},{"role":"assistant","content":"summary acknowledged"},{"role":"user","content":"continue after compact"}]}`)
	trimmed, replace := incrementalGoalRequestWithMode(compacted, durable)
	if !replace || string(trimmed) != string(compacted) {
		t.Fatalf("full Claude replacement snapshot replace=%v body=%s", replace, trimmed)
	}
}

func TestGoalPersistenceDegradedAuditIsCoalesced(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()
	const terminal = "codex_terminal_pressure_test"
	err := storage.ErrGoalStorageBudget
	for index := 0; index < 100; index++ {
		h.app.auditGoalPersistenceDegraded(ctx, terminal, err)
	}
	var rows int
	if queryErr := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='goal_persistence_degraded' AND reason=?`, terminal).Scan(&rows); queryErr != nil || rows != 1 {
		t.Fatalf("coalesced audit rows=%d err=%v, want 1", rows, queryErr)
	}
	key := fmt.Sprintf("%p\x00%s\x00%s", h.app.store, terminal, goalPersistenceErrorCode(err))
	value, ok := goalPersistenceAuditBuckets.Load(key)
	if !ok {
		t.Fatal("coalescing bucket was not retained")
	}
	bucket := value.(*goalPersistenceAuditBucket)
	bucket.mu.Lock()
	bucket.last = time.Now().Add(-goalPersistenceAuditWindow)
	bucket.mu.Unlock()
	h.app.auditGoalPersistenceDegraded(ctx, terminal, err)
	if queryErr := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='goal_persistence_degraded' AND reason=?`, terminal).Scan(&rows); queryErr != nil || rows != 2 {
		t.Fatalf("coalesced audit flush rows=%d err=%v, want 2", rows, queryErr)
	}
	var detail string
	if queryErr := h.store.DB().QueryRowContext(ctx, `SELECT detail FROM audit_log WHERE action='goal_persistence_degraded' AND reason=? ORDER BY id DESC LIMIT 1`, terminal).Scan(&detail); queryErr != nil || detail != "error_code=storage_budget suppressed=99" {
		t.Fatalf("coalesced audit detail=%q err=%v", detail, queryErr)
	}
	goalPersistenceAuditBuckets.Delete(key)
}

func TestCommitGoalTurnWithStorageRecoveryRetriesExactlyOnceAfterColdReclaim(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()
	payload := func(seed uint32, size int) string {
		raw := make([]byte, size)
		for index := range raw {
			seed = seed*1664525 + 1013904223
			raw[index] = byte(seed >> 24)
		}
		return base64.StdEncoding.EncodeToString(raw)
	}
	turn := func(alias, response, input string) storage.GoalTurn {
		return storage.GoalTurn{
			Protocol: "codex", DownstreamKeyHash: "key-" + alias, WorkspaceHash: "workspace-" + alias,
			InitialGoalHash: "initial-" + alias, ResponseID: response,
			Aliases:           []storage.GoalAlias{{Type: "codex_root_thread", Value: alias}},
			CheckpointPayload: `{"model":"gpt-test","input":[]}`,
			SegmentPayload:    `{"input":"` + input + `","output":"ok"}`,
			ExpiresAt:         storage.Now() + 86400,
		}
	}
	var cold []storage.GoalSession
	for index := 0; index < 2; index++ {
		created, err := h.store.CommitGoalTurn(ctx, turn(fmt.Sprintf("api-cold-%d", index), fmt.Sprintf("api-cold-r%d", index), payload(uint32(index+1), 80<<10)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = h.store.DB().ExecContext(ctx, `UPDATE goal_session SET state='completed',updated_at=? WHERE id=?`, storage.Now()-100-int64(index), created.ID); err != nil {
			t.Fatal(err)
		}
		cold = append(cold, created)
	}
	current, err := h.store.CommitGoalTurn(ctx, turn("api-current", "api-current-r1", "first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.store.AcquireGoalRun(ctx, current.ID, "api-current-owner", "running", time.Minute); err != nil {
		t.Fatal(err)
	}
	var used, currentBytes int64
	if err = h.store.DB().QueryRowContext(ctx, `SELECT SUM(storage_bytes) FROM goal_session`).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if err = h.store.DB().QueryRowContext(ctx, `SELECT storage_bytes FROM goal_session WHERE id=?`, current.ID).Scan(&currentBytes); err != nil {
		t.Fatal(err)
	}
	appendTurn := turn("api-current", "api-current-r2", payload(77, 96<<10))
	appendTurn.StorageMaxBytes = used + 1
	_, err = h.store.CommitGoalTurn(ctx, appendTurn)
	var probe *storage.GoalStorageBudgetError
	if !errors.As(err, &probe) {
		t.Fatalf("budget probe error=%v", err)
	}
	appendTurn.StorageMaxBytes = currentBytes + probe.AdditionalBytes + 1024
	updated, err := h.app.commitGoalTurnWithStorageRecovery(ctx, appendTurn, appendTurn.StorageMaxBytes)
	if err != nil || updated.ID != current.ID {
		t.Fatalf("recovered commit=%+v err=%v, want current %s", updated, err, current.ID)
	}
	for _, goal := range cold {
		if _, err = h.store.GetGoalSession(ctx, goal.ID); !errors.Is(err, storage.ErrGoalNotFound) {
			t.Fatalf("cold goal %s survived recovery: %v", goal.ID, err)
		}
	}
	var recoveredAudits int
	if err = h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='goal_storage_commit_recovered'`).Scan(&recoveredAudits); err != nil || recoveredAudits != 1 {
		t.Fatalf("recovery audit count=%d err=%v", recoveredAudits, err)
	}
}

func TestCommitGoalTurnWithStorageRecoveryConsolidatesOnlyLiveGoalExactly(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()
	turn := func(response, marker string) storage.GoalTurn {
		return storage.GoalTurn{
			Protocol: "codex", DownstreamKeyHash: "only-live-key", WorkspaceHash: "only-live-workspace",
			InitialGoalHash: "only-live-initial", ResponseID: response,
			Aliases:           []storage.GoalAlias{{Type: "codex_root_thread", Value: "only-live-root"}},
			CheckpointPayload: `{"model":"gpt-test","input":[]}`,
			SegmentPayload:    `{"history_key":"input","input":[{"role":"user","content":"` + marker + `"}],"output":[{"type":"message","content":"output-` + marker + `"}]}`,
			WorkingState:      `{"marker":"` + marker + `"}`,
			ExpiresAt:         storage.Now() + 86400,
		}
	}
	first := turn("only-live-r0", "first-marker")
	goal, err := h.store.CommitGoalTurn(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 24; index++ {
		marker := fmt.Sprintf("middle-marker-%02d-%s", index, strings.Repeat("repeatable-context-", 12))
		next := turn(fmt.Sprintf("only-live-r%d", index), marker)
		if index == 24 {
			next.AwaitingTool = true
			next.SegmentPayload = `{"history_key":"input","input":[{"type":"custom_tool_call","call_id":"pending-call","name":"tool","arguments":"{}"}],"output":[{"type":"message","content":"pending-tool-marker"}]}`
		}
		if _, err = h.store.CommitGoalTurn(ctx, next); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = h.store.AcquireGoalRun(ctx, goal.ID, "only-live-owner", "running", time.Minute); err != nil {
		t.Fatal(err)
	}
	beforeReplay, beforeSession, err := h.store.BuildGoalReplay(ctx, goal.ID)
	if err != nil || beforeSession.State != "awaiting_tool_result" {
		t.Fatalf("before consolidation session=%+v err=%v", beforeSession, err)
	}
	var beforeBytes int64
	if err = h.store.DB().QueryRowContext(ctx, `SELECT storage_bytes FROM goal_session WHERE id=?`, goal.ID).Scan(&beforeBytes); err != nil {
		t.Fatal(err)
	}
	appendTurn := turn("only-live-final", "after-tool-marker")
	appendTurn.SegmentPayload = `{"history_key":"input","input":[{"type":"custom_tool_call_output","call_id":"pending-call","output":"after-tool-marker"}],"output":[{"type":"message","content":"latest-marker"}]}`
	appendTurn.StorageMaxBytes = beforeBytes - 1
	updated, err := h.app.commitGoalTurnWithStorageRecovery(ctx, appendTurn, appendTurn.StorageMaxBytes)
	if err != nil || updated.ID != goal.ID {
		t.Fatalf("only-live exact consolidation goal=%+v err=%v", updated, err)
	}
	afterReplay, _, err := h.store.BuildGoalReplay(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"first-marker", "middle-marker-12", "pending-tool-marker", "pending-call", "after-tool-marker", "latest-marker"} {
		if !strings.Contains(string(afterReplay), marker) {
			t.Fatalf("exact consolidation lost %s replay=%s", marker, afterReplay)
		}
	}
	// Every item from the prior replay remains represented; the rewritten checkpoint
	// only removes physical segment fragmentation before appending the final turn.
	var beforeRoot, afterRoot map[string]interface{}
	if err = json.Unmarshal(beforeReplay, &beforeRoot); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(afterReplay, &afterRoot); err != nil {
		t.Fatal(err)
	}
	beforeInput, _ := beforeRoot["input"].([]interface{})
	afterInput, _ := afterRoot["input"].([]interface{})
	if len(afterInput) <= len(beforeInput) || !reflect.DeepEqual(beforeInput, afterInput[:len(beforeInput)]) {
		t.Fatalf("exact consolidation changed prior history before=%d after=%d", len(beforeInput), len(afterInput))
	}
	var afterBytes int64
	if err = h.store.DB().QueryRowContext(ctx, `SELECT storage_bytes FROM goal_session WHERE id=?`, goal.ID).Scan(&afterBytes); err != nil {
		t.Fatal(err)
	}
	if afterBytes >= beforeBytes {
		t.Fatalf("exact consolidation storage before/after=%d/%d, want physical reduction", beforeBytes, afterBytes)
	}
	var recovered int
	if err = h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log WHERE action='goal_storage_commit_recovered' AND reason='bounded_reclaim_exact_current_consolidation'`).Scan(&recovered); err != nil || recovered != 1 {
		t.Fatalf("exact consolidation audit=%d err=%v", recovered, err)
	}
}

func TestGoalContinuityScopesSharedKeyByNativeClientSession(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.app.cfg.GoalContinuityEnabled = true
	const sharedKeyHash = "shared-downstream-key-hash"

	persist := func(clientID, marker, responseID string) storage.GoalSession {
		t.Helper()
		body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"` + marker + `"}]}]}`)
		response := []byte(`{"id":"` + responseID + `","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer-` + marker + `"}]}]}`)
		req, err := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Thread-Id", "same-client-visible-thread")
		req.Header.Set("Session-Id", clientID)
		ctx := withDownstreamKey(withGoalOriginalBody(context.Background(), body), downstreamPolicy{KeyHash: sharedKeyHash})
		goal, err := h.app.persistGoalContinuity(ctx, req, "codex", body, response)
		if err != nil {
			t.Fatal(err)
		}
		return goal
	}

	clientA := persist("client-installation-a", "only-client-a", "response-client-a")
	clientB := persist("client-installation-b", "only-client-b", "response-client-b")
	if clientA.ID == clientB.ID {
		t.Fatalf("same API key and visible thread merged two client installations into goal %s", clientA.ID)
	}
	goals, err := h.store.ListGoalSessions(context.Background(), 10)
	if err != nil || len(goals) != 2 {
		t.Fatalf("client-scoped goals=%+v err=%v", goals, err)
	}

	current := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"continue-a"}]}]}`)
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(current)))
	req.Header.Set("Thread-Id", "same-client-visible-thread")
	req.Header.Set("Session-Id", "client-installation-a")
	ctx := withDownstreamKey(context.Background(), downstreamPolicy{KeyHash: sharedKeyHash})
	replay := h.app.goalReplayBody(ctx, req, "codex", current)
	if replay.Kind != goalResumeFound || replay.Session.ID != clientA.ID ||
		!strings.Contains(string(replay.Body), "only-client-a") ||
		strings.Contains(string(replay.Body), "only-client-b") {
		t.Fatalf("client A replay crossed namespace: kind=%s session=%s body=%s", replay.Kind, replay.Session.ID, replay.Body)
	}
}

func TestMessagesGoalPersistenceNeverStoresInjectedSuperInstructSystem(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.app.cfg.GoalContinuityEnabled = true
	const (
		keyHash   = "messages-clean-checkpoint-key"
		sessionID = "messages-clean-checkpoint-session"
	)
	original := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"keep the client task"}]}`)
	injected := []byte(`{"model":"claude-sonnet-4-6","system":"M1 BRIDGE\n\n# Super-Instruct Codex 5.6\n\nCYBER SKILL","messages":[{"role":"user","content":"keep the client task"}]}`)
	response := []byte(`{"id":"msg-clean-checkpoint","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(original)))
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	ctx := withDownstreamKey(withGoalOriginalBody(context.Background(), original), downstreamPolicy{KeyHash: keyHash})
	goal, err := h.app.persistGoalContinuity(ctx, req, "claude", injected, response)
	if err != nil {
		t.Fatal(err)
	}
	durable, _, err := h.store.BuildGoalReplay(context.Background(), goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(durable), legacySuperInstructBundleHeader) || strings.Contains(string(durable), "CYBER SKILL") {
		t.Fatalf("gateway-owned M1 system became durable: %s", durable)
	}
	if !strings.Contains(string(durable), "keep the client task") {
		t.Fatalf("client history was not retained: %s", durable)
	}
}

func TestMessagesGoalReplayScrubsHistoricalSuperInstructSystemWhenGateIsClosed(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.app.cfg.GoalContinuityEnabled = true
	const (
		keyHash   = "messages-legacy-checkpoint-key"
		sessionID = "messages-legacy-checkpoint-session"
	)
	legacy := []byte(`{"model":"claude-sonnet-4-6","system":"OLD M1 BRIDGE\n\n# Super-Instruct Codex 5.6\n\nLEGACY CYBER SKILL","messages":[{"role":"user","content":"historical task"}]}`)
	response := []byte(`{"id":"msg-legacy-checkpoint","type":"message","role":"assistant","content":[{"type":"text","text":"historical answer"}],"stop_reason":"end_turn"}`)
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(legacy)))
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	// No goalOriginalBody models a checkpoint written by a version that persisted
	// the already-injected upstream envelope.
	ctx := withDownstreamKey(context.Background(), downstreamPolicy{KeyHash: keyHash})
	if _, err := h.app.persistGoalContinuity(ctx, req, "claude", legacy, response); err != nil {
		t.Fatal(err)
	}

	current := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"continue safely"}]}`)
	currentReq, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(current)))
	currentReq.Header.Set("X-Claude-Code-Session-Id", sessionID)
	entitled := storage.Group{SuperInstructEnabled: true}
	masked := superInstructPolicyForClient(entitled, currentReq) // missing opt-in header closes the gate
	replayCtx := withRequestAccountGroupPolicy(withDownstreamKey(context.Background(), downstreamPolicy{KeyHash: keyHash}), masked)
	replay := h.app.goalReplayBody(replayCtx, currentReq, "claude", current)
	if replay.Kind != goalResumeFound {
		t.Fatalf("historical goal did not replay: %+v", replay)
	}
	for _, forbidden := range []string{legacySuperInstructBundleHeader, "LEGACY CYBER SKILL", "OLD M1 BRIDGE"} {
		if strings.Contains(string(replay.Body), forbidden) {
			t.Fatalf("closed gate replayed %q upstream: %s", forbidden, replay.Body)
		}
	}
	for _, retained := range []string{"historical task", "historical answer", "continue safely"} {
		if !strings.Contains(string(replay.Body), retained) {
			t.Fatalf("durable client history %q was lost: %s", retained, replay.Body)
		}
	}
}

func TestGoalNamespaceUsesNativeClaudeAndCodexHeadersWithoutExtraConfig(t *testing.T) {
	const keyHash = "shared-native-header-key"
	claudeA, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	claudeB, _ := http.NewRequest(http.MethodPost, "/v1/messages", nil)
	claudeA.Header.Set("X-Claude-Code-Session-Id", "claude-native-a")
	claudeB.Header.Set("X-Claude-Code-Session-Id", "claude-native-b")
	claudeScopeA := downstreamClientScope(keyHash, claudeA)
	claudeScopeB := downstreamClientScope(keyHash, claudeB)
	if claudeScopeA == "" || claudeScopeB == "" || claudeScopeA == claudeScopeB {
		t.Fatalf("Claude Code native sessions were not isolated: a=%q b=%q", claudeScopeA, claudeScopeB)
	}

	codexA, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	codexB, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	codexA.Header.Set("Thread-Id", "codex-native-thread-a")
	codexB.Header.Set("Thread-Id", "codex-native-thread-b")
	codexScopeA := downstreamClientScope(keyHash, codexA)
	codexScopeB := downstreamClientScope(keyHash, codexB)
	if codexScopeA == "" || codexScopeB == "" || codexScopeA == codexScopeB {
		t.Fatalf("Codex native thread fallback was not isolated: a=%q b=%q", codexScopeA, codexScopeB)
	}
	for _, scope := range []string{claudeScopeA, claudeScopeB, codexScopeA, codexScopeB} {
		if strings.Contains(scope, "native-") {
			t.Fatalf("raw protocol-native identity leaked into scope: %q", scope)
		}
	}
}

func TestGoalClientNamespaceMigratesOnlyExactLegacyStateAlias(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.app.cfg.GoalContinuityEnabled = true
	ctx := context.Background()
	legacy, err := h.store.CommitGoalTurn(ctx, storage.GoalTurn{
		Protocol:          "codex",
		DownstreamKeyHash: "legacy-shared-key",
		WorkspaceHash:     "legacy-workspace",
		InitialGoalHash:   "legacy-initial",
		ResponseID:        "legacy-exact-response",
		Aliases:           []storage.GoalAlias{{Type: "codex_root_thread", Value: "legacy-visible-root"}},
		CheckpointPayload: `{"model":"gpt-5.6-sol","input":[]}`,
		SegmentPayload:    `{"history_key":"input","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"legacy durable task"}]}],"output":[]}`,
		ExpiresAt:         storage.Now() + 3600,
		StorageMaxBytes:   8 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"gpt-5.6-sol","previous_response_id":"legacy-exact-response","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"migrate exact state"}]}]}`)
	response := []byte(`{"id":"namespaced-response","status":"completed","output":[]}`)
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	req.Header.Set("Thread-Id", "legacy-visible-root")
	req.Header.Set("Session-Id", "upgraded-installation")
	requestCtx := withDownstreamKey(withGoalOriginalBody(ctx, body), downstreamPolicy{KeyHash: "legacy-shared-key"})
	migrated, err := h.app.persistGoalContinuity(requestCtx, req, "codex", body, response)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.ID != legacy.ID {
		t.Fatalf("exact legacy response did not migrate in place: old=%s new=%s", legacy.ID, migrated.ID)
	}

	// Once rebound, the scoped root works without a legacy response pointer.
	next := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"scoped next"}]}]}`)
	nextReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(next)))
	nextReq.Header.Set("Thread-Id", "legacy-visible-root")
	nextReq.Header.Set("Session-Id", "upgraded-installation")
	replay := h.app.goalReplayBody(withDownstreamKey(ctx, downstreamPolicy{KeyHash: "legacy-shared-key"}), nextReq, "codex", next)
	if replay.Kind != goalResumeFound || replay.Session.ID != legacy.ID {
		t.Fatalf("scoped alias was not rebound after migration: %+v", replay)
	}

	// A different installation with only the weak/root alias never crosses the
	// one-time exact-state bridge.
	otherReq, _ := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(next)))
	otherReq.Header.Set("Thread-Id", "legacy-visible-root")
	otherReq.Header.Set("Session-Id", "different-installation")
	other := h.app.goalReplayBody(withDownstreamKey(ctx, downstreamPolicy{KeyHash: "legacy-shared-key"}), otherReq, "codex", next)
	if other.Kind != goalResumeUnidentified {
		t.Fatalf("weak legacy alias crossed client namespaces: %+v", other)
	}
}

func TestClaudeCodeCompactionReplacesDurableGoalHistory(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.app.cfg.GoalContinuityEnabled = true

	request := func(body []byte) *http.Request {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Claude-Code-Session-Id", "claude-compaction-session")
		return req
	}
	ctxFor := func(body []byte) context.Context {
		return withDownstreamKey(withGoalOriginalBody(context.Background(), body), downstreamPolicy{KeyHash: "claude-compaction-key"})
	}

	firstBody := []byte(`{"model":"claude-opus-5","system":"You are Claude Code","messages":[{"role":"user","content":"old detail that must be summarized"}]}`)
	firstResponse := []byte(`{"id":"msg-before-compact","type":"message","role":"assistant","content":[{"type":"text","text":"old detailed answer"}],"stop_reason":"end_turn"}`)
	goal, err := h.app.persistGoalContinuity(ctxFor(firstBody), request(firstBody), "claude", firstBody, firstResponse)
	if err != nil {
		t.Fatal(err)
	}

	compactBody := []byte(`{
		"model":"claude-opus-5",
		"system":"You are a helpful AI assistant tasked with summarizing conversations.",
		"messages":[
			{"role":"user","content":"old detail that must be summarized"},
			{"role":"assistant","content":[{"type":"text","text":"old detailed answer"}]},
			{"role":"user","content":"Your task is to create a detailed summary of the conversation so far. REMINDER: Do NOT call any tools. Respond with plain text only"}
		]
	}`)
	compactResponse := []byte(`{"id":"msg-compact","type":"message","role":"assistant","content":[{"type":"text","text":"bounded durable summary"}],"stop_reason":"end_turn"}`)
	compacted, err := h.app.persistGoalContinuity(ctxFor(compactBody), request(compactBody), "claude", compactBody, compactResponse)
	if err != nil {
		t.Fatal(err)
	}
	if compacted.ID != goal.ID {
		t.Fatalf("compaction opened a new goal: before=%s after=%s", goal.ID, compacted.ID)
	}
	replay, session, err := h.store.BuildGoalReplay(context.Background(), goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	text := string(replay)
	if !strings.Contains(text, "bounded durable summary") ||
		strings.Contains(text, "old detail that must be summarized") ||
		strings.Contains(text, "old detailed answer") {
		t.Fatalf("Claude compaction did not replace old durable history: %s", replay)
	}
	var checkpoints, segments int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM goal_checkpoint WHERE goal_id=?`, session.ID).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM goal_segment WHERE goal_id=?`, session.ID).Scan(&segments); err != nil {
		t.Fatal(err)
	}
	if checkpoints != 1 || segments != 1 {
		t.Fatalf("compaction retained superseded physical history checkpoints=%d segments=%d", checkpoints, segments)
	}
}

func TestIncrementalGoalRequestRecognizesPostCompactLiteSnapshot(t *testing.T) {
	durable := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"old_tool"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"old base"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"retained task"}]},{"type":"compaction","encrypted_content":"opaque"}]}`)
	current := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"new_tool","format":{"const":900719925474099312345}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"new base"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"retained task"}]},{"type":"compaction","id":"new-id","encrypted_content":"opaque"},{"type":"message","role":"user","content":[{"type":"input_text","text":"current turn"}]}]}`)
	trimmed, replace := incrementalGoalRequestWithMode(current, durable)
	if !replace || string(trimmed) != string(current) {
		t.Fatalf("post-compact snapshot replace=%v body=%s", replace, trimmed)
	}

	durableRoot, err := decodeContextJSONMap(durable)
	if err != nil {
		t.Fatal(err)
	}
	currentRoot, err := decodeContextJSONMap(current)
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeCodexGoalReplayInput(durableRoot["input"], currentRoot["input"])
	encoded, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, marker := range []string{"new_tool", "new base", "retained task", "opaque", "current turn"} {
		if strings.Count(text, marker) != 1 {
			t.Fatalf("marker %q count=%d input=%s", marker, strings.Count(text, marker), text)
		}
	}
	if strings.Contains(text, "old_tool") || strings.Contains(text, "old base") || !strings.Contains(text, "900719925474099312345") {
		t.Fatalf("Lite prefix was not replaced exactly: %s", text)
	}
}

func TestCodexRemoteCompactionPersistenceUsesDurableLogicalInput(t *testing.T) {
	durable := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"base"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"durable real task"}]},{"type":"reasoning","encrypted_content":"old reasoning"}]}`)
	incremental := []byte(`{"model":"gpt-5.6-sol","previous_response_id":"resp-before-compact","input":[{"type":"compaction_trigger"}]}`)
	response := []byte(`{"id":"resp-compact","status":"completed","output":[{"type":"message","role":"assistant","content":"ignored"},{"type":"compaction","encrypted_content":"opaque-v2"}]}`)
	replacement, ok := codexRemoteCompactionReplacement(durable, incremental, response, false)
	if !ok || len(replacement) != 2 {
		t.Fatalf("replacement=%+v ok=%v", replacement, ok)
	}
	encoded, _ := json.Marshal(replacement)
	if !strings.Contains(string(encoded), "durable real task") || !strings.Contains(string(encoded), "opaque-v2") || strings.Contains(string(encoded), "old reasoning") || strings.Contains(string(encoded), "base") {
		t.Fatalf("logical compact replacement=%s", encoded)
	}

	fullWithPrevious := []byte(`{"model":"gpt-5.6-sol","previous_response_id":"resp-before-compact","input":[{"type":"additional_tools","role":"developer","tools":[]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"base"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"durable real task"}]},{"type":"reasoning","encrypted_content":"old reasoning"},{"type":"compaction_trigger"}]}`)
	replacement, ok = codexRemoteCompactionReplacement(durable, fullWithPrevious, response, false)
	if !ok {
		t.Fatal("full input with previous_response_id was not recognized as RemoteCompactionV2")
	}
	encoded, _ = json.Marshal(replacement)
	if strings.Count(string(encoded), "durable real task") != 1 || strings.Count(string(encoded), "opaque-v2") != 1 {
		t.Fatalf("full logical input was prefixed with durable history twice: %s", encoded)
	}

	for _, invalid := range [][]byte{
		[]byte(`{"id":"resp-partial","output":[{"type":"compaction","encrypted_content":"must-not-install"}]}`),
		[]byte(`{"id":"resp-failed","status":"failed","output":[{"type":"compaction","encrypted_content":"must-not-install"}]}`),
	} {
		if replacement, accepted := codexRemoteCompactionReplacement(durable, incremental, invalid, false); accepted || replacement != nil {
			t.Fatalf("non-completed compaction installed replacement=%+v accepted=%v response=%s", replacement, accepted, invalid)
		}
	}
}

func TestPersistGoalContinuityFullRootRemoteCompactionRetainsLogicalUsers(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.app.cfg.GoalContinuityEnabled = true
	ctx := context.Background()

	request := func(body []byte) *http.Request {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("thread-id", "compact-full-root-thread")
		return req
	}

	firstBody := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[
			{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"apply_patch"}]},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"current base instructions"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"durable real task"}]}
		]
	}`)
	firstResponse := []byte(`{
		"id":"resp-before-full-root-compact",
		"status":"completed",
		"output":[
			{"type":"reasoning","encrypted_content":"old reasoning"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]}
		]
	}`)
	firstCtx := withGoalOriginalBody(ctx, firstBody)
	goal, err := h.app.persistGoalContinuity(firstCtx, request(firstBody), "codex", firstBody, firstResponse)
	if err != nil {
		t.Fatal(err)
	}

	durable, _, err := h.store.BuildGoalReplay(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	fullRoot, err := decodeContextJSONMap(durable)
	if err != nil {
		t.Fatal(err)
	}
	fullRoot["input"] = append(appendItems(nil, fullRoot["input"]), map[string]interface{}{"type": "compaction_trigger"})
	delete(fullRoot, "previous_response_id")
	fullRootBody, err := json.Marshal(fullRoot)
	if err != nil {
		t.Fatal(err)
	}
	compactResponse := []byte(`{
		"id":"resp-full-root-compact",
		"status":"completed",
		"output":[
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ignored side output"}]},
			{"type":"compaction","encrypted_content":"opaque-full-root"}
		]
	}`)
	compactCtx := withGoalOriginalBody(ctx, fullRootBody)
	goal, err = h.app.persistGoalContinuity(compactCtx, request(fullRootBody), "codex", fullRootBody, compactResponse)
	if err != nil {
		t.Fatal(err)
	}

	replay, _, err := h.store.BuildGoalReplay(ctx, goal.ID)
	if err != nil {
		t.Fatal(err)
	}
	text := string(replay)
	for _, retained := range []string{"apply_patch", "current base instructions", "durable real task", "opaque-full-root"} {
		if strings.Count(text, retained) != 1 {
			t.Fatalf("retained marker %q count=%d replay=%s", retained, strings.Count(text, retained), text)
		}
	}
	for _, compacted := range []string{"old reasoning", "old answer", "ignored side output", "compaction_trigger"} {
		if strings.Contains(text, compacted) {
			t.Fatalf("compacted marker %q remained in replay=%s", compacted, text)
		}
	}
	var checkpoints, segments int
	if err := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_checkpoint WHERE goal_id=?`, goal.ID).Scan(&checkpoints); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM goal_segment WHERE goal_id=?`, goal.ID).Scan(&segments); err != nil {
		t.Fatal(err)
	}
	if checkpoints != 1 || segments != 1 {
		t.Fatalf("Codex compaction retained superseded physical history checkpoints=%d segments=%d", checkpoints, segments)
	}
}

func TestPersistGoalContinuityRejectedRemoteCompactionKeepsDurableHistory(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		response string
	}{
		{
			name:     "missing status",
			response: `{"id":"resp-statusless","output":[{"type":"compaction","encrypted_content":"must-not-install"}]}`,
		},
		{
			name:     "failed status",
			response: `{"id":"resp-failed","status":"failed","output":[{"type":"compaction","encrypted_content":"must-not-install"}]}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
			h.app.cfg.GoalContinuityEnabled = true
			ctx := context.Background()
			threadID := "rejected-compact-" + strings.ReplaceAll(testCase.name, " ", "-")
			request := func(body []byte) *http.Request {
				t.Helper()
				req, err := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("thread-id", threadID)
				return req
			}

			firstBody := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"history must survive"}]}]}`)
			firstResponse := []byte(`{"id":"resp-before-rejected-compact","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer must survive"}]}]}`)
			firstCtx := withGoalOriginalBody(ctx, firstBody)
			goal, err := h.app.persistGoalContinuity(firstCtx, request(firstBody), "codex", firstBody, firstResponse)
			if err != nil {
				t.Fatal(err)
			}

			trigger := []byte(`{"model":"gpt-5.6-sol","previous_response_id":"resp-before-rejected-compact","input":[{"type":"compaction_trigger"}]}`)
			triggerCtx := withGoalOriginalBody(ctx, trigger)
			goal, err = h.app.persistGoalContinuity(triggerCtx, request(trigger), "codex", trigger, []byte(testCase.response))
			if err != nil {
				t.Fatal(err)
			}
			replay, _, err := h.store.BuildGoalReplay(ctx, goal.ID)
			if err != nil {
				t.Fatal(err)
			}
			text := string(replay)
			for _, marker := range []string{"history must survive", "answer must survive"} {
				if strings.Count(text, marker) != 1 {
					t.Fatalf("rejected compaction changed %q count=%d replay=%s", marker, strings.Count(text, marker), text)
				}
			}
		})
	}
}

func TestGoalReplayRestoresCurrentLiteToolAndReasoningContract(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()
	_, err := h.store.CommitGoalTurn(ctx, storage.GoalTurn{
		Protocol:          "codex",
		DownstreamKeyHash: "lite-key",
		WorkspaceHash:     "lite-workspace",
		InitialGoalHash:   "lite-initial",
		ResponseID:        "resp-lite-before-migration",
		CheckpointPayload: `{"model":"gpt-5.6-sol","instructions":"stale top instructions","tools":[{"type":"custom","name":"stale_top_tool"}],"service_tier":"stale-tier","input":[]}`,
		SegmentPayload: `{
			"history_key":"input",
			"input":[
				{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"old_tool"}]},
				{"type":"message","role":"developer","content":[{"type":"input_text","text":"old base"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"durable task"}]}
			],
			"output":[{"type":"custom_tool_call","call_id":"call-lite","name":"apply_patch","input":"{}"}]
		}`,
		ExpiresAt:       storage.Now() + 3600,
		StorageMaxBytes: 8 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	current := []byte(`{
		"model":"gpt-5.6-sol",
		"previous_response_id":"resp-lite-before-migration",
		"reasoning":{"effort":"max","context":"all_turns"},
		"parallel_tool_calls":false,
		"tool_choice":"auto",
		"text":{"verbosity":"high"},
		"future_current_field":{"keep":true},
		"input":[
			{"type":"additional_tools","role":"developer","tools":[{"type":"custom","name":"apply_patch","format":{"const":900719925474099312345}}]},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"current base"}]},
			{"type":"custom_tool_call_output","call_id":"call-lite","output":"tool completed"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue now"}]}
		]
	}`)
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(current)))
	if err != nil {
		t.Fatal(err)
	}
	replay := h.app.goalReplayBody(ctx, req, "codex", current)
	if replay.Kind != goalResumeFound {
		t.Fatalf("replay=%+v", replay)
	}
	text := string(replay.Body)
	for _, marker := range []string{"apply_patch", "900719925474099312345", "current base", "durable task", "call-lite", "tool completed", "continue now", `"effort":"max"`, `"context":"all_turns"`, `"future_current_field":{"keep":true}`} {
		if !strings.Contains(text, marker) {
			t.Fatalf("missing %q in replay=%s", marker, text)
		}
	}
	if strings.Contains(text, "stale_top_tool") || strings.Contains(text, "stale top instructions") || strings.Contains(text, "stale-tier") || strings.Contains(text, "old_tool") || strings.Contains(text, "old base") || strings.Contains(text, "previous_response_id") {
		t.Fatalf("stale or account-local context leaked: %s", text)
	}
	if strings.Index(text, `"type":"additional_tools"`) > strings.Index(text, "durable task") || strings.Index(text, `"type":"custom_tool_call"`) > strings.Index(text, `"type":"custom_tool_call_output"`) {
		t.Fatalf("Lite prefix or tool pair order is invalid: %s", text)
	}
}

// skipLegacyCodexGoalReplay marks tests for the v1 Codex goal/checkpoint engine.
// CPA-v2 deliberately keeps Codex context only in the original upstream session;
// equivalent mapping/EOF/tool-result coverage lives in codex_session_mapping_test.go.
func skipLegacyCodexGoalReplay(t *testing.T) {
	t.Helper()
	t.Skip("Codex goal/journal replay was retired in favor of strict CPA-v2 mapping")
}

func TestGoalContinuityRebuildsResponseAliasAfterRestartStyleRequest(t *testing.T) {
	skipLegacyCodexGoalReplay(t)
	var upstreamCalls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if upstreamCalls.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"id":"resp_goal_v2","object":"response","status":"completed","model":"gpt","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_goal_v2_next","object":"response","status":"completed","model":"gpt","output":[{"type":"message","content":[{"type":"output_text","text":"next done"}]}]}`)
	})
	h.importAccount(t, "goal-v2", "upstream-goal-v2", "access-goal-v2")
	first := `{"model":"gpt","input":[{"role":"user","content":"durable task"}]}`
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d", resp.StatusCode)
	}
	goals, err := h.store.ListGoalSessions(context.Background(), 10)
	if err != nil || len(goals) != 1 {
		t.Fatalf("goals=%+v err=%v", goals, err)
	}
	adminResp, err := http.Get(h.pool.URL + "/admin/goals")
	if err != nil {
		t.Fatal(err)
	}
	adminBody, _ := io.ReadAll(adminResp.Body)
	adminResp.Body.Close()
	if adminResp.StatusCode != http.StatusOK || strings.Contains(string(adminBody), "durable task") || !strings.Contains(string(adminBody), goals[0].ID) {
		t.Fatalf("safe admin goal listing status=%d body=%s", adminResp.StatusCode, adminBody)
	}
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","previous_response_id":"resp_goal_v2","input":[{"role":"user","content":"continue"}]}`))
	replay := h.app.goalReplayBody(context.Background(), request, "codex", []byte(`{"model":"gpt","previous_response_id":"resp_goal_v2","input":[{"role":"user","content":"continue"}]}`))
	if replay.Kind != goalResumeFound || !strings.Contains(string(replay.Body), "durable task") || !strings.Contains(string(replay.Body), "continue") {
		t.Fatalf("goal replay=%+v body=%s", replay, replay.Body)
	}
	second, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","previous_response_id":"resp_goal_v2","input":[{"role":"user","content":"continue"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("resume status=%d", second.StatusCode)
	}
	goals, err = h.store.ListGoalSessions(context.Background(), 10)
	if err != nil || len(goals) != 1 {
		t.Fatalf("resume must advance original goal, got goals=%+v err=%v", goals, err)
	}
	resolved, err := h.store.ResolveGoalAliases(context.Background(), []storage.GoalAlias{{Type: "response_id", Value: "resp_goal_v2_next"}})
	if err != nil || resolved.Session.ID != goals[0].ID {
		t.Fatalf("new response alias=%+v err=%v", resolved, err)
	}
}

func TestGoalContinuityAcceptsCustomToolCallOutputOnResume(t *testing.T) {
	skipLegacyCodexGoalReplay(t)
	var upstreamCalls atomic.Int32
	var pairedReplay atomic.Bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if upstreamCalls.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"id":"resp_goal_custom","object":"response","status":"completed","model":"gpt","output":[{"type":"custom_tool_call","call_id":"call_goal_custom","name":"patch","input":"{}"}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		callIndex := strings.Index(string(body), `"type":"custom_tool_call"`)
		outputIndex := strings.Index(string(body), `"type":"custom_tool_call_output"`)
		pairedReplay.Store(callIndex >= 0 && outputIndex > callIndex)
		_, _ = io.WriteString(w, `{"id":"resp_goal_custom_done","object":"response","status":"completed","model":"gpt","output":[{"type":"message","content":[{"type":"output_text","text":"patched"}]}]}`)
	})
	h.importAccount(t, "goal-custom", "upstream-goal-custom", "access-goal-custom")
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":[{"role":"user","content":"tool task"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	goals, err := h.store.ListGoalSessions(context.Background(), 10)
	if err != nil || len(goals) != 1 || goals[0].State != "awaiting_tool_result" {
		t.Fatalf("custom tool call must persist awaiting state, goals=%+v err=%v", goals, err)
	}
	current := []byte(`{"model":"gpt","previous_response_id":"resp_goal_custom","input":[{"type":"custom_tool_call_output","call_id":"call_goal_custom","output":"ok"}]}`)
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(string(current)))
	replay := h.app.goalReplayBody(context.Background(), request, "codex", current)
	if replay.Kind != goalResumeFound || !strings.Contains(string(replay.Body), "custom_tool_call_output") || !strings.Contains(string(replay.Body), "custom_tool_call") {
		t.Fatalf("custom tool goal replay=%+v body=%s", replay, replay.Body)
	}
	second, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(string(current)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusOK || upstreamCalls.Load() != 2 || !pairedReplay.Load() {
		t.Fatalf("paired custom tool replay status=%d upstreamCalls=%d paired=%v", second.StatusCode, upstreamCalls.Load(), pairedReplay.Load())
	}
	goals, err = h.store.ListGoalSessions(context.Background(), 10)
	if err != nil || len(goals) != 1 || goals[0].State != "ready" {
		t.Fatalf("completed tool result must advance goal state, goals=%+v err=%v", goals, err)
	}
}

func TestGoalContinuityRejectsUnpairedCustomToolCallBeforeUpstream(t *testing.T) {
	skipLegacyCodexGoalReplay(t)
	var upstreamCalls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if upstreamCalls.Add(1) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"No tool output found for custom tool call call_goal_pending."},"status":400}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_goal_pending","object":"response","status":"completed","model":"gpt","output":[{"type":"custom_tool_call","call_id":"call_goal_pending","name":"patch","input":"{}"}]}`)
	})
	h.importAccount(t, "goal-pending", "upstream-goal-pending", "access-goal-pending")
	first, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":[{"role":"user","content":"make a patch"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first status=%d", first.StatusCode)
	}
	goals, err := h.store.ListGoalSessions(context.Background(), 10)
	if err != nil || len(goals) != 1 || goals[0].State != "awaiting_tool_result" {
		t.Fatalf("pending custom call state goals=%+v err=%v", goals, err)
	}

	// Emulate a checkpoint written by the version that did not recognize
	// custom_tool_call.  Its session says ready, but the segment still contains the
	// call and must be protected by the reconstruction-time pairing check.
	if _, err := h.store.DB().ExecContext(context.Background(), `UPDATE goal_session SET state='ready' WHERE id=?`, goals[0].ID); err != nil {
		t.Fatal(err)
	}
	resume := `{"model":"gpt","stream":true,"previous_response_id":"resp_goal_pending","input":[{"role":"user","content":"continue without tool output"}]}`
	request, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(resume))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-MiCliProxy-Goal-Error") != "goal_resume_requires_tool_result" ||
		!strings.Contains(string(body), "response.failed") || !strings.Contains(string(body), "goal_resume_requires_tool_result") {
		t.Fatalf("missing tool result must be a visible SSE terminal status=%d headers=%v body=%s", response.StatusCode, response.Header, body)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("unpaired custom call was forwarded upstream %d times", upstreamCalls.Load())
	}
}

func TestGoalContinuityStreamCustomToolCallRequiresResultBeforeResume(t *testing.T) {
	skipLegacyCodexGoalReplay(t)
	var upstreamCalls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if upstreamCalls.Add(1) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"No tool output found for custom tool call call_goal_stream_pending."},"status":400}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_goal_stream_pending\",\"model\":\"gpt\"}}\n\n"+
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"custom_tool_call\",\"call_id\":\"call_goal_stream_pending\",\"name\":\"patch\",\"input\":\"{}\"}}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_goal_stream_pending\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt\"}}\n\n")
	})
	h.importAccount(t, "goal-stream-pending", "upstream-goal-stream-pending", "access-goal-stream-pending")
	first, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"input":[{"role":"user","content":"make a streamed patch"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	firstBody, _ := io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK || !strings.Contains(string(firstBody), "response.completed") {
		t.Fatalf("streamed first turn status=%d body=%s", first.StatusCode, firstBody)
	}
	goals, err := h.store.ListGoalSessions(context.Background(), 10)
	if err != nil || len(goals) != 1 || goals[0].State != "awaiting_tool_result" {
		t.Fatalf("streamed custom call must persist awaiting state, goals=%+v err=%v", goals, err)
	}
	resume := `{"model":"gpt","stream":true,"previous_response_id":"resp_goal_stream_pending","input":[{"role":"user","content":"continue without tool output"}]}`
	second, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(resume))
	if err != nil {
		t.Fatal(err)
	}
	secondBody, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusOK || second.Header.Get("X-MiCliProxy-Goal-Error") != "goal_resume_requires_tool_result" ||
		!strings.Contains(string(secondBody), "response.failed") || !strings.Contains(string(secondBody), "goal_resume_requires_tool_result") {
		t.Fatalf("streamed missing result terminal status=%d headers=%v body=%s", second.StatusCode, second.Header, secondBody)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("streamed unpaired custom call was forwarded upstream %d times", upstreamCalls.Load())
	}
}

func TestGoalContinuitySendsBoundedContinueBeforeSynthesizingEOFError(t *testing.T) {
	skipLegacyCodexGoalReplay(t)
	var calls atomic.Int32
	var continuationInstruction atomic.Bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_eof_first\",\"model\":\"gpt\"}}\n\n"+
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"partial \"}\n\n")
			return
		}
		continuationInstruction.Store(strings.Contains(string(body), "Please continue from exactly where you left off"))
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_eof_second\",\"model\":\"gpt\"}}\n\n"+
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\"}}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"continued\"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_eof_second\",\"object\":\"response\",\"status\":\"completed\",\"output_text\":\"partial continued\"}}\n\n")
	})
	h.importAccount(t, "goal-eof", "upstream-goal-eof", "access-goal-eof")
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"input":[{"role":"user","content":"long task"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	result, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 2 || !continuationInstruction.Load() || !strings.Contains(string(result), "response.completed") || strings.Contains(string(result), "response.failed") {
		t.Fatalf("bounded EOF continuation status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), result)
	}
	resume := []byte(`{"model":"gpt","previous_response_id":"resp_eof_second","input":[{"role":"user","content":"next"}]}`)
	request, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(string(resume)))
	replay := h.app.goalReplayBody(context.Background(), request, "codex", resume)
	if replay.Kind != goalResumeFound || !strings.Contains(string(replay.Body), "long task") || !strings.Contains(string(replay.Body), "continued") {
		t.Fatalf("continuation terminal was not persisted replay=%+v body=%s", replay, replay.Body)
	}
}

func TestGoalContinuityDoesNotContinueAQuietLongPollThatLaterTerminates(t *testing.T) {
	var calls atomic.Int32
	var continuationSeen atomic.Bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if calls.Add(1) != 1 {
			continuationSeen.Store(strings.Contains(string(body), "Please continue from exactly where you left off"))
			t.Fatalf("quiet long poll must not issue a continuation request: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_quiet_long_poll\",\"model\":\"gpt\"}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// This intentionally exceeds the configured keepalive interval. It models an
		// upstream long-poll/task turn that is still alive but has no payload to emit.
		time.Sleep(1200 * time.Millisecond)
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_quiet_long_poll\",\"object\":\"response\",\"status\":\"completed\",\"output_text\":\"finished after polling\"}}\n\n")
	})
	h.app.cfg.StreamKeepAliveSeconds = 1
	h.importAccount(t, "goal-quiet-poll", "upstream-goal-quiet-poll", "access-goal-quiet-poll")

	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"input":[{"role":"user","content":"wait for task"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	result, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 1 || continuationSeen.Load() ||
		!strings.Contains(string(result), "response.in_progress") || !strings.Contains(string(result), "response.completed") || strings.Contains(string(result), "response.failed") {
		t.Fatalf("quiet long poll status=%d calls=%d continuation=%v body=%s", resp.StatusCode, calls.Load(), continuationSeen.Load(), result)
	}
}

func TestGoalContinuityMarksVisibleUpstreamFailureRetryableWithoutSecondTerminal(t *testing.T) {
	skipLegacyCodexGoalReplay(t)
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_goal_before_failure","object":"response","status":"completed","model":"gpt","output":[{"type":"message","content":[{"type":"output_text","text":"checkpoint"}]}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_goal_failure\",\"model\":\"gpt\"}}\n\n"+
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_goal_failure\",\"object\":\"response\",\"status\":\"failed\",\"error\":{\"code\":\"upstream_failed\"}}}\n\n")
	})
	h.importAccount(t, "goal-visible-failure", "upstream-goal-visible-failure", "access-goal-visible-failure")
	post := func(body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("thread-id", "goal-visible-failure-thread")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		result, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(result)
	}
	if status, _ := post(`{"model":"gpt","input":"first"}`); status != http.StatusOK {
		t.Fatalf("checkpoint turn status=%d", status)
	}
	status, result := post(`{"model":"gpt","stream":true,"input":"resume"}`)
	if status != http.StatusOK || calls.Load() != 2 || strings.Count(result, "event: response.failed") != 1 || strings.Contains(result, "event: response.completed") {
		t.Fatalf("visible failure status=%d calls=%d body=%s", status, calls.Load(), result)
	}
	resolved, err := h.store.ResolveGoalAliases(context.Background(), []storage.GoalAlias{{Type: "codex_root_thread", Value: "goal-visible-failure-thread"}})
	if err != nil || resolved.Session.State != "retryable" {
		t.Fatalf("visible failed terminal must retain retryable checkpoint session=%+v err=%v", resolved.Session, err)
	}
}

func TestGoalContinuitySeparatesConcurrentCLIThreadsWithSharedWeakSession(t *testing.T) {
	skipLegacyCodexGoalReplay(t)
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		kind := "unknown"
		for _, candidate := range []string{"alpha-start", "beta-start", "alpha-next", "beta-next"} {
			if strings.Contains(string(body), candidate) {
				kind = candidate
				break
			}
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"resp_%s","object":"response","status":"completed","model":"gpt","output":[{"type":"message","content":[{"type":"output_text","text":"answer-%s"}]}]}`,
			strings.ReplaceAll(kind, "-", "_"), kind)
	})
	h.importAccount(t, "goal-cli-isolation", "upstream-goal-cli-isolation", "access-goal-cli-isolation")

	post := func(thread, body string) (int, string, error) {
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			return 0, "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("thread-id", thread)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, "", err
		}
		result, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(result), nil
	}

	// These represent independent Codex CLI windows. Some clients expose a common
	// process-level session_id while their thread-id is the actual conversation key.
	// The weak shared marker must never cause their goal chains or leases to merge.
	if status, body, err := post("cli-thread-alpha", `{"model":"gpt","session_id":"shared-cli-process","input":"alpha-start"}`); err != nil || status != http.StatusOK || !strings.Contains(body, "resp_alpha_start") {
		t.Fatalf("alpha initial status=%d body=%s", status, body)
	}
	if status, body, err := post("cli-thread-beta", `{"model":"gpt","session_id":"shared-cli-process","input":"beta-start"}`); err != nil || status != http.StatusOK || !strings.Contains(body, "resp_beta_start") {
		t.Fatalf("beta initial status=%d body=%s", status, body)
	}
	goals, err := h.store.ListGoalSessions(context.Background(), 10)
	if err != nil || len(goals) != 2 {
		t.Fatalf("shared weak session merged independent CLI threads: goals=%+v err=%v", goals, err)
	}

	type result struct {
		name   string
		status int
		body   string
		err    error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for _, tc := range []struct {
		name, thread, body string
	}{
		{"alpha", "cli-thread-alpha", `{"model":"gpt","session_id":"shared-cli-process","previous_response_id":"resp_alpha_start","input":"alpha-next"}`},
		{"beta", "cli-thread-beta", `{"model":"gpt","session_id":"shared-cli-process","previous_response_id":"resp_beta_start","input":"beta-next"}`},
	} {
		wg.Add(1)
		go func(tc struct{ name, thread, body string }) {
			defer wg.Done()
			status, body, err := post(tc.thread, tc.body)
			results <- result{name: tc.name, status: status, body: body, err: err}
		}(tc)
	}
	wg.Wait()
	close(results)
	for got := range results {
		if got.err != nil || got.status != http.StatusOK || strings.Contains(got.body, "goal_in_progress") || strings.Contains(got.body, "goal_resume_ambiguous") {
			t.Fatalf("%s concurrent resume stalled status=%d body=%s", got.name, got.status, got.body)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("upstream calls=%d, want four isolated turns", calls.Load())
	}
	for _, request := range h.requests() {
		switch {
		case strings.Contains(request.Body, "alpha-next"):
			if !strings.Contains(request.Body, "alpha-start") || strings.Contains(request.Body, "beta-start") {
				t.Fatalf("alpha replay crossed into beta: %s", request.Body)
			}
		case strings.Contains(request.Body, "beta-next"):
			if !strings.Contains(request.Body, "beta-start") || strings.Contains(request.Body, "alpha-start") {
				t.Fatalf("beta replay crossed into alpha: %s", request.Body)
			}
		}
	}
}

func TestGoalContinuityRebuildsClaudeSessionIntoNativeMessages(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"id":"msg_goal_claude_1","type":"message","role":"assistant","model":"claude-sonnet-4.6","content":[{"type":"text","text":"first answer"}],"stop_reason":"end_turn"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg_goal_claude_2","type":"message","role":"assistant","model":"claude-sonnet-4.6","content":[{"type":"text","text":"second answer"}],"stop_reason":"end_turn"}`)
	})
	account := storage.Account{ID: "goal-claude", Label: "goal-claude", GroupName: "cyber", Provider: "claude", Status: "active"}
	if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{AccessToken: "sk-ant-api-goal"}); err != nil {
		t.Fatal(err)
	}
	post := func(body string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Pool-Provider", "claude")
		req.Header.Set("X-Claude-Code-Session-Id", "goal-claude-session")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	first := post(`{"model":"claude-sonnet-4-6","system":"keep this system","session_id":"goal-claude-session","messages":[{"role":"user","content":"first question"}]}`)
	_, _ = io.ReadAll(first.Body)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first claude status=%d", first.StatusCode)
	}
	second := post(`{"model":"claude-sonnet-4-6","session_id":"goal-claude-session","messages":[{"role":"user","content":"second question"}]}`)
	_, _ = io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusOK || second.Header.Get("X-MiCliProxy-Context-Status") != "rebuilt" || calls.Load() != 2 {
		t.Fatalf("second claude status=%d headers=%v calls=%d", second.StatusCode, second.Header, calls.Load())
	}
	requests := h.requests()
	if len(requests) != 2 || !strings.Contains(requests[1].Body, "first question") || !strings.Contains(requests[1].Body, "first answer") || !strings.Contains(requests[1].Body, "second question") || !strings.Contains(requests[1].Body, "keep this system") {
		t.Fatalf("claude continuation did not receive rebuilt native messages: %+v", requests)
	}
}

func TestGoalContinuityPersistsClaudeBoundedContinuation(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_goal_stream_first\",\"role\":\"assistant\"}}\n\n"+
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial \"}}\n\n")
			return
		}
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_goal_stream_second\",\"role\":\"assistant\"}}\n\n"+
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"+
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"continued\"}}\n\n"+
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"+
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"+
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	account := storage.Account{ID: "goal-claude-stream", Label: "goal-claude-stream", GroupName: "cyber", Provider: "claude", Status: "active"}
	if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{AccessToken: "sk-ant-api-goal-stream"}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","stream":true,"session_id":"goal-claude-stream-session","messages":[{"role":"user","content":"long claude task"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Pool-Provider", "claude")
	req.Header.Set("X-Claude-Code-Session-Id", "goal-claude-stream-session")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || calls.Load() != 2 || !strings.Contains(string(result), "message_stop") || strings.Contains(string(result), "goal_stream_interrupted") {
		t.Fatalf("claude continuation status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), result)
	}
	resume := []byte(`{"model":"claude-sonnet-4-6","session_id":"goal-claude-stream-session","messages":[{"role":"user","content":"next"}]}`)
	replayReq, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/messages", strings.NewReader(string(resume)))
	replayReq.Header.Set("X-Claude-Code-Session-Id", "goal-claude-stream-session")
	replay := h.app.goalReplayBody(context.Background(), replayReq, "claude", resume)
	if replay.Kind != goalResumeFound || !strings.Contains(string(replay.Body), "partial") || !strings.Contains(string(replay.Body), "continued") || !strings.Contains(string(replay.Body), "next") {
		t.Fatalf("claude continuation was not durable replay=%+v body=%s", replay, replay.Body)
	}
}

func TestGoalContinuitySchedulesBoundedCheckpointCompaction(t *testing.T) {
	skipLegacyCodexGoalReplay(t)
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		id := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"resp_goal_compact_%d","object":"response","status":"completed","model":"gpt","output":[{"type":"message","content":[{"type":"output_text","text":"answer-%d"}]}]}`, id, id)
	})
	h.app.cfg.GoalCompressionMaxStages = 1
	h.app.cfg.GoalCompressionChunkRatio = 0.5
	h.importAccount(t, "goal-compact", "upstream-goal-compact", "access-goal-compact")
	for _, input := range []string{"one", "two", "three"} {
		req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt","input":[{"role":"user","content":"`+input+`"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("thread-id", "goal-compaction-thread")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("turn %s status=%d", input, resp.StatusCode)
		}
	}
	resolved, err := h.store.ResolveGoalAliases(context.Background(), []storage.GoalAlias{{Type: "codex_root_thread", Value: "goal-compaction-thread"}})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		needed, checkErr := h.store.NeedsGoalCompaction(context.Background(), resolved.Session.ID, 1)
		if checkErr != nil {
			t.Fatal(checkErr)
		}
		if !needed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduled compaction did not finish its resumable chunks")
		}
		time.Sleep(25 * time.Millisecond)
	}
	body, _, err := h.store.BuildGoalReplay(context.Background(), resolved.Session.ID)
	if err != nil || !strings.Contains(string(body), "one") || !strings.Contains(string(body), "two") || !strings.Contains(string(body), "three") {
		t.Fatalf("scheduled compaction replay=%s err=%v", body, err)
	}
}

func TestGoalCompactionWorkerRequeuesChunksFairly(t *testing.T) {
	ctx := context.Background()
	store, err := storage.OpenInMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	commitGoal := func(alias string) storage.GoalSession {
		t.Helper()
		var goal storage.GoalSession
		for turn := 1; turn <= 4; turn++ {
			goal, err = store.CommitGoalTurn(ctx, storage.GoalTurn{
				Protocol: "codex", Aliases: []storage.GoalAlias{{Type: "codex_root_thread", Value: alias}}, ResponseID: fmt.Sprintf("%s-response-%d", alias, turn),
				CheckpointPayload: `{"model":"gpt-test","input":[]}`, SegmentPayload: fmt.Sprintf(`{"history_key":"input","input":"%s-input-%d","output":"%s-output-%d"}`, alias, turn, alias, turn),
				ExpiresAt: storage.Now() + 86400, StorageMaxBytes: 8 << 20,
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		return goal
	}
	first, second := commitGoal("fair-first"), commitGoal("fair-second")
	cfg := config.Default()
	cfg.GoalCompressionMaxStages = 1
	cfg.GoalCompressionChunkRatio = 1
	s := &Server{cfg: cfg, store: store}
	workerCtx, cancel := context.WithCancel(context.Background())
	queue := make(chan string, 8)
	s.goalCompactionCtx, s.goalCompactionCancel = workerCtx, cancel
	s.goalCompactionQueue = queue
	s.goalCompactionQueued = map[string]bool{first.ID: true, second.ID: true}
	queue <- first.ID
	queue <- second.ID
	s.asyncWG.Add(1)
	go func() {
		defer supervisor.Recover("goal-compaction-fairness-test")
		defer s.asyncWG.Done()
		s.goalCompactionWorker(workerCtx, queue)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		firstNeeded, firstErr := store.NeedsGoalCompaction(ctx, first.ID, 1)
		secondNeeded, secondErr := store.NeedsGoalCompaction(ctx, second.ID, 1)
		if firstErr != nil || secondErr != nil {
			cancel()
			t.Fatalf("compaction state errors: %v %v", firstErr, secondErr)
		}
		if !firstNeeded && !secondNeeded {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("fair compaction queue did not drain")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	s.asyncWG.Wait()
	rows, err := store.DB().Query(`SELECT detail FROM audit_log WHERE action='goal_compaction_completed' ORDER BY id LIMIT 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var order []string
	for rows.Next() {
		var detail string
		if err = rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		order = append(order, detail)
	}
	if len(order) != 2 || !strings.Contains(order[0], first.ID) || !strings.Contains(order[1], second.ID) {
		t.Fatalf("compaction queue was not FIFO-fair: %v", order)
	}
}

func TestGoalContinuityCanStopLegacySnapshotDualWrite(t *testing.T) {
	skipLegacyCodexGoalReplay(t)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_goal_no_v1","object":"response","status":"completed","model":"gpt","output":[{"type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
	})
	h.app.cfg.GoalLegacyJournalDualWrite = false
	h.importAccount(t, "goal-no-v1", "upstream-goal-no-v1", "access-goal-no-v1")
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","input":"new durable goal"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var journals, goals int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM context_journal`).Scan(&journals); err != nil {
		t.Fatal(err)
	}
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM goal_session`).Scan(&goals); err != nil {
		t.Fatal(err)
	}
	if journals != 0 || goals != 1 {
		t.Fatalf("dual-write disabled journals=%d goals=%d", journals, goals)
	}
}

func TestGoalResumeUnidentifiedAlwaysProducesStreamTerminal(t *testing.T) {
	skipLegacyCodexGoalReplay(t)
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unidentified resume must not contact upstream")
	})
	h.importAccount(t, "goal-unidentified", "upstream-goal-unidentified", "access-goal-unidentified")
	resp, err := http.Post(h.pool.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt","stream":true,"previous_response_id":"missing-goal-response","input":"resume"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-MiCliProxy-Goal-Error") != "goal_resume_context_unidentified" || !strings.Contains(string(body), "response.failed") || !strings.Contains(string(body), "goal_resume_context_unidentified") {
		t.Fatalf("unidentified stream must terminate status=%d body=%s", resp.StatusCode, body)
	}
}

// TestGoalResumeRefusesResponsesHistoryForMessagesFamilyProviders pins the fix for
// "switch a chatgpt account to kiro/antigravity and goal breaks". The same Claude Code
// session identifiers can own a Responses-family goal, because handleMessagesViaCodex
// rewrites /v1/messages into /v1/responses while keeping the client's headers. A later
// kiro/antigravity turn must never receive that Responses history under the "messages"
// key, and must never silently proceed as if no durable context existed.
func TestGoalResumeRefusesResponsesHistoryForMessagesFamilyProviders(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	h.app.cfg.GoalContinuityEnabled = true
	const (
		keyHash   = "cross-family-key-hash"
		sessionID = "cross-family-claude-session"
	)

	// Turn 1: the session reached a chatgpt/codex account through the bridge, so the
	// durable goal is Responses-family even though the client is Claude Code.
	codexBody := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"responses-only-history"}]}]}`)
	codexResponse := []byte(`{"id":"resp_cross_family_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"responses-only-answer"}]}]}`)
	codexReq, err := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(codexBody)))
	if err != nil {
		t.Fatal(err)
	}
	codexReq.Header.Set("X-Claude-Code-Session-Id", sessionID)
	codexCtx := withDownstreamKey(withGoalOriginalBody(context.Background(), codexBody), downstreamPolicy{KeyHash: keyHash})
	codexGoal, err := h.app.persistGoalContinuity(codexCtx, codexReq, "codex", codexBody, codexResponse)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(codexGoal.ID) == "" {
		t.Fatal("codex turn did not create a durable goal")
	}

	// Turn 2: the same session is now served by kiro / antigravity / a custom Messages
	// provider. It carries its own complete history, so the turn can proceed — but as a
	// NEW same-family goal, with the restart made visible rather than replaying.
	for _, protocol := range []string{"kiro", "antigravity", "custom_messages"} {
		current := []byte(`{"model":"claude-sonnet-4-6","session_id":"` + sessionID + `","messages":[{"role":"user","content":"switched provider"}]}`)
		req, err := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(current)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Claude-Code-Session-Id", sessionID)
		ctx := withDownstreamKey(context.Background(), downstreamPolicy{KeyHash: keyHash})
		replay := h.app.goalReplayBody(ctx, req, protocol, current)
		if replay.Kind != goalResumeFamilyRestart {
			t.Fatalf("%s resume kind = %q, want %q (reason=%q)", protocol, replay.Kind, goalResumeFamilyRestart, replay.Reason)
		}
		// The decisive property: no Responses-family history may be handed to a
		// Messages-family upstream in any form.
		if strings.Contains(string(replay.Body), "responses-only-history") ||
			strings.Contains(string(replay.Body), "responses-only-answer") ||
			strings.Contains(string(replay.Body), `"input"`) {
			t.Fatalf("%s received Responses-family history: %s", protocol, replay.Body)
		}
	}

	// A request that depends on pool-side history (it carries an upstream state handle
	// instead of its own messages) cannot be restarted, so it must be refused with a
	// diagnosable code instead of being sent upstream in the wrong shape.
	stateful := []byte(`{"model":"claude-sonnet-4-6","session_id":"` + sessionID + `","messages":[{"role":"user","content":"switched provider"}]}`)
	statefulReq, err := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(stateful)))
	if err != nil {
		t.Fatal(err)
	}
	statefulReq.Header.Set("X-Claude-Code-Session-Id", sessionID)
	statefulReq.Header.Set("X-Codex-Turn-State", "carried-over-upstream-state")
	statefulCtx := withDownstreamKey(context.Background(), downstreamPolicy{KeyHash: keyHash})
	refused := h.app.goalReplayBody(statefulCtx, statefulReq, "kiro", stateful)
	if refused.Kind != goalResumeProtocolMismatch || len(refused.Body) != 0 {
		t.Fatalf("stateful cross-family resume kind=%q body=%s", refused.Kind, refused.Body)
	}
	nonStream := httptest.NewRecorder()
	writeGoalResumeError(nonStream, false, "kiro", refused.Kind, refused.Reason)
	if nonStream.Code != http.StatusConflict || !strings.Contains(nonStream.Body.String(), "goal_resume_protocol_family_mismatch") {
		t.Fatalf("non-stream refusal is not diagnosable: status=%d body=%s", nonStream.Code, nonStream.Body.String())
	}
	streamed := httptest.NewRecorder()
	writeGoalResumeError(streamed, true, "kiro", refused.Kind, refused.Reason)
	if streamed.Header().Get("X-MiCliProxy-Goal-Error") != "goal_resume_protocol_family_mismatch" {
		t.Fatalf("stream refusal header = %q", streamed.Header().Get("X-MiCliProxy-Goal-Error"))
	}
	// A Messages-family client must get a Messages-family terminal, or the CLI hangs
	// on a stream it cannot parse instead of showing the refusal.
	if !strings.Contains(streamed.Body.String(), "event: error") ||
		!strings.Contains(streamed.Body.String(), "message_stop") ||
		strings.Contains(streamed.Body.String(), "response.failed") {
		t.Fatalf("stream refusal used the wrong protocol terminal: %s", streamed.Body.String())
	}

	// The Responses-family goal must survive untouched: the refusal is a scoping
	// decision, not a data loss event, so returning to codex still resumes.
	back := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"back to codex"}]}]}`)
	backReq, err := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(back)))
	if err != nil {
		t.Fatal(err)
	}
	backReq.Header.Set("X-Claude-Code-Session-Id", sessionID)
	backCtx := withDownstreamKey(context.Background(), downstreamPolicy{KeyHash: keyHash})
	backReplay := h.app.goalReplayBody(backCtx, backReq, "codex", back)
	if backReplay.Kind != goalResumeFound || backReplay.Session.ID != codexGoal.ID ||
		!strings.Contains(string(backReplay.Body), "responses-only-history") {
		t.Fatalf("codex resume regressed: kind=%q session=%q body=%s", backReplay.Kind, backReplay.Session.ID, backReplay.Body)
	}
}

// TestAntigravityGoalContinuityAdvancesAcrossTwoTurns covers the path that previously
// had no goal handling at all: an Antigravity turn must leave a durable checkpoint, and
// the next turn of the same session must resume it. The replay call uses the literal
// "claude" protocol that handleMessages passes before provider selection, so this also
// pins that a Messages-family goal stored as "antigravity" stays resolvable there.
func TestAntigravityGoalContinuityAdvancesAcrossTwoTurns(t *testing.T) {
	var upstreamBodies []string
	var mu sync.Mutex
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBodies = append(upstreamBodies, string(body))
		turn := len(upstreamBodies)
		mu.Unlock()
		answer := "antigravity-answer-one"
		if turn > 1 {
			answer = "antigravity-answer-two"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"`+answer+`"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":4}}}`)
	})
	h.app.cfg.GoalContinuityEnabled = true

	ctx := context.Background()
	const (
		accountID = "antigravity-goal-two-turns"
		keyHash   = "antigravity-goal-key-hash"
		sessionID = "antigravity-goal-session"
		model     = "claude-sonnet-4-6"
	)
	account := storage.Account{ID: accountID, Label: accountID, GroupName: "cyber", Provider: "antigravity", Status: "active"}
	if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAntigravityCredentials(ctx, storage.AntigravityCredentials{
		AccountID: accountID, ProjectID: "goal-project", AccessToken: "antigravity-access",
		ExpiresAt: time.Now().Add(2 * time.Hour).Unix(), BaseURL: h.upstream.URL,
	}); err != nil {
		t.Fatal(err)
	}

	newRequest := func(body []byte) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Claude-Code-Session-Id", sessionID)
		return req.WithContext(withDownstreamKey(withGoalOriginalBody(context.Background(), body), downstreamPolicy{KeyHash: keyHash}))
	}
	send := func(body []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := newRequest(body)
		w := httptest.NewRecorder()
		if got := h.app.antigravityMessagesWithLease(w, req, body, model, scheduler.Lease{Account: account}, map[string]bool{}); got != outcomeDone {
			t.Fatalf("antigravity outcome = %v body=%s", got, w.Body.String())
		}
		if w.Code != http.StatusOK {
			t.Fatalf("antigravity status = %d body=%s", w.Code, w.Body.String())
		}
		return w
	}

	// --- Turn 1: nothing durable exists yet, and the terminal must create the goal.
	firstBody := []byte(`{"model":"` + model + `","max_tokens":128,"session_id":"` + sessionID + `","messages":[{"role":"user","content":"antigravity-question-one"}]}`)
	first := send(firstBody)
	if !strings.Contains(first.Body.String(), "antigravity-answer-one") {
		t.Fatalf("turn 1 response = %s", first.Body.String())
	}
	h.app.WaitForAsyncWrites()

	goals, err := h.store.ListGoalSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(goals) != 1 || goals[0].Protocol != "antigravity" {
		t.Fatalf("antigravity turn 1 did not persist exactly one antigravity goal: %+v", goals)
	}
	firstGoalID := goals[0].ID

	// --- Turn 2: the same replay call handleMessages makes must find that goal and
	// rebuild the native Messages history before the account is selected again.
	secondBody := []byte(`{"model":"` + model + `","max_tokens":128,"session_id":"` + sessionID + `","messages":[{"role":"user","content":"antigravity-question-two"}]}`)
	replay := h.app.goalReplayBody(newRequest(secondBody).Context(), newRequest(secondBody), "claude", secondBody)
	if replay.Kind != goalResumeFound || replay.Session.ID != firstGoalID {
		t.Fatalf("antigravity turn 2 replay kind=%q session=%q reason=%q", replay.Kind, replay.Session.ID, replay.Reason)
	}
	rebuilt := string(replay.Body)
	if !strings.Contains(rebuilt, "antigravity-question-one") ||
		!strings.Contains(rebuilt, "antigravity-answer-one") ||
		!strings.Contains(rebuilt, "antigravity-question-two") {
		t.Fatalf("antigravity replay lost a turn: %s", rebuilt)
	}
	// Messages-family history must stay under "messages"; an "input" key here is the
	// exact malformed body that broke provider switches.
	if strings.Contains(rebuilt, `"input"`) {
		t.Fatalf("antigravity replay used the Responses history key: %s", rebuilt)
	}

	second := send(replay.Body)
	if !strings.Contains(second.Body.String(), "antigravity-answer-two") {
		t.Fatalf("turn 2 response = %s", second.Body.String())
	}
	h.app.WaitForAsyncWrites()

	// Turn 2 must advance the SAME goal, and the upstream must have received turn 1's
	// question and answer alongside turn 2's question.
	goals, err = h.store.ListGoalSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(goals) != 1 || goals[0].ID != firstGoalID {
		t.Fatalf("antigravity turn 2 forked the goal: %+v", goals)
	}
	mu.Lock()
	sent := append([]string(nil), upstreamBodies...)
	mu.Unlock()
	if len(sent) != 2 {
		t.Fatalf("upstream calls = %d", len(sent))
	}
	if !strings.Contains(sent[1], "antigravity-question-one") ||
		!strings.Contains(sent[1], "antigravity-answer-one") ||
		!strings.Contains(sent[1], "antigravity-question-two") {
		t.Fatalf("antigravity turn 2 upstream body lost history: %s", sent[1])
	}
}
