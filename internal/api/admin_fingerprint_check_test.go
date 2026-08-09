package api

import (
	"testing"

	"codex-account-pool/internal/upstream/tlsclient"
)

func TestInProcessProfileNameRecognizesCapturedClaudeProfile(t *testing.T) {
	for _, label := range []string{"claude", "claude-cli", "bun", tlsclient.ProfileClaude} {
		if got := inProcessProfileName(label); got != tlsclient.ProfileClaude {
			t.Errorf("inProcessProfileName(%q) = %q, want %q", label, got, tlsclient.ProfileClaude)
		}
	}
}

func TestAkamaiFingerprintMatchRecognizesClaudeHTTP1Shape(t *testing.T) {
	if !akamaiFingerprintMatches(tlsclient.ProfileClaude, "", "") {
		t.Fatal("two intentionally absent Claude HTTP/2 fingerprints must match")
	}
	if akamaiFingerprintMatches(tlsclient.ProfileClaude, "unexpected-h2", "unexpected-h2") {
		t.Fatal("Claude profile unexpectedly accepted an HTTP/2 fingerprint")
	}
	if !akamaiFingerprintMatches(tlsclient.ProfileChrome, "same", "same") {
		t.Fatal("matching browser HTTP/2 fingerprints were rejected")
	}
	if akamaiFingerprintMatches(tlsclient.ProfileChrome, "", "") {
		t.Fatal("browser profile accepted two missing HTTP/2 fingerprints")
	}
}
