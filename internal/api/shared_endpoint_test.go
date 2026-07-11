package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"codex-account-pool/internal/storage"
)

func seedClaudePassthroughAccount(t *testing.T, h *testHarness) {
	t.Helper()
	if err := h.store.UpsertAccount(context.Background(), storage.Account{
		ID:        "claude-pass",
		Label:     "claude pass",
		GroupName: "cyber",
		Provider:  "claude",
		Status:    "active",
	}, storage.AccountToken{AccessToken: "claude-token"}); err != nil {
		t.Fatal(err)
	}
}

func TestSharedEndpointCodexHintDoesNotFallThroughToClaude(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("OpenAI/Codex /v1/skills request should not hit Claude upstream: %s %s", r.Method, r.URL.Path)
	})
	seedClaudePassthroughAccount(t, h)
	seedDownstreamKey(t, h, "cap_codex_hint", "codex")

	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/skills", strings.NewReader(`{"name":"local-skill"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer cap_codex_hint")
	req.Header.Set("OpenAI-Beta", "skills=v1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	errObj := decodeErrorBody(t, resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, error=%#v", resp.StatusCode, errObj)
	}
	if errObj["type"] != "capability_unavailable" {
		t.Fatalf("error type = %#v, want capability_unavailable", errObj["type"])
	}
	if errObj["current_route"] != "codex_shared_endpoint" {
		t.Fatalf("current_route = %#v", errObj["current_route"])
	}
	if len(h.requests()) != 0 {
		t.Fatalf("unexpected upstream calls: %+v", h.requests())
	}
}

func TestSharedEndpointClaudeHintRoutesToClaudePassthrough(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/files" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Anthropic-Beta") != "files-api-2025-04-14" {
			t.Fatalf("Anthropic-Beta not preserved: %q", r.Header.Get("Anthropic-Beta"))
		}
		if r.Header.Get("Authorization") != "Bearer claude-token" {
			t.Fatalf("auth = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "opaque-bytes" {
			t.Fatalf("body changed: %q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"file_1"}`))
	})
	seedClaudePassthroughAccount(t, h)
	seedDownstreamKey(t, h, "cap_claude_hint", "claude")

	req, err := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/files", strings.NewReader("opaque-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", "cap_claude_hint")
	req.Header.Set("Anthropic-Beta", "files-api-2025-04-14")
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Path != "/v1/files" {
		t.Fatalf("unexpected upstream calls: %+v", reqs)
	}
}

func TestSharedEndpointAutoWithoutProviderSignalIsExplicitError(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("ambiguous shared endpoint should not hit upstream: %s %s", r.Method, r.URL.Path)
	})
	seedClaudePassthroughAccount(t, h)
	seedDownstreamKey(t, h, "cap_auto_hint", "auto")

	req, err := http.NewRequest(http.MethodGet, h.pool.URL+"/v1/files", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer cap_auto_hint")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	errObj := decodeErrorBody(t, resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, error=%#v", resp.StatusCode, errObj)
	}
	if errObj["type"] != "capability_unavailable" {
		t.Fatalf("error type = %#v", errObj["type"])
	}
	if !strings.Contains(errObj["fix_hint"].(string), "provider_hint") {
		t.Fatalf("fix_hint missing provider_hint guidance: %#v", errObj["fix_hint"])
	}
	if len(h.requests()) != 0 {
		t.Fatalf("unexpected upstream calls: %+v", h.requests())
	}
}

func TestClaudeResourceIDRemainsBoundToOriginalAccountAndEgress(t *testing.T) {
	upstreamCalls := 0
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"file_bound"}`))
	})
	for _, account := range []storage.Account{
		{ID: "claude-resource-a", Label: "a", GroupName: "cyber", Provider: "claude", Status: "active"},
		{ID: "claude-resource-b", Label: "b", GroupName: "cyber", Provider: "claude", Status: "active"},
	} {
		if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{AccessToken: "sk-ant-api-" + account.ID}); err != nil {
			t.Fatal(err)
		}
	}
	create, _ := http.NewRequest(http.MethodPost, h.pool.URL+"/v1/files", strings.NewReader("opaque"))
	create.Header.Set("X-Pool-Provider", "claude")
	create.Header.Set("Content-Type", "application/octet-stream")
	response, err := http.DefaultClient.Do(create)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || upstreamCalls != 1 {
		t.Fatalf("create status=%d calls=%d", response.StatusCode, upstreamCalls)
	}
	affinity, _, _ := claudeResourceAffinity("/v1/files/file_bound")
	bound, err := h.store.GetAffinityBinding(context.Background(), affinity.Hash)
	if err != nil || bound.Provider != "claude" || bound.Model != "resource:files" || bound.EgressID == "" {
		t.Fatalf("resource binding=%+v err=%v", bound, err)
	}
	if err := h.store.SetAccountStatus(context.Background(), bound.AccountID, "disabled"); err != nil {
		t.Fatal(err)
	}
	h.app.scheduler.InvalidateAccountCache()
	get, _ := http.NewRequest(http.MethodGet, h.pool.URL+"/v1/files/file_bound", nil)
	get.Header.Set("X-Pool-Provider", "claude")
	response, err = http.DefaultClient.Do(get)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusConflict || upstreamCalls != 1 || !strings.Contains(string(body), "bound_account_unavailable") {
		t.Fatalf("bound resource switched account: status=%d calls=%d body=%s", response.StatusCode, upstreamCalls, body)
	}
}
