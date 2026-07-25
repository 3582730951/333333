package api

import (
	"testing"
)

func TestNormalizeProviderHint(t *testing.T) {
	valid := []string{"auto", "codex", "claude", "kiro", "antigravity", "custom:myp"}
	for _, v := range valid {
		got, ok := normalizeProviderHint(v)
		if !ok || got != v {
			t.Errorf("normalizeProviderHint(%q): got %q ok=%v, want valid", v, got, ok)
		}
	}

	// Truly invalid: unrecognised names, and "custom:" with empty suffix.
	invalid := []string{"openai", "custom:", "custom: "}
	for _, v := range invalid {
		_, ok := normalizeProviderHint(v)
		if ok {
			t.Errorf("normalizeProviderHint(%q): expected invalid, got ok=true", v)
		}
	}

	// Case-normalisation: uppercase/mixed-case inputs fold to lowercase and are valid.
	cases := []struct{ in, want string }{
		{"AUTO", "auto"},
		{"Codex", "codex"},
		{"KIRO", "kiro"},
		{"ANTIGRAVITY", "antigravity"},
		// Empty string folds to "auto" via normalizeProviderHintLoose.
		{"", "auto"},
	}
	for _, tc := range cases {
		got, ok := normalizeProviderHint(tc.in)
		if !ok || got != tc.want {
			t.Errorf("normalizeProviderHint(%q): got %q ok=%v, want %q valid", tc.in, got, ok, tc.want)
		}
	}
}
