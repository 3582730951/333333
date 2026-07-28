package api

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

func saturatedHTTPHarness(t *testing.T) (*testHarness, scheduler.Lease) {
	t.Helper()
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"status\":\"completed\"}}\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_ok","status":"completed","output":[]}`))
	})
	h.importAccount(t, "only", "upstream-only", "access-only")
	profile, _ := h.store.GetEgressProfile(context.Background(), storage.DefaultDirectEgressID)
	profile.MaxConcurrency = 1
	if err := h.store.UpsertEgressProfile(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	cfg := h.app.scheduler.Config()
	cfg.SchedulerHeartbeatSeconds = 1
	h.app.scheduler.UpdateConfig(cfg)
	held, err := h.app.scheduler.Select(context.Background(), scheduler.Route{Group: "cyber", Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	return h, held
}

func TestStreamingSaturationWaitsWithSSEHeartbeatThenResumes(t *testing.T) {
	h, held := saturatedHTTPHarness(t)
	defer held.Release()
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.2","input":"hello","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	responseCh := make(chan *http.Response, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		responseCh <- resp
	}()
	var resp *http.Response
	select {
	case err := <-errCh:
		t.Fatal(err)
	case resp = <-responseCh:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("streaming scheduler heartbeat was not committed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != "event: response.in_progress\n" {
		t.Fatalf("heartbeat event line=%q err=%v", line, err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || line != "data: {\"type\":\"response.in_progress\"}\n" {
		t.Fatalf("heartbeat data line=%q err=%v", line, err)
	}
	_, _ = reader.ReadString('\n')
	var action, state, reason string
	if err := h.store.DB().QueryRowContext(context.Background(), `SELECT action,state,reason FROM audit_log WHERE action='codex_scheduler_wait' ORDER BY id DESC LIMIT 1`).Scan(&action, &state, &reason); err != nil {
		t.Fatalf("scheduler wait audit missing: %v", err)
	}
	if action != "codex_scheduler_wait" || state != "queued" || reason == "" {
		t.Fatalf("scheduler wait audit=%q/%q/%q", action, state, reason)
	}
	held.Release()
	rest, err := io.ReadAll(reader)
	if err != nil || !bytes.Contains(rest, []byte("resp_ok")) {
		t.Fatalf("resumed stream body=%s err=%v", rest, err)
	}
}

func TestResponsesSchedulerWaitTerminalIsCanonicalSSE(t *testing.T) {
	recorder := &flushRecorder{header: make(http.Header)}
	ctx := withSchedulerWait(context.Background(), recorder, true, "responses")
	schedulerWaitCallback(ctx)("capacity", time.Second)
	if !schedulerWaitTerminal(ctx, context.DeadlineExceeded.Error()) {
		t.Fatal("committed scheduler wait was not terminated as SSE")
	}
	body := recorder.body.String()
	for _, want := range []string{
		"event: response.in_progress\n",
		"event: response.failed\n",
		`"code":"server_error"`,
		publicRetryMessage,
		"data: [DONE]\n\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("terminal stream missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "idle timeout") {
		t.Fatalf("terminal stream leaked an idle-timeout implementation detail:\n%s", body)
	}
}

func TestNonStreamingSaturationDoesNotCommitBeforeCapacity(t *testing.T) {
	h, held := saturatedHTTPHarness(t)
	defer held.Release()
	req, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.2","input":"hello","stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		done <- result{resp: resp, err: err}
	}()
	select {
	case got := <-done:
		if got.resp != nil {
			got.resp.Body.Close()
		}
		t.Fatalf("non-streaming request committed while saturated: err=%v", got.err)
	case <-time.After(120 * time.Millisecond):
	}
	held.Release()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		defer got.resp.Body.Close()
		body, _ := io.ReadAll(got.resp.Body)
		if got.resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("resp_ok")) {
			t.Fatalf("status=%d body=%s", got.resp.StatusCode, body)
		}
	case <-time.After(time.Second):
		t.Fatal("non-streaming request did not resume after lease release")
	}
}
