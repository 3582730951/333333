package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

func TestAdminAccountsSummaryUsesAggregatedCounts(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	now := storage.Now()
	rows := []struct {
		account storage.Account
		token   storage.AccountToken
	}{
		{storage.Account{ID: "codex-active", GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "codex-token"}},
		{storage.Account{ID: "claude-legacy", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "sk-ant-oat-test"}},
		{storage.Account{ID: "custom-disabled", GroupName: "cyber", Provider: "deepseek", Status: "disabled"}, storage.AccountToken{OpenAIAPIKey: "sk-custom"}},
		{storage.Account{ID: "codex-quarantined", GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "codex-token-2"}},
		{storage.Account{ID: "kiro-active", GroupName: "kiro", Provider: "kiro", Status: "active"}, storage.AccountToken{AccessToken: "kiro-token"}},
		{storage.Account{ID: "cursor-active", GroupName: "cursor", Provider: "cursor", Status: "active"}, storage.AccountToken{AccessToken: "cursor-token"}},
	}
	for _, row := range rows {
		if err := h.store.UpsertAccount(ctx, row.account, row.token); err != nil {
			t.Fatalf("upsert %s: %v", row.account.ID, err)
		}
	}
	if err := h.store.SetBindingCooldown(ctx, "codex-active", now+300); err != nil {
		t.Fatalf("set cooldown: %v", err)
	}
	if err := h.store.SetBindingRecheckPending(ctx, "claude-legacy", true); err != nil {
		t.Fatalf("set recheck pending: %v", err)
	}
	if err := h.store.SetAccountQuarantine(ctx, "codex-quarantined", now+600, "test"); err != nil {
		t.Fatalf("set quarantine: %v", err)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/accounts/summary", "")
	if code != http.StatusOK {
		t.Fatalf("accounts summary = %d: %s", code, raw)
	}
	var got storage.AccountPoolSummary
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode summary: %v\n%s", err, raw)
	}
	want := storage.AccountPoolSummary{
		Total:       6,
		Active:      4,
		Quarantined: 1,
		Cooling:     1,
		Recheck:     1,
		Codex:       2,
		Claude:      1,
		Kiro:        1,
		Cursor:      1,
		Other:       1,
	}
	if got != want {
		t.Fatalf("summary = %#v, want %#v", got, want)
	}
	var shape map[string]interface{}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	if _, ok := shape["accounts"]; ok {
		t.Fatalf("summary endpoint returned full account list: %s", raw)
	}
}

func TestAdminAccountsPageExpandsRowsWithBatchData(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	if err := h.store.UpsertAccount(ctx,
		storage.Account{ID: "claude-legacy", GroupName: "cyber", Status: "active"},
		storage.AccountToken{AccessToken: "sk-ant-oat-test"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetBindingRecheckPending(ctx, "claude-legacy", true); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: "claude-legacy", ModelSlug: "claude-sonnet"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.InsertUsageRecord(ctx, "claude-legacy", "", "", "", "claude-sonnet", 10, 20, 30, 4, nil); err != nil {
		t.Fatal(err)
	}

	code, raw := grpReq(t, h, http.MethodGet, "/admin/accounts?page=1&pageSize=20", "")
	if code != http.StatusOK {
		t.Fatalf("accounts page = %d: %s", code, raw)
	}
	var payload struct {
		Accounts []struct {
			ID           string                    `json:"id"`
			Provider     string                    `json:"provider"`
			Capabilities []storage.ModelCapability `json:"capabilities"`
			Egress       struct {
				RecheckPending bool `json:"recheck_pending"`
			} `json:"egress_binding"`
			Usage *storage.UsageSummaryRow `json:"usage"`
		} `json:"accounts"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode accounts page: %v\n%s", err, raw)
	}
	if payload.Total != 1 || len(payload.Accounts) != 1 {
		t.Fatalf("accounts page shape = total %d rows %d: %s", payload.Total, len(payload.Accounts), raw)
	}
	row := payload.Accounts[0]
	if row.ID != "claude-legacy" || row.Provider != "claude" {
		t.Fatalf("row provider = id %q provider %q, want claude legacy fallback", row.ID, row.Provider)
	}
	if len(row.Capabilities) != 1 || row.Capabilities[0].ModelSlug != "claude-sonnet" {
		t.Fatalf("row capabilities = %#v, want claude-sonnet", row.Capabilities)
	}
	if !row.Egress.RecheckPending {
		t.Fatalf("row egress binding = %#v, want recheck pending", row.Egress)
	}
	if row.Usage == nil || row.Usage.Requests != 1 || row.Usage.TotalTokens != 30 || row.Usage.CachedTokens != 4 {
		t.Fatalf("row usage = %#v, want aggregated current-row usage", row.Usage)
	}
}
