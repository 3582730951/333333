package api

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"codex-account-pool/internal/ban"
	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func TestHandleBannedAccountAutoDeleteAuditsAndCascades(t *testing.T) {
	store := apiTestStore(t)
	ctx := context.Background()
	account := storage.Account{ID: "banned-acc", Label: "Banned", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "access-token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: account.ID, ModelSlug: "gpt-5.5", Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: "ban-route", RouteKey: "k", Source: "test", AccountID: account.ID}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{AccountID: account.ID, Provider: "codex", LimiterType: "tokens", Source: "tokens", RemainingTokens: 0, ResetAt: storage.Now() + 60}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCodexReauthConfig(ctx, storage.AccountCodexReauthConfig{AccountID: account.ID, LoginEmail: "user@example.com", Password: "secret", AutoEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.EnqueueCodexReauthJob(ctx, account.ID, "test"); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.BanAutoDelete = true
	cfg.BanDetectionEnabled = false
	app := &Server{cfg: cfg, store: store}
	app.handleBannedAccount(ctx, account, ban.Verdict{State: ban.Banned, Reason: "workspace_deactivated"}, 403, []byte(`{"error":"workspace_deactivated"}`), "health_test")

	if _, err := store.GetAccount(ctx, account.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("account lookup err = %v, want sql.ErrNoRows", err)
	}
	if _, err := store.GetAffinityBinding(ctx, "ban-route"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("affinity lookup err = %v, want sql.ErrNoRows", err)
	}
	if caps, err := store.ListCapabilities(ctx, account.ID); err != nil || len(caps) != 0 {
		t.Fatalf("capabilities after delete = %+v, err=%v; want none", caps, err)
	}
	if snaps, err := store.ListAccountRateLimits(ctx); err != nil || len(snaps) != 0 {
		t.Fatalf("rate limits after delete = %+v, err=%v; want none", snaps, err)
	}
	if _, ok, err := store.GetCodexReauthConfig(ctx, account.ID); err != nil || ok {
		t.Fatalf("reauth config after delete ok=%v err=%v, want missing", ok, err)
	}
	if jobs, err := store.ListCodexReauthJobs(ctx, account.ID, 10); err != nil || len(jobs) != 0 {
		t.Fatalf("reauth jobs after delete = %+v, err=%v; want none", jobs, err)
	}
	rows, err := store.ListAuditLogForAccount(ctx, account.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || rows[0].Action != "ban_delete" || rows[0].State != string(ban.Banned) {
		t.Fatalf("audit rows = %+v, want ban_delete banned", rows)
	}
	if !strings.Contains(rows[0].Detail, "source=health_test") {
		t.Fatalf("audit detail = %q, want source", rows[0].Detail)
	}
}

func TestOnUpstreamErrorDeletesConfirmedBanEvenWhenDetectionToggleOff(t *testing.T) {
	store := apiTestStore(t)
	ctx := context.Background()
	account := storage.Account{ID: "banned-upstream", Label: "Banned", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "access-token"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.BanAutoDelete = true
	cfg.BanDetectionEnabled = false
	app := &Server{cfg: cfg, store: store}

	v := app.onUpstreamError(ctx, account, 403, nil, []byte(`{"error":"workspace_deactivated"}`))
	if !v.IsBanned() {
		t.Fatalf("verdict = %+v, want banned", v)
	}
	if _, err := store.GetAccount(ctx, account.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("account lookup err = %v, want sql.ErrNoRows", err)
	}
}
