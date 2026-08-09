package scheduler

import (
	"context"
	"strings"
	"testing"

	"codex-account-pool/internal/config"
	"codex-account-pool/internal/storage"
)

// TestKiroModelBlockReasonsAreDistinguishable is the observability assertion for
// "this Kiro account has quota but is never selected". Every rejection inside
// resolveKiroRouteModel used to return the same bare `false`, collapsing ~15 distinct
// causes into one indistinguishable outcome. Different causes must now report different
// reasons, and a successful resolution must report none.
func TestKiroModelBlockReasonsAreDistinguishable(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	account := storage.Account{ID: "kiro-reasons", Label: "reasons", GroupName: "cyber", PlanType: "KIRO PRO"}
	seedUnverifiedKiroAccount(t, store, account)
	s := New(store, config.Default())

	baseRoute := Route{Group: "cyber", KiroDefaultRegion: "us-east-1"}

	// A model outside both the verified set and the static seed set.
	unknownRoute := baseRoute
	unknownRoute.Model = "gemini-3-pro"
	_, _, ok, unknownReason := s.resolveKiroRouteModelDetailed(ctx, account, unknownRoute)
	if ok {
		t.Fatal("an unknown model resolved")
	}
	if unknownReason == "" {
		t.Fatal("rejection reported no reason — the silent-skip defect")
	}

	// A 1m-context demand with no live catalog is a distinct, actionable cause.
	oneMRoute := baseRoute
	oneMRoute.Model = "claude-opus-5"
	oneMRoute.ContextMode = "1m"
	_, _, ok, oneMReason := s.resolveKiroRouteModelDetailed(ctx, account, oneMRoute)
	if ok {
		t.Fatal("1m context resolved without a complete live catalog")
	}
	if oneMReason != "kiro_1m_context_requires_live_catalog" {
		t.Fatalf("1m rejection reason = %q", oneMReason)
	}
	if oneMReason == unknownReason {
		t.Fatalf("two different causes share one reason (%q)", oneMReason)
	}

	// An endpoint outside the allowlist is a configuration mismatch, not quota.
	allowlistRoute := baseRoute
	allowlistRoute.Model = "claude-opus-5"
	allowlistRoute.KiroEndpointAllowlist = []string{"https://only-this.example"}
	if err := store.UpsertKiroCredentials(ctx, storage.KiroCredentials{
		AccountID: account.ID, AuthMethod: "api_key", KiroAPIKey: "kiro-key",
		APIRegion: "us-east-1", Endpoint: "https://something-else.example",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, reason := s.resolveKiroRouteModelDetailed(ctx, account, allowlistRoute); ok {
		t.Fatal("a non-allowlisted endpoint resolved")
	} else if reason != "kiro_endpoint_not_allowlisted" {
		t.Fatalf("allowlist rejection reason = %q", reason)
	}

	// A resolvable model reports no reason, and the wrapper keeps its old signature.
	if err := store.UpsertKiroCredentials(ctx, storage.KiroCredentials{
		AccountID: account.ID, AuthMethod: "api_key", KiroAPIKey: "kiro-key", APIRegion: "us-east-1",
	}); err != nil {
		t.Fatal(err)
	}
	okRoute := baseRoute
	okRoute.Model = "claude-opus-5"
	resolved, _, ok, reason := s.resolveKiroRouteModelDetailed(ctx, account, okRoute)
	if !ok {
		t.Fatalf("verified model did not resolve: reason=%q", reason)
	}
	if reason != "" {
		t.Errorf("successful resolution reported reason %q", reason)
	}
	wrapped, _, wrappedOK := s.resolveKiroRouteModel(ctx, account, okRoute)
	if !wrappedOK || wrapped != resolved {
		t.Errorf("wrapper disagrees with detailed resolver: %q/%t vs %q", wrapped, wrappedOK, resolved)
	}
}

// TestQualifiedBlockReasonPreservesClassification: the qualifier is diagnostic only. If
// it changed how a reason is classified, it would change scheduling behavior.
func TestQualifiedBlockReasonPreservesClassification(t *testing.T) {
	if got := qualifiedBlockReason(leaseBlockModelUnsupported, ""); got != leaseBlockModelUnsupported {
		t.Errorf("empty detail changed the reason: %q", got)
	}
	qualified := qualifiedBlockReason(leaseBlockModelUnsupported, "kiro_plan_forbids_bootstrap")
	if !strings.HasPrefix(string(qualified), string(leaseBlockModelUnsupported)) {
		t.Errorf("qualified reason lost its base: %q", qualified)
	}
	if !strings.Contains(string(qualified), "kiro_plan_forbids_bootstrap") {
		t.Errorf("qualified reason lost its detail: %q", qualified)
	}
	if baseBlockReason(qualified) != leaseBlockModelUnsupported {
		t.Errorf("baseBlockReason(%q) = %q", qualified, baseBlockReason(qualified))
	}

	// Classification of every base reason must be identical qualified or not.
	for _, base := range []leaseBlockReason{
		leaseBlockTokenBudget, leaseBlockConcurrency, leaseBlockRateLimitCooldown,
		leaseBlockRecheckPending, leaseBlockEgressCooldown, leaseBlockCoordinator,
		leaseBlockModelUnsupported, leaseBlockInactive, leaseBlockQuarantined,
	} {
		q := qualifiedBlockReason(base, "some_detail")
		if statefulStickyWaitReason(q) != statefulStickyWaitReason(base) {
			t.Errorf("%q: qualifier changed sticky-wait classification", base)
		}
		if waitReasonForBlock(q) != waitReasonForBlock(base) {
			t.Errorf("%q: qualifier changed wait reason (%q vs %q)", base, waitReasonForBlock(q), waitReasonForBlock(base))
		}
	}
}
