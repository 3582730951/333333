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
