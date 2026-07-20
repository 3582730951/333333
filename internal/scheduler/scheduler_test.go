package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/config"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

func testStore(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "pool.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestAdaptiveZeroLimitsDoNotReject(t *testing.T) {
	if concurrencyLimited(0, 10_000) {
		t.Fatal("max_concurrency=0 must not impose a static limit")
	}
	if tokenBudgetLimited(0, false, 100, 1_000_000, 1_000_000) {
		t.Fatal("account_token_budget=0 must disable the token budget")
	}
	if !concurrencyLimited(2, 2) {
		t.Fatal("positive concurrency limit must remain a hard cap")
	}
	if !tokenBudgetLimited(100, false, 1, 90, 20) {
		t.Fatal("positive token budget must remain enforced")
	}
}

func TestSelectComposesAccountSidecarOverRealProxyEgress(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	account := storage.Account{ID: "claude-sidecar-proxy", GroupName: "cyber", Provider: "claude", Status: "active"}
	if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "sk-ant-oat-test"}); err != nil {
		t.Fatal(err)
	}
	base := storage.EgressProfile{ID: "proxy-exit", Type: "socks5h_proxy", Endpoint: "socks5h://proxy.example:1080", Region: "US", ExitIP: "198.51.100.20", Health: "healthy", MaxConcurrency: 8}
	sidecar := storage.EgressProfile{ID: "sidecar-transport", Type: storage.CurlCFFISidecarEgressType, Endpoint: "http://127.0.0.1:8790", Health: "healthy", MaxConcurrency: 16}
	if err := store.UpsertEgressProfile(ctx, base); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressProfile(ctx, sidecar); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{AccountID: account.ID, PrimaryEgressID: base.ID, SidecarEgressID: sidecar.ID}); err != nil {
		t.Fatal(err)
	}

	s := New(store, config.Default())
	lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Egress.ID != base.ID || lease.Egress.Region != base.Region || lease.Egress.ExitIP != base.ExitIP {
		t.Fatalf("real egress attribution changed: %+v", lease.Egress)
	}
	if lease.Egress.Type != storage.CurlCFFISidecarEgressType || lease.Egress.Endpoint != sidecar.Endpoint || lease.Egress.ChainProxy != base.Endpoint {
		t.Fatalf("sidecar transport was not composed: %+v", lease.Egress)
	}
	if lease.Binding.SidecarEgressID != sidecar.ID {
		t.Fatalf("lease binding sidecar = %q", lease.Binding.SidecarEgressID)
	}
}

func TestSelectFailsClosedWhenBoundSidecarIsMissing(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	account := storage.Account{ID: "claude-missing-sidecar", GroupName: "cyber", Provider: "claude", Status: "active"}
	if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "sk-ant-oat-test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{AccountID: account.ID, PrimaryEgressID: storage.DefaultDirectEgressID, SidecarEgressID: "deleted-sidecar"}); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	if _, err := s.Select(ctx, Route{Group: "cyber", Provider: "claude"}); err == nil {
		t.Fatal("missing explicit sidecar fell back to Go direct transport")
	}
}

func TestSharedSidecarConcurrencyIsEnforcedAcrossBaseEgresses(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	sidecar := storage.EgressProfile{
		ID: "shared-sidecar", Type: storage.CurlCFFISidecarEgressType,
		Endpoint: "http://127.0.0.1:8790", Health: "healthy", MaxConcurrency: 1,
	}
	if err := store.UpsertEgressProfile(ctx, sidecar); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"claude-a", "claude-b"} {
		account := storage.Account{ID: id, GroupName: "cyber", Provider: "claude", Status: "active"}
		if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "sk-ant-oat-test"}); err != nil {
			t.Fatal(err)
		}
		base := storage.EgressProfile{
			ID: id + "-proxy", Type: "http_proxy", Endpoint: "http://" + id + ".example:8080",
			Health: "healthy", MaxConcurrency: 4,
		}
		if err := store.UpsertEgressProfile(ctx, base); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{
			AccountID: id, PrimaryEgressID: base.ID, SidecarEgressID: sidecar.ID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	s := New(store, config.Default())
	first, err := s.Select(ctx, Route{Group: "cyber", Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Select(ctx, Route{Group: "cyber", Provider: "claude"}); err == nil {
		first.Release()
		t.Fatal("second lease bypassed the shared sidecar max_concurrency=1 limit")
	}
	first.Release()
	second, err := s.Select(ctx, Route{Group: "cyber", Provider: "claude"})
	if err != nil {
		t.Fatalf("sidecar capacity was not released: %v", err)
	}
	second.Release()
}

func TestAffinityBindsSameAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	s := New(store, config.Default())
	key := routing.AffinityKey{Key: "parent:1", Hash: "hash-parent-1", Source: "test"}
	first, err := s.Select(ctx, Route{Group: "cyber", Affinity: key})
	if err != nil {
		t.Fatalf("select first: %v", err)
	}
	firstID := first.Account.ID
	first.Release()
	second, err := s.Select(ctx, Route{Group: "cyber", Affinity: key})
	if err != nil {
		t.Fatalf("select second: %v", err)
	}
	defer second.Release()
	if second.Account.ID != firstID {
		t.Fatalf("second account = %q, want %q", second.Account.ID, firstID)
	}
}

func TestLegacyAffinityBindingIsCompletedAfterStickySelection(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	account := storage.Account{ID: "acc-legacy", Label: "legacy", GroupName: "cyber", Provider: "codex", Status: "active"}
	if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	key := routing.AffinityKey{Key: "legacy-session", Hash: "legacy-session-hash", Source: "test"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: account.ID}); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Affinity: key, Model: "gpt-5.4"})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	bound, err := store.GetAffinityBinding(ctx, key.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Provider != "codex" || bound.Model != "gpt-5.4" || bound.EgressID == "" {
		t.Fatalf("legacy affinity was not completed: %+v", bound)
	}
}

func TestClaudeKiroAutoAffinityCannotSwitchProviderAfterFirstSelection(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	claude := storage.Account{ID: "auto-claude", Label: "claude", GroupName: "cyber", Provider: "claude", Status: "active"}
	kiroAccount := storage.Account{ID: "auto-kiro", Label: "kiro", GroupName: "cyber", Provider: "kiro", Status: "active"}
	for _, account := range []storage.Account{claude, kiroAccount} {
		if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "token"}); err != nil {
			t.Fatal(err)
		}
	}
	model := "claude-sonnet-4.6"
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: claude.ID, ModelSlug: model, Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertKiroCredentials(ctx, storage.KiroCredentials{AccountID: kiroAccount.ID, AuthMethod: "api_key", KiroAPIKey: "key", APIRegion: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	endpointHash, err := kirowire.EndpointHash("", "us-east-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ObserveKiroCapability(ctx, kiroAccount.ID, endpointHash, model, storage.KiroCapabilityObservation{ModelSucceeded: true, ThinkingRequested: true}); err != nil {
		t.Fatal(err)
	}
	key := routing.AffinityKey{Key: "auto-mixed", Hash: "auto-mixed-hash", Source: "test"}
	route := Route{Group: "cyber", AllowedProviders: []string{"claude", "kiro"}, Affinity: key, Model: model, ThinkingRequired: true}
	s := New(store, config.Default())
	first, err := s.Select(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	selectedID, selectedProvider := first.Account.ID, first.Account.Provider
	first.Release()
	bound, err := store.GetAffinityBinding(ctx, key.Hash)
	if err != nil || bound.AccountID != selectedID || bound.Provider != selectedProvider || bound.Model != model || bound.EgressID == "" {
		t.Fatalf("first auto binding=%+v err=%v selected=%s/%s", bound, err, selectedProvider, selectedID)
	}
	if err := store.SetAccountStatus(ctx, selectedID, "disabled"); err != nil {
		t.Fatal(err)
	}
	s.InvalidateAccountCache()
	route.ImmutableAffinity = true
	if _, err := s.Select(ctx, route); !errors.Is(err, ErrBoundAccountUnavailable) {
		t.Fatalf("immutable mixed-provider affinity err=%v, want ErrBoundAccountUnavailable", err)
	}
}

func TestAffinityBindingCannotCrossGroups(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-a", Label: "acc-a", GroupName: "group-a", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-b", Label: "acc-b", GroupName: "group-b", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: "hash-shared", RouteKey: "cache_prefix:gpt:abc", Source: "cache_prefix_hash", AccountID: "acc-a"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	lease, err := s.Select(ctx, Route{Group: "group-b", Affinity: routing.AffinityKey{Hash: "hash-shared", Key: "cache_prefix:gpt:abc", Source: "cache_prefix_hash"}})
	if err != nil {
		t.Fatalf("select group-b: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "acc-b" {
		t.Fatalf("sticky affinity crossed groups: got %q, want acc-b", lease.Account.ID)
	}
}

func TestStrictStickyDoesNotSwitchUnavailableAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: "hash", RouteKey: "key", Source: "test", AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAccountQuarantine(ctx, "acc-1", storage.Now()+3600, "test"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	_, err := s.Select(ctx, Route{Group: "cyber", Affinity: routing.AffinityKey{Hash: "hash", Key: "key"}, Strict: true})
	if !errors.Is(err, ErrStrictUnavailable) {
		t.Fatalf("err = %v, want ErrStrictUnavailable (wrapped)", err)
	}
	lease, err := s.Select(ctx, Route{Group: "cyber", Affinity: routing.AffinityKey{Hash: "hash", Key: "key"}, Strict: false})
	if err != nil {
		t.Fatalf("nonstrict should fail over: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "acc-2" {
		t.Fatalf("failed over to %q, want acc-2", lease.Account.ID)
	}
}

func TestCapabilityAwareRoutingPrefersAccountWithModel(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	// Only acc-2 has the requested model probed.
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: "acc-2", ModelSlug: "gpt-rare", NativeMaxContextWindow: 200000}}); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	for i := 0; i < 5; i++ {
		lease, err := s.Select(ctx, Route{Group: "cyber", Model: "gpt-rare"})
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if lease.Account.ID != "acc-2" {
			t.Fatalf("capability routing chose %q, want acc-2 (the only account with the model)", lease.Account.ID)
		}
		lease.Release()
	}
	// A model nobody probed must NOT over-restrict — selection still succeeds.
	lease, err := s.Select(ctx, Route{Group: "cyber", Model: "gpt-unknown"})
	if err != nil {
		t.Fatalf("unknown (unprobed) model should still select an account: %v", err)
	}
	lease.Release()
}

func TestSelectSkipsCurrentModelLimitedAccountOnly(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	var caps []storage.ModelCapability
	for _, id := range []string{"acc-1", "acc-2"} {
		for _, model := range []string{"gpt-5", "gpt-4"} {
			caps = append(caps, storage.ModelCapability{AccountID: id, ModelSlug: model, NativeMaxContextWindow: 200000, Source: "test", LastProbeAt: storage.Now()})
		}
	}
	if err := store.UpsertCapabilities(ctx, caps); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID: "acc-1", Provider: "codex", Model: "gpt-5", LimiterType: "tokens", Source: "tokens",
		UsedPercent: 100, LimitTokens: 1000, RemainingTokens: 0,
		LimitRequests: -1, RemainingRequests: -1, ResetAt: storage.Now() + 300,
		Status: "rejected",
	}); err != nil {
		t.Fatal(err)
	}

	s := New(store, config.Default())
	limited, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Model: "gpt-5"})
	if err != nil {
		t.Fatalf("select gpt-5: %v", err)
	}
	if limited.Account.ID != "acc-2" {
		t.Fatalf("gpt-5 should avoid limited acc-1, chose %s", limited.Account.ID)
	}
	limited.Release()

	otherModel, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Model: "gpt-4"})
	if err != nil {
		t.Fatalf("select gpt-4: %v", err)
	}
	defer otherModel.Release()
	if otherModel.Account.ID != "acc-1" {
		t.Fatalf("gpt-4 should still use acc-1; model-specific gpt-5 limit must not bench whole account, chose %s", otherModel.Account.ID)
	}
}

func TestExpiredBreakerEgressStateIsSelectable(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", Label: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	now := storage.Now()
	for _, health := range []string{"cooldown", "tripped"} {
		profile, err := store.GetEgressProfile(ctx, storage.DefaultDirectEgressID)
		if err != nil {
			t.Fatal(err)
		}
		profile.Health = health
		profile.CooldownUntil = now - 1
		if err := store.UpsertEgressProfile(ctx, profile); err != nil {
			t.Fatal(err)
		}
		s := New(store, config.Default())
		lease, err := s.Select(ctx, Route{Group: "cyber"})
		if err != nil {
			t.Fatalf("%s egress after cooldown should be selectable: %v", health, err)
		}
		lease.Release()
	}
}

func TestActiveBreakerEgressCooldownIsNotSelectable(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", Label: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	profile, err := store.GetEgressProfile(ctx, storage.DefaultDirectEgressID)
	if err != nil {
		t.Fatal(err)
	}
	profile.Health = "cooldown"
	profile.CooldownUntil = storage.Now() + 60
	if err := store.UpsertEgressProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	_, err = s.Select(ctx, Route{Group: "cyber"})
	if err != ErrNoAccount {
		t.Fatalf("err = %v, want ErrNoAccount while egress cooldown is active", err)
	}
}

// TestSelectFreshRoundRobinSpread verifies new conversations spread evenly across
// equally-idle accounts instead of always hammering the oldest one.
func TestSelectFreshRoundRobinSpread(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-a", "acc-b", "acc-c"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	s := New(store, config.Default())
	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		l, err := s.Select(ctx, Route{Group: "cyber"})
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		counts[l.Account.ID]++
		l.Release() // back to idle so all three stay equal-load (score 0)
	}
	for _, id := range []string{"acc-a", "acc-b", "acc-c"} {
		if counts[id] != 3 {
			t.Fatalf("account %s got %d/9 selections, want 3 (even round-robin); counts=%v", id, counts[id], counts)
		}
	}
}

func TestProviderAPIKeyAndOAuthAccountsBalanceEqually(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	accounts := []struct {
		id, method string
	}{{"claude-oauth", "oauth"}, {"claude-api-key", "api_key"}}
	for _, item := range accounts {
		account := storage.Account{ID: item.id, Label: item.id, GroupName: "cyber", Provider: "claude", Status: "active"}
		if err := store.UpsertAccount(ctx, account, storage.AccountToken{AuthMethod: item.method, AccessToken: "credential-" + item.id}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{
			AccountID: item.id, ModelSlug: "claude-sonnet-4-6", AvailabilityState: capability.AvailabilityVerified, Source: "claude_probe",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	s := New(store, config.Default())
	counts := map[string]int{}
	for i := 0; i < 8; i++ {
		lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "claude", Model: "claude-sonnet-4-6"})
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		counts[lease.Account.ID]++
		lease.Release()
	}
	if counts["claude-oauth"] != 4 || counts["claude-api-key"] != 4 {
		t.Fatalf("auth method influenced balancing: %v", counts)
	}
}

func TestClaudeAliasRequiresVerifiedCapabilityAndRejectionsStayModelScoped(t *testing.T) {
	aliasRoute := Route{Model: "opus"}
	if resolved, _, ok := resolveClaudeRouteModel(aliasRoute, []storage.ModelCapability{{
		ModelSlug: "claude-opus-4-8", AvailabilityState: capability.AvailabilityUnverified, Source: "claude_static_unverified",
	}}); ok || resolved != "" {
		t.Fatalf("unverified static alias resolved to %q", resolved)
	}
	if resolved, _, ok := resolveClaudeRouteModel(aliasRoute, []storage.ModelCapability{{
		ModelSlug: "claude-opus-4-8", AvailabilityState: capability.AvailabilityVerified, Source: "claude_runtime_inference",
	}}); !ok || resolved != "claude-opus-4-8" {
		t.Fatalf("verified alias=(%q,%v)", resolved, ok)
	}

	caps := []storage.ModelCapability{{
		ModelSlug: "claude-opus-4-8", AvailabilityState: capability.AvailabilityUnsupported, Source: "claude_runtime_rejected",
	}}
	if resolved, _, ok := resolveClaudeRouteModel(Route{Model: "claude-opus-4-8"}, caps); ok || resolved != "" {
		t.Fatalf("rejected exact model was retried as %q", resolved)
	}
	if resolved, bootstrap, ok := resolveClaudeRouteModel(Route{Model: "claude-sonnet-4-6"}, caps); !ok || !bootstrap || resolved == "" {
		t.Fatalf("one rejection blocked a different model: resolved=%q bootstrap=%v ok=%v", resolved, bootstrap, ok)
	}
	if resolved, _, ok := resolveCodexRouteModel(Route{Model: "gpt-5.6-sol"}, []storage.ModelCapability{{
		ModelSlug: "gpt-5.6-sol", AvailabilityState: capability.AvailabilityUnsupported, Source: "codex_runtime_rejected",
	}}); ok || resolved != "" {
		t.Fatalf("rejected Codex model was retried as %q", resolved)
	}
}

func TestSuccessfulEmptyModelCatalogPreventsConcreteBootstrap(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	account := storage.Account{ID: "claude-empty", Label: "claude-empty", GroupName: "cyber", Provider: "claude", Status: "active"}
	if err := store.UpsertAccount(ctx, account, storage.AccountToken{AuthMethod: "oauth", AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceCapabilities(ctx, account.ID, nil); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	_, err := s.Select(ctx, Route{Group: "cyber", Provider: "claude", Model: "claude-opus-4-8"})
	var noAccount *NoAccountError
	if !errors.As(err, &noAccount) || noAccount.Counters.ModelUnsupported != 1 {
		t.Fatalf("empty authoritative catalog bootstrapped model: err=%v", err)
	}
}

func TestTokenBudgetPreventsOverloadedAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", Label: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	s := New(store, cfg)
	lease, err := s.Select(ctx, Route{Group: "cyber", EstimatedTokens: 90})
	if err != nil {
		t.Fatalf("first select: %v", err)
	}
	defer lease.Release()
	_, err = s.Select(ctx, Route{Group: "cyber", EstimatedTokens: 20})
	if err != ErrNoAccount {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
}

// A single request whose estimate exceeds the entire per-account budget must still be
// admitted on an idle account — the budget guards CONCURRENT over-commit, not single-
// request size. Rejecting it (the old behavior) produced a 409/503 that no account
// could satisfy. This is the regression test for the reported "token budget exceeded"
// 409 on a large-context sticky turn.
func TestSoloRequestOverBudgetIsAdmitted(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", Label: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	s := New(store, cfg)
	lease, err := s.Select(ctx, Route{Group: "cyber", EstimatedTokens: 5000})
	if err != nil {
		t.Fatalf("solo over-budget request should be admitted, got: %v", err)
	}
	lease.Release()
}

// The same, via the strict sticky path: an over-budget solo turn on its bound account
// must NOT return ErrStrictUnavailable("token budget exceeded").
func TestStrictStickyOverBudgetSoloDoesNotConflict(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", Label: "acc-1", GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: "h", RouteKey: "k", Source: "test", AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	lease, err := s.Select(ctx, Route{Group: "cyber", Affinity: routing.AffinityKey{Hash: "h", Key: "k"}, Strict: true, EstimatedTokens: 5000})
	if err != nil {
		t.Fatalf("solo strict over-budget request should be admitted, got: %v", err)
	}
	lease.Release()
}

// An excluded account must be skipped even by the sticky/affinity path: the request
// fails over to a fresh account and the affinity rebinds to it. This is what keeps a
// just-failed account from reappearing in the same request's retry.
func TestExcludeBypassesStickyAndRebinds(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: "h", RouteKey: "k", Source: "test", AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	lease, err := s.Select(ctx, Route{Group: "cyber", Affinity: routing.AffinityKey{Hash: "h", Key: "k"}, Strict: true, Exclude: map[string]bool{"acc-1": true}})
	if err != nil {
		t.Fatalf("excluded sticky account should fail over: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "acc-2" {
		t.Fatalf("failed over to %q, want acc-2", lease.Account.ID)
	}
	b, err := store.GetAffinityBinding(ctx, "h")
	if err != nil {
		t.Fatal(err)
	}
	if b.AccountID != "acc-2" {
		t.Fatalf("affinity rebound to %q, want acc-2", b.AccountID)
	}
}

// A recheck-pending account stays out of the candidate pool even after its cooldown
// has elapsed — until a probe clears it (ClearBindingRecheck). This is the "must pass
// 测活 before re-entering the pool" guarantee.
func TestRecheckPendingAccountIsNotSelected(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	// Cooldown already elapsed (now-1) but recheck_pending is set: acc-1 must remain
	// ineligible regardless.
	if err := store.BenchBindingForRecheck(ctx, "acc-1", storage.Now()-1); err != nil {
		t.Fatal(err)
	}
	s := New(store, config.Default())
	for i := 0; i < 5; i++ {
		lease, err := s.Select(ctx, Route{Group: "cyber"})
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		if lease.Account.ID != "acc-2" {
			t.Fatalf("selected %q, want acc-2 (acc-1 is recheck-pending)", lease.Account.ID)
		}
		lease.Release()
	}
	// After a passing probe clears the flag, acc-1 is selectable again.
	if err := store.ClearBindingRecheck(ctx, "acc-1"); err != nil {
		t.Fatal(err)
	}
	lease, err := s.Select(ctx, Route{Group: "cyber", Exclude: map[string]bool{"acc-2": true}})
	if err != nil {
		t.Fatalf("after clear, acc-1 should be selectable: %v", err)
	}
	if lease.Account.ID != "acc-1" {
		t.Fatalf("after clear selected %q, want acc-1", lease.Account.ID)
	}
	lease.Release()
}

// A strict self-contained request bound to a recheck-pending account must fail over
// and rebind rather than return ErrStrictUnavailable, because the request can be
// resent to a healthy account without server-side state.
func TestStrictStickyFailsOverWhenBoundAccountRecheckPending(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: "h", RouteKey: "k", Source: "test", AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.BenchBindingForRecheck(ctx, "acc-1", storage.Now()-1); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	lease, err := s.Select(ctx, Route{Group: "cyber", Affinity: routing.AffinityKey{Hash: "h", Key: "k"}, Strict: true})
	if err != nil {
		t.Fatalf("strict self-contained request should fail over: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "acc-2" {
		t.Fatalf("failed over to %q, want acc-2", lease.Account.ID)
	}
	b, err := store.GetAffinityBinding(ctx, "h")
	if err != nil {
		t.Fatal(err)
	}
	if b.AccountID != "acc-2" {
		t.Fatalf("affinity rebound to %q, want acc-2", b.AccountID)
	}
}

func TestMovableStrictStickyTokenBudgetFailsOverAndRebinds(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	key := routing.AffinityKey{Hash: "h-budget-movable", Key: "k", Source: "test"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)

	held, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true, EstimatedTokens: 90})
	if err != nil {
		t.Fatalf("hold acc-1: %v", err)
	}
	defer held.Release()
	if held.Account.ID != "acc-1" {
		t.Fatalf("held account = %q, want acc-1", held.Account.ID)
	}

	lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true, Movable: true, EstimatedTokens: 20})
	if err != nil {
		t.Fatalf("movable strict request should fail over on token budget: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "acc-2" {
		t.Fatalf("movable strict request selected %q, want acc-2", lease.Account.ID)
	}
	b, err := store.GetAffinityBinding(ctx, key.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if b.AccountID != "acc-2" {
		t.Fatalf("affinity rebound to %q, want acc-2", b.AccountID)
	}
}

func TestNonMovableStrictStickyTokenBudgetStaysPinned(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	key := routing.AffinityKey{Hash: "h-budget-stateful", Key: "k", Source: "test"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)

	held, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true, EstimatedTokens: 90})
	if err != nil {
		t.Fatalf("hold acc-1: %v", err)
	}
	defer held.Release()

	selectCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = s.Select(selectCtx, Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true, ServerSideState: true, EstimatedTokens: 20})
	if !errors.Is(err, ErrStrictUnavailable) {
		t.Fatalf("non-movable strict request err = %v, want ErrStrictUnavailable", err)
	}
	b, err := store.GetAffinityBinding(ctx, key.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if b.AccountID != "acc-1" {
		t.Fatalf("affinity rebound to %q, want acc-1", b.AccountID)
	}
}

func TestServerSideStateStrictStickyTokenBudgetStaysPinned(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	key := routing.AffinityKey{Hash: "h-budget-stateful-server", Key: "k", Source: "test"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)

	held, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true, EstimatedTokens: 90})
	if err != nil {
		t.Fatalf("hold acc-1: %v", err)
	}
	defer held.Release()

	selectCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = s.Select(selectCtx, Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true, ServerSideState: true, EstimatedTokens: 20})
	if !errors.Is(err, ErrStrictUnavailable) {
		t.Fatalf("server-side-state strict request err = %v, want ErrStrictUnavailable", err)
	}
	b, err := store.GetAffinityBinding(ctx, key.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if b.AccountID != "acc-1" {
		t.Fatalf("affinity rebound to %q, want acc-1", b.AccountID)
	}
}

func TestServerSideStateStrictStickyTokenBudgetWaitsForPinnedAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	key := routing.AffinityKey{Hash: "h-budget-stateful-wait", Key: "k", Source: "test"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	cfg.StickyWaitMillis = 1
	cfg.StatefulStickyWaitSeconds = 1
	s := New(store, cfg)

	held, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true, EstimatedTokens: 90})
	if err != nil {
		t.Fatalf("hold acc-1: %v", err)
	}
	if held.Account.ID != "acc-1" {
		t.Fatalf("held account = %q, want acc-1", held.Account.ID)
	}

	result := make(chan struct {
		lease Lease
		err   error
	}, 1)
	selectCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	go func() {
		lease, err := s.Select(selectCtx, Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true, ServerSideState: true, EstimatedTokens: 20})
		result <- struct {
			lease Lease
			err   error
		}{lease: lease, err: err}
	}()

	select {
	case got := <-result:
		if got.err == nil {
			got.lease.Release()
		}
		t.Fatalf("stateful strict request returned before pinned account released: lease=%s err=%v", got.lease.Account.ID, got.err)
	case <-time.After(50 * time.Millisecond):
	}

	held.Release()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("stateful strict request after release: %v", got.err)
		}
		defer got.lease.Release()
		if got.lease.Account.ID != "acc-1" {
			t.Fatalf("stateful strict request selected %q, want pinned acc-1", got.lease.Account.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("stateful strict request did not resume after pinned account release")
	}
}

func TestServerSideStateStrictStickyTokenBudgetWaitTimeout(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	key := routing.AffinityKey{Hash: "h-budget-stateful-timeout", Key: "k", Source: "test"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	cfg.StickyWaitMillis = 1
	cfg.StatefulStickyWaitSeconds = 1
	s := New(store, cfg)

	held, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true, EstimatedTokens: 90})
	if err != nil {
		t.Fatalf("hold acc-1: %v", err)
	}
	defer held.Release()

	selectCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	_, err = s.Select(selectCtx, Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true, ServerSideState: true, EstimatedTokens: 20})
	if !errors.Is(err, ErrStrictUnavailable) {
		t.Fatalf("stateful strict request err = %v, want ErrStrictUnavailable", err)
	}
	msg := err.Error()
	for _, want := range []string{"stateful sticky wait timeout", "acc-1", "token budget"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("timeout error = %q, want substring %q", msg, want)
		}
	}
}

func TestServerSideStateStrictStickyConcurrencyWaitsForPinnedAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	profile, err := store.GetEgressProfile(ctx, storage.DefaultDirectEgressID)
	if err != nil {
		t.Fatal(err)
	}
	profile.MaxConcurrency = 1
	if err := store.UpsertEgressProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	key := routing.AffinityKey{Hash: "h-concurrency-stateful-wait", Key: "k", Source: "test"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	cfg.StatefulStickyWaitSeconds = 1
	s := New(store, cfg)

	held, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true})
	if err != nil {
		t.Fatalf("hold acc-1: %v", err)
	}
	if held.Account.ID != "acc-1" {
		t.Fatalf("held account = %q, want acc-1", held.Account.ID)
	}

	result := make(chan struct {
		lease Lease
		err   error
	}, 1)
	selectCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	go func() {
		lease, err := s.Select(selectCtx, Route{Group: "cyber", Provider: "codex", Affinity: key, Strict: true, ServerSideState: true})
		result <- struct {
			lease Lease
			err   error
		}{lease: lease, err: err}
	}()

	select {
	case got := <-result:
		if got.err == nil {
			got.lease.Release()
		}
		t.Fatalf("stateful strict request returned before pinned account concurrency released: lease=%s err=%v", got.lease.Account.ID, got.err)
	case <-time.After(50 * time.Millisecond):
	}

	held.Release()
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("stateful strict request after release: %v", got.err)
		}
		defer got.lease.Release()
		if got.lease.Account.ID != "acc-1" {
			t.Fatalf("stateful strict request selected %q, want pinned acc-1", got.lease.Account.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("stateful strict request did not resume after pinned account concurrency release")
	}
}

func TestServerSideStateStrictStickyRateLimitWaitsForPinnedAccount(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	key := routing.AffinityKey{Hash: "h-rl-stateful-no-wait", Key: "k", Source: "test"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID: "acc-1", Provider: "codex", Model: "gpt-5", LimiterType: "tokens", Source: "tokens",
		RemainingTokens: 0, RemainingRequests: -1, LimitTokens: 100, LimitRequests: -1, ResetAt: storage.Now() + 1, Status: "rejected",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	cfg.StatefulStickyWaitSeconds = 1
	s := New(store, cfg)

	selectCtx, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Select(selectCtx, Route{Group: "cyber", Provider: "codex", Model: "gpt-5", Affinity: key, Strict: true, ServerSideState: true})
	if !errors.Is(err, ErrStrictUnavailable) {
		t.Fatalf("stateful strict request err = %v, want ErrStrictUnavailable", err)
	}
	if time.Since(start) < 60*time.Millisecond {
		t.Fatalf("stateful strict rate-limit path returned without waiting: %v", time.Since(start))
	}
	if !strings.Contains(err.Error(), "stateful sticky wait timeout") || !strings.Contains(err.Error(), "rate limit cooldown") {
		t.Fatalf("rate-limit wait error should identify the pinned-account wait: %v", err)
	}
}

func TestCompactionSkipsLocalTokenBudget(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.UpsertAccount(ctx, storage.Account{ID: "acc-1", Label: "acc-1", GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	s := New(store, cfg)

	held, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", EstimatedTokens: 90})
	if err != nil {
		t.Fatalf("hold acc-1: %v", err)
	}
	defer held.Release()

	lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Strict: true, Compaction: true, EstimatedTokens: 20})
	if err != nil {
		t.Fatalf("compaction select should bypass local token budget: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "acc-1" {
		t.Fatalf("compaction selected %q, want acc-1", lease.Account.ID)
	}
	inflight, tokens := s.currentLoad("acc-1")
	if inflight != 2 || tokens != 110 {
		t.Fatalf("load after compaction lease = inflight %d tokens %d, want 2 and 110", inflight, tokens)
	}
}

func TestMovableStrictStickyAccountRateLimitLongCooldownFailsOver(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	key := routing.AffinityKey{Hash: "h-rl-movable", Key: "k", Source: "test"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID: "acc-1", Provider: "codex", Model: "gpt-5", LimiterType: "tokens", Source: "tokens",
		RemainingTokens: 0, RemainingRequests: -1, LimitTokens: 100, LimitRequests: -1, ResetAt: storage.Now() + 120, Status: "rejected",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StrictStickyMaxCooldownSeconds = 60
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)

	lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Model: "gpt-5", Affinity: key, Strict: true, Movable: true})
	if err != nil {
		t.Fatalf("movable strict request should fail over on long account rate-limit cooldown: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "acc-2" {
		t.Fatalf("selected %q, want acc-2", lease.Account.ID)
	}
}

func TestSelfContainedStrictStickyAccountRateLimitCooldownFailsOver(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-1", "acc-2"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	key := routing.AffinityKey{Hash: "h-rl-zero", Key: "k", Source: "test"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: "acc-1"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID: "acc-1", Provider: "codex", Model: "gpt-5", LimiterType: "tokens", Source: "tokens",
		RemainingTokens: 0, RemainingRequests: -1, LimitTokens: 100, LimitRequests: -1, ResetAt: storage.Now() + 120, Status: "rejected",
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)

	lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Model: "gpt-5", Affinity: key, Strict: true})
	if err != nil {
		t.Fatalf("strict self-contained request should fail over on account rate-limit cooldown: %v", err)
	}
	defer lease.Release()
	if lease.Account.ID != "acc-2" {
		t.Fatalf("selected %q, want acc-2", lease.Account.ID)
	}
}

func TestNoAccountErrorIncludesSkipCounters(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, id := range []string{"acc-model", "acc-rate", "acc-budget"} {
		if err := store.UpsertAccount(ctx, storage.Account{ID: id, Label: id, GroupName: "cyber", Provider: "codex", Status: "active"}, storage.AccountToken{AccessToken: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{
		{AccountID: "acc-model", ModelSlug: "gpt-other", AvailabilityState: capability.AvailabilityVerified, NativeMaxContextWindow: 200000},
		{AccountID: "acc-rate", ModelSlug: "gpt-5", NativeMaxContextWindow: 200000},
		{AccountID: "acc-budget", ModelSlug: "gpt-5", NativeMaxContextWindow: 200000},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertAccountRateLimit(ctx, storage.AccountRateLimit{
		AccountID: "acc-rate", Provider: "codex", Model: "gpt-5", LimiterType: "tokens", Source: "tokens",
		RemainingTokens: 0, RemainingRequests: -1, LimitTokens: 100, LimitRequests: -1, ResetAt: storage.Now() + 300, Status: "rejected",
	}); err != nil {
		t.Fatal(err)
	}
	key := routing.AffinityKey{Hash: "h-budget-counter", Key: "k", Source: "test"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: "acc-budget"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AccountTokenBudget = 100
	cfg.StickyWaitMillis = 1
	s := New(store, cfg)
	held, err := s.Select(ctx, Route{Group: "cyber", Provider: "codex", Model: "gpt-5", Affinity: key, EstimatedTokens: 90})
	if err != nil {
		t.Fatalf("hold acc-budget: %v", err)
	}
	defer held.Release()

	_, err = s.Select(ctx, Route{Group: "cyber", Provider: "codex", Model: "gpt-5", EstimatedTokens: 20})
	if !errors.Is(err, ErrNoAccount) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
	var noAccount *NoAccountError
	if !errors.As(err, &noAccount) {
		t.Fatalf("err type = %T, want *NoAccountError", err)
	}
	if noAccount.Counters.ModelUnsupported != 1 || noAccount.Counters.RateLimitCooldown != 1 || noAccount.Counters.TokenBudget != 1 {
		t.Fatalf("skip counters = %+v, want model=1 rate-limit=1 token_budget=1", noAccount.Counters)
	}
}
