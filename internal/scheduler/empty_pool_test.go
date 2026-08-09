package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// A group holding no accounts must say so. The counters cannot express it — there are no
// rows to skip, so every counter stays zero and the message collapses to the same bare
// "no active account available" a fully saturated pool produces. A production export
// showed 120+ requests routed to two empty groups across 14 continuous hours, every one
// recorded with a reason that gave the operator nothing to act on.
func TestEmptyGroupIsReportedAsEmptyPool(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	// Accounts exist, but in a different group than the one this route targets.
	if err := store.UpsertAccount(ctx, storage.Account{
		ID: "acc-elsewhere", Label: "acc-elsewhere", GroupName: "populated",
		Provider: "codex", Status: "active",
	}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	_, err := s.Select(ctx, Route{Group: "gpt-pro", Provider: "codex", ExplicitProvider: true, SkipWait: true})
	if !errors.Is(err, ErrNoAccount) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
	var noAccount *NoAccountError
	if !errors.As(err, &noAccount) {
		t.Fatalf("err type = %T, want *NoAccountError", err)
	}
	if !noAccount.EmptyPool {
		t.Fatalf("empty group not reported as an empty pool: %+v", noAccount)
	}
	if !strings.Contains(err.Error(), "holds no accounts") {
		t.Fatalf("error message does not name the cause: %q", err.Error())
	}
	// Waiting cannot make an account appear, so this must not be advertised as retryable
	// saturation — that would turn a permanent misconfiguration into a 429 retry loop.
	if noAccount.Retryable() {
		t.Fatalf("empty pool reported as retryable saturation: %+v", noAccount)
	}
}

// A populated group whose accounts are all skipped keeps the counter-based explanation
// and must NOT be labeled an empty pool.
func TestPopulatedGroupIsNotReportedAsEmptyPool(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{
		ID: "acc-wrong-provider", Label: "acc-wrong-provider", GroupName: "cyber",
		Provider: "claude", Status: "active",
	}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	_, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", ExplicitProvider: true, SkipWait: true})
	var noAccount *NoAccountError
	if !errors.As(err, &noAccount) {
		t.Fatalf("err type = %T (%v), want *NoAccountError", err, err)
	}
	if noAccount.EmptyPool {
		t.Fatalf("populated group reported as empty: %+v", noAccount)
	}
	if noAccount.Counters.ProviderMismatch != 1 {
		t.Fatalf("counters = %+v, want provider_mismatch=1", noAccount.Counters)
	}
}
