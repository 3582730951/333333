package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

const codexGoalContinuationFixture = `<codex_internal_context source="goal">
Continue working toward the active thread goal.
</codex_internal_context>`

func goalQuotaFixtureBody(t *testing.T, stream bool, cacheKey string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"model":            "gpt",
		"stream":           stream,
		"prompt_cache_key": cacheKey,
		"client_metadata":  map[string]string{"turn_id": "goal-turn-1"},
		"input": []interface{}{map[string]interface{}{
			"role": "user",
			"content": []interface{}{map[string]string{
				"type": "input_text", "text": codexGoalContinuationFixture,
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func postGoalQuotaFixture(t *testing.T, h *testHarness, body []byte, session string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session-Id", session)
	req.Header.Set("Thread-Id", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp, out
}

func pinGoalFixtureToAccount(t *testing.T, h *testHarness, body []byte, session, accountID string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Session-Id", session)
	req.Header.Set("Thread-Id", session)
	key := routing.ExtractAffinityKey(req, body)
	if key.Hash == "" {
		t.Fatal("goal fixture produced no affinity key")
	}
	if err := h.store.UpsertAffinityBinding(context.Background(), storage.AffinityBinding{
		RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: accountID,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexGoalSignalUsesNewestExplicitState(t *testing.T) {
	active := goalQuotaFixtureBody(t, true, "goal-signal")
	if got := codexGoalSignal(active); got != codexGoalTurnActive {
		t.Fatalf("Goal marker signal=%v", got)
	}

	var root map[string]interface{}
	if err := json.Unmarshal(active, &root); err != nil {
		t.Fatal(err)
	}
	root["input"] = append(root["input"].([]interface{}), map[string]interface{}{
		"role": "user", "content": []interface{}{map[string]string{"type": "input_text", "text": "ordinary later turn"}},
	})
	ordinary, _ := json.Marshal(root)
	if got := codexGoalSignal(ordinary); got != codexGoalTurnInactive {
		t.Fatalf("newer ordinary user signal=%v", got)
	}

	toolOutput := []byte(`{"input":[{"type":"function_call_output","call_id":"goal-call","output":"{\"goal\":{\"status\":\"active\"}}"}]}`)
	if got := codexGoalSignal(toolOutput); got != codexGoalTurnActive {
		t.Fatalf("Goal tool result signal=%v", got)
	}
}

func TestCodexGoalQuotaPredicateAndTerminal(t *testing.T) {
	ctx := withCodexGoalQuotaGrace(context.Background(), true)
	generic := []byte(`{"error":{"type":"rate_limit_exceeded","message":"retry later"}}`)
	fixed := []byte(`{"error":{"type":"usage_limit_reached","message":"private upstream details"}}`)
	if !codexGoalHoldsNonAuthoritativeQuotaSignal(ctx, http.StatusTooManyRequests, nil, generic) {
		t.Fatal("generic 429 did not retain Goal account")
	}
	if codexGoalHoldsNonAuthoritativeQuotaSignal(ctx, http.StatusTooManyRequests, nil, fixed) ||
		!codexGoalAuthoritativeUsageLimit(ctx, http.StatusTooManyRequests, fixed) {
		t.Fatal("fixed usage terminal was not promoted to account switch")
	}

	recorder := httptest.NewRecorder()
	writeCodexGoalUsageLimitTerminal(recorder, fixed, false)
	if recorder.Code != http.StatusTooManyRequests || !strings.Contains(recorder.Body.String(), `"type":"usage_limit_reached"`) ||
		strings.Contains(recorder.Body.String(), "private upstream details") {
		t.Fatalf("HTTP terminal=%d %s", recorder.Code, recorder.Body.String())
	}
	sse := []byte(`{"type":"response.failed","response":{"error":{"code":"insufficient_quota","message":"private stream details"}}}`)
	recorder = httptest.NewRecorder()
	writeCodexGoalUsageLimitTerminal(recorder, sse, true)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "event: response.failed") ||
		!strings.Contains(recorder.Body.String(), `"code":"insufficient_quota"`) || strings.Contains(recorder.Body.String(), "private stream details") {
		t.Fatalf("SSE terminal=%d %s", recorder.Code, recorder.Body.String())
	}
}

func TestCodexGoalGeneric429KeepsBoundAccount(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer goal-generic-a" {
			t.Errorf("Goal generic 429 switched account: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_exceeded","message":"retry later"}}`)
	})
	enableCodexSessionMappingForTest(h)
	body := goalQuotaFixtureBody(t, false, "goal-generic-hold")
	accountA := h.importAccount(t, "goal-generic-a", "upstream-goal-generic-a", "goal-generic-a")
	h.importAccount(t, "goal-generic-b", "upstream-goal-generic-b", "goal-generic-b")
	pinGoalFixtureToAccount(t, h, body, "goal-generic-root", accountA)

	_, _ = postGoalQuotaFixture(t, h, body, "goal-generic-root")
	if calls.Load() != 1 || len(h.requests()) != 1 {
		t.Fatalf("generic Goal 429 attempts=%d requests=%+v", calls.Load(), h.requests())
	}
	binding, err := h.store.GetEgressBinding(context.Background(), accountA)
	if err != nil {
		t.Fatal(err)
	}
	if binding.RecheckPending || binding.CooldownUntil != 0 {
		t.Fatalf("generic Goal 429 changed account health: %+v", binding)
	}
}

func TestCodexGoalFixedUsageErrorSwitchesAndCapsAttempts(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		attempt := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if attempt < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"fixed"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp-goal-ok","object":"response","status":"completed","model":"gpt","output":[]}`)
	})
	enableCodexSessionMappingForTest(h)
	h.app.cfg.FailoverMaxAttempts = 999999
	body := goalQuotaFixtureBody(t, false, "goal-fixed-switch")
	accountA := h.importAccount(t, "goal-fixed-a", "upstream-goal-fixed-a", "goal-fixed-a")
	h.importAccount(t, "goal-fixed-b", "upstream-goal-fixed-b", "goal-fixed-b")
	h.importAccount(t, "goal-fixed-c", "upstream-goal-fixed-c", "goal-fixed-c")
	h.importAccount(t, "goal-fixed-d", "upstream-goal-fixed-d", "goal-fixed-d")
	pinGoalFixtureToAccount(t, h, body, "goal-fixed-root", accountA)

	resp, result := postGoalQuotaFixture(t, h, body, "goal-fixed-root")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(result), "resp-goal-ok") {
		t.Fatalf("Goal fixed-error recovery status=%d body=%s", resp.StatusCode, result)
	}
	requests := h.requests()
	if calls.Load() != 3 || len(requests) != 3 || requests[0].Auth != "Bearer goal-fixed-a" ||
		requests[1].Auth == requests[0].Auth || requests[2].Auth == requests[1].Auth {
		t.Fatalf("Goal switch lifecycle calls=%d requests=%+v", calls.Load(), requests)
	}
}

func TestCodexGoalFinalFixedUsageErrorReachesCLI(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"account-private detail"}}`)
	})
	enableCodexSessionMappingForTest(h)
	body := goalQuotaFixtureBody(t, false, "goal-fixed-terminal")
	account := h.importAccount(t, "goal-terminal", "upstream-goal-terminal", "goal-terminal")
	pinGoalFixtureToAccount(t, h, body, "goal-terminal-root", account)

	resp, result := postGoalQuotaFixture(t, h, body, "goal-terminal-root")
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(result), `"type":"usage_limit_reached"`) ||
		strings.Contains(string(result), "account-private detail") || len(h.requests()) != 1 {
		t.Fatalf("final fixed terminal status=%d requests=%d body=%s", resp.StatusCode, len(h.requests()), result)
	}
}

func TestCodexGoalSSEFixedUsageErrorSwitches(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) {
		attempt := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if attempt == 1 {
			_, _ = io.WriteString(w, "event: response.failed\n"+
				`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"insufficient_quota","message":"fixed"}}}`+"\n\n")
			return
		}
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp-goal-stream-ok","object":"response","status":"completed","model":"gpt","output":[]}}`+"\n\n"+
			"data: [DONE]\n\n")
	})
	enableCodexSessionMappingForTest(h)
	body := goalQuotaFixtureBody(t, true, "goal-stream-fixed")
	accountA := h.importAccount(t, "goal-stream-a", "upstream-goal-stream-a", "goal-stream-a")
	h.importAccount(t, "goal-stream-b", "upstream-goal-stream-b", "goal-stream-b")
	pinGoalFixtureToAccount(t, h, body, "goal-stream-root", accountA)

	resp, result := postGoalQuotaFixture(t, h, body, "goal-stream-root")
	if resp.StatusCode != http.StatusOK || calls.Load() != 2 || !strings.Contains(string(result), "resp-goal-stream-ok") ||
		strings.Contains(string(result), "insufficient_quota") {
		t.Fatalf("SSE Goal switch status=%d calls=%d body=%s", resp.StatusCode, calls.Load(), result)
	}
}

func TestCodexGoalStatefulFixedUsageErrorRebuildsAndPreservesToolExchange(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		switch r.Header.Get("Authorization") {
		case "Bearer goal-stateful-a":
			if call == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp-goal-stateful-origin","object":"response","status":"completed","model":"gpt","output":[]}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"fixed private account detail"}}`)
		case "Bearer goal-stateful-b":
			raw, _ := io.ReadAll(r.Body)
			payload := string(raw)
			for _, want := range []string{
				`"type":"agent_message"`, "readable-agent-summary",
				`"type":"function_call"`, `"type":"function_call_output"`,
				"call-preserve", "preserve-tool-result",
			} {
				if !strings.Contains(payload, want) {
					t.Errorf("rebuilt Goal request lost %q: %s", want, payload)
				}
			}
			for _, rejected := range []string{"previous_response_id", "resp-goal-stateful-origin", "foreign-agent-ciphertext"} {
				if strings.Contains(payload, rejected) {
					t.Errorf("rebuilt Goal request retained account-local %q: %s", rejected, payload)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp-goal-stateful-rebuilt","object":"response","status":"completed","model":"gpt","output":[]}`)
		default:
			t.Errorf("unexpected Goal account: %q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	enableCodexSessionMappingForTest(h)
	firstBody := goalQuotaFixtureBody(t, false, "goal-stateful-rebuild")
	accountA := h.importAccount(t, "goal-stateful-a", "upstream-goal-stateful-a", "goal-stateful-a")
	h.importAccount(t, "goal-stateful-b", "upstream-goal-stateful-b", "goal-stateful-b")
	if err := h.store.SetAccountIgnoreRateLimitControls(context.Background(), accountA, true); err != nil {
		t.Fatal(err)
	}
	pinGoalFixtureToAccount(t, h, firstBody, "goal-stateful-root", accountA)

	first, firstResult := postGoalQuotaFixture(t, h, firstBody, "goal-stateful-root")
	if first.StatusCode != http.StatusOK || !strings.Contains(string(firstResult), "resp-goal-stateful-origin") {
		t.Fatalf("initial Goal root status=%d body=%s", first.StatusCode, firstResult)
	}
	secondBody := []byte(`{
		"model":"gpt","stream":false,"prompt_cache_key":"goal-stateful-rebuild",
		"previous_response_id":"resp-goal-stateful-origin",
		"client_metadata":{"turn_id":"goal-turn-1"},
		"input":[
			{"type":"agent_message","author":"/root/subagent","content":[
				{"type":"input_text","text":"readable-agent-summary"},
				{"type":"encrypted_content","encrypted_content":"foreign-agent-ciphertext"}
			]},
			{"type":"function_call","name":"shell","call_id":"call-preserve","arguments":"{}"},
			{"type":"function_call_output","call_id":"call-preserve","output":"preserve-tool-result"}
		]
	}`)
	second, secondResult := postGoalQuotaFixture(t, h, secondBody, "goal-stateful-root")
	if second.StatusCode != http.StatusOK || second.Header.Get("X-MiCliProxy-Context-Status") != "rebuilt" ||
		!strings.Contains(string(secondResult), "resp-goal-stateful-rebuilt") {
		t.Fatalf("stateful Goal recovery status=%d context=%q body=%s", second.StatusCode, second.Header.Get("X-MiCliProxy-Context-Status"), secondResult)
	}
	requests := h.requests()
	if calls.Load() != 3 || len(requests) != 3 || requests[0].Auth != "Bearer goal-stateful-a" ||
		requests[1].Auth != "Bearer goal-stateful-a" || requests[2].Auth != "Bearer goal-stateful-b" {
		t.Fatalf("stateful Goal fixed-error sends=%d requests=%+v", calls.Load(), requests)
	}
	if !strings.Contains(requests[1].Body, "foreign-agent-ciphertext") {
		t.Fatalf("normal durable request was sanitized before its bound account: %s", requests[1].Body)
	}
}

func TestCodexGoalFixedUsageErrorExitsIgnoredSameAccountRetry(t *testing.T) {
	previousFloor := ignoredRateLimitRetryFloor
	ignoredRateLimitRetryFloor = time.Millisecond
	t.Cleanup(func() { ignoredRateLimitRetryFloor = previousFloor })

	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch call := calls.Add(1); call {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_exceeded","message":"generic retry"}}`)
		case 2:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"fixed terminal"}}`)
		case 3:
			_, _ = io.WriteString(w, `{"id":"resp-goal-ignore-ok","object":"response","status":"completed","model":"gpt","output":[]}`)
		default:
			t.Errorf("fixed Goal terminal remained in same-account retry loop: call=%d auth=%q", call, r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	enableCodexSessionMappingForTest(h)
	body := goalQuotaFixtureBody(t, false, "goal-ignore-fixed")
	accountA := h.importAccount(t, "goal-ignore-a", "upstream-goal-ignore-a", "goal-ignore-a")
	h.importAccount(t, "goal-ignore-b", "upstream-goal-ignore-b", "goal-ignore-b")
	if err := h.store.SetAccountIgnoreRateLimitControls(context.Background(), accountA, true); err != nil {
		t.Fatal(err)
	}
	pinGoalFixtureToAccount(t, h, body, "goal-ignore-root", accountA)

	resp, result := postGoalQuotaFixture(t, h, body, "goal-ignore-root")
	requests := h.requests()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(result), "resp-goal-ignore-ok") ||
		calls.Load() != 3 || len(requests) != 3 || requests[0].Auth != "Bearer goal-ignore-a" ||
		requests[1].Auth != "Bearer goal-ignore-a" || requests[2].Auth != "Bearer goal-ignore-b" {
		t.Fatalf("ignored Goal fixed lifecycle status=%d calls=%d requests=%+v body=%s", resp.StatusCode, calls.Load(), requests, result)
	}
}

func TestCodexGoalStatefulSSEFixedUsageErrorRebuildsWithinThreeSends(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		switch r.Header.Get("Authorization") {
		case "Bearer goal-stateful-sse-a":
			if call == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp-goal-stateful-sse-origin","object":"response","status":"completed","model":"gpt","output":[]}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.failed\n"+
				`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"insufficient_quota","message":"fixed private stream detail"}}}`+"\n\n")
		case "Bearer goal-stateful-sse-b":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.completed\n"+
				`data: {"type":"response.completed","response":{"id":"resp-goal-stateful-sse-rebuilt","object":"response","status":"completed","model":"gpt","output":[]}}`+"\n\n"+
				"data: [DONE]\n\n")
		default:
			t.Errorf("unexpected stateful SSE Goal account: %q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	enableCodexSessionMappingForTest(h)
	firstBody := goalQuotaFixtureBody(t, false, "goal-stateful-sse")
	accountA := h.importAccount(t, "goal-stateful-sse-a", "upstream-goal-stateful-sse-a", "goal-stateful-sse-a")
	h.importAccount(t, "goal-stateful-sse-b", "upstream-goal-stateful-sse-b", "goal-stateful-sse-b")
	if err := h.store.SetAccountIgnoreRateLimitControls(context.Background(), accountA, true); err != nil {
		t.Fatal(err)
	}
	pinGoalFixtureToAccount(t, h, firstBody, "goal-stateful-sse-root", accountA)

	first, firstResult := postGoalQuotaFixture(t, h, firstBody, "goal-stateful-sse-root")
	if first.StatusCode != http.StatusOK || !strings.Contains(string(firstResult), "resp-goal-stateful-sse-origin") {
		t.Fatalf("initial SSE Goal root status=%d body=%s", first.StatusCode, firstResult)
	}
	secondBody := []byte(`{
		"model":"gpt","stream":true,"prompt_cache_key":"goal-stateful-sse",
		"previous_response_id":"resp-goal-stateful-sse-origin",
		"client_metadata":{"turn_id":"goal-turn-1"},
		"input":[
			{"type":"function_call","name":"shell","call_id":"call-sse-preserve","arguments":"{}"},
			{"type":"function_call_output","call_id":"call-sse-preserve","output":"sse-tool-result"}
		]
	}`)
	second, secondResult := postGoalQuotaFixture(t, h, secondBody, "goal-stateful-sse-root")
	requests := h.requests()
	if second.StatusCode != http.StatusOK || second.Header.Get("X-MiCliProxy-Context-Status") != "rebuilt" ||
		!strings.Contains(string(secondResult), "resp-goal-stateful-sse-rebuilt") || strings.Contains(string(secondResult), "insufficient_quota") ||
		calls.Load() != 3 || len(requests) != 3 || requests[0].Auth != "Bearer goal-stateful-sse-a" ||
		requests[1].Auth != "Bearer goal-stateful-sse-a" || requests[2].Auth != "Bearer goal-stateful-sse-b" {
		t.Fatalf("stateful SSE Goal lifecycle status=%d context=%q calls=%d requests=%+v body=%s", second.StatusCode, second.Header.Get("X-MiCliProxy-Context-Status"), calls.Load(), requests, secondResult)
	}
}
