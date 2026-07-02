package upstream

import (
	"testing"

	"codex-account-pool/internal/config"
)

// TestClientUpdateConfigOverlay verifies the atomic config overlay behind Phase ①'s
// runtime hot-reload: before any UpdateConfig the client reads the boot config; after
// UpdateConfig the fingerprint/identity read sites see the new values without a
// restart or a client rebuild.
func TestClientUpdateConfigOverlay(t *testing.T) {
	cfg := config.Default()
	cfg.CodexJA3Override = "boot-codex"
	cfg.ClaudeJA3Override = "boot-claude"
	cfg.ClaudeForceDirect = false
	c := NewClient(cfg)

	if got := c.cfgSnapshot().CodexJA3Override; got != "boot-codex" {
		t.Fatalf("snapshot before update CodexJA3Override = %q, want boot-codex", got)
	}
	if c.cfgSnapshot().ClaudeForceDirect {
		t.Fatalf("snapshot before update ClaudeForceDirect = true, want false")
	}

	live := cfg
	live.CodexJA3Override = "live-codex"
	live.ClaudeJA3Override = "live-claude"
	live.ClaudeForceDirect = true
	c.UpdateConfig(live)

	if got := c.cfgSnapshot().CodexJA3Override; got != "live-codex" {
		t.Fatalf("snapshot after update CodexJA3Override = %q, want live-codex", got)
	}
	if got := c.cfgSnapshot().ClaudeJA3Override; got != "live-claude" {
		t.Fatalf("snapshot after update ClaudeJA3Override = %q, want live-claude", got)
	}
	if !c.cfgSnapshot().ClaudeForceDirect {
		t.Fatalf("snapshot after update ClaudeForceDirect = false, want true")
	}
}
