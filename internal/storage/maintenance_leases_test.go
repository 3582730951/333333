package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newMaintenanceLeaseStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "maintenance.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestMaintenanceLeaseFencesTakeoversAndStaleRenewals(t *testing.T) {
	store := newMaintenanceLeaseStore(t)
	ctx := context.Background()

	first, acquired, err := store.AcquireMaintenanceLease(ctx, "active-worker", "worker-a", 5*time.Second)
	if err != nil || !acquired || first.FencingToken != 1 {
		t.Fatalf("first acquire = %+v acquired=%v err=%v", first, acquired, err)
	}
	blocked, acquired, err := store.AcquireMaintenanceLease(ctx, "active-worker", "worker-b", 5*time.Second)
	if err != nil || acquired || blocked.OwnerID != "worker-a" || blocked.FencingToken != first.FencingToken {
		t.Fatalf("blocked acquire = %+v acquired=%v err=%v", blocked, acquired, err)
	}
	renewed, err := store.RenewMaintenanceLease(ctx, first, 10*time.Second)
	if err != nil || renewed.FencingToken != first.FencingToken || renewed.ExpiresAt <= first.ExpiresAt {
		t.Fatalf("renewed lease = %+v err=%v", renewed, err)
	}
	if err := store.ReleaseMaintenanceLease(ctx, renewed); err != nil {
		t.Fatal(err)
	}
	second, acquired, err := store.AcquireMaintenanceLease(ctx, "active-worker", "worker-b", 5*time.Second)
	if err != nil || !acquired || second.FencingToken != first.FencingToken+1 {
		t.Fatalf("takeover = %+v acquired=%v err=%v", second, acquired, err)
	}
	if _, err := store.RenewMaintenanceLease(ctx, first, 5*time.Second); !errors.Is(err, ErrMaintenanceLeaseLost) {
		t.Fatalf("stale renewal err=%v, want ErrMaintenanceLeaseLost", err)
	}
}

func TestMaintenanceLeaseExpiredSameOwnerGetsNewFencingToken(t *testing.T) {
	store := newMaintenanceLeaseStore(t)
	ctx := context.Background()
	first, acquired, err := store.AcquireMaintenanceLease(ctx, "gc", "worker-a", time.Second)
	if err != nil || !acquired {
		t.Fatalf("first acquire = %+v acquired=%v err=%v", first, acquired, err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE maintenance_leases SET expires_at=? WHERE lease_name='gc'`, Now()-1); err != nil {
		t.Fatal(err)
	}
	second, acquired, err := store.AcquireMaintenanceLease(ctx, "gc", "worker-a", time.Second)
	if err != nil || !acquired || second.FencingToken != first.FencingToken+1 {
		t.Fatalf("same-owner reacquire = %+v acquired=%v err=%v", second, acquired, err)
	}
}
