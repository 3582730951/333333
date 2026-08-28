package plantier

import "testing"

// The literals below are the ones that actually occur: assigned in-repo, produced
// by ChatGPT JWT claims / `subscription_type` / Codex stream headers, or carried in
// third-party account bundles. Keeping them in one table means a new upstream
// vocabulary is a one-line change with a visible expectation.
func TestNormalizeCoversEveryObservedLiteral(t *testing.T) {
	cases := map[string]Tier{
		// assigned directly in-repo
		"api":    API,
		"cursor": Unknown, // a provider label, not a tier
		"cosmic": Unknown, // ditto
		"payg":   API,

		// ChatGPT / Codex vocabularies, all three casings seen in the wild
		"free": Free, "Free": Free, "FREE": Free,
		"plus": Plus, "Plus": Plus,
		"pro": Pro, "Pro": Pro, "PRO": Pro,
		"max": Max, "Max": Max,
		"max_20x": Max,

		// Kiro imports prefix the provider onto the tier
		"KIRO FREE": Free,
		"KIRO PRO":  Pro,

		// third-party bundles: compound names resolve to their strongest component
		"Plus Team": Team,
		"Pro Plus":  Pro,

		// sub2api
		"self_serve_business_usage_based": Business,

		// absent / whitespace is information-free, not a denial
		"":    Unknown,
		"   ": Unknown,
	}
	for raw, want := range cases {
		if got := Normalize(raw); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", raw, got, want)
		}
	}
}

// This is the defect the package exists to prevent. Substring matching on plan
// names silently denies model access to paying accounts, and the denial has no log
// line because from the code's point of view nothing went wrong.
func TestNormalizeDoesNotMatchTierNamesInsideOtherWords(t *testing.T) {
	traps := []string{
		"freedom",     // contains "free"
		"proxy",       // contains "pro"
		"provisional", // contains "pro"
		"maximus",     // contains "max"
		"teamster",    // contains "team"
		"surplus",     // contains "plus"
	}
	for _, raw := range traps {
		if got := Normalize(raw); got != Unknown {
			t.Errorf("Normalize(%q) = %q, want Unknown: a tier name embedded in another "+
				"word must not resolve to that tier", raw, got)
		}
	}
}

// A paid tier whose name also contains "free" must not read as free. This is the
// exact shape of the KiroPlanAllowsBootstrap defect: `Contains(plan, "FREE")`
// stripped Opus from any such plan.
func TestPaidTierContainingFreeResolvesToThePaidTier(t *testing.T) {
	for _, raw := range []string{"business_free", "FREE Business", "free_trial_enterprise"} {
		got := Normalize(raw)
		if got == Free || got == Unknown {
			t.Errorf("Normalize(%q) = %q; a compound name with a paid component must "+
				"resolve to the paid tier, not to free", raw, got)
		}
	}
}

func TestSameTierIgnoresSpellingButNotRealChange(t *testing.T) {
	same := [][2]string{
		{"pro", "Pro"},
		{"PRO", "pro"},
		{"KIRO PRO", "pro"}, // the write-amplification case
		{"Pro Plus", "pro"}, //
		{"max_20x", "Max"},  //
		{"self_serve_business_usage_based", "business"},
	}
	for _, pair := range same {
		if !SameTier(pair[0], pair[1]) {
			t.Errorf("SameTier(%q, %q) = false, want true", pair[0], pair[1])
		}
	}

	differ := [][2]string{
		{"plus", "pro"},
		{"pro", "max"},
		{"", "pro"}, // first value ever seen must still land
		{"free", "plus"},
	}
	for _, pair := range differ {
		if SameTier(pair[0], pair[1]) {
			t.Errorf("SameTier(%q, %q) = true, want false", pair[0], pair[1])
		}
	}
}

// Two unrecognized-but-different spellings must still compare as different, or the
// stored plan would freeze at whatever was written first and never track upstream.
func TestSameTierFallsBackToRawCompareWhenUnrecognized(t *testing.T) {
	if SameTier("cosmic", "cursor") {
		t.Error("two unknown plans with different text compared equal; a genuinely new " +
			"value would never be persisted")
	}
	if !SameTier("cosmic", "COSMIC") {
		t.Error("the raw fallback must stay case-insensitive")
	}
}
