package scheduler

import (
	"context"
	"errors"
	"testing"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/config"
	kirowire "codex-account-pool/internal/kiro"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

func seedUnverifiedKiroAccount(t *testing.T, store *storage.Store, account storage.Account) string {
	t.Helper()
	ctx := context.Background()
	if account.Provider == "" {
		account.Provider = "kiro"
	}
	if account.Status == "" {
		account.Status = "active"
	}
	if err := store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "kiro-token"}); err != nil {
		t.Fatal(err)
	}
	staticModels := capability.StaticKiroModels(account.ID)
	if err := store.UpsertCapabilities(ctx, staticModels); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertKiroCredentials(ctx, storage.KiroCredentials{AccountID: account.ID, AuthMethod: "api_key", KiroAPIKey: "kiro-key", APIRegion: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	endpointHash, err := kirowire.EndpointHash("", "us-east-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	models := make([]string, 0, len(staticModels))
	for _, model := range staticModels {
		models = append(models, model.ModelSlug)
	}
	if err := store.EnsureKiroRuntimeModels(ctx, account.ID, endpointHash, models); err != nil {
		t.Fatal(err)
	}
	return endpointHash
}

func requireModelUnsupported(t *testing.T, err error, want int) {
	t.Helper()
	var noAccount *NoAccountError
	if !errors.As(err, &noAccount) || noAccount.Counters.ModelUnsupported != want {
		t.Fatalf("error=%v, want model_unsupported=%d", err, want)
	}
}

func seedKiroLiveCatalog(t *testing.T, store *storage.Store, accountID string, models []storage.KiroModelDescriptor) {
	t.Helper()
	endpointHash, err := kirowire.EndpointHash("", "us-east-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	capabilityKey, governanceKey := kirowire.KiroCapabilityKey(endpointHash, "us-east-1", "")
	now := storage.Now()
	for index := range models {
		models[index].AccountID = accountID
		models[index].CapabilityKey = capabilityKey
		models[index].Complete = true
		models[index].Generation = now
		models[index].ObservedAt = now
		models[index].ExpiresAt = now + 21600
		models[index].Source = "kiro_live_catalog"
	}
	if err := store.ReplaceKiroModelCatalog(context.Background(), storage.KiroProbeState{
		AccountID: accountID, CapabilityKey: capabilityKey, Region: "us-east-1",
		EndpointHash: endpointHash, GovernanceKey: governanceKey, Source: "kiro_live_catalog",
		Generation: now, ExpiresAt: now + 21600, PageCount: 1, Complete: true,
	}, models); err != nil {
		t.Fatal(err)
	}
}

func TestKiroLiveCatalogControlsDefaultAliasesAndFirstOneMillionRequest(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	account := storage.Account{ID: "kiro-live", Label: "live", GroupName: "cyber", PlanType: "KIRO FREE"}
	seedUnverifiedKiroAccount(t, store, account)
	seedKiroLiveCatalog(t, store, account.ID, []storage.KiroModelDescriptor{
		{UpstreamID: "claude-opus-4.8", PublicID: "claude-opus-4.8", MaxInputTokens: 1_000_000},
		{UpstreamID: "claude-opus-5", PublicID: "claude-opus-5", MaxInputTokens: 1_000_000, MaxOutputTokens: 128_000},
		{UpstreamID: "claude-sonnet-5", PublicID: "claude-sonnet-5", Default: true, MaxInputTokens: 1_000_000},
		{UpstreamID: "auto", PublicID: "auto", MaxInputTokens: 1_000_000},
	})
	scheduler := New(store, config.Default())
	for _, test := range []struct {
		requested   string
		contextMode string
		want        string
	}{
		{requested: "default", want: "claude-sonnet-5"},
		{requested: "opus", want: "claude-opus-5"},
		{requested: "auto", want: "auto"},
		{requested: "claude-opus-5", contextMode: "1m", want: "claude-opus-5"},
	} {
		resolved, bootstrap, ok := scheduler.resolveKiroRouteModel(ctx, account, Route{
			Group: "cyber", Model: test.requested, ContextMode: test.contextMode,
			KiroDefaultRegion: "us-east-1",
		})
		if !ok || bootstrap || resolved != test.want {
			t.Fatalf("request=%q context=%q resolved=%q bootstrap=%t ok=%t", test.requested, test.contextMode, resolved, bootstrap, ok)
		}
	}
	if _, _, ok := scheduler.resolveKiroRouteModel(ctx, account, Route{
		Group: "cyber", Model: "claude-haiku-4-5", KiroDefaultRegion: "us-east-1",
	}); ok {
		t.Fatal("complete live catalog silently bootstrapped a missing model")
	}
}

func TestAutoKiroOnlyConcreteModelBootstrapsThenBecomesVerified(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	endpointHash := seedUnverifiedKiroAccount(t, store, storage.Account{ID: "kiro-bootstrap", Label: "kiro", GroupName: "cyber", PlanType: "KIRO PRO"})
	s := New(store, config.Default())
	route := Route{
		Group: "cyber", AllowedProviders: []string{"claude", "kiro"},
		Model: "claude-opus-4-8", ThinkingRequired: true,
	}
	lease, err := s.Select(ctx, route)
	if err != nil {
		t.Fatalf("auto bootstrap select: %v", err)
	}
	if lease.Account.ID != "kiro-bootstrap" || lease.ResolvedModel != "claude-opus-4.8" {
		t.Fatalf("bootstrap lease=%+v", lease)
	}
	lease.Release()
	state, err := store.GetKiroRuntimeCapability(ctx, "kiro-bootstrap", endpointHash, "claude-opus-4.8")
	if err != nil || state.ModelState != "unknown" {
		t.Fatalf("pre-observation runtime state=%+v err=%v", state, err)
	}
	state, err = store.ObserveKiroCapability(ctx, "kiro-bootstrap", endpointHash, "claude-opus-4.8", storage.KiroCapabilityObservation{ModelSucceeded: true, ThinkingRequested: true})
	if err != nil || state.ModelState != "verified" || state.ThinkingState != "verified" {
		t.Fatalf("post-observation runtime state=%+v err=%v", state, err)
	}
	verifiedLease, err := s.Select(ctx, route)
	if err != nil {
		t.Fatalf("verified select: %v", err)
	}
	verifiedLease.Release()
}

func TestKiroGPTConcreteModelBootstrapsWithoutThinkingAndUsesModelEvidence(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	endpointHash := seedUnverifiedKiroAccount(t, store, storage.Account{ID: "kiro-gpt-bootstrap", Label: "kiro gpt", GroupName: "cyber", PlanType: "KIRO PRO"})
	s := New(store, config.Default())
	route := Route{
		Group: "cyber", AllowedProviders: []string{"kiro"}, ExplicitProvider: true,
		Model: "gpt-5.6-sol",
	}

	// GPT models use Kiro's non-Claude generation envelope. A static exact match
	// must therefore bootstrap even though adaptive-thinking evidence does not
	// exist for it.
	lease, err := s.Select(ctx, route)
	if err != nil {
		t.Fatalf("GPT bootstrap select: %v", err)
	}
	if lease.Account.ID != "kiro-gpt-bootstrap" || lease.ResolvedModel != "gpt-5.6-sol" {
		t.Fatalf("GPT bootstrap lease=%+v", lease)
	}
	lease.Release()

	// GPT conversion records model success without a Claude thinking request. It
	// must nevertheless become runtime-verified for subsequent scheduling.
	if _, err := store.ObserveKiroCapability(ctx, "kiro-gpt-bootstrap", endpointHash, "gpt-5.6-sol", storage.KiroCapabilityObservation{ModelSucceeded: true}); err != nil {
		t.Fatal(err)
	}
	resolved, bootstrap, ok := s.resolveKiroRouteModel(ctx, storage.Account{
		ID: "kiro-gpt-bootstrap", PlanType: "KIRO PRO",
	}, route)
	if !ok || bootstrap || resolved != "gpt-5.6-sol" {
		t.Fatalf("GPT runtime evidence resolved=%q bootstrap=%t ok=%t", resolved, bootstrap, ok)
	}
}

func TestKiroGPTAccountsUseNormalFairSchedulerRotation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	seedUnverifiedKiroAccount(t, store, storage.Account{ID: "kiro-gpt-a", Label: "kiro gpt a", GroupName: "cyber", PlanType: "KIRO PRO"})
	seedUnverifiedKiroAccount(t, store, storage.Account{ID: "kiro-gpt-b", Label: "kiro gpt b", GroupName: "cyber", PlanType: "KIRO PRO"})
	s := New(store, config.Default())

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		lease, err := s.Select(ctx, Route{Group: "cyber", Provider: "kiro", Model: "gpt-5.6-sol"})
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		seen[lease.Account.ID] = true
		lease.Release()
	}
	if !seen["kiro-gpt-a"] || !seen["kiro-gpt-b"] {
		t.Fatalf("equal-load Kiro GPT accounts were not fairly rotated: %v", seen)
	}
}

func TestFairSchedulingBypassesStickyAffinityForAutoKiroGPT(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	seedUnverifiedKiroAccount(t, store, storage.Account{ID: "kiro-fair-a", Label: "kiro fair a", GroupName: "cyber", PlanType: "KIRO PRO"})
	seedUnverifiedKiroAccount(t, store, storage.Account{ID: "kiro-fair-b", Label: "kiro fair b", GroupName: "cyber", PlanType: "KIRO PRO"})
	affinity := routing.AffinityFromKey("auto-gpt-fair-session", "test")
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{
		RouteKeyHash: affinity.Hash, RouteKey: affinity.Key, Source: affinity.Source,
		AccountID: "kiro-fair-a", Provider: "kiro", Model: "gpt-5.6-sol", EgressID: storage.DefaultDirectEgressID,
	}); err != nil {
		t.Fatal(err)
	}

	s := New(store, config.Default())
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		lease, err := s.Select(ctx, Route{
			Group: "cyber", Provider: "kiro", Model: "gpt-5.6-sol", Affinity: affinity, FairScheduling: true,
		})
		if err != nil {
			t.Fatalf("select %d: %v", i, err)
		}
		seen[lease.Account.ID] = true
		lease.Release()
	}
	if !seen["kiro-fair-a"] || !seen["kiro-fair-b"] {
		t.Fatalf("fair route reused the sticky account instead of rotating: %v", seen)
	}
	bound, err := store.GetAffinityBinding(ctx, affinity.Hash)
	if err != nil || bound.AccountID != "kiro-fair-a" {
		t.Fatalf("fair route unexpectedly rewrote its existing affinity binding: %+v err=%v", bound, err)
	}
}

func TestKiroOneMillionRequiresCompleteLiveCatalog(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	endpointHash := seedUnverifiedKiroAccount(t, store, storage.Account{ID: "kiro-1m", Label: "kiro", GroupName: "cyber", PlanType: "KIRO PRO"})
	s := New(store, config.Default())
	route := Route{Group: "cyber", AllowedProviders: []string{"kiro"}, ExplicitProvider: true, Model: "claude-opus-4-8", ContextMode: "1m", ThinkingRequired: true}
	if _, err := s.Select(ctx, route); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("unverified Kiro 1M capability routed: %v", err)
	}
	if _, err := store.ObserveKiroCapability(ctx, "kiro-1m", endpointHash, "claude-opus-4.8", storage.KiroCapabilityObservation{ModelSucceeded: true, ThinkingRequested: true}); err != nil {
		t.Fatal(err)
	}
	s.InvalidateAccountCache()
	if _, err := s.Select(ctx, route); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("runtime inference alone established Kiro 1M: %v", err)
	}
	seedKiroLiveCatalog(t, store, "kiro-1m", []storage.KiroModelDescriptor{{
		UpstreamID: "claude-opus-4.8", PublicID: "claude-opus-4.8", MaxInputTokens: 1_000_000,
	}})
	s.InvalidateAccountCache()
	lease, err := s.Select(ctx, route)
	if err != nil {
		t.Fatalf("catalog-verified Kiro Opus 1M did not route: %v", err)
	}
	if lease.ResolvedModel != "claude-opus-4.8" {
		t.Fatalf("resolved model=%q", lease.ResolvedModel)
	}
	lease.Release()
}

func TestKiroOneMillionRejectsFreeAndUnknownPlansAfterRuntimeVerification(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan string
	}{
		{name: "free", plan: "KIRO FREE"},
		{name: "unknown", plan: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			accountID := "kiro-1m-" + tc.name
			endpointHash := seedUnverifiedKiroAccount(t, store, storage.Account{ID: accountID, Label: tc.name, GroupName: "cyber", PlanType: tc.plan})
			if _, err := store.ObserveKiroCapability(ctx, accountID, endpointHash, "claude-sonnet-4.6", storage.KiroCapabilityObservation{ModelSucceeded: true, ThinkingRequested: true}); err != nil {
				t.Fatal(err)
			}
			s := New(store, config.Default())
			_, err := s.Select(ctx, Route{Group: "cyber", AllowedProviders: []string{"kiro"}, ExplicitProvider: true, Model: "claude-sonnet-4-6", ContextMode: "1m", ThinkingRequired: true})
			requireModelUnsupported(t, err, 1)
		})
	}
}

func TestKiroBootstrapCandidatePriority(t *testing.T) {
	t.Run("official claude before bootstrap", func(t *testing.T) {
		store := testStore(t)
		ctx := context.Background()
		claude := storage.Account{ID: "official", Label: "claude", GroupName: "cyber", Provider: "claude", Status: "active"}
		if err := store.UpsertAccount(ctx, claude, storage.AccountToken{AccessToken: "claude-token"}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: claude.ID, ModelSlug: "claude-opus-4-8", Source: "claude_static"}}); err != nil {
			t.Fatal(err)
		}
		seedUnverifiedKiroAccount(t, store, storage.Account{ID: "bootstrap", Label: "kiro", GroupName: "cyber", PlanType: "KIRO PRO"})
		lease, err := New(store, config.Default()).Select(ctx, Route{Group: "cyber", AllowedProviders: []string{"claude", "kiro"}, Model: "claude-opus-4-8", ThinkingRequired: true})
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		if lease.Account.ID != claude.ID {
			t.Fatalf("selected %s, want official Claude before bootstrap Kiro", lease.Account.ID)
		}
	})

	t.Run("verified kiro before bootstrap", func(t *testing.T) {
		store := testStore(t)
		ctx := context.Background()
		endpointHash := seedUnverifiedKiroAccount(t, store, storage.Account{ID: "verified", Label: "verified", GroupName: "cyber", PlanType: "KIRO PRO"})
		seedUnverifiedKiroAccount(t, store, storage.Account{ID: "bootstrap", Label: "bootstrap", GroupName: "cyber", PlanType: "KIRO PRO"})
		if _, err := store.ObserveKiroCapability(ctx, "verified", endpointHash, "claude-opus-4.8", storage.KiroCapabilityObservation{ModelSucceeded: true, ThinkingRequested: true}); err != nil {
			t.Fatal(err)
		}
		lease, err := New(store, config.Default()).Select(ctx, Route{Group: "cyber", AllowedProviders: []string{"kiro"}, Model: "claude-opus-4-8", ThinkingRequired: true})
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		if lease.Account.ID != "verified" {
			t.Fatalf("selected %s, want verified Kiro before bootstrap Kiro", lease.Account.ID)
		}
	})
}

func TestKiroBootstrapPreservesProviderHintSemantics(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	seedUnverifiedKiroAccount(t, store, storage.Account{ID: "kiro-only", Label: "kiro", GroupName: "cyber", PlanType: "KIRO PRO"})
	s := New(store, config.Default())
	_, err := s.Select(ctx, Route{Group: "cyber", AllowedProviders: []string{"claude"}, Model: "claude-opus-4-8"})
	var noAccount *NoAccountError
	if !errors.As(err, &noAccount) || noAccount.Counters.ProviderMismatch != 1 {
		t.Fatalf("explicit Claude fell back to Kiro: %v", err)
	}
	lease, err := s.Select(ctx, Route{Group: "cyber", AllowedProviders: []string{"kiro"}, ExplicitProvider: true, Model: "claude-opus-4-8"})
	if err != nil {
		t.Fatalf("explicit Kiro did not bootstrap: %v", err)
	}
	lease.Release()
}

func TestKiroBootstrapRejectsAliasesUnsupportedThinkingAndFreeOpus(t *testing.T) {
	t.Run("aliases require verification", func(t *testing.T) {
		store := testStore(t)
		seedUnverifiedKiroAccount(t, store, storage.Account{ID: "kiro", Label: "kiro", GroupName: "cyber", PlanType: "KIRO PRO"})
		s := New(store, config.Default())
		for _, model := range []string{"auto", "opus", "sonnet"} {
			_, err := s.Select(context.Background(), Route{Group: "cyber", AllowedProviders: []string{"kiro"}, Model: model})
			requireModelUnsupported(t, err, 1)
		}
	})

	t.Run("thinking must be statically supported", func(t *testing.T) {
		store := testStore(t)
		seedUnverifiedKiroAccount(t, store, storage.Account{ID: "kiro", Label: "kiro", GroupName: "cyber", PlanType: "KIRO PRO"})
		s := New(store, config.Default())
		_, err := s.Select(context.Background(), Route{Group: "cyber", AllowedProviders: []string{"kiro"}, Model: "claude-sonnet-4-5"})
		requireModelUnsupported(t, err, 1)
		lease, err := s.Select(context.Background(), Route{Group: "cyber", AllowedProviders: []string{"kiro"}, Model: "claude-opus-4-8"})
		if err != nil {
			t.Fatalf("known thinking model did not bootstrap: %v", err)
		}
		lease.Release()
	})

	t.Run("free plan cannot bootstrap stale opus", func(t *testing.T) {
		store := testStore(t)
		seedUnverifiedKiroAccount(t, store, storage.Account{ID: "free", Label: "free", GroupName: "cyber", PlanType: "KIRO FREE"})
		s := New(store, config.Default())
		_, err := s.Select(context.Background(), Route{Group: "cyber", AllowedProviders: []string{"kiro"}, Model: "claude-opus-4-8"})
		requireModelUnsupported(t, err, 1)
		lease, err := s.Select(context.Background(), Route{Group: "cyber", AllowedProviders: []string{"kiro"}, Model: "claude-sonnet-4-6"})
		if err != nil {
			t.Fatalf("free plan non-Opus bootstrap failed: %v", err)
		}
		lease.Release()
	})
}

func TestLegacyKiroAffinityIsCompletedDuringBootstrap(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	seedUnverifiedKiroAccount(t, store, storage.Account{ID: "legacy-kiro", Label: "kiro", GroupName: "cyber", PlanType: "KIRO PRO"})
	key := routing.AffinityKey{Key: "legacy-kiro-session", Hash: "legacy-kiro-hash", Source: "test"}
	if err := store.UpsertAffinityBinding(ctx, storage.AffinityBinding{RouteKeyHash: key.Hash, RouteKey: key.Key, Source: key.Source, AccountID: "legacy-kiro"}); err != nil {
		t.Fatal(err)
	}
	lease, err := New(store, config.Default()).Select(ctx, Route{
		Group: "cyber", AllowedProviders: []string{"claude", "kiro"}, Affinity: key,
		ImmutableAffinity: true, Model: "claude-opus-4-8", ThinkingRequired: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	bound, err := store.GetAffinityBinding(ctx, key.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Provider != "kiro" || bound.Model != "claude-opus-4.8" || bound.EgressID == "" {
		t.Fatalf("legacy Kiro affinity was not completed: %+v", bound)
	}
}
