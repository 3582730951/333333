package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCodexResetCreditConsumptionClaimIsUniquePerSevenDayWindow(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, Account{ID: "acc", GroupName: "cyber", Status: "active", Provider: "codex"}, AccountToken{AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	resetAt := int64(1_700_086_400)
	now := int64(1_700_000_000)

	first, err := store.ClaimCodexResetCreditConsumption(ctx, "acc", resetAt, "redeem-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Claimed || first.Row.Status != "in_progress" || first.Row.RedeemRequestID != "redeem-1" {
		t.Fatalf("first claim = %+v, want claimed in_progress redeem-1", first)
	}

	second, err := store.ClaimCodexResetCreditConsumption(ctx, "acc", resetAt, "redeem-2", now+1)
	if err != nil {
		t.Fatal(err)
	}
	if second.Claimed || second.Row.RedeemRequestID != "redeem-1" || second.Row.Status != "in_progress" {
		t.Fatalf("second claim = %+v, want existing in_progress redeem-1", second)
	}
}

func TestCodexResetCreditConsumptionStaleInProgressConvergesToUnknown(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, Account{ID: "acc", GroupName: "cyber", Status: "active", Provider: "codex"}, AccountToken{AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	resetAt := int64(1_700_086_400)
	now := int64(1_700_000_000)
	if _, err := store.ClaimCodexResetCreditConsumption(ctx, "acc", resetAt, "redeem-1", now); err != nil {
		t.Fatal(err)
	}

	stale, err := store.ClaimCodexResetCreditConsumption(ctx, "acc", resetAt, "redeem-2", now+121)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Claimed || stale.Row.Status != "unknown" || stale.Row.RedeemRequestID != "redeem-1" {
		t.Fatalf("stale claim = %+v, want unknown existing redeem-1 without new claim", stale)
	}
}

func TestCodexResetCreditConsumptionMigrationCreatesTable(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(context.Background(), Account{ID: "acc", GroupName: "cyber", Status: "active", Provider: "codex"}, AccountToken{AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(context.Background(), `INSERT INTO codex_reset_credit_consumptions(account_id, seven_day_reset_at, redeem_request_id, status, created_at, updated_at)
VALUES('acc', 1700086400, 'redeem', 'success', 1700000000, 1700000001)`); err != nil {
		t.Fatalf("insert into migrated table: %v", err)
	}
}
