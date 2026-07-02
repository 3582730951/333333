package api

import (
	"context"
	"net/http"
	"testing"

	"codex-account-pool/internal/storage"
)

// TestHealthTestClearsQuarantine verifies that a successful health-test (alive=true)
// auto-clears any existing quarantine when the config enables it (default on).
func TestHealthTestClearsQuarantine(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// Upstream returns 200 → alive=true, non-ban verdict.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp-1"}`))
	})
	ctx := context.Background()
	accID := h.importAccount(t, "qtest", "upstream-1", "tok-1")
	// Quarantine the account (simulating a prior ban/auth error).
	now := storage.Now()
	if err := h.store.SetAccountQuarantine(ctx, accID, now+7200, "test quarantine"); err != nil {
		t.Fatal(err)
	}
	acc, _ := h.store.GetAccount(ctx, accID)
	if acc.QuarantineUntil <= now {
		t.Fatalf("quarantine_until not set: %d", acc.QuarantineUntil)
	}
	// Run health-test → should clear quarantine.
	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+accID+"/health-test", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health-test status = %d, want 200", resp.StatusCode)
	}
	// Verify quarantine is cleared (quarantine_until = 0).
	acc, _ = h.store.GetAccount(ctx, accID)
	if acc.QuarantineUntil != 0 {
		t.Fatalf("health-test did not clear quarantine: quarantine_until=%d, want 0", acc.QuarantineUntil)
	}
}

// TestClearQuarantineEndpoint verifies the new manual clear-quarantine endpoint.
func TestClearQuarantineEndpoint(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	ctx := context.Background()
	accID := h.importAccount(t, "manual-clear", "upstream-2", "tok-2")
	now := storage.Now()
	if err := h.store.SetAccountQuarantine(ctx, accID, now+7200, "manual test"); err != nil {
		t.Fatal(err)
	}
	// Clear via the new endpoint.
	resp, err := http.Post(h.pool.URL+"/admin/accounts/"+accID+"/clear-quarantine", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clear-quarantine status = %d, want 200", resp.StatusCode)
	}
	// Verify cleared.
	acc, _ := h.store.GetAccount(ctx, accID)
	if acc.QuarantineUntil != 0 {
		t.Fatalf("manual clear did not work: quarantine_until=%d, want 0", acc.QuarantineUntil)
	}
}

// TestQuarantineDurationConfigurable confirms the new config field controls how long
// an account is quarantined (default 72h, not the prior hard-coded 30 days).
func TestQuarantineDurationConfigurable(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"account suspended"}}`))
	})
	ctx := context.Background()
	// Override config: 1 hour quarantine, ban detection on, no auto-delete.
	h.app.cfg.QuarantineDurationHours = 1
	h.app.cfg.BanDetectionEnabled = true
	h.app.cfg.BanAutoDelete = false
	accID := h.importAccount(t, "short-q", "upstream-3", "tok-3")
	// Run health-test → triggers ban quarantine.
	_, _ = http.Post(h.pool.URL+"/admin/accounts/"+accID+"/health-test", "application/json", nil)
	acc, _ := h.store.GetAccount(ctx, accID)
	now := storage.Now()
	if acc.QuarantineUntil == 0 {
		t.Fatalf("account was not quarantined (quarantine_until=0); ban detection may not have triggered")
	}
	duration := acc.QuarantineUntil - now
	// Should be ~3600s (1h), not 30*24*3600 (30 days = 2592000).
	if duration < 3500 || duration > 3700 {
		t.Fatalf("quarantine duration = %ds, want ~3600 (1h per config)", duration)
	}
}
