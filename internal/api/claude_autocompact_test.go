package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

func TestBuildClaudeAutoCompactPlanUsesSelectedNativeWindow(t *testing.T) {
	raw := []byte(`{"model":"claude-opus-4-8","max_tokens":32000,"messages":[{"role":"user","content":"` + strings.Repeat("x", 900_000) + `"}]}`)
	capRow := storage.ModelCapability{
		ModelSlug:                     "claude-opus-4-8",
		NativeContextWindow:           200_000,
		NativeMaxContextWindow:        1_000_000,
		EffectiveContextWindowPercent: 100,
	}
	for _, tc := range []struct {
		provider string
		limit    int64
		policy   string
	}{
		{provider: "claude", limit: 167_000, policy: "claude_code_standard_window"},
		{provider: "kiro", limit: 160_000, policy: "kiro_official_80pct"},
		{provider: "antigravity", limit: 100_000, policy: "antigravity_google_50pct"},
	} {
		plan, ok := buildClaudeAutoCompactPlan(raw, "claude-opus-4-8[1m]", capRow.ModelSlug, tc.provider, capRow, true, true)
		if !ok {
			t.Fatalf("%s virtual 1M request did not trigger compaction at the selected 200K window", tc.provider)
		}
		if plan.Provider != tc.provider || plan.NativeWindow != 200_000 || plan.EffectiveLimit != tc.limit || plan.TriggerPolicy != tc.policy || !plan.Virtual1M {
			t.Fatalf("%s plan = %+v, want native=200K trigger=%d policy=%s virtual=true", tc.provider, plan, tc.limit, tc.policy)
		}
	}

	native, ok := buildClaudeAutoCompactPlan(raw, "claude-opus-4-8[1m]", capRow.ModelSlug, "claude", capRow, true, false)
	if ok {
		t.Fatalf("300K request was compacted against a real 1M Claude account: %+v", native)
	}
	nativeRaw := []byte(`{"model":"claude-opus-4-8","max_tokens":32000,"messages":[{"role":"user","content":"` + strings.Repeat("x", 3_000_000) + `"}]}`)
	native, ok = buildClaudeAutoCompactPlan(nativeRaw, "claude-opus-4-8[1m]", capRow.ModelSlug, "claude", capRow, true, false)
	if ok {
		t.Fatalf("oversized native Claude 1M request was intercepted instead of forwarded: %+v", native)
	}
}

func TestProviderClaudeAutoCompactLimitsFollowSelectedAccountPolicy(t *testing.T) {
	tests := []struct {
		name, provider, policy        string
		nativeWindow, effectiveWindow int64
		want                          int64
	}{
		{name: "Claude Pro 200K", provider: "claude", nativeWindow: 200_000, effectiveWindow: 200_000, want: 167_000, policy: "claude_code_standard_window"},
		{name: "Kiro standard", provider: "kiro", nativeWindow: 200_000, effectiveWindow: 200_000, want: 160_000, policy: "kiro_official_80pct"},
		{name: "Antigravity standard", provider: "antigravity", nativeWindow: 200_000, effectiveWindow: 200_000, want: 100_000, policy: "antigravity_google_50pct"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, policy := providerClaudeAutoCompactLimit(tc.provider, tc.nativeWindow, tc.effectiveWindow)
			if got != tc.want || policy != tc.policy {
				t.Fatalf("limit=%d policy=%q, want %d %q", got, policy, tc.want, tc.policy)
			}
		})
	}
}

func TestSelectedClaudeAutoCompactPlanChangesWithSelectedAccount(t *testing.T) {
	h := newHarness(t, func(http.ResponseWriter, *http.Request) {})
	for _, id := range []string{"claude-pro-200k", "claude-native-1m"} {
		if err := h.store.UpsertAccount(t.Context(), storage.Account{ID: id, GroupName: "cyber", Provider: "claude", Status: "active"}, storage.AccountToken{AccountID: id, AccessToken: "token-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	rows := []storage.ModelCapability{
		{
			AccountID: "claude-pro-200k", ModelSlug: "claude-opus-4-8",
			AvailabilityState: capability.AvailabilityVerified, Context1MState: capability.Context1MUnsupported,
			NativeContextWindow: 200_000, NativeMaxContextWindow: 1_000_000, EffectiveContextWindowPercent: 100,
		},
		{
			AccountID: "claude-native-1m", ModelSlug: "claude-opus-4-8",
			AvailabilityState: capability.AvailabilityVerified, Context1MState: capability.Context1MSupported,
			NativeContextWindow: 200_000, NativeMaxContextWindow: 1_000_000, EffectiveContextWindowPercent: 100,
		},
	}
	if err := h.store.UpsertCapabilities(t.Context(), rows); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"model":"claude-opus-4-8","max_tokens":32000,"messages":[{"role":"user","content":"` + strings.Repeat("x", 900_000) + `"}]}`)

	pro, ok := h.app.selectedClaudeAutoCompactPlan(t.Context(), raw, scheduler.Lease{Account: storage.Account{ID: rows[0].AccountID, Provider: "claude"}}, rows[0].ModelSlug, "1m", true)
	if !ok || pro.NativeWindow != 200_000 || pro.EffectiveLimit != 167_000 || pro.TriggerPolicy != "claude_code_standard_window" {
		t.Fatalf("selected Pro plan = %+v, want its real 200K account boundary", pro)
	}
	native, ok := h.app.selectedClaudeAutoCompactPlan(t.Context(), raw, scheduler.Lease{Account: storage.Account{ID: rows[1].AccountID, Provider: "claude"}}, rows[1].ModelSlug, "1m", false)
	if ok {
		t.Fatalf("selected native-1M account compacted a 300K request: %+v", native)
	}
	nativeRaw := []byte(`{"model":"claude-opus-4-8","max_tokens":32000,"messages":[{"role":"user","content":"` + strings.Repeat("x", 3_000_000) + `"}]}`)
	native, ok = h.app.selectedClaudeAutoCompactPlan(t.Context(), nativeRaw, scheduler.Lease{Account: storage.Account{ID: rows[1].AccountID, Provider: "claude"}}, rows[1].ModelSlug, "1m", false)
	if ok {
		t.Fatalf("selected native-1M account was intercepted instead of forwarded: %+v", native)
	}
}

func TestBuildClaudeAutoCompactPlanReservesRequestedOutput(t *testing.T) {
	raw := []byte(`{"max_tokens":80000,"messages":[{"role":"user","content":"` + strings.Repeat("x", 370_000) + `"}]}`)
	plan, ok := buildClaudeAutoCompactPlan(raw, "claude", "claude", "kiro", storage.ModelCapability{
		NativeContextWindow: 200_000, EffectiveContextWindowPercent: 100,
	}, false, false)
	if !ok || plan.EffectiveLimit != 120_000 || plan.OutputReserve != 80_000 {
		t.Fatalf("plan = %+v, want output-reserved trigger 120K", plan)
	}
	impossible, ok := buildClaudeAutoCompactPlan([]byte(`{"max_tokens":200000,"messages":[{"role":"user","content":"hi"}]}`), "claude", "claude", "kiro", storage.ModelCapability{
		NativeContextWindow: 200_000, EffectiveContextWindowPercent: 100,
	}, false, false)
	if !ok || impossible.EffectiveLimit != 1 {
		t.Fatalf("full-window output reservation was not rejected locally: %+v", impossible)
	}
}

func TestWriteClaudeAutoCompactRequiredUsesClaudeCodeGrammar(t *testing.T) {
	w := httptest.NewRecorder()
	writeClaudeAutoCompactRequired(w, claudeAutoCompactPlan{
		RequestedModel: "claude-opus-4-8[1m]", ResolvedModel: "claude-opus-4-8", Provider: "kiro",
		EstimatedInput: 160001, EffectiveLimit: 160000, NativeWindow: 200000, OutputReserve: 32000, Virtual1M: true,
		TriggerPolicy: "kiro_official_80pct",
	})
	if w.Code != http.StatusBadRequest || w.Header().Get("X-MiCliProxy-Auto-Compact") != "claude_code_reactive" ||
		w.Header().Get("X-MiCliProxy-Context-Mode") != "virtual_1m" ||
		w.Header().Get("X-MiCliProxy-Context-Policy") != "kiro_official_80pct" {
		t.Fatalf("status/headers = %d %#v", w.Code, w.Header())
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "invalid_request_error" || body.Error.Code != "context_length_exceeded" ||
		!strings.HasPrefix(body.Error.Message, "Prompt is too long: 160001 tokens > 160000") ||
		!strings.Contains(body.Error.Message, "Claude Code should automatically compact") {
		t.Fatalf("error = %+v", body.Error)
	}
}
