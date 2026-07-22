package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/storage"
)

func TestGoalContinuityRebuildsResponseAliasAfterRestartStyleRequest(t *testing.T) {
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

func TestGoalContinuityCanStopLegacySnapshotDualWrite(t *testing.T) {
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
