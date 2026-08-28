package capability

import (
	"testing"

	"codex-account-pool/internal/storage"
)

// Entitlement decisions moved from substring matching on the raw plan name to the
// normalized tier. That refactor is only safe if it is behaviour-preserving for
// every plan spelling that actually reaches the pool, so this table pins the 1M
// outcome for all of them. A future vocabulary change that shifts any of these
// rows is changing an authorization boundary and must be justified, not absorbed.
//
// The model is claude-opus-4-8: extended-context eligible (so the Supported branch
// is reachable) but not one of the generation-5 model-default-1M slugs (so the plan
// actually gets consulted instead of being short-circuited).
func TestClaudeOAuthOneMillionEntitlementUnchangedForEveryKnownPlanLiteral(t *testing.T) {
	cases := []struct {
		plan string
		want string
	}{
		// no tier evidence -> Unknown, never a denial
		{"", Context1MUnknown},
		{"api", Context1MUnknown},
		{"payg", Context1MUnknown},
		{"cursor", Context1MUnknown},
		{"cosmic", Context1MUnknown},

		// free and plus carry no 1M entitlement and never did
		{"free", Context1MUnknown},
		{"Free", Context1MUnknown},
		{"FREE", Context1MUnknown},
		{"KIRO FREE", Context1MUnknown},
		{"plus", Context1MUnknown},
		{"Plus", Context1MUnknown},

		// pro is explicitly capped at the standard window
		{"pro", Context1MUnsupported},
		{"Pro", Context1MUnsupported},
		{"PRO", Context1MUnsupported},
		{"KIRO PRO", Context1MUnsupported},
		{"Pro Plus", Context1MUnsupported},

		// max / team are the eligible OAuth tiers
		{"max", Context1MSupported},
		{"Max", Context1MSupported},
		{"max_20x", Context1MSupported},
		{"Plus Team", Context1MSupported},

		// business was not an eligible tier before and still is not
		{"self_serve_business_usage_based", Context1MUnknown},
	}

	for _, tc := range cases {
		caps := ParseStaticCopy("equivalence")
		if len(caps) == 0 {
			t.Fatal("static claude-opus-4-8 capability missing; fixture cannot prove anything")
		}
		got := ApplyClaudeAccountPolicy(caps,
			storage.Account{Provider: "claude", PlanType: tc.plan},
			storage.AccountToken{AuthMethod: "oauth", AccessToken: "oauth"})
		if got[0].Context1MState != tc.want {
			t.Errorf("plan %q: Context1MState = %q, want %q (source=%q)",
				tc.plan, got[0].Context1MState, tc.want, got[0].Context1MSource)
		}
	}
}

// A plan name that merely contains a tier word must not be read as that tier. The
// old `Contains(plan, "pro")` branch was evaluated first, so it also beat an
// explicit `max` appearing later in the same string.
func TestClaudeEntitlementIgnoresTierWordsEmbeddedInOtherWords(t *testing.T) {
	for _, plan := range []string{"provisional", "proxy_subscription"} {
		got := ApplyClaudeAccountPolicy(ParseStaticCopy("embedded"),
			storage.Account{Provider: "claude", PlanType: plan},
			storage.AccountToken{AuthMethod: "oauth", AccessToken: "oauth"})
		if got[0].Context1MState == Context1MUnsupported {
			t.Errorf("plan %q was denied 1M because its name contains %q as a substring",
				plan, "pro")
		}
	}

	// An explicit max tier must win even when an unrelated pro-ish word precedes it.
	got := ApplyClaudeAccountPolicy(ParseStaticCopy("compound"),
		storage.Account{Provider: "claude", PlanType: "provisional max"},
		storage.AccountToken{AuthMethod: "oauth", AccessToken: "oauth"})
	if got[0].Context1MState != Context1MSupported {
		t.Errorf("explicit max tier lost to a substring match: state=%q source=%q",
			got[0].Context1MState, got[0].Context1MSource)
	}
}

// KIRO FREE genuinely excludes Opus, and that must keep working. What must NOT
// happen is a paid plan losing Opus because the word "free" appears somewhere in
// its name — a silent capability denial with nothing logged to explain it.
func TestKiroBootstrapDeniesOpusOnlyForTheActualFreeTier(t *testing.T) {
	if KiroPlanAllowsBootstrap("KIRO FREE", "claude-opus-4-8") {
		t.Error("KIRO FREE must not bootstrap Opus")
	}
	if !KiroPlanAllowsBootstrap("KIRO FREE", "claude-sonnet-4-6") {
		t.Error("KIRO FREE must still bootstrap Sonnet")
	}
	if !KiroPlanAllowsBootstrap("KIRO PRO", "claude-opus-4-8") {
		t.Error("KIRO PRO must bootstrap Opus")
	}

	for _, paid := range []string{"business_free", "free_trial_enterprise", "FREE Business"} {
		if !KiroPlanAllowsBootstrap(paid, "claude-opus-4-8") {
			t.Errorf("paid plan %q lost Opus because its name contains \"free\"", paid)
		}
	}
}
