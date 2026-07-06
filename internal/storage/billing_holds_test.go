package storage

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestExpireStaleBillingHoldsOnlyExpiresOldHeldRows(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	now := Now()

	oldHeld, err := store.CreateBillingHold(ctx, "route-old", "acc-old", 10)
	if err != nil {
		t.Fatal(err)
	}
	freshHeld, err := store.CreateBillingHold(ctx, "route-fresh", "acc-fresh", 20)
	if err != nil {
		t.Fatal(err)
	}
	settled, err := store.CreateBillingHold(ctx, "route-settled", "acc-settled", 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SettleBillingHold(ctx, settled, "settled"); err != nil {
		t.Fatal(err)
	}
	failed, err := store.CreateBillingHold(ctx, "route-failed", "acc-failed", 40)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SettleBillingHold(ctx, failed, "failed_upstream"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE billing_holds SET created_at = ?, updated_at = ? WHERE id IN (?, ?, ?)`, now-7200, now-7200, oldHeld, settled, failed); err != nil {
		t.Fatal(err)
	}

	expired, err := store.ExpireStaleBillingHolds(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if expired != 1 {
		t.Fatalf("expired = %d, want 1", expired)
	}
	assertHoldStatus(t, store, oldHeld, "expired_unsettled")
	assertHoldStatus(t, store, freshHeld, "held")
	assertHoldStatus(t, store, settled, "settled")
	assertHoldStatus(t, store, failed, "failed_upstream")
}

func assertHoldStatus(t *testing.T, store *Store, id, want string) {
	t.Helper()
	hold, err := store.GetBillingHold(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if hold.Status != want {
		t.Fatalf("hold %s status = %q, want %q", id, hold.Status, want)
	}
}
