package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-account-pool/internal/leakfilter"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const codexWebSocketTestDeadline = 10 * time.Second

type codexStabilityCapture struct {
	session string
	thread  string
	body    string
}

func postMappedResponses(t *testing.T, h *testHarness, body, root, thread, parent string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if root != "" {
		req.Header.Set("Session-Id", root)
	}
	if thread != "" {
		req.Header.Set("Thread-Id", thread)
	}
	if parent != "" {
		req.Header.Set("X-Codex-Parent-Thread-Id", parent)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Clone(), string(payload)
}

func requireMappedUUIDv7(t *testing.T, value string) {
	t.Helper()
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Version() != 7 {
		t.Fatalf("mapped session %q is not UUIDv7: %v", value, err)
	}
}

func TestCodexMappedFirstRootRiskRotatesWithoutLeakingDownstreamIdentity(t *testing.T) {
	const downstreamID = "client-session-must-never-reach-upstream"
	var mu sync.Mutex
	var calls []codexStabilityCapture
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, codexStabilityCapture{
			session: r.Header.Get("Session-Id"),
			thread:  r.Header.Get("Thread-Id"),
			body:    string(raw),
		})
		call := len(calls)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"session_blocked","message":"session flagged by risk control"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp-first-risk-recovered","object":"response","model":"gpt-5.6-sol","status":"completed","output":[]}`)
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "first-risk", "upstream-first-risk", "access-first-risk")

	body := `{"model":"gpt-5.6-sol","session_id":"` + downstreamID + `","thread_id":"` + downstreamID + `","client_metadata":{"session_id":"` + downstreamID + `","thread_id":"` + downstreamID + `"},"input":"start"}`
	status, headers, responseBody := postMappedResponses(t, h, body, downstreamID, downstreamID, "")
	if status != http.StatusOK || !strings.Contains(responseBody, "resp-first-risk-recovered") || headers.Get("X-MiCliProxy-Context-Status") != "rotated" {
		t.Fatalf("first-root recovery status=%d context=%q body=%s", status, headers.Get("X-MiCliProxy-Context-Status"), responseBody)
	}

	mu.Lock()
	got := append([]codexStabilityCapture(nil), calls...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("upstream calls=%d, want one risk response and one retry", len(got))
	}
	for _, call := range got {
		requireMappedUUIDv7(t, call.session)
		if call.thread != call.session || strings.Contains(call.body, downstreamID) || call.session == downstreamID {
			t.Fatalf("downstream identity leaked or mapped headers drifted: %+v", call)
		}
	}
	if got[0].session == got[1].session {
		t.Fatalf("risk retry reused upstream session UUID %q", got[0].session)
	}
	namespace := codexNativeNamespaceForTest(t, "", downstreamID)
	rows, err := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "response", Value: "resp-first-risk-recovered"})
	if err != nil || len(rows) != 1 || rows[0].RootSessionID != got[1].session {
		t.Fatalf("recovered response mapping rows=%+v err=%v", rows, err)
	}
}

func TestCodexMappedRiskRotationRestoresPairedToolContext(t *testing.T) {
	const downstreamID = "client-tool-root"
	var mu sync.Mutex
	var calls []codexStabilityCapture
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, codexStabilityCapture{
			session: r.Header.Get("Session-Id"),
			thread:  r.Header.Get("Thread-Id"),
			body:    string(raw),
		})
		call := len(calls)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			_, _ = io.WriteString(w, `{"id":"resp-tool-risk-root","object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"type":"custom_tool_call","id":"ctc-risk","call_id":"call-risk-pair","name":"apply_patch","input":"{}"}]}`)
		case 2:
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"session_risk","message":"session blocked by risk control"}}`)
		case 3:
			if strings.Contains(string(raw), "previous_response_id") ||
				!strings.Contains(string(raw), `"type":"custom_tool_call"`) ||
				!strings.Contains(string(raw), `"type":"custom_tool_call_output"`) ||
				strings.Count(string(raw), "call-risk-pair") != 2 ||
				!strings.Contains(string(raw), "tool-result-kept") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"tool call/output pair was not restored"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"resp-tool-risk-recovered","object":"response","model":"gpt-5.6-sol","status":"completed","output":[]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "tool-risk", "upstream-tool-risk", "access-tool-risk")

	if status, _, body := postMappedResponses(t, h, `{"model":"gpt-5.6-sol","input":"create tool call"}`, downstreamID, downstreamID, ""); status != http.StatusOK || !strings.Contains(body, "resp-tool-risk-root") {
		t.Fatalf("root status=%d body=%s", status, body)
	}
	status, headers, responseBody := postMappedResponses(t, h, `{"model":"gpt-5.6-sol","previous_response_id":"resp-tool-risk-root","input":[{"type":"custom_tool_call_output","call_id":"call-risk-pair","output":"tool-result-kept"}]}`, downstreamID, downstreamID, "")
	if status != http.StatusOK || headers.Get("X-MiCliProxy-Context-Status") != "rebuilt" || !strings.Contains(responseBody, "resp-tool-risk-recovered") {
		t.Fatalf("tool recovery status=%d context=%q body=%s", status, headers.Get("X-MiCliProxy-Context-Status"), responseBody)
	}

	mu.Lock()
	got := append([]codexStabilityCapture(nil), calls...)
	mu.Unlock()
	if len(got) != 3 || got[0].session != got[1].session || got[2].session == got[1].session {
		t.Fatalf("unexpected mapped identity lifecycle: %+v", got)
	}
	for _, call := range got {
		if strings.Contains(call.body, downstreamID) {
			t.Fatalf("downstream identity leaked in upstream body: %s", call.body)
		}
	}
	namespace := codexNativeNamespaceForTest(t, "", downstreamID)
	oldRows, oldErr := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "response", Value: "resp-tool-risk-root"})
	newRows, newErr := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "response", Value: "resp-tool-risk-recovered"})
	if oldErr != nil || len(oldRows) != 1 || oldRows[0].State != "retired" || newErr != nil || len(newRows) != 1 || newRows[0].State != "active" {
		t.Fatalf("rotation state old=%+v old_err=%v new=%+v new_err=%v", oldRows, oldErr, newRows, newErr)
	}
}

func TestCodexChildRiskDoesNotRotateRootSession(t *testing.T) {
	var mu sync.Mutex
	var calls []codexStabilityCapture
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, codexStabilityCapture{session: r.Header.Get("Session-Id"), thread: r.Header.Get("Thread-Id"), body: string(raw)})
		call := len(calls)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			_, _ = io.WriteString(w, `{"id":"resp-child-risk-root","object":"response","model":"gpt-5.6-sol","status":"completed","output":[]}`)
		case 2:
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"session_risk","message":"child session blocked by risk control"}}`)
		case 3:
			_, _ = io.WriteString(w, `{"id":"resp-child-risk-root-next","object":"response","model":"gpt-5.6-sol","status":"completed","output":[]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "child-risk", "upstream-child-risk", "access-child-risk")

	if status, _, body := postMappedResponses(t, h, `{"model":"gpt-5.6-sol","input":"root"}`, "client-root", "client-root", ""); status != http.StatusOK || !strings.Contains(body, "resp-child-risk-root") {
		t.Fatalf("root status=%d body=%s", status, body)
	}
	if status, _, body := postMappedResponses(t, h, `{"model":"gpt-5.6-sol","session_id":"client-root","thread_id":"client-child","parent_thread_id":"client-root","input":"child"}`, "client-root", "client-child", "client-root"); status != http.StatusConflict || !strings.Contains(body, "session_risk") {
		t.Fatalf("child risk status=%d body=%s", status, body)
	}
	if status, _, body := postMappedResponses(t, h, `{"model":"gpt-5.6-sol","input":"root still active"}`, "client-root", "client-root", ""); status != http.StatusOK || !strings.Contains(body, "resp-child-risk-root-next") {
		t.Fatalf("root after child failure status=%d body=%s", status, body)
	}

	mu.Lock()
	got := append([]codexStabilityCapture(nil), calls...)
	mu.Unlock()
	if len(got) != 3 || got[0].session == "" || got[0].session != got[1].session || got[0].session != got[2].session || got[1].thread == got[1].session {
		t.Fatalf("child failure altered root identity: %+v", got)
	}
	namespace := codexNativeNamespaceForTest(t, "", "client-root")
	root, err := h.store.ResolveCodexSessionAliases(context.Background(), namespace, []storage.CodexSessionAlias{{Type: "response", Value: "resp-child-risk-root"}})
	if err != nil || root.State != "active" || root.RootSessionID != got[0].session {
		t.Fatalf("root mapping after child failure=%+v err=%v", root, err)
	}
}

func TestCodexMappedRecoveryRejectsUnpairedToolOutputWithoutCheckpoint(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	enableCodexSessionMappingForTest(h)
	body := []byte(`{"model":"gpt-5.6-sol","previous_response_id":"resp-missing-checkpoint","input":[{"type":"custom_tool_call_output","call_id":"call-no-checkpoint","output":"must not be relabeled"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Session-Id", "downstream-root")
	req.Header.Set("Thread-Id", "downstream-root")
	mapping := &codexSessionMapping{
		enabled:   true,
		namespace: "unauthenticated",
		identity:  codexDownstreamSessionIdentity(req.Header, body),
		binding: &storage.CodexSessionBinding{
			ID: "missing-checkpoint-binding", TreeID: "missing-checkpoint-tree", State: "active",
			RootSessionID: "019f0000-0000-7000-8000-000000000901",
			ThreadID:      "019f0000-0000-7000-8000-000000000901",
		},
	}
	migration, recovered, err := h.app.recoverCodexSessionMapping(
		context.Background(), req, body, req.Header, downstreamPolicy{}, mapping,
		leakfilter.ResponsesContextErrorNone, "mapped_session_risk",
	)
	if recovered || !errors.Is(err, errCodexToolContextUnrecoverable) || len(migration.Retry.Raw) != 0 {
		t.Fatalf("unsafe tool recovery migration=%+v recovered=%v err=%v", migration, recovered, err)
	}
}

func TestCodexMappedSessionRiskClassification(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout,
		http.StatusConflict, http.StatusLocked, http.StatusTooEarly,
		http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable,
	} {
		if !codexMappedSessionRiskError(status, nil) {
			t.Fatalf("status %d must rotate a mapped root session", status)
		}
	}
	if !codexMappedSessionRiskError(http.StatusBadRequest, []byte(`{"error":{"message":"session expired by risk control"}}`)) {
		t.Fatal("explicit session-risk 400 must rotate")
	}
	for _, test := range []struct {
		status int
		body   string
	}{
		{http.StatusBadRequest, `{"error":{"message":"invalid model"}}`},
		{http.StatusBadRequest, `{"error":{"message":"session metadata has wrong schema"}}`},
		{http.StatusNotFound, `{"error":{"message":"not found"}}`},
		{http.StatusUnprocessableEntity, `{"error":{"message":"bad input"}}`},
	} {
		if codexMappedSessionRiskError(test.status, []byte(test.body)) {
			t.Fatalf("request error unexpectedly classified as session risk: status=%d body=%s", test.status, test.body)
		}
	}
}

func TestCodexMappedSessionRotationSeparatesAccountAndSessionFailures(t *testing.T) {
	cfHeader := http.Header{"Cf-Ray": []string{"edge-only"}}
	tests := []struct {
		name                 string
		status               int
		header               http.Header
		body                 string
		movable              bool
		hasFailoverCandidate bool
		want                 bool
	}{
		{name: "unknown_401_rotates", status: http.StatusUnauthorized, movable: true, want: true},
		{name: "known_expired_token_uses_account_failover", status: http.StatusUnauthorized, body: `{"error":"token expired"}`, movable: true},
		{name: "known_missing_scope_uses_account_failover", status: http.StatusForbidden, body: `{"error":"Missing scopes: api.responses.write"}`, movable: true},
		{name: "known_usage_limit_uses_account_failover", status: http.StatusTooManyRequests, body: `{"error":"You've hit your usage limit"}`, movable: true},
		{name: "cf_edge_uses_account_failover", status: http.StatusForbidden, header: cfHeader, body: `{"error":"challenge"}`, movable: true},
		{name: "stateful_expired_token_rebuilds_for_alternate", status: http.StatusUnauthorized, body: `{"error":"token expired"}`, hasFailoverCandidate: true, want: true},
		{name: "stateful_expired_token_without_alternate_stays_native", status: http.StatusUnauthorized, body: `{"error":"token expired"}`},
		{name: "stateful_cf_can_rotate_egress_without_alternate", status: http.StatusForbidden, header: cfHeader, body: `{"error":"challenge"}`, want: true},
		{name: "explicit_session_400_rotates", status: http.StatusBadRequest, body: `{"error":"session blocked by risk control"}`, movable: true, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := codexMappedSessionRotationRequired(test.status, test.header, []byte(test.body), test.movable, test.hasFailoverCandidate); got != test.want {
				t.Fatalf("rotation=%v, want %v", got, test.want)
			}
		})
	}
}

func TestCodexMappedRiskAccountSwitchPreservesPairedToolContext(t *testing.T) {
	for _, riskStatus := range []int{http.StatusConflict, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(riskStatus), func(t *testing.T) {
			const downstreamID = "client-account-switch-root"
			const callID = "call-account-switch-pair"
			var mu sync.Mutex
			var calls []struct {
				auth    string
				session string
				thread  string
				body    string
			}
			h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				capture := struct {
					auth    string
					session string
					thread  string
					body    string
				}{r.Header.Get("Authorization"), r.Header.Get("Session-Id"), r.Header.Get("Thread-Id"), string(raw)}
				mu.Lock()
				calls = append(calls, capture)
				mu.Unlock()

				w.Header().Set("Content-Type", "application/json")
				switch capture.auth {
				case "Bearer access-risk-switch-a":
					if !strings.Contains(capture.body, "previous_response_id") {
						_, _ = io.WriteString(w, `{"id":"resp-risk-switch-root","object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"type":"custom_tool_call","id":"ctc-risk-switch","call_id":"`+callID+`","name":"apply_patch","input":"{}"}]}`)
						return
					}
					w.WriteHeader(riskStatus)
					_, _ = io.WriteString(w, `{"error":{"code":"session_risk","message":"session blocked by risk control"}}`)
				case "Bearer access-risk-switch-b":
					callIndex := strings.Index(capture.body, `"type":"custom_tool_call"`)
					outputIndex := strings.Index(capture.body, `"type":"custom_tool_call_output"`)
					if strings.Contains(capture.body, "previous_response_id") || callIndex < 0 || outputIndex <= callIndex ||
						strings.Count(capture.body, callID) != 2 || !strings.Contains(capture.body, "account-switch-result") {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, `{"error":"paired tool context was not rebuilt"}`)
						return
					}
					_, _ = io.WriteString(w, `{"id":"resp-risk-switch-recovered","object":"response","model":"gpt-5.6-sol","status":"completed","output":[]}`)
				default:
					w.WriteHeader(http.StatusUnauthorized)
				}
			})
			enableCodexSessionMappingForTest(h)
			h.importAccount(t, "risk-switch-a", "upstream-risk-switch-a", "access-risk-switch-a")

			if status, _, body := postMappedResponses(t, h, `{"model":"gpt-5.6-sol","input":"create tool call"}`, downstreamID, downstreamID, ""); status != http.StatusOK || !strings.Contains(body, "resp-risk-switch-root") {
				t.Fatalf("root status=%d body=%s", status, body)
			}
			h.importAccount(t, "risk-switch-b", "upstream-risk-switch-b", "access-risk-switch-b")
			status, header, body := postMappedResponses(t, h, `{"model":"gpt-5.6-sol","previous_response_id":"resp-risk-switch-root","input":[{"type":"custom_tool_call_output","call_id":"`+callID+`","output":"account-switch-result"}]}`, downstreamID, downstreamID, "")
			if status != http.StatusOK || header.Get("X-MiCliProxy-Context-Status") != "rebuilt" || !strings.Contains(body, "resp-risk-switch-recovered") {
				t.Fatalf("recovery status=%d context=%q body=%s", status, header.Get("X-MiCliProxy-Context-Status"), body)
			}

			mu.Lock()
			got := append([]struct {
				auth    string
				session string
				thread  string
				body    string
			}(nil), calls...)
			mu.Unlock()
			if len(got) != 3 || got[0].auth != "Bearer access-risk-switch-a" || got[1].auth != got[0].auth || got[2].auth != "Bearer access-risk-switch-b" {
				t.Fatalf("account lifecycle=%+v", got)
			}
			if got[0].session == "" || got[1].session != got[0].session || got[2].session == got[1].session || got[2].thread != got[2].session {
				t.Fatalf("mapped identity lifecycle=%+v", got)
			}
			for _, call := range got {
				requireMappedUUIDv7(t, call.session)
				if call.session == downstreamID || strings.Contains(call.body, downstreamID) {
					t.Fatalf("downstream session leaked upstream: %+v", call)
				}
			}
		})
	}
}

func TestCodexMappedConcurrentRootSerializesCommitAndReleasesCancelledWaiter(t *testing.T) {
	const downstreamID = "client-concurrent-root"
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var mu sync.Mutex
	var upstreamSessions []string
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstreamSessions = append(upstreamSessions, r.Header.Get("Session-Id"))
		call := len(upstreamSessions)
		mu.Unlock()
		if call == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"resp-concurrent-%d","object":"response","model":"gpt-5.6-sol","status":"completed","output":[]}`, call))
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "concurrent-root", "upstream-concurrent-root", "access-concurrent-root")

	type requestResult struct {
		status int
		body   string
		err    error
	}
	post := func(ctx context.Context, input string) requestResult {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"`+input+`"}`))
		if err != nil {
			return requestResult{err: err}
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Session-Id", downstreamID)
		req.Header.Set("Thread-Id", downstreamID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return requestResult{err: err}
		}
		defer resp.Body.Close()
		payload, readErr := io.ReadAll(resp.Body)
		return requestResult{status: resp.StatusCode, body: string(payload), err: readErr}
	}
	waitForGateRefs := func(want int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			h.app.codexSessionGatesMu.Lock()
			refs := 0
			for _, gate := range h.app.codexSessionGates {
				refs += gate.refs
			}
			h.app.codexSessionGatesMu.Unlock()
			if refs == want {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("session gate never reached %d refs", want)
	}

	leaderResult := make(chan requestResult, 1)
	go func() { leaderResult <- post(context.Background(), "leader") }()
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("leader never reached upstream")
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancelledResult := make(chan requestResult, 1)
	go func() { cancelledResult <- post(cancelCtx, "cancelled waiter") }()
	waitForGateRefs(2)
	cancel()
	select {
	case result := <-cancelledResult:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("cancelled waiter err=%v status=%d body=%s", result.err, result.status, result.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled session waiter did not return")
	}
	waitForGateRefs(1)

	followerResult := make(chan requestResult, 1)
	go func() { followerResult <- post(context.Background(), "follower") }()
	waitForGateRefs(2)
	mu.Lock()
	beforeRelease := len(upstreamSessions)
	mu.Unlock()
	if beforeRelease != 1 {
		t.Fatalf("follower reached upstream before leader committed: calls=%d", beforeRelease)
	}
	close(releaseFirst)

	leader := <-leaderResult
	follower := <-followerResult
	if leader.err != nil || leader.status != http.StatusOK || !strings.Contains(leader.body, "resp-concurrent-1") {
		t.Fatalf("leader result=%+v", leader)
	}
	if follower.err != nil || follower.status != http.StatusOK || !strings.Contains(follower.body, "resp-concurrent-2") {
		t.Fatalf("follower result=%+v", follower)
	}
	waitForGateRefs(0)

	mu.Lock()
	sessions := append([]string(nil), upstreamSessions...)
	mu.Unlock()
	if len(sessions) != 2 || sessions[0] == "" || sessions[1] != sessions[0] || sessions[0] == downstreamID {
		t.Fatalf("serialized upstream sessions=%q", sessions)
	}
	requireMappedUUIDv7(t, sessions[0])
	namespace := codexNativeNamespaceForTest(t, "", downstreamID)
	first, firstErr := h.store.ResolveCodexSessionAliases(context.Background(), namespace, []storage.CodexSessionAlias{{Type: "response", Value: "resp-concurrent-1"}})
	second, secondErr := h.store.ResolveCodexSessionAliases(context.Background(), namespace, []storage.CodexSessionAlias{{Type: "response", Value: "resp-concurrent-2"}})
	if firstErr != nil || secondErr != nil || first.ID == "" || second.ID != first.ID || first.RootSessionID != sessions[0] {
		t.Fatalf("serialized aliases first=%+v err=%v second=%+v err=%v", first, firstErr, second, secondErr)
	}
}

func TestCodexMappedStreamingRiskSwitchesBeforeCommitAndAcceptsTerminalTail(t *testing.T) {
	const downstreamID = "client-stream-risk-root"
	const callID = "call-stream-risk-pair"
	var mu sync.Mutex
	var calls []struct {
		auth    string
		session string
		body    string
	}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		capture := struct {
			auth    string
			session string
			body    string
		}{r.Header.Get("Authorization"), r.Header.Get("Session-Id"), string(raw)}
		mu.Lock()
		calls = append(calls, capture)
		mu.Unlock()

		switch capture.auth {
		case "Bearer access-stream-risk-a":
			if !strings.Contains(capture.body, "previous_response_id") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp-stream-risk-root","object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"type":"custom_tool_call","id":"ctc-stream-risk","call_id":"`+callID+`","name":"apply_patch","input":"{}"}]}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.failed\r\n"+
				`data: {"type":"response.failed","status":503,"response":{"id":"resp-stream-risk-root","status":"failed","error":{"code":"session_risk","message":"session blocked by risk control"}}}`+"\r\n\r\n"+
				"data: [DONE]\r\n\r\n")
		case "Bearer access-stream-risk-b":
			callIndex := strings.Index(capture.body, `"type":"custom_tool_call"`)
			outputIndex := strings.Index(capture.body, `"type":"custom_tool_call_output"`)
			if strings.Contains(capture.body, "previous_response_id") || callIndex < 0 || outputIndex <= callIndex ||
				strings.Count(capture.body, callID) != 2 || !strings.Contains(capture.body, "stream-risk-result") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"stream tool context was not rebuilt"}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.completed\r\n"+
				`data: {"type":"response.completed","response":{"id":"resp-stream-risk-recovered","object":"response","model":"gpt-5.6-sol","status":"completed","output":[]}}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "stream-risk-a", "upstream-stream-risk-a", "access-stream-risk-a")

	if status, _, body := postMappedResponses(t, h, `{"model":"gpt-5.6-sol","input":"create streamed tool call"}`, downstreamID, downstreamID, ""); status != http.StatusOK || !strings.Contains(body, "resp-stream-risk-root") {
		t.Fatalf("root status=%d body=%s", status, body)
	}
	h.importAccount(t, "stream-risk-b", "upstream-stream-risk-b", "access-stream-risk-b")
	status, header, body := postMappedResponses(t, h, `{"model":"gpt-5.6-sol","stream":true,"previous_response_id":"resp-stream-risk-root","input":[{"type":"custom_tool_call_output","call_id":"`+callID+`","output":"stream-risk-result"}]}`, downstreamID, downstreamID, "")
	if status != http.StatusOK || header.Get("X-MiCliProxy-Context-Status") != "rebuilt" || !strings.Contains(body, "resp-stream-risk-recovered") {
		t.Fatalf("stream recovery status=%d context=%q body=%s", status, header.Get("X-MiCliProxy-Context-Status"), body)
	}
	for _, leaked := range []string{"resp-stream-risk-root\",\"status\":\"failed", "session_risk", "stream disconnected before completion", "codex_native_continue_failed"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("stream recovery leaked %q: %s", leaked, body)
		}
	}
	mu.Lock()
	got := append([]struct {
		auth    string
		session string
		body    string
	}(nil), calls...)
	mu.Unlock()
	if len(got) != 3 || got[0].auth != "Bearer access-stream-risk-a" || got[1].auth != got[0].auth || got[2].auth != "Bearer access-stream-risk-b" || got[0].session != got[1].session || got[2].session == got[1].session {
		t.Fatalf("stream account/session lifecycle=%+v", got)
	}
	namespace := codexNativeNamespaceForTest(t, "", downstreamID)
	if rows, err := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "response", Value: "resp-stream-risk-recovered"}); err != nil || len(rows) != 1 || rows[0].RootSessionID != got[2].session {
		t.Fatalf("stream terminal mapping rows=%+v err=%v", rows, err)
	}
}

func TestCodexMappedDownstreamWebSocketQuotaRotatesAccountAndRestoresToolContext(t *testing.T) {
	const downstreamID = "client-websocket-quota-root"
	const callID = "call-websocket-quota-pair"
	type capture struct {
		auth    string
		session string
		thread  string
		body    string
	}
	var mu sync.Mutex
	var calls []capture
	connections := map[string]int{}
	recoveredPayloadValid := false
	upgrader := websocket.Upgrader{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		sessionID := r.Header.Get("Session-Id")
		threadID := r.Header.Get("Thread-Id")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream websocket upgrade: %v", err)
			return
		}
		defer conn.Close()
		mu.Lock()
		connections[auth]++
		mu.Unlock()
		for {
			_, raw, readErr := conn.ReadMessage()
			if readErr != nil {
				return
			}
			got := capture{auth: auth, session: sessionID, thread: threadID, body: string(raw)}
			mu.Lock()
			calls = append(calls, got)
			mu.Unlock()

			switch auth {
			case "Bearer access-websocket-quota-a":
				if !strings.Contains(got.body, "previous_response_id") {
					_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-websocket-quota-root","object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"type":"custom_tool_call","id":"ctc-websocket-quota","call_id":"`+callID+`","name":"apply_patch","input":"{}"}]}}`))
					continue
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status_code":429,"error":{"type":"usage_limit_reached","code":"usage_limit_reached","message":"usage limit reached","resets_at":1785583257}}`))
				return
			case "Bearer access-websocket-quota-b":
				callIndex := strings.Index(got.body, `"type":"custom_tool_call"`)
				outputIndex := strings.Index(got.body, `"type":"custom_tool_call_output"`)
				valid := !strings.Contains(got.body, "previous_response_id") &&
					callIndex >= 0 && outputIndex > callIndex &&
					strings.Count(got.body, callID) == 2 &&
					strings.Contains(got.body, "websocket-quota-result")
				mu.Lock()
				recoveredPayloadValid = valid
				mu.Unlock()
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-websocket-quota-recovered","object":"response","model":"gpt-5.6-sol","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}}`))
				return
			default:
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","status_code":401,"error":{"message":"unexpected account"}}`))
				return
			}
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "websocket-quota-a", "upstream-websocket-quota-a", "access-websocket-quota-a")

	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/v1/responses"
	downstreamHeader := http.Header{}
	downstreamHeader.Set("Session-Id", downstreamID)
	downstreamHeader.Set("Thread-Id", downstreamID)
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, downstreamHeader)
	if err != nil {
		if response != nil && response.Body != nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("downstream websocket dial: %v body=%s", err, body)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	waitForCompleted := func(request, responseID string) string {
		t.Helper()
		if err := conn.SetWriteDeadline(time.Now().Add(codexWebSocketTestDeadline)); err != nil {
			t.Fatal(err)
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatal(err)
		}
		if err := conn.SetReadDeadline(time.Now().Add(codexWebSocketTestDeadline)); err != nil {
			t.Fatal(err)
		}
		var received strings.Builder
		for eventCount := 0; eventCount < 8; eventCount++ {
			_, event, err := conn.ReadMessage()
			if err != nil {
				mu.Lock()
				snapshot := append([]capture(nil), calls...)
				valid := recoveredPayloadValid
				mu.Unlock()
				t.Fatalf("read downstream websocket terminal %q: %v; events=%s upstream_calls=%+v recovered_payload_valid=%v", responseID, err, received.String(), snapshot, valid)
			}
			received.Write(event)
			if bytes.Contains(event, []byte("usage_limit_reached")) || bytes.Contains(event, []byte(`"status_code":429`)) || bytes.Contains(event, []byte(`"type":"response.failed"`)) {
				t.Fatalf("upstream quota frame leaked downstream: %s", event)
			}
			if bytes.Contains(event, []byte(`"type":"response.completed"`)) {
				if !bytes.Contains(event, []byte(responseID)) {
					t.Fatalf("completed event missing response id %q: %s", responseID, event)
				}
				return received.String()
			}
		}
		t.Fatalf("no response.completed for %q: %s", responseID, received.String())
		return ""
	}

	waitForCompleted(`{"type":"response.create","model":"gpt-5.6-sol","session_id":"`+downstreamID+`","thread_id":"`+downstreamID+`","input":"create websocket tool call"}`, "resp-websocket-quota-root")
	h.importAccount(t, "websocket-quota-b", "upstream-websocket-quota-b", "access-websocket-quota-b")
	waitForCompleted(`{"type":"response.append","model":"gpt-5.6-sol","input":[{"type":"custom_tool_call_output","call_id":"`+callID+`","output":"websocket-quota-result"}]}`, "resp-websocket-quota-recovered")

	mu.Lock()
	gotCalls := append([]capture(nil), calls...)
	gotConnections := map[string]int{}
	for auth, count := range connections {
		gotConnections[auth] = count
	}
	validRecovery := recoveredPayloadValid
	mu.Unlock()
	if len(gotCalls) != 3 || gotCalls[0].auth != "Bearer access-websocket-quota-a" || gotCalls[1].auth != gotCalls[0].auth || gotCalls[2].auth != "Bearer access-websocket-quota-b" {
		t.Fatalf("websocket account lifecycle=%+v", gotCalls)
	}
	if gotConnections["Bearer access-websocket-quota-a"] != 1 || gotConnections["Bearer access-websocket-quota-b"] != 1 {
		t.Fatalf("websocket connection lifecycle=%v", gotConnections)
	}
	if !validRecovery {
		t.Fatalf("websocket recovery did not preserve the paired tool context: %s", gotCalls[2].body)
	}
	if gotCalls[0].session == "" || gotCalls[0].session != gotCalls[0].thread || gotCalls[1].session != gotCalls[0].session || gotCalls[2].session == gotCalls[1].session || gotCalls[2].thread != gotCalls[2].session {
		t.Fatalf("websocket mapped identity lifecycle=%+v", gotCalls)
	}
	for _, call := range gotCalls {
		requireMappedUUIDv7(t, call.session)
		if call.session == downstreamID || strings.Contains(call.body, downstreamID) {
			t.Fatalf("downstream websocket session leaked upstream: %+v", call)
		}
	}
	namespace := codexNativeNamespaceForTest(t, "", downstreamID)
	oldRows, oldErr := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "response", Value: "resp-websocket-quota-root"})
	newRows, newErr := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "response", Value: "resp-websocket-quota-recovered"})
	if oldErr != nil || len(oldRows) != 1 || oldRows[0].State != "retired" || newErr != nil || len(newRows) != 1 || newRows[0].State != "active" || newRows[0].RootSessionID != gotCalls[2].session {
		t.Fatalf("websocket rotation state old=%+v old_err=%v new=%+v new_err=%v", oldRows, oldErr, newRows, newErr)
	}
}

func TestDownstreamWebSocketPersistsGoalAfterMappingCommitConflict(t *testing.T) {
	const downstreamID = "client-websocket-mapping-conflict"
	const responseID = "resp-websocket-mapping-conflict"
	const callID = "call-websocket-mapping-conflict"
	var h *testHarness
	var conflictErr error
	namespace := codexNativeNamespaceForTest(t, "", downstreamID)
	upgrader := websocket.Upgrader{}
	h = newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream websocket upgrade: %v", err)
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read upstream websocket request: %v", err)
			return
		}
		_, conflictErr = h.store.CommitCodexSessionBinding(context.Background(), storage.CodexSessionCommit{
			Namespace: namespace,
			Binding: storage.CodexSessionBinding{
				ID: "competing-binding", TreeID: "competing-tree", AccountID: "competing-account", EgressID: storage.DefaultDirectEgressID,
				State: "active", RootSessionID: "competing-session", ThreadID: "competing-session",
			},
			Aliases:   []storage.CodexSessionAlias{{Type: "response", Value: responseID}},
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
		payload := `{"type":"response.completed","response":{"id":"` + responseID + `","object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"type":"custom_tool_call","id":"ctc-websocket-mapping-conflict","call_id":"` + callID + `","name":"apply_patch","input":"{}"}]}}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Errorf("write upstream websocket response: %v", err)
		}
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "websocket-mapping-conflict", "upstream-websocket-mapping-conflict", "access-websocket-mapping-conflict")

	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/v1/responses"
	header := http.Header{}
	header.Set("Session-Id", downstreamID)
	header.Set("Thread-Id", downstreamID)
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, header)
	if err != nil {
		if response != nil && response.Body != nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("downstream websocket dial: %v body=%s", err, body)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","session_id":"`+downstreamID+`","thread_id":"`+downstreamID+`","input":"create tool call"}`)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(codexWebSocketTestDeadline)); err != nil {
		t.Fatal(err)
	}
	_, terminal, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read downstream websocket terminal: %v", err)
	}
	if conflictErr != nil || !bytes.Contains(terminal, []byte(`"type":"response.completed"`)) || !bytes.Contains(terminal, []byte(responseID)) {
		t.Fatalf("upstream success was not delivered conflict_setup_err=%v terminal=%s", conflictErr, terminal)
	}
	goals, err := h.store.ListGoalSessions(context.Background(), 10)
	if err != nil || len(goals) != 1 {
		t.Fatalf("goal sessions=%+v err=%v", goals, err)
	}
	replay, _, err := h.store.BuildGoalReplay(context.Background(), goals[0].ID)
	if err != nil || !bytes.Contains(replay, []byte(`"type":"custom_tool_call"`)) || !bytes.Contains(replay, []byte(callID)) {
		t.Fatalf("mapping conflict discarded tool checkpoint replay=%s err=%v", replay, err)
	}
	var commitFailures int
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM audit_log WHERE action='codex_session_mapping_commit_failed' AND reason='codex_session_mapping_ambiguous'`).Scan(&commitFailures); err != nil || commitFailures != 1 {
		t.Fatalf("mapping failure audit count=%d err=%v", commitFailures, err)
	}
}

func TestCodexMappedDownstreamWebSocketSurvivesTruncatedUpstreamTurn(t *testing.T) {
	const downstreamID = "client-websocket-truncated-root"
	var mu sync.Mutex
	var upstreamSessions []string
	upgrader := websocket.Upgrader{}
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upstreamSessions = append(upstreamSessions, r.Header.Get("Session-Id"))
		connection := len(upstreamSessions)
		mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream websocket upgrade: %v", err)
			return
		}
		defer conn.Close()
		if _, raw, err := conn.ReadMessage(); err != nil {
			t.Errorf("read upstream websocket request: %v", err)
			return
		} else if strings.Contains(string(raw), downstreamID) {
			t.Errorf("downstream session leaked in upstream websocket payload: %s", raw)
			return
		}
		if connection == 1 {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp-websocket-truncated","object":"response","model":"gpt-5.6-sol","status":"in_progress"}}`))
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-websocket-after-truncation","object":"response","model":"gpt-5.6-sol","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}}`))
	})
	enableCodexSessionMappingForTest(h)
	h.importAccount(t, "websocket-truncated", "upstream-websocket-truncated", "access-websocket-truncated")

	wsURL := "ws" + strings.TrimPrefix(h.pool.URL, "http") + "/v1/responses"
	downstreamHeader := http.Header{}
	downstreamHeader.Set("Session-Id", downstreamID)
	downstreamHeader.Set("Thread-Id", downstreamID)
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, downstreamHeader)
	if err != nil {
		if response != nil && response.Body != nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			t.Fatalf("downstream websocket dial: %v body=%s", err, body)
		}
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"first turn is truncated"}`)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(codexWebSocketTestDeadline)); err != nil {
		t.Fatal(err)
	}
	var firstEvents strings.Builder
	for eventCount := 0; eventCount < 4; eventCount++ {
		_, event, err := conn.ReadMessage()
		if err != nil {
			mu.Lock()
			sessions := append([]string(nil), upstreamSessions...)
			mu.Unlock()
			t.Fatalf("downstream websocket disconnected before failure terminal: %v; events=%s sessions=%q", err, firstEvents.String(), sessions)
		}
		firstEvents.Write(event)
		if bytes.Contains(event, []byte(`"type":"response.failed"`)) {
			break
		}
	}
	first := firstEvents.String()
	if !strings.Contains(first, `"type":"response.failed"`) || !strings.Contains(first, `"code":"server_error"`) || !strings.Contains(first, publicRetryMessage) || strings.Contains(first, "stream closed before response.completed") || strings.Contains(first, "stream disconnected before completion") {
		t.Fatalf("truncated upstream websocket did not produce a stable terminal: %s", first)
	}

	if err := conn.SetWriteDeadline(time.Now().Add(codexWebSocketTestDeadline)); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"second turn still works"}`)); err != nil {
		t.Fatalf("downstream websocket was not reusable after failure: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(codexWebSocketTestDeadline)); err != nil {
		t.Fatal(err)
	}
	_, completed, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read second websocket turn: %v", err)
	}
	if !bytes.Contains(completed, []byte(`"type":"response.completed"`)) || !bytes.Contains(completed, []byte("resp-websocket-after-truncation")) {
		t.Fatalf("second websocket turn did not complete: %s", completed)
	}

	mu.Lock()
	sessions := append([]string(nil), upstreamSessions...)
	mu.Unlock()
	if len(sessions) != 2 || sessions[0] == "" || sessions[1] == "" || sessions[0] == sessions[1] {
		t.Fatalf("truncated websocket session lifecycle=%q", sessions)
	}
	for _, sessionID := range sessions {
		requireMappedUUIDv7(t, sessionID)
		if sessionID == downstreamID {
			t.Fatalf("downstream session reached upstream: %q", sessions)
		}
	}
	namespace := codexNativeNamespaceForTest(t, "", downstreamID)
	if rows, err := h.store.FindCodexSessionAlias(context.Background(), namespace, storage.CodexSessionAlias{Type: "response", Value: "resp-websocket-after-truncation"}); err != nil || len(rows) != 1 || rows[0].RootSessionID != sessions[1] {
		t.Fatalf("post-truncation terminal mapping rows=%+v err=%v", rows, err)
	}
}

func TestCodexSSETerminalsSurviveLineEndingsAndByteSplits(t *testing.T) {
	tests := []struct {
		name       string
		stream     string
		terminal   string
		successful bool
	}{
		{
			name: "completed_crlf",
			stream: "event: response.completed\r\n" +
				`data: {"type":"response.completed","response":{"id":"resp-crlf","status":"completed"}}` + "\r\n\r\n",
			terminal: "response.completed", successful: true,
		},
		{
			name: "failed_lf_without_trailing_blank",
			stream: "event: response.failed\n" +
				`data: {"type":"response.failed","response":{"id":"resp-failed","status":"failed"}}`,
			terminal: "response.failed",
		},
		{
			name:     "incomplete_mixed_without_response",
			stream:   "event: response.incomplete\r\ndata: {\"type\":\"response.incomplete\"}\n\r\n",
			terminal: "response.incomplete",
		},
		{
			name:     "response_error_without_trailing_blank",
			stream:   `data: {"type":"response.error","error":{"message":"terminal"}}`,
			terminal: "response.error",
		},
		{
			name:     "error_crlf_without_response",
			stream:   "event: error\r\ndata: {\"type\":\"error\",\"error\":{\"message\":\"terminal\"}}\r\n\r\n",
			terminal: "error",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := newCodexStreamLedgerRecorder()
			for _, oneByte := range []byte(test.stream) {
				if _, err := recorder.Write([]byte{oneByte}); err != nil {
					t.Fatal(err)
				}
			}
			recorder.finish()
			if !recorder.reachedTerminal() || recorder.completedSuccessfully() != test.successful || recorder.terminal != test.terminal {
				t.Fatalf("terminal=%q reached=%v successful=%v response=%s", recorder.terminal, recorder.reachedTerminal(), recorder.completedSuccessfully(), recorder.ResponseJSON())
			}
			if len(recorder.ResponseJSON()) == 0 {
				t.Fatal("terminal response JSON was not retained")
			}
		})
	}
}

func TestTerminalCommitWriterHandlesCRLFByteSplitsAndTailWithoutBlankLine(t *testing.T) {
	recorder := httptest.NewRecorder()
	commits := 0
	w := newTerminalCommitWriter(recorder, func() error {
		commits++
		if strings.Contains(recorder.Body.String(), "response.completed") {
			t.Fatal("terminal was written before its mapping commit")
		}
		return nil
	})
	stream := "event: response.completed\r\n" +
		`data: {"type":"response.completed","response":{"id":"resp-commit-tail","status":"completed"}}`
	for _, oneByte := range []byte(stream) {
		if _, err := w.Write([]byte{oneByte}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if commits != 1 || !strings.Contains(recorder.Body.String(), "resp-commit-tail") {
		t.Fatalf("commits=%d body=%q", commits, recorder.Body.String())
	}
}

func TestUserGroupCompletedResponseBindingPinsPreviousResponseTarget(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	if err := h.store.CreateGroup(t.Context(), storage.Group{Name: "response-binding-secondary"}); err != nil {
		t.Fatal(err)
	}
	targets := []storage.TargetRef{
		{Kind: storage.TargetKindAccountPoolGroup, ID: "cyber"},
		{Kind: storage.TargetKindAccountPoolGroup, ID: "response-binding-secondary"},
	}
	createRouteTestGroup(t, h, "ug_response_binding", targets, nil)
	raw := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":"start"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	recorder := httptest.NewRecorder()
	var selected storage.TargetRef
	handled := h.app.dispatchUserGroupRouteCandidates(recorder, req, raw, raw, downstreamPolicy{UserGroupID: "ug_response_binding"}, func(w http.ResponseWriter, candidate *http.Request) {
		selected, _ = userGroupRouteOverride(candidate.Context())
		w.Header().Set("Content-Type", "text/event-stream")
		frame := "event: response.completed\r\n" +
			`data: {"type":"response.completed","response":{"id":"resp-user-group-pin","status":"completed"}}`
		for _, oneByte := range []byte(frame) {
			_, _ = w.Write([]byte{oneByte})
		}
	})
	if !handled || selected.ID == "" || !strings.Contains(recorder.Body.String(), "resp-user-group-pin") {
		t.Fatalf("handled=%v selected=%+v body=%s", handled, selected, recorder.Body.String())
	}

	resumeRaw := []byte(`{"model":"gpt-5.6-sol","previous_response_id":"resp-user-group-pin","input":"continue"}`)
	resume := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	plan, err := resolveUserGroupRouteCandidates(resume.Context(), h.store, downstreamPolicy{UserGroupID: "ug_response_binding"}, resume, resumeRaw)
	if err != nil || len(plan.Candidates) != 2 || plan.Candidates[0] != selected {
		t.Fatalf("previous_response target plan=%+v selected=%+v err=%v", plan, selected, err)
	}
	hash := routing.ResponseAffinityKey("resp-user-group-pin").Hash
	binding, found, err := h.store.GetUserGroupTargetBinding(t.Context(), "ug_response_binding", hash, "")
	if err != nil || !found || binding.Target != selected {
		t.Fatalf("response target binding=%+v found=%v err=%v", binding, found, err)
	}
	if _, rawFound, err := h.store.GetUserGroupTargetBinding(t.Context(), "ug_response_binding", "resp-user-group-pin", ""); err != nil || rawFound {
		t.Fatalf("raw response id must not be stored as affinity key: found=%v err=%v", rawFound, err)
	}
}
