package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"codex-account-pool/internal/capability"
	"codex-account-pool/internal/routing"
	"codex-account-pool/internal/storage"
)

func seedRoutableCatalogAccount(t *testing.T, h *testHarness, accountID, group, provider, model, source string) {
	t.Helper()
	ctx := t.Context()
	if err := h.store.UpsertAccount(ctx, storage.Account{
		ID: accountID, GroupName: group, Provider: provider, Status: "active",
	}, storage.AccountToken{AccessToken: "token-" + accountID}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{
		AccountID: accountID, PrimaryEgressID: storage.DefaultDirectEgressID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertCapabilities(ctx, []storage.ModelCapability{{
		AccountID: accountID, ModelSlug: model,
		AvailabilityState:   capability.AvailabilityVerified,
		NativeContextWindow: 128000, NativeMaxContextWindow: 128000,
		EffectiveContextWindowPercent: 100, Visibility: "list",
		Source: source, LastProbeAt: storage.Now(),
	}}); err != nil {
		t.Fatal(err)
	}
}

func fetchOpenAIModelIDs(t *testing.T, h *testHarness, plainKey, providerHint string) map[string]bool {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, h.pool.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+plainKey)
	if providerHint != "" {
		request.Header.Set("X-Pool-Provider", providerHint)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, raw)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]bool, len(body.Data))
	for _, model := range body.Data {
		ids[model.ID] = true
	}
	return ids
}

func fetchAnthropicModelIDs(t *testing.T, h *testHarness, plainKey, providerHint string) map[string]bool {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, h.pool.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("x-api-key", plainKey)
	request.Header.Set("anthropic-version", "2023-06-01")
	if providerHint != "" {
		request.Header.Set("X-Pool-Provider", providerHint)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, raw)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]bool, len(body.Data))
	for _, model := range body.Data {
		ids[model.ID] = true
	}
	return ids
}

func TestAnthropicModelsEndpointAdvertisesClaudeFacingKiroModelID(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	account := storage.Account{
		ID: "kiro-model-catalog", GroupName: "cyber", Provider: "kiro", PlanType: "KIRO PRO", Status: "active",
	}
	if err := h.store.UpsertAccount(context.Background(), account, storage.AccountToken{}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertEgressBinding(context.Background(), storage.AccountEgressBinding{AccountID: account.ID, PrimaryEgressID: storage.DefaultDirectEgressID}); err != nil {
		t.Fatal(err)
	}
	caps := capability.StaticKiroModels(account.ID)
	for i := range caps {
		caps[i].AvailabilityState = capability.AvailabilityVerified
		caps[i].Source = "kiro_runtime"
		if canonical, ok := capability.KiroCanonicalModel(caps[i].ModelSlug); ok && canonical == "claude-opus-5" {
			caps[i].Context1MState = capability.Context1MSupported
			caps[i].Context1MSource = "kiro_live_catalog"
		}
	}
	if err := h.store.UpsertCapabilities(context.Background(), caps); err != nil {
		t.Fatal(err)
	}

	request, _ := http.NewRequest(http.MethodGet, h.pool.URL+"/v1/models", nil)
	request.Header.Set("anthropic-version", "2023-06-01")
	request.Header.Set("X-Pool-Provider", "kiro")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, model := range body.Data {
		ids[model.ID] = true
	}
	if !ids["claude-opus-4-8"] || ids["claude-opus-4.8"] ||
		!ids["claude-opus-5"] || !ids["claude-opus-5[1m]"] {
		t.Fatalf("unexpected Anthropic Kiro model catalog: %+v", ids)
	}
}

func TestUserGroupModelCatalogFiltersOnlyTheBlockedAccountPoolScope(t *testing.T) {
	group := storage.UserGroup{
		BlockClaudeTargetGroups: []string{"pool-a"},
		BlockGPTTargetGroups:    []string{"pool-b"},
	}
	caps := []storage.ModelCapability{
		{ModelSlug: "claude-opus-5"},
		{ModelSlug: "gpt-5.6-sol"},
		{ModelSlug: "gemini-3.2-pro"},
	}
	models := func(values []storage.ModelCapability) map[string]bool {
		out := make(map[string]bool, len(values))
		for _, capability := range values {
			out[capability.ModelSlug] = true
		}
		return out
	}

	poolA := models(filterUserGroupBlockedCapabilities(group, storage.TargetRef{
		Kind: storage.TargetKindAccountPoolGroup, ID: "pool-a",
	}, caps))
	if poolA["claude-opus-5"] || !poolA["gpt-5.6-sol"] || !poolA["gemini-3.2-pro"] {
		t.Fatalf("pool-a catalog filter=%v", poolA)
	}
	poolB := models(filterUserGroupBlockedCapabilities(group, storage.TargetRef{
		Kind: storage.TargetKindAccountPoolGroup, ID: "pool-b",
	}, caps))
	if !poolB["claude-opus-5"] || poolB["gpt-5.6-sol"] || !poolB["gemini-3.2-pro"] {
		t.Fatalf("pool-b catalog filter=%v", poolB)
	}
	provider := models(filterUserGroupBlockedCapabilities(group, storage.TargetRef{
		Kind: storage.TargetKindModelProvider, ID: "claude",
	}, caps))
	if len(provider) != len(caps) {
		t.Fatalf("provider target was incorrectly filtered: %v", provider)
	}
}

func TestPublicModelsOnlyIncludesVerifiedActiveRoutableAccounts(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	ctx := context.Background()
	accounts := []storage.Account{
		{ID: "active", GroupName: "cyber", Provider: "codex", Status: "active"},
		{ID: "quarantined", GroupName: "cyber", Provider: "codex", Status: "active", QuarantineUntil: storage.Now() + 3600},
		{ID: "unverified", GroupName: "cyber", Provider: "codex", Status: "active"},
		{ID: "unsupported", GroupName: "cyber", Provider: "codex", Status: "active"},
		{ID: "inactive", GroupName: "cyber", Provider: "codex", Status: "inactive"},
		{ID: "no-egress", GroupName: "cyber", Provider: "codex", Status: "active"},
	}
	for _, account := range accounts {
		if err := h.store.UpsertAccount(ctx, account, storage.AccountToken{AccessToken: "token"}); err != nil {
			t.Fatal(err)
		}
		if account.ID != "no-egress" {
			if err := h.store.UpsertEgressBinding(ctx, storage.AccountEgressBinding{AccountID: account.ID, PrimaryEgressID: storage.DefaultDirectEgressID}); err != nil {
				t.Fatal(err)
			}
		}
		state := capability.AvailabilityVerified
		if account.ID == "unverified" {
			state = capability.AvailabilityUnverified
		} else if account.ID == "unsupported" {
			state = capability.AvailabilityUnsupported
		}
		if err := h.store.UpsertCapabilities(ctx, []storage.ModelCapability{{AccountID: account.ID, ModelSlug: "gpt-" + account.ID, AvailabilityState: state, NativeContextWindow: 128000, Source: "probe"}}); err != nil {
			t.Fatal(err)
		}
	}

	response, err := http.Get(h.pool.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, raw)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 1 || body.Data[0].ID != "gpt-active" {
		t.Fatalf("non-routable capability polluted public list: %s", raw)
	}
}

func TestUserGroupModelsCatalogUsesTargetUnionAndProviderScope(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	ctx := t.Context()
	const (
		baseGroup    = "models-catalog-base"
		targetGroupA = "models-catalog-a"
		targetGroupB = "models-catalog-b"
		outsideGroup = "models-catalog-outside"
		userGroupID  = "models-catalog-user-group"
		autoKey      = "models-catalog-auto-key"
		agKey        = "models-catalog-antigravity-key"
	)
	for _, group := range []string{baseGroup, targetGroupA, targetGroupB, outsideGroup} {
		if err := h.store.CreateGroup(ctx, storage.Group{Name: group}); err != nil {
			t.Fatal(err)
		}
	}

	seedRoutableCatalogAccount(t, h, "catalog-account-a", targetGroupA, "codex", "gpt-target-a", "probe")
	seedRoutableCatalogAccount(t, h, "catalog-account-b", targetGroupB, "codex", "gpt-target-b", "probe")
	seedRoutableCatalogAccount(t, h, "catalog-account-outside", outsideGroup, "codex", "gpt-outside", "probe")
	seedRoutableCatalogAccount(t, h, "catalog-account-base-codex", baseGroup, "codex", "gpt-base-not-targeted", "probe")
	seedRoutableCatalogAccount(t, h, "catalog-account-antigravity", baseGroup, "antigravity", "gemini-target-ag", "antigravity_model_probe")

	targetA := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: targetGroupA}
	targetB := storage.TargetRef{Kind: storage.TargetKindAccountPoolGroup, ID: targetGroupB}
	targetAG := storage.TargetRef{Kind: storage.TargetKindModelProvider, ID: "antigravity"}
	targetClaude := storage.TargetRef{Kind: storage.TargetKindModelProvider, ID: "claude"}
	targetKiro := storage.TargetRef{Kind: storage.TargetKindModelProvider, ID: "kiro"}
	// CreateUserGroupDefinition appends unmentioned compatible targets as fallback
	// tiers. Therefore the catalog union remains reachable even with an explicit
	// per-model primary tier rather than advertising a dead target.
	allTargets := []storage.TargetRef{targetA, targetB, targetAG, targetClaude, targetKiro}
	createRouteTestGroup(t, h, userGroupID, allTargets, []storage.ModelRoutingRule{{
		Model: "gpt-target-a", Tiers: [][]storage.TargetRef{{targetA}},
	}})
	storedGroup, found, err := h.store.GetUserGroup(ctx, userGroupID)
	if err != nil || !found {
		t.Fatalf("load user group found=%v err=%v", found, err)
	}
	reachableTiers, err := compatibleUserGroupTiers(ctx, h.store, storedGroup, "gpt-target-a")
	if err != nil {
		t.Fatal(err)
	}
	reachableTargets := map[string]bool{}
	for _, tier := range reachableTiers {
		for _, target := range tier {
			reachableTargets[target.Kind+"\x00"+target.ID] = true
		}
	}
	for _, target := range allTargets {
		if !reachableTargets[target.Kind+"\x00"+target.ID] {
			t.Fatalf("catalog target is not reachable through normalized model routing: target=%+v tiers=%+v", target, reachableTiers)
		}
	}
	const explicitRaw = `{"model":"gemini-target-ag"}`
	boundRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	boundRequest.Header.Set("X-Claude-Code-Session-Id", "models-explicit-provider-affinity")
	boundAffinity := routing.ExtractClaudeAffinityKey(boundRequest, []byte(explicitRaw))
	if boundAffinity.Hash == "" {
		t.Fatal("explicit-provider fixture did not produce an affinity hash")
	}
	if err := h.store.UpsertUserGroupTargetBinding(ctx, storage.UserGroupTargetBinding{
		UserGroupID: userGroupID, AffinityKey: boundAffinity.Hash, Target: targetKiro,
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		pol  downstreamPolicy
		head bool
	}{
		{name: "header", pol: downstreamPolicy{Group: baseGroup, UserGroupID: userGroupID, ProviderHint: "auto"}, head: true},
		{name: "api key policy", pol: downstreamPolicy{Group: baseGroup, UserGroupID: userGroupID, ProviderHint: "antigravity"}},
	} {
		t.Run("explicit provider plan "+tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			request.Header.Set("X-Claude-Code-Session-Id", "models-explicit-provider-affinity")
			if tc.head {
				request.Header.Set("X-Pool-Provider", "antigravity")
			}
			plan, planErr := resolveUserGroupRouteCandidates(request.Context(), h.store, tc.pol, request, []byte(explicitRaw))
			if planErr != nil {
				t.Fatal(planErr)
			}
			seenAG := false
			for _, target := range plan.Candidates {
				if target.Kind != storage.TargetKindModelProvider {
					continue
				}
				if target.ID != "antigravity" {
					t.Fatalf("explicit Antigravity plan retained mismatched provider target: %+v", plan.Candidates)
				}
				seenAG = true
			}
			if !seenAG {
				t.Fatalf("explicit Antigravity plan dropped matching provider target: %+v", plan.Candidates)
			}
		})
	}
	for _, key := range []storage.APIKey{
		{KeyHash: hashAPIKey(autoKey), Label: "models auto", GroupName: baseGroup, UserGroupID: userGroupID, ProviderHint: "auto", Enabled: true},
		{KeyHash: hashAPIKey(agKey), Label: "models antigravity", GroupName: baseGroup, UserGroupID: userGroupID, ProviderHint: "antigravity", Enabled: true},
	} {
		if err := h.store.UpsertAPIKey(ctx, key); err != nil {
			t.Fatal(err)
		}
	}

	caps, err := h.app.modelsRoutableCapabilities(ctx, baseGroup, userGroupID)
	if err != nil {
		t.Fatal(err)
	}
	capModels := map[string]bool{}
	for _, cap := range caps {
		capModels[cap.ModelSlug] = true
	}
	for _, want := range []string{"gpt-target-a", "gpt-target-b", "gemini-target-ag"} {
		if !capModels[want] {
			t.Fatalf("target union missing %q: %+v", want, capModels)
		}
	}
	for _, hidden := range []string{"gpt-outside", "gpt-base-not-targeted"} {
		if capModels[hidden] {
			t.Fatalf("route-external model %q leaked into union: %+v", hidden, capModels)
		}
	}

	autoIDs := fetchOpenAIModelIDs(t, h, autoKey, "")
	for _, want := range []string{"gpt-target-a", "gpt-target-b", "gemini-target-ag"} {
		if !autoIDs[want] {
			t.Fatalf("auto catalog missing %q: %+v", want, autoIDs)
		}
	}
	for _, hidden := range []string{"gpt-outside", "gpt-base-not-targeted"} {
		if autoIDs[hidden] {
			t.Fatalf("auto catalog leaked %q: %+v", hidden, autoIDs)
		}
	}

	for name, ids := range map[string]map[string]bool{
		"header hint":            fetchOpenAIModelIDs(t, h, autoKey, "antigravity"),
		"api key hint":           fetchOpenAIModelIDs(t, h, agKey, ""),
		"anthropic api key hint": fetchAnthropicModelIDs(t, h, agKey, ""),
	} {
		if len(ids) != 1 || !ids["gemini-target-ag"] {
			t.Fatalf("%s escaped Antigravity target scope: %+v", name, ids)
		}
	}
}

func TestUserGroupModelsCatalogWithoutTargetsFallsBackToBaseGroup(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	ctx := t.Context()
	const (
		baseGroup   = "models-empty-target-base"
		userGroupID = "models-empty-target-user-group"
		plainKey    = "models-empty-target-key"
	)
	if err := h.store.CreateGroup(ctx, storage.Group{Name: baseGroup}); err != nil {
		t.Fatal(err)
	}
	seedRoutableCatalogAccount(t, h, "models-empty-target-account", baseGroup, "codex", "gpt-base-fallback", "probe")
	// CreateUserGroup is the legacy base-row API and intentionally permits a row
	// with no target children, which resolveUserGroupRoute treats as base-group mode.
	if err := h.store.CreateUserGroup(ctx, storage.UserGroup{ID: userGroupID, Name: userGroupID}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.UpsertAPIKey(ctx, storage.APIKey{
		KeyHash: hashAPIKey(plainKey), Label: "empty target fallback", GroupName: baseGroup,
		UserGroupID: userGroupID, ProviderHint: "auto", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	ids := fetchOpenAIModelIDs(t, h, plainKey, "")
	if len(ids) != 1 || !ids["gpt-base-fallback"] {
		t.Fatalf("empty-target fallback catalog=%+v", ids)
	}
}
