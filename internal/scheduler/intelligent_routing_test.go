package scheduler

import (
	"context"
	"errors"
	"testing"

	"sync/atomic"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

func irChoices(groups ...string) []RouteChoice {
	choices := make([]RouteChoice, 0, len(groups))
	for _, group := range groups {
		choices = append(choices, RouteChoice{ChoiceKey: group, Route: Route{Group: group, Provider: "codex", SkipWait: true}})
	}
	return choices
}

func irAddAccount(t *testing.T, store *storage.Store, id, group string) {
	t.Helper()
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: group, Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "token-" + id}); err != nil {
		t.Fatal(err)
	}
}

// A cooldown-stuck account with no other candidate must be probed as a
// last-resort trial rather than left waiting: the production symptom was an
// account with real quota whose stale cooldown kept every request from reaching
// it until IgnoreRateLimitControls was toggled. The trial selection is the
// machine-side equivalent of that toggle.
func TestCooldownTrialSelectsCooldownStuckAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	irAddAccount(t, store, "acc-quota", "cyber")
	if err := store.SetBindingCooldown(ctx, "acc-quota", storage.Now()+3600); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	defer s.Close()

	lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", SkipWait: true})
	if err != nil {
		t.Fatalf("cooldown trial selection failed: %v", err)
	}
	if !lease.Trial {
		t.Fatalf("lease.Trial = false, want true (cooldown-trial selection)")
	}
	if lease.Account.ID != "acc-quota" {
		t.Fatalf("leased account = %s, want acc-quota", lease.Account.ID)
	}
	lease.Release()
	if got := atomic.LoadInt64(&s.metrics.TrialSelections); got != 1 {
		t.Fatalf("trial selections metric = %d, want 1", got)
	}
}

// The flag must be the kill-switch: with intelligent routing off, a cooldown
// account is excluded exactly as it was before, and the request fails instead of
// probing. Without this, the trial path would change behavior on existing
// deployments that have not opted in.
func TestCooldownTrialDisabledByConfig(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	irAddAccount(t, store, "acc-quota", "cyber")
	if err := store.SetBindingCooldown(ctx, "acc-quota", storage.Now()+3600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.IntelligentRoutingEnabled = false
	s := New(store, cfg)
	defer s.Close()

	_, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", SkipWait: true})
	if !errors.Is(err, ErrNoAccount) {
		t.Fatalf("err = %v, want ErrNoAccount when intelligent routing is disabled", err)
	}
	if got := atomic.LoadInt64(&s.metrics.TrialSelections); got != 0 {
		t.Fatalf("trial selections metric = %d, want 0", got)
	}
}

// Quarantine is an administrator's explicit hold-out: no trial may probe a
// quarantined account, even when the cooldown gate fired for a sibling account.
func TestCooldownTrialSkipsQuarantinedAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	irAddAccount(t, store, "acc-quarantined", "cyber")
	if err := store.SetAccountQuarantine(ctx, "acc-quarantined", storage.Now()+3600, "intelligent-routing test"); err != nil {
		t.Fatal(err)
	}
	irAddAccount(t, store, "acc-quota", "cyber")
	if err := store.SetBindingCooldown(ctx, "acc-quota", storage.Now()+3600); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	defer s.Close()

	lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", SkipWait: true})
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	if lease.Account.ID != "acc-quota" {
		t.Fatalf("leased account = %s, want acc-quota (quarantined account must be skipped)", lease.Account.ID)
	}
	if lease.Trial != true {
		t.Fatalf("lease.Trial = false, want true (cooldown trial)")
	}
	lease.Release()
}

// A recheck-pending account is held out for an explicit liveness probe by the
// recheck loop; the trial path must not bypass that isolation.
func TestCooldownTrialSkipsRecheckPendingAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	irAddAccount(t, store, "acc-recheck", "cyber")
	if err := store.SetBindingCooldown(ctx, "acc-recheck", storage.Now()+3600); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBindingRecheckPending(ctx, "acc-recheck", true); err != nil {
		t.Fatal(err)
	}
	irAddAccount(t, store, "acc-quota", "cyber")
	if err := store.SetBindingCooldown(ctx, "acc-quota", storage.Now()+3600); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	defer s.Close()

	lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", SkipWait: true})
	if err != nil {
		t.Fatalf("selection failed: %v", err)
	}
	if lease.Account.ID != "acc-quota" {
		t.Fatalf("leased account = %s, want acc-quota (recheck-pending account must be skipped)", lease.Account.ID)
	}
	lease.Release()
}

// A quarantine-only failure (no cooldown class counter fired) must not probe at
// all: quarantine is a wait-or-escalate condition, not a stale-quota signal.
func TestCooldownTrialDoesNotProbeWithoutCooldownCounters(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	irAddAccount(t, store, "acc-quarantined", "cyber")
	if err := store.SetAccountQuarantine(ctx, "acc-quarantined", storage.Now()+3600, "intelligent-routing test"); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	defer s.Close()

	_, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", SkipWait: true})
	var noAccount *NoAccountError
	if !errors.As(err, &noAccount) {
		t.Fatalf("err type = %T (%v), want *NoAccountError", err, err)
	}
	if noAccount.Counters.Quarantined != 1 {
		t.Fatalf("counters = %+v, want quarantined=1", noAccount.Counters)
	}
	if got := atomic.LoadInt64(&s.metrics.TrialSelections); got != 0 {
		t.Fatalf("trial selections metric = %d, want 0", got)
	}
}

// Every fallback group empty at once must read as an empty pool (configuration,
// not load) so the audit reason stays group_has_no_accounts for the whole chain.
func TestSelectAcrossEmptyPoolForAllEmptyChoices(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	// Accounts exist only in an unrelated group.
	irAddAccount(t, store, "acc-elsewhere", "populated")
	s := New(store, config.Default())
	defer s.Close()

	_, err := s.SelectAcross(ctx, irChoices("gpt-pro", "gpt-team"))
	var noAccount *NoAccountError
	if !errors.As(err, &noAccount) {
		t.Fatalf("err type = %T (%v), want *NoAccountError", err, err)
	}
	if !noAccount.EmptyPool {
		t.Fatalf("all-empty choices not reported as an empty pool: %+v", noAccount)
	}
}

// The core fallback behavior: when the primary group holds no accounts, the
// across evaluation steps to the next group and serves it — no 503.
func TestSelectAcrossFallsBackWhenPrimaryEmpty(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	irAddAccount(t, store, "acc-fb", "fallback-live")
	s := New(store, config.Default())
	defer s.Close()

	routed, err := s.SelectAcross(ctx, irChoices("primary-empty", "fallback-live"))
	if err != nil {
		t.Fatalf("across fallback failed: %v", err)
	}
	if routed.ChoiceKey != "fallback-live" {
		t.Fatalf("choice key = %q, want fallback-live", routed.ChoiceKey)
	}
	if routed.Account.ID != "acc-fb" {
		t.Fatalf("leased account = %s, want acc-fb", routed.Account.ID)
	}
	routed.Release()
}

// Primary-first: a healthy primary group serves instantly via the ordinary
// single-group path instead of competing with fallback targets in the across
// coordinator's round-robin. Iterating surfaces the distinction: a pure across
// evaluation rotates between two equally-idle targets, so repeated selections
// must all land on the primary account here but would split without this rule.
func TestPrimaryRouteChoiceServesPrimaryWhenHealthy(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	irAddAccount(t, store, "acc-primary", "cyber")
	irAddAccount(t, store, "acc-fb", "cyber-fb")
	s := New(store, config.Default())
	defer s.Close()

	ctx, _ = WithPrimaryRouteChoices(ctx, "cyber", irChoices("cyber", "cyber-fb"), nil)
	for i := 0; i < 12; i++ {
		lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", SkipWait: true})
		if err != nil {
			t.Fatalf("primary selection %d failed: %v", i, err)
		}
		if lease.Account.ID != "acc-primary" {
			t.Fatalf("selection %d leased account = %s, want acc-primary (healthy primary must always win, got a fallback rotate)", i, lease.Account.ID)
		}
		lease.Release()
	}
}

// Primary-first fallback: with an empty primary group the same request steps to
// the fallback group through the across evaluation.
func TestPrimaryRouteChoiceFallsBackWhenPrimaryEmpty(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	irAddAccount(t, store, "acc-fb", "cyber-fb")
	s := New(store, config.Default())
	defer s.Close()

	ctx, state := WithPrimaryRouteChoices(ctx, "cyber", irChoices("cyber", "cyber-fb"), nil)
	lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", SkipWait: true})
	if err != nil {
		t.Fatalf("fallback selection failed: %v", err)
	}
	if lease.Account.ID != "acc-fb" {
		t.Fatalf("leased account = %s, want acc-fb", lease.Account.ID)
	}
	if state.SelectedChoice() != "cyber-fb" {
		t.Fatalf("selected choice = %q, want cyber-fb", state.SelectedChoice())
	}
	lease.Release()
}
