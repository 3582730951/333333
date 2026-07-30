package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"codex-account-pool/internal/scheduler"
	"codex-account-pool/internal/storage"
)

func TestAdminClearCooldownClearsBindingQuotaAndPublishesSchedulerState(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	accountID := h.importAccount(t, "clear-cooldown", "upstream-clear-cooldown", "token-clear-cooldown")
	now := storage.Now()
	if err := h.store.BenchBindingForRecheck(ctx, accountID, now+3600); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID:         accountID,
		Provider:          "codex",
		Model:             "",
		LimiterType:       "requests",
		RemainingTokens:   -1,
		RemainingRequests: 0,
		ResetAt:           now + 1800,
		Status:            "rejected",
	}); err != nil {
		t.Fatal(err)
	}

	h.app.scheduler.InvalidateAccountCache()
	if lease, err := h.app.scheduler.Select(ctx, scheduler.Route{Group: "cyber", Provider: "codex"}); err == nil {
		lease.Release()
		t.Fatal("cooled account unexpectedly remained schedulable")
	}

	getResp, err := http.Get(h.pool.URL + "/admin/accounts/" + accountID + "/clear-cooldown")
	if err != nil {
		t.Fatal(err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET clear-cooldown status = %d, want 405", getResp.StatusCode)
	}

	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+accountID+"/clear-cooldown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST clear-cooldown status = %d, want 200", resp.StatusCode)
	}
	var payload struct {
		AccountID                 string `json:"account_id"`
		CooldownUntil             int64  `json:"cooldown_until"`
		RecheckPending            bool   `json:"recheck_pending"`
		RateLimitSnapshotsCleared int64  `json:"rate_limit_snapshots_cleared"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.AccountID != accountID || payload.CooldownUntil != 0 || payload.RecheckPending || payload.RateLimitSnapshotsCleared != 1 {
		t.Fatalf("clear-cooldown response = %+v", payload)
	}

	binding, err := h.store.GetEgressBinding(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.CooldownUntil != 0 || binding.RecheckPending {
		t.Fatalf("binding remained cooled: %+v", binding)
	}
	if until, limited, err := h.store.AccountRateLimitCooldownUntil(ctx, accountID, "codex", "", now); err != nil || limited || until != 0 {
		t.Fatalf("quota cooldown remained: until=%d limited=%v err=%v", until, limited, err)
	}
	lease, err := h.app.scheduler.Select(ctx, scheduler.Route{Group: "cyber", Provider: "codex"})
	if err != nil {
		t.Fatalf("cleared account was not immediately schedulable: %v", err)
	}
	if lease.Account.ID != accountID {
		lease.Release()
		t.Fatalf("selected account = %q, want %q", lease.Account.ID, accountID)
	}
	lease.Release()

	audits, err := h.store.ListAuditLogForAccount(ctx, accountID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) == 0 || audits[0].Action != "clear_cooldown" || audits[0].State != "manual" {
		t.Fatalf("missing clear_cooldown audit: %+v", audits)
	}

	idempotent, err := http.Post(h.pool.URL+"/admin/accounts/"+accountID+"/clear-cooldown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer idempotent.Body.Close()
	if idempotent.StatusCode != http.StatusOK {
		t.Fatalf("idempotent clear-cooldown status = %d, want 200", idempotent.StatusCode)
	}
	var secondPayload struct {
		RateLimitSnapshotsCleared int64 `json:"rate_limit_snapshots_cleared"`
	}
	if err := json.NewDecoder(idempotent.Body).Decode(&secondPayload); err != nil {
		t.Fatal(err)
	}
	if secondPayload.RateLimitSnapshotsCleared != 0 {
		t.Fatalf("idempotent clear reported %d quota snapshots", secondPayload.RateLimitSnapshotsCleared)
	}
}

func TestAdminClearCooldownUnknownAccountReturnsNotFound(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	resp, err := http.Post(h.pool.URL+"/admin/accounts/missing-account/clear-cooldown", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing account clear-cooldown status = %d, want 404", resp.StatusCode)
	}
}
