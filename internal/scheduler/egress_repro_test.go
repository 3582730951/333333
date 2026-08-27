package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// Reproduces the reported symptom "账号有额度但流量到不了那个账号 / 排队不一起走出口"
// when every account is bound to the same default outlet (egress_direct) and that
// outlet carries a non-zero max_concurrency cap.
//
// The whole account pool shares ONE coordinator resource (the egress) and ONE
// egress-load counter. concurrencyLimited(cap, inflight) rejects every candidate
// as soon as the outlet is full — no matter how much per-account quota remains.
func TestReproSharedDefaultEgressCapBlocksHealthyAccounts(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// The default outlet. MaxConcurrency=2 is tiny on purpose so the cap is hit
	// with a handful of concurrent requests. In production the equivalent non-zero
	// cap comes from the DB (migration writes 0=unlimited, but any subsequent
	// UpsertEgressProfile turns a zero back into the 16 default — see
	// storage.UpsertEgressProfile), or from a pre-migration 128.
	if err := store.UpsertEgressProfile(ctx, storage.EgressProfile{
		ID: storage.DefaultDirectEgressID, Type: "direct", Health: "healthy", MaxConcurrency: 2,
	}); err != nil {
		t.Fatal(err)
	}

	// 10 fully healthy accounts, no explicit egress binding → all fall back to the
	// group primary = egress_direct. Every account has quota (no rate-limit rows,
	// no cooldown, no quarantine).
	const n = 10
	for i := range n {
		if err := store.UpsertAccount(ctx, storage.Account{
			ID: "healthy-" + string(rune('a'+i)), GroupName: "cyber", Provider: "codex", Status: "active",
		}, storage.AccountToken{AccessToken: "token"}); err != nil {
			t.Fatal(err)
		}
	}

	s := New(store, config.Default())

	var leased, rejected atomic.Int32
	held := make(chan Lease, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", SkipWait: true})
			if err != nil {
				var noAccount *NoAccountError
				if errors.As(err, &noAccount) {
					rejected.Add(1)
				}
				return
			}
			held <- lease // hold in-flight so the outlet cap actually engages
			leased.Add(1)
		}()
	}
	wg.Wait()
	close(held)
	for lease := range held {
		lease.Release()
	}

	t.Logf("shared outlet cap=2, healthy accounts=%d -> leased=%d rejected=%d",
		n, leased.Load(), rejected.Load())
	if leased.Load() > 2 {
		t.Fatalf("outlet cap exceeded: leased=%d", leased.Load())
	}
	if leased.Load() < 1 {
		t.Fatalf("nothing leased on a healthy pool")
	}
	if rejected.Load() == 0 {
		t.Fatalf("cap did not block: all %d leased concurrently on a cap-2 outlet", leased.Load())
	}
}

// With the same pool but the outlet unlimited (max_concurrency=0, the migrated
// default), every healthy account must lease at once — the "大有余量却排队" state
// disappears the moment the cap is removed.
func TestReproSharedDefaultEgressUnlimitedLetsAllThrough(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	if err := store.UpsertEgressProfile(ctx, storage.EgressProfile{
		ID: storage.DefaultDirectEgressID, Type: "direct", Health: "healthy", MaxConcurrency: 0,
	}); err != nil {
		t.Fatal(err)
	}

	const n = 10
	for i := range n {
		if err := store.UpsertAccount(ctx, storage.Account{
			ID: "healthy-" + string(rune('a'+i)), GroupName: "cyber", Provider: "codex", Status: "active",
		}, storage.AccountToken{AccessToken: "token"}); err != nil {
			t.Fatal(err)
		}
	}

	s := New(store, config.Default())

	var leased atomic.Int32
	held := make(chan Lease, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", SkipWait: true})
			if err != nil {
				return
			}
			held <- lease
			leased.Add(1)
		}()
	}
	wg.Wait()
	close(held)
	for lease := range held {
		lease.Release()
	}

	t.Logf("shared outlet cap=0(unlimited), healthy accounts=%d -> leased=%d",
		n, leased.Load())
	if leased.Load() != n {
		t.Fatalf("unlimited outlet still blocked traffic: leased=%d want=%d", leased.Load(), n)
	}
}
