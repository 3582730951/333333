package storage

import (
	"context"
	"strings"
	"testing"
)

func TestWrapEgressWithSidecarPreservesRealExitIdentity(t *testing.T) {
	base := EgressProfile{
		ID: "proxy-br", Name: "BR residential", Type: "http_proxy",
		Endpoint: "http://user:pass@proxy.example:8080", Region: "BR",
		ExitIP: "198.51.100.10", Health: "healthy", MaxConcurrency: 7,
	}
	sidecar := EgressProfile{
		ID: "sidecar-local", Type: CurlCFFISidecarEgressType,
		Endpoint: "http://127.0.0.1:8790", ChainProxy: "socks5h://default.invalid:1080",
	}

	wrapped, err := WrapEgressWithSidecar(base, sidecar)
	if err != nil {
		t.Fatal(err)
	}
	if wrapped.ID != base.ID || wrapped.Region != base.Region || wrapped.ExitIP != base.ExitIP || wrapped.MaxConcurrency != base.MaxConcurrency {
		t.Fatalf("real exit identity changed: %+v", wrapped)
	}
	if wrapped.Type != CurlCFFISidecarEgressType || wrapped.Endpoint != sidecar.Endpoint || wrapped.ChainProxy != base.Endpoint {
		t.Fatalf("sidecar chain = %+v", wrapped)
	}
	if wrapped.TransportSidecarID != sidecar.ID {
		t.Fatalf("transport sidecar id = %q", wrapped.TransportSidecarID)
	}

	restored := WithoutSidecarTransport(wrapped)
	if restored.Type != base.Type || restored.Endpoint != base.Endpoint || restored.ChainProxy != base.ChainProxy || restored.TransportSidecarID != "" {
		t.Fatalf("force-direct restoration = %+v, want %+v", restored, base)
	}
}

func TestWrapDirectEgressKeepsSidecarDefaultChain(t *testing.T) {
	wrapped, err := WrapEgressWithSidecar(
		EgressProfile{ID: DefaultDirectEgressID, Type: "direct"},
		EgressProfile{ID: "sidecar", Type: CurlCFFISidecarEgressType, Endpoint: "http://127.0.0.1:8790", ChainProxy: "socks5h://127.0.0.1:40000"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if wrapped.ChainProxy != "socks5h://127.0.0.1:40000" {
		t.Fatalf("direct wrapper chain = %q", wrapped.ChainProxy)
	}
}

func TestSidecarEgressBindingPersistsAndFailsClosed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := Account{ID: "acc-sidecar-binding", GroupName: "cyber", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	base := EgressProfile{ID: "proxy", Type: "socks5h_proxy", Endpoint: "socks5h://proxy.example:1080", Health: "healthy"}
	sidecar := EgressProfile{ID: "sidecar", Type: CurlCFFISidecarEgressType, Endpoint: "http://127.0.0.1:8790", Health: "healthy"}
	if err := store.UpsertEgressProfile(ctx, base); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressProfile(ctx, sidecar); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertEgressBinding(ctx, AccountEgressBinding{AccountID: account.ID, PrimaryEgressID: base.ID, SidecarEgressID: sidecar.ID}); err != nil {
		t.Fatal(err)
	}
	binding, err := store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.SidecarEgressID != sidecar.ID {
		t.Fatalf("sidecar binding = %q", binding.SidecarEgressID)
	}
	resolved, err := store.ResolvePrimaryEgressBinding(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Type != CurlCFFISidecarEgressType || resolved.ChainProxy != base.Endpoint || resolved.ID != base.ID {
		t.Fatalf("resolved binding = %+v", resolved)
	}

	binding.SidecarEgressID = "missing-sidecar"
	if _, err := store.ResolvePrimaryEgressBinding(ctx, binding); err == nil || !strings.Contains(err.Error(), "missing-sidecar") {
		t.Fatalf("missing explicit sidecar must fail closed, got %v", err)
	}
}

func TestUnavailableSidecarDoesNotAdvertiseRoutableCapabilities(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	account := Account{ID: "cap-sidecar", GroupName: "cyber", Provider: "claude", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertCapabilities(ctx, []ModelCapability{{
		AccountID: account.ID, ModelSlug: "claude-sonnet-4-6", AvailabilityState: "verified", Source: "probe",
	}}); err != nil {
		t.Fatal(err)
	}
	binding := AccountEgressBinding{
		AccountID: account.ID, PrimaryEgressID: DefaultDirectEgressID, SidecarEgressID: "cap-sidecar-transport",
	}
	if err := store.UpsertEgressBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	assertHidden := func() {
		t.Helper()
		caps, err := store.ListRoutableCapabilities(ctx, "cyber")
		if err != nil || len(caps) != 0 {
			t.Fatalf("unavailable sidecar advertised public capability: %+v err=%v", caps, err)
		}
		accounts, err := store.AccountsWithModelAndContext(ctx, "cyber", "claude-sonnet-4-6", "")
		if err != nil || len(accounts) != 0 {
			t.Fatalf("unavailable sidecar remained model-routable: %+v err=%v", accounts, err)
		}
	}
	assertHidden()

	sidecar := EgressProfile{
		ID: binding.SidecarEgressID, Type: CurlCFFISidecarEgressType,
		Endpoint: "http://127.0.0.1:8790", Health: "healthy",
	}
	if err := store.UpsertEgressProfile(ctx, sidecar); err != nil {
		t.Fatal(err)
	}
	caps, err := store.ListRoutableCapabilities(ctx, "cyber")
	if err != nil || len(caps) != 1 {
		t.Fatalf("healthy sidecar capability missing: %+v err=%v", caps, err)
	}
	accounts, err := store.AccountsWithModelAndContext(ctx, "cyber", "claude-sonnet-4-6", "")
	if err != nil || !accounts[account.ID] {
		t.Fatalf("healthy sidecar account missing: %+v err=%v", accounts, err)
	}

	sidecar.Health = "disabled"
	if err := store.UpsertEgressProfile(ctx, sidecar); err != nil {
		t.Fatal(err)
	}
	assertHidden()
}

func TestEffectiveEgressBindingFollowsGroupUnlessAccountExplicitlyOverrides(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"group-egress-a", "group-egress-b"} {
		if err := store.UpsertEgressProfile(ctx, EgressProfile{ID: id, Type: "direct", Health: "healthy"}); err != nil {
			t.Fatal(err)
		}
	}
	group, err := store.GetGroup(ctx, "cyber")
	if err != nil {
		t.Fatal(err)
	}
	group.EgressIDs = []string{"group-egress-a"}
	if err := store.UpdateGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	account := Account{ID: "effective-egress-account", GroupName: group.Name, Provider: "codex", Status: "active"}
	if err := store.UpsertAccount(ctx, account, AccountToken{AccessToken: "token"}); err != nil {
		t.Fatal(err)
	}
	binding, err := store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if binding.BindingScope != EgressBindingScopeGroup {
		t.Fatalf("new account binding scope = %q, want group", binding.BindingScope)
	}

	group.EgressIDs = []string{"group-egress-b"}
	if err := store.UpdateGroup(ctx, group); err != nil {
		t.Fatal(err)
	}
	effective, err := store.EffectiveEgressBinding(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	if effective.PrimaryEgressID != "group-egress-b" || effective.BindingScope != EgressBindingScopeGroup ||
		effective.CookieJarKey != account.ID+":group-egress-b" || effective.StandbyEgressIDs != "" {
		t.Fatalf("group-inherited binding = %+v", effective)
	}

	if err := store.UpsertEgressBinding(ctx, AccountEgressBinding{
		AccountID: account.ID, PrimaryEgressID: "group-egress-a", StandbyEgressIDs: "group-egress-b",
		BindingScope: EgressBindingScopeAccount,
	}); err != nil {
		t.Fatal(err)
	}
	explicit, err := store.GetEgressBinding(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	effective, err = store.EffectiveEgressBinding(ctx, explicit)
	if err != nil {
		t.Fatal(err)
	}
	if effective.PrimaryEgressID != "group-egress-a" || effective.BindingScope != EgressBindingScopeAccount ||
		effective.CookieJarKey != account.ID+":group-egress-a" || effective.StandbyEgressIDs != "" {
		t.Fatalf("explicit account binding = %+v", effective)
	}
}
