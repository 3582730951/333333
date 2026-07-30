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

func seedCodexPassthroughAccount(t *testing.T, h *testHarness, label, token string) {
	t.Helper()
	h.importAccount(t, label, "", token)
}

func TestSharedEndpointCodexHintDoesNotFallThroughToClaude(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/skills" {
			t.Fatalf("path = %s, want Codex skills path", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer codex-skill-token" {
			t.Fatalf("auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"skill_1"}`))
	})
	seedClaudePassthroughAccount(t, h)
	seedCodexPassthroughAccount(t, h, "codex-skill", "codex-skill-token")
	// A provider-shaped protocol signal outranks the downstream key's policy.
	seedDownstreamKey(t, h, "cap_codex_hint", "claude")

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
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Path != "/backend-api/codex/skills" || reqs[0].Auth != "Bearer codex-skill-token" {
		t.Fatalf("unexpected upstream calls: %+v", reqs)
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
	seedCodexPassthroughAccount(t, h, "codex-should-not-win", "codex-should-not-win-token")
	// Anthropic wire headers are stronger than a conflicting key policy.
	seedDownstreamKey(t, h, "cap_claude_hint", "codex")

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

func TestSharedEndpointAutoWithoutProviderSignalDefaultsToCodex(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/files" {
			t.Fatalf("path = %s, want Codex files path", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer codex-default-token" {
			t.Fatalf("auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	seedClaudePassthroughAccount(t, h)
	seedCodexPassthroughAccount(t, h, "codex-default", "codex-default-token")
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
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Path != "/backend-api/codex/files" || reqs[0].Auth != "Bearer codex-default-token" {
		t.Fatalf("unexpected upstream calls: %+v", reqs)
	}
}

func TestSharedEndpointCodexClientSignalsOverrideKeyPolicy(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/skills" {
			t.Fatalf("path = %s, want Codex skills path", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer codex-client-token" {
			t.Fatalf("auth = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	seedClaudePassthroughAccount(t, h)
	seedCodexPassthroughAccount(t, h, "codex-client", "codex-client-token")
	seedDownstreamKey(t, h, "cap_conflicting_claude_hint", "claude")

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "user agent", key: "User-Agent", value: "codex_cli_rs/0.146.0 (Linux; x86_64)"},
		{name: "originator", key: "Originator", value: "codex_exec"},
		{name: "protocol header", key: "X-Codex-Window-ID", value: "window_fixture"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, h.pool.URL+"/v1/skills", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer cap_conflicting_claude_hint")
			req.Header.Set(test.key, test.value)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d body=%s", resp.StatusCode, body)
			}
		})
	}
	if reqs := h.requests(); len(reqs) != len(tests) {
		t.Fatalf("upstream calls = %d, want %d: %+v", len(reqs), len(tests), reqs)
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
