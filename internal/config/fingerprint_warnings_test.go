package config

import (
	"strings"
	"testing"
)

func hasWarning(ws []string, substr string) bool {
	for _, w := range ws {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

func TestFingerprintWarningsCleanByDefault(t *testing.T) {
	c := &Config{}
	if ws := c.FingerprintWarnings(); len(ws) != 0 {
		t.Fatalf("expected no warnings for an all-default (unset) fingerprint, got %v", ws)
	}
}

func TestFingerprintWarningsAllThreeCoherent(t *testing.T) {
	c := &Config{
		ClaudeCLIVersionOverride: "2.1.206",
		ClaudeStainlessVersion:   "0.94.0",
		ClaudeNodeVersion:        "v26.3.0",
	}
	if ws := c.FingerprintWarnings(); len(ws) != 0 {
		t.Fatalf("expected no warnings for three coherent axes, got %v", ws)
	}
}

func TestFingerprintWarningsPartialOverrideWarns(t *testing.T) {
	c := &Config{ClaudeCLIVersionOverride: "2.1.206"} // only one axis pinned
	ws := c.FingerprintWarnings()
	if !hasWarning(ws, "only some of") {
		t.Fatalf("expected a coherence warning, got %v", ws)
	}
}

func TestFingerprintWarningsBadShapes(t *testing.T) {
	c := &Config{
		ClaudeCLIVersionOverride: "v2", // not a dotted semver
		ClaudeStainlessVersion:   "latest",
		ClaudeNodeVersion:        "22.14.0", // missing leading v
	}
	ws := c.FingerprintWarnings()
	if !hasWarning(ws, "claude_cli_version") {
		t.Fatalf("expected claude_cli_version shape warning, got %v", ws)
	}
	if !hasWarning(ws, "claude_stainless_version") {
		t.Fatalf("expected claude_stainless_version shape warning, got %v", ws)
	}
	if !hasWarning(ws, "claude_node_version") {
		t.Fatalf("expected claude_node_version shape warning, got %v", ws)
	}
}

func TestLooksLikeDotVersion(t *testing.T) {
	good := []string{"2.1.206", "0.94.0", "1.0"}
	bad := []string{"v2", "latest", "2", "2.", ".2", "2.x.1", ""}
	for _, g := range good {
		if !looksLikeDotVersion(g) {
			t.Errorf("expected %q to look like a dot version", g)
		}
	}
	for _, b := range bad {
		if looksLikeDotVersion(b) {
			t.Errorf("expected %q NOT to look like a dot version", b)
		}
	}
}
