package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/routing"
)

func TestClaudeCacheRolloutPercentDoesNotVaryByClaudeModel(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	h.app.cfg.ClaudeCacheOptimizationRollout = `{"groups":["g"],"percent":53}`

	system := strings.Repeat("stable system prompt. ", 140)
	body := []byte(`{"system":"` + system + `","tools":[{"name":"Read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"latest request"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	opusAffinity := h.app.claudeSelectionAffinity(context.Background(), req, body, body, "g", "api", "claude-opus-4-8")
	sonnetAffinity := h.app.claudeSelectionAffinity(context.Background(), req, body, body, "g", "api", "claude-sonnet-4-6")
	if opusAffinity.Source != "cache_prefix_hash" || sonnetAffinity.Source != "cache_prefix_hash" {
		t.Fatalf("rollout decision varied by Claude model: opus=%q sonnet=%q", opusAffinity.Source, sonnetAffinity.Source)
	}
}

func TestClaudeCacheBreakpointDefaultIsAggressiveEvenOnCoarseRoute(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp"}`))
	})
	policy := h.app.claudeCacheBreakpointPolicy(context.Background(), routing.AffinityFromKey("coarse", "downstream_api_project_model"), "acc", "cyber", "keyhash")
	if policy != "aggressive" {
		t.Fatalf("default claude cache breakpoint policy = %q, want aggressive (coarse_safe must be explicit)", policy)
	}
	h.app.cfg.ClaudeCacheBreakpointPolicy = "coarse_safe"
	policy = h.app.claudeCacheBreakpointPolicy(context.Background(), routing.AffinityFromKey("coarse", "downstream_api_project_model"), "acc", "cyber", "keyhash")
	if policy != "coarse_safe" {
		t.Fatalf("explicit coarse_safe policy = %q", policy)
	}
}
