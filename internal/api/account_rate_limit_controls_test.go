package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-account-pool/internal/cf"
	"codex-account-pool/internal/storage"
)

func TestAdminAccountRateLimitControlsAreIndividual(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	first := h.importAccount(t, "controls-first", "up-controls-first", "access-controls-first")
	second := h.importAccount(t, "controls-second", "up-controls-second", "access-controls-second")

	code, body := grpReq(t, h, http.MethodPost, "/admin/accounts/"+first+"/rate-limit-controls", `{"ignore_rate_limit_controls":true}`)
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"ignore_rate_limit_controls":true`)) {
		t.Fatalf("enable override status=%d body=%s", code, body)
	}
	firstAccount, err := h.store.GetAccount(context.Background(), first)
	if err != nil || !firstAccount.IgnoreRateLimitControls {
		t.Fatalf("first account override=%v err=%v", firstAccount.IgnoreRateLimitControls, err)
	}
	secondAccount, err := h.store.GetAccount(context.Background(), second)
	if err != nil || secondAccount.IgnoreRateLimitControls {
		t.Fatalf("second account leaked override=%v err=%v", secondAccount.IgnoreRateLimitControls, err)
	}

	code, body = grpReq(t, h, http.MethodPatch, "/admin/accounts/"+first+"/rate-limit-controls", `{"ignore_rate_limit_controls":false}`)
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"ignore_rate_limit_controls":false`)) {
		t.Fatalf("disable override status=%d body=%s", code, body)
	}
	firstAccount, err = h.store.GetAccount(context.Background(), first)
	if err != nil || firstAccount.IgnoreRateLimitControls {
		t.Fatalf("first account should be restored to normal controls: %+v err=%v", firstAccount, err)
	}
}

func TestIgnoredControlsDoNotCoolAccountAfterCloudflare(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	accountID := h.importAccount(t, "cf-ignored", "up-cf-ignored", "access-cf-ignored")
	if err := h.store.SetAccountIgnoreRateLimitControls(context.Background(), accountID, true); err != nil {
		t.Fatal(err)
	}
	account, err := h.store.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	h.app.handleCFEvent(context.Background(), account, storage.EgressProfile{ID: storage.DefaultDirectEgressID}, http.StatusForbidden, cf.Detection{
		Matched: true, Category: "cf_body", CFRay: "ignored-test-ray",
	})
	binding, err := h.store.GetEgressBinding(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CooldownUntil != 0 || binding.RecheckPending {
		t.Fatalf("ignored CF event changed binding: %+v", binding)
	}
	updated, err := h.store.GetAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.QuarantineUntil != 0 {
		t.Fatalf("ignored CF event quarantined account: %+v", updated)
	}
}

func TestCodexIgnoredRateLimitRetriesSameStrictCPAAccount(t *testing.T) {
	var calls atomic.Int32
	var unexpectedAccount atomic.Bool
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer access-rate-control" {
			unexpectedAccount.Store(true)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-rate-root","object":"response","model":"gpt","status":"completed","output":[]}`))
		case 2:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate_limit_exceeded"}}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-rate-retried","object":"response","model":"gpt","status":"completed","output":[]}`))
		}
	})
	enableCodexSessionMappingForTest(h)
	accountID := h.importAccount(t, "rate-control", "up-rate-control", "access-rate-control")
	if err := h.store.SetAccountIgnoreRateLimitControls(context.Background(), accountID, true); err != nil {
		t.Fatal(err)
	}

	oldFloor := ignoredRateLimitRetryFloor
	ignoredRateLimitRetryFloor = time.Millisecond
	t.Cleanup(func() { ignoredRateLimitRetryFloor = oldFloor })

	post := func(body, turnState string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Thread-Id", "rate-control-thread")
		req.Header.Set("Session-Id", "rate-control-thread")
		if turnState != "" {
			req.Header.Set("X-Codex-Turn-State", turnState)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		payload, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(payload)
	}

	if status, body := post(`{"model":"gpt","thread_id":"rate-control-thread","session_id":"rate-control-thread","input":"root"}`, ""); status != http.StatusOK || !strings.Contains(body, "resp-rate-root") {
		t.Fatalf("root status=%d body=%s", status, body)
	}
	status, body := post(`{"model":"gpt","previous_response_id":"resp-rate-root","input":"resume"}`, "")
	if status != http.StatusOK || strings.Contains(body, "429") || !strings.Contains(body, "resp-rate-retried") {
		t.Fatalf("retry response status=%d body=%s", status, body)
	}
	if calls.Load() != 3 || unexpectedAccount.Load() {
		t.Fatalf("expected root + same-account 429 retry, calls=%d unexpected_account=%v", calls.Load(), unexpectedAccount.Load())
	}
	binding, err := h.store.GetEgressBinding(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CooldownUntil != 0 || binding.RecheckPending {
		t.Fatalf("429 should not bench overridden account: %+v", binding)
	}
}
